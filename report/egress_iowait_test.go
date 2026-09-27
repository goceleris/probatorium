package report

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// cpuLogIOUringShape is a two-row mpstat log with the per-mode split the
// retained 20260829 amd64 io_uring column averaged to (celeris#585 review:
// usr 17.6 sys 23.7 iowait 40.9 soft 14.1 idle 3.7).
const cpuLogIOUringShape = `Linux 7.0.0-30-generic (msa2-server) 	2026-08-29 	_x86_64_	(32 CPU)

09:00:13     CPU    %usr   %nice    %sys %iowait    %irq   %soft  %steal  %guest  %gnice   %idle
09:00:14     all   17.60    0.00   23.70   40.90    0.00   14.10    0.00    0.00    0.00    3.70
09:00:14       0   50.00    0.00    0.00   50.00    0.00    0.00    0.00    0.00    0.00    0.00
09:00:15     all   17.60    0.00   23.70   40.90    0.00   14.10    0.00    0.00    0.00    3.70
Average:     all   17.60    0.00   23.70   40.90    0.00   14.10    0.00    0.00    0.00    3.70
`

// cpuLogNoIOWait is a layout without the %iowait column (older sysstat
// builds and some kernels' reduced mpstat output).
const cpuLogNoIOWait = `Linux 6.8.0 (h) 	2026-08-29 	_x86_64_	(4 CPU)

09:00:13     CPU    %usr    %sys   %soft   %idle
09:00:14     all   40.00   10.00    5.00   45.00
`

// TestIOWaitIsNotBusy is the celeris#585 finding at report/resources.go:
// mean_cpu_pct = 100 - %idle counts %iowait as busy, and io_uring charges
// a worker waiting for completions in io_uring_enter as iowait. The
// iowait-excluding reading must say 55.4 where mean_cpu_pct says 96.3.
func TestIOWaitIsNotBusy(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpu.log")
	if err := os.WriteFile(path, []byte(cpuLogIOUringShape), 0o644); err != nil {
		t.Fatal(err)
	}
	mean, series, ok, err := ParseMPStat(path)
	if err != nil || !ok {
		t.Fatalf("ParseMPStat: ok=%v err=%v", ok, err)
	}
	if len(series) != 2 {
		t.Fatalf("series: %d rows, want the 2 `all` rows", len(series))
	}
	stats := SummarizeResources(nil, mean, ok, series)
	// Unchanged: the historical metric keeps its meaning.
	assertF(t, "mean_cpu_pct", stats.Summary.MeanCPUPct, 96.3)
	assertF(t, "mean_iowait_pct", stats.Summary.MeanIOWaitPct, 40.9)
	assertF(t, "mean_cpu_ex_iowait_pct", stats.Summary.MeanCPUExIOWaitPct, 55.4)
}

// Negative control: a log with no %iowait column must leave both new
// fields absent, not report 0 iowait / busy-as-executing.
func TestIOWaitAbsentWithoutTheColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cpu.log")
	if err := os.WriteFile(path, []byte(cpuLogNoIOWait), 0o644); err != nil {
		t.Fatal(err)
	}
	mean, series, ok, err := ParseMPStat(path)
	if err != nil || !ok {
		t.Fatalf("ParseMPStat: ok=%v err=%v", ok, err)
	}
	stats := SummarizeResources(nil, mean, ok, series)
	assertF(t, "mean_cpu_pct", stats.Summary.MeanCPUPct, 55)
	if stats.Summary.MeanIOWaitPct != nil || stats.Summary.MeanCPUExIOWaitPct != nil {
		t.Errorf("no %%iowait column: iowait=%v ex=%v, want both nil",
			stats.Summary.MeanIOWaitPct, stats.Summary.MeanCPUExIOWaitPct)
	}
}

// observationsDDLEgress is the v5.17 observer table (cmd/observer schemaSQL).
var observationsDDLEgress = observationsDDLTicks[:len(observationsDDLTicks)-2] + `,
	engine_zc_sends_submitted INTEGER,
	engine_zc_notifs INTEGER,
	engine_inline_bytes INTEGER,
	engine_ring_bytes INTEGER,
	engine_bytes_written INTEGER
);`

// egRow is one observer row with the five egress counters; -1 is how the
// observer writes "not published by this scrape".
type egRow struct {
	ts                                     int64
	zc, notifs, inline, ring, bytesWritten int64
}

func writeObserverDBEgress(t *testing.T, path string, rows []egRow) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(observationsDDLEgress); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	for _, r := range rows {
		if _, err := db.Exec(`INSERT INTO observations
			(ts, host, pid, fd_count, rss_bytes, goroutine_count, heap_inuse_bytes, gc_pause_p99_ns,
			 accepted_conn_total, closed_conn_total, panic_count, cpu_utime_ticks, cpu_stime_ticks,
			 engine_zc_sends_submitted, engine_zc_notifs, engine_inline_bytes, engine_ring_bytes, engine_bytes_written)
			VALUES (?, 'h', 1, 10, 100, 0, 0, 0, 0, 0, 0, 1, 1, ?, ?, ?, ?, ?)`,
			r.ts, r.zc, r.notifs, r.inline, r.ring, r.bytesWritten); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

// TestWindowEgressDeltas: a scenario window reports what the SUT's engine
// did inside it -- the counter deltas first-to-last sample -- so a SEND_ZC
// A/B can require the treatment to have fired (ON: submits > 0 and most
// bytes on the ring) and the control to be clean (OFF: exactly 0).
func TestWindowEgressDeltas(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "observer.sqlite")
	base := utc(t, "2026-09-13T10:00:00Z").Unix()
	writeObserverDBEgress(t, dbPath, []egRow{
		// before the window: must not count
		{base + 0, 5, 5, 1000, 1000, 2000},
		{base + 1, 5, 5, 1000, 1000, 2000},
		// scenario A window [base+2, base+5]: ZC fired, ring carries 3/4
		{base + 2, 10, 9, 2000, 4000, 6000},
		{base + 3, 30, 29, 2500, 5500, 8000},
		{base + 4, 50, 49, 3000, 7000, 10000},
		{base + 5, 70, 70, 3000, 10000, 13000},
		// scenario B window [base+8, base+9]: nothing ZC, 0 is a reading
		{base + 8, 70, 70, 3000, 10000, 13000},
		{base + 9, 70, 70, 4000, 10000, 14000},
	})
	samples, err := ParseObserverDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	a, st := WindowResources(samples, nil, utc(t, "2026-09-13T10:00:02Z"), utc(t, "2026-09-13T10:00:05Z"))
	if st != WindowOK {
		t.Fatalf("A: status %s", st)
	}
	wantI(t, "A zc_sends_submitted", a.Summary.ZCSendsSubmitted, 60)
	wantI(t, "A zc_notifs", a.Summary.ZCNotifs, 61)
	wantI(t, "A inline_bytes", a.Summary.InlineBytes, 1000)
	wantI(t, "A ring_bytes", a.Summary.RingBytes, 6000)
	wantI(t, "A bytes_written", a.Summary.BytesWritten, 7000)
	assertF(t, "A ring_bytes_fraction", a.Summary.RingBytesFraction, 6000.0/7000.0)

	b, st := WindowResources(samples, nil, utc(t, "2026-09-13T10:00:08Z"), utc(t, "2026-09-13T10:00:09Z"))
	if st != WindowOK {
		t.Fatalf("B: status %s", st)
	}
	wantI(t, "B zc_sends_submitted", b.Summary.ZCSendsSubmitted, 0)
	wantI(t, "B ring_bytes", b.Summary.RingBytes, 0)
	assertF(t, "B ring_bytes_fraction", b.Summary.RingBytesFraction, 0)

	// 0 must survive into the JSON (an OFF arm's proof), nil must not appear.
	raw, err := json.Marshal(b.Summary)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"zc_sends_submitted":0`) {
		t.Errorf("a measured 0 must serialize, got %s", raw)
	}
}

// Negative controls for the egress fields: a scrape that did not publish a
// counter (-1), a DB from a pre-5.17 observer (no columns at all), and a
// counter that went backwards (another process answered) must all leave
// the field nil -- never a 0 that an OFF arm could be mistaken for, and
// never a negative or wrapped delta.
func TestWindowEgressAbsentIsNil(t *testing.T) {
	dir := t.TempDir()
	base := utc(t, "2026-09-13T10:00:00Z").Unix()

	unpublished := filepath.Join(dir, "unpublished.sqlite")
	writeObserverDBEgress(t, unpublished, []egRow{
		{base + 0, -1, -1, -1, -1, -1},
		{base + 1, -1, -1, -1, -1, -1},
	})
	samples, err := ParseObserverDB(unpublished)
	if err != nil {
		t.Fatal(err)
	}
	s := SummarizeResources(samples, 0, false, nil)
	if s.Summary.ZCSendsSubmitted != nil || s.Summary.RingBytes != nil || s.Summary.RingBytesFraction != nil {
		t.Errorf("unpublished counters: zc=%v ring=%v frac=%v, want nil", s.Summary.ZCSendsSubmitted, s.Summary.RingBytes, s.Summary.RingBytesFraction)
	}

	legacy := filepath.Join(dir, "legacy.sqlite")
	writeObserverDBTicks(t, legacy, []obsTickRow{{base, 100, 10, 10}, {base + 1, 100, 20, 20}})
	samples, err = ParseObserverDB(legacy)
	if err != nil {
		t.Fatalf("a v5.9 DB must still parse: %v", err)
	}
	s = SummarizeResources(samples, 0, false, nil)
	if s.Summary.ZCSendsSubmitted != nil || s.Summary.InlineBytes != nil {
		t.Errorf("pre-5.17 DB: egress must be nil, got zc=%v inline=%v", s.Summary.ZCSendsSubmitted, s.Summary.InlineBytes)
	}

	backwards := filepath.Join(dir, "backwards.sqlite")
	writeObserverDBEgress(t, backwards, []egRow{
		{base + 0, 900, 900, 50, 900, 950},
		{base + 1, 3, 3, 10, 3, 13},
	})
	samples, err = ParseObserverDB(backwards)
	if err != nil {
		t.Fatal(err)
	}
	s = SummarizeResources(samples, 0, false, nil)
	if s.Summary.ZCSendsSubmitted != nil || s.Summary.RingBytes != nil {
		t.Errorf("counter went backwards: zc=%v ring=%v, want nil", s.Summary.ZCSendsSubmitted, s.Summary.RingBytes)
	}
}

// A scrape that failed on the window's LAST second (the observer wrote -1)
// is a gap, not a reading: the delta runs to the last sample that did
// publish, and the window keeps its egress figures.
func TestWindowEgressScrapeGapAtTheEdge(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "observer.sqlite")
	base := utc(t, "2026-09-13T10:00:00Z").Unix()
	writeObserverDBEgress(t, dbPath, []egRow{
		{base + 0, 10, 10, 100, 100, 200},
		{base + 1, -1, -1, -1, -1, -1},
		{base + 2, 25, 24, 150, 400, 550},
		{base + 3, -1, -1, -1, -1, -1},
	})
	samples, err := ParseObserverDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s := SummarizeResources(samples, 0, false, nil)
	wantI(t, "zc_sends_submitted", s.Summary.ZCSendsSubmitted, 15)
	wantI(t, "ring_bytes", s.Summary.RingBytes, 300)
	wantI(t, "inline_bytes", s.Summary.InlineBytes, 50)
}

// A window in which ONE sample published the counters spans zero seconds:
// its first-to-last delta is 0, and a 0 there reads as a measured "the ZC arm
// never fired" -- exactly the B-arm proof (zc_sends_submitted == 0, present)
// the #585 verdict requires. It must be absent, as processCPUPct is when
// first == last (probatorium#411 review). Includes the realistic shape: a
// window whose other scrapes all failed (-1).
func TestWindowEgressOneSampleIsNotAReading(t *testing.T) {
	base := utc(t, "2026-09-13T10:00:00Z").Unix()
	for name, rows := range map[string][]egRow{
		"one row":            {{base + 0, 10, 10, 100, 100, 200}},
		"one published of 3": {{base + 0, -1, -1, -1, -1, -1}, {base + 1, 10, 10, 100, 100, 200}, {base + 2, -1, -1, -1, -1, -1}},
	} {
		t.Run(name, func(t *testing.T) {
			dbPath := filepath.Join(t.TempDir(), "observer.sqlite")
			writeObserverDBEgress(t, dbPath, rows)
			samples, err := ParseObserverDB(dbPath)
			if err != nil {
				t.Fatal(err)
			}
			s := SummarizeResources(samples, 0, false, nil).Summary
			if s.ZCSendsSubmitted != nil || s.ZCNotifs != nil || s.InlineBytes != nil || s.RingBytes != nil || s.BytesWritten != nil || s.RingBytesFraction != nil {
				t.Errorf("one published sample: zc=%v notifs=%v inline=%v ring=%v written=%v frac=%v, want all nil (a zero-second window measured nothing)",
					fmtI(s.ZCSendsSubmitted), fmtI(s.ZCNotifs), fmtI(s.InlineBytes), fmtI(s.RingBytes), fmtI(s.BytesWritten), fmtF(s.RingBytesFraction))
			}
		})
	}
}

// A counter that decreased BETWEEN the first and last published samples
// (another process answered one scrape, or the engine restarted) has no
// window delta, even when last >= first: 900 -> 3 -> 950 is not +50
// (CodeRabbit on probatorium#411, resources.go:507). The other counters of
// the same window, monotonic, keep theirs.
func TestWindowEgressIntermediateDecreaseIsNil(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "observer.sqlite")
	base := utc(t, "2026-09-13T10:00:00Z").Unix()
	//                        zc   notifs inline ring  written
	writeObserverDBEgress(t, dbPath, []egRow{
		{base + 0, 900, 900, 50, 900, 950},
		{base + 1, 3, 901, 60, 3, 1000},
		{base + 2, 950, 902, 70, 960, 1100},
	})
	samples, err := ParseObserverDB(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	s := SummarizeResources(samples, 0, false, nil).Summary
	if s.ZCSendsSubmitted != nil || s.RingBytes != nil || s.RingBytesFraction != nil {
		t.Errorf("zc 900->3->950, ring 900->3->960: zc=%v ring=%v frac=%v, want nil", fmtI(s.ZCSendsSubmitted), fmtI(s.RingBytes), fmtF(s.RingBytesFraction))
	}
	wantI(t, "zc_notifs (monotonic)", s.ZCNotifs, 2)
	wantI(t, "inline_bytes (monotonic)", s.InlineBytes, 20)
	wantI(t, "bytes_written (monotonic)", s.BytesWritten, 150)
}

func fmtI(p *int64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatInt(*p, 10)
}

func fmtF(p *float64) string {
	if p == nil {
		return "nil"
	}
	return strconv.FormatFloat(*p, 'g', 6, 64)
}

// ReduceResources must carry the v5.17 fields as medians across runs.
func TestReduceResourcesCarriesEgressAndIOWait(t *testing.T) {
	mk := func(zc int64, iow, ex, frac float64) *ResourceStats {
		return &ResourceStats{Summary: ResourceSummary{
			ZCSendsSubmitted: ptrI64(zc), MeanIOWaitPct: ptrF64(iow),
			MeanCPUExIOWaitPct: ptrF64(ex), RingBytesFraction: ptrF64(frac),
		}}
	}
	r := ReduceResources([]*ResourceStats{mk(10, 40, 55, 0.5), nil, mk(30, 42, 57, 0.7), mk(20, 41, 56, 0.6)})
	wantI(t, "zc", r.Summary.ZCSendsSubmitted, 20)
	assertF(t, "iowait", r.Summary.MeanIOWaitPct, 41)
	assertF(t, "ex", r.Summary.MeanCPUExIOWaitPct, 56)
	assertF(t, "frac", r.Summary.RingBytesFraction, 0.6)
}

func wantI(t *testing.T, name string, got *int64, want int64) {
	t.Helper()
	if got == nil {
		t.Errorf("%s: nil, want %d", name, want)
		return
	}
	if *got != want {
		t.Errorf("%s: %d, want %d", name, *got, want)
	}
}
