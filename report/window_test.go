package report

import (
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// cpuLogWindowFixture is a synthetic `mpstat -P ALL 1 N` log as the bench
// launches it (S_TIME_FORMAT=ISO TZ=UTC): ISO banner date, 24 h row
// times, %soft present. Twelve 1 Hz `all` rows from 10:00:00 to 10:00:11
// with busy = 100-%idle chosen so every window below has a distinct,
// hand-computable mean. Per-CPU rows are interleaved on two seconds to
// prove they are ignored.
const cpuLogWindowFixture = `Linux 7.0.0-30-generic (msa2-server) 	2026-09-13 	_x86_64_	(32 CPU)

10:00:00     CPU    %usr   %nice    %sys %iowait    %irq   %soft  %steal  %guest  %gnice   %idle
10:00:00     all   10.00    0.00    0.00    0.00    0.00    1.00    0.00    0.00    0.00   90.00
10:00:00       0   99.00    0.00    0.00    0.00    0.00    0.00    0.00    0.00    0.00    1.00
10:00:01     all   20.00    0.00    0.00    0.00    0.00    2.00    0.00    0.00    0.00   80.00
10:00:02     all   30.00    0.00    0.00    0.00    0.00    3.00    0.00    0.00    0.00   70.00
10:00:03     all   40.00    0.00    0.00    0.00    0.00    4.00    0.00    0.00    0.00   60.00
10:00:04     all   50.00    0.00    0.00    0.00    0.00    5.00    0.00    0.00    0.00   50.00
10:00:05     all   60.00    0.00    0.00    0.00    0.00    6.00    0.00    0.00    0.00   40.00
10:00:05       0   99.00    0.00    0.00    0.00    0.00    0.00    0.00    0.00    0.00    1.00
10:00:06     all   70.00    0.00    0.00    0.00    0.00    7.00    0.00    0.00    0.00   30.00
10:00:07     all   80.00    0.00    0.00    0.00    0.00    8.00    0.00    0.00    0.00   20.00
10:00:08     all   90.00    0.00    0.00    0.00    0.00    9.00    0.00    0.00    0.00   10.00
10:00:09     all   96.00    0.00    0.00    0.00    0.00   10.00    0.00    0.00    0.00    4.00
10:00:10     all   12.00    0.00    0.00    0.00    0.00   11.00    0.00    0.00    0.00   88.00
10:00:11     all   14.00    0.00    0.00    0.00    0.00   12.00    0.00    0.00    0.00   86.00
Average:     CPU    %usr   %nice    %sys %iowait    %irq   %soft  %steal  %guest  %gnice   %idle
Average:     all   47.67    0.00    0.00    0.00    0.00    6.50    0.00    0.00    0.00   52.33
`

// observationsDDLTicks is the v5.9 observer table: the v5.2 columns plus
// the cpu tick pair.
var observationsDDLTicks = observationsDDL[:len(observationsDDL)-2] + `,
	cpu_utime_ticks INTEGER,
	cpu_stime_ticks INTEGER
);`

type obsTickRow struct {
	ts, rss, utime, stime int64
}

func writeObserverDBTicks(t *testing.T, path string, rows []obsTickRow) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(observationsDDLTicks); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	stmt, err := db.Prepare(`INSERT INTO observations
		(ts, host, pid, fd_count, rss_bytes, goroutine_count, heap_inuse_bytes, gc_pause_p99_ns, accepted_conn_total, closed_conn_total, panic_count, cpu_utime_ticks, cpu_stime_ticks)
		VALUES (?, 'h', 1, 7, ?, 0, 0, 0, 0, 0, 0, ?, ?)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.Exec(r.ts, r.rss, r.utime, r.stime); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

func utc(t *testing.T, s string) time.Time {
	t.Helper()
	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parse %q: %v", s, err)
	}
	return v
}

// TestWindowResourcesTwoScenarios pins the per-scenario slice against
// hand-computed expectations: two scenarios share one column-wide
// cpu.log + observer.sqlite, each must see ONLY its own rows, the warm-up
// must be trimmed, and the column-wide mean must differ from both.
func TestWindowResourcesTwoScenarios(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	cpuPath := filepath.Join(dir, "cpu.log")
	if err := os.WriteFile(cpuPath, []byte(cpuLogWindowFixture), 0o644); err != nil {
		t.Fatalf("write cpu.log: %v", err)
	}
	colMean, cpu, ok, err := ParseMPStat(cpuPath)
	if err != nil || !ok {
		t.Fatalf("ParseMPStat: ok=%v err=%v", ok, err)
	}
	if len(cpu) != 12 {
		t.Fatalf("cpu rows: want 12 got %d", len(cpu))
	}
	// Column-wide mean over all 12 rows: (10+20+...+96+12+14)/12 = 572/12.
	if want := 572.0 / 12; abs(colMean-want) > 1e-9 {
		t.Fatalf("column mean=%v want %v", colMean, want)
	}
	base := utc(t, "2026-09-13T10:00:00Z").Unix()
	if cpu[0].TSUnix != base || cpu[11].TSUnix != base+11 {
		t.Fatalf("timestamps: first=%d last=%d want %d..%d", cpu[0].TSUnix, cpu[11].TSUnix, base, base+11)
	}
	if cpu[3].SoftPct != 4 {
		t.Fatalf("soft[3]=%v want 4", cpu[3].SoftPct)
	}

	// Observer: one row per second 10:00:00..10:00:11. The SUT burns 50
	// ticks/s (0.5 core) until 10:00:05, then 250 ticks/s (2.5 cores).
	dbPath := filepath.Join(dir, "observer.sqlite")
	var rows []obsTickRow
	ticks := int64(1000)
	for i := int64(0); i < 12; i++ {
		rows = append(rows, obsTickRow{ts: base + i, rss: 100 + i, utime: ticks, stime: 0})
		if i < 5 {
			ticks += 50
		} else {
			ticks += 250
		}
	}
	writeObserverDBTicks(t, dbPath, rows)
	samples, err := ParseObserverDB(dbPath)
	if err != nil {
		t.Fatalf("ParseObserverDB: %v", err)
	}
	if len(samples) != 12 || !samples[0].CPUTicksOK || samples[0].CPUTicks != 1000 {
		t.Fatalf("samples: n=%d ok=%v ticks=%d", len(samples), samples[0].CPUTicksOK, samples[0].CPUTicks)
	}

	const warmup = 2 * time.Second

	// Scenario A ran 10:00:00..10:00:05; the 2 s warm-up trims it to
	// rows 02..05: busy (30+40+50+60)/4 = 45, soft (3+4+5+6)/4 = 4.5.
	// Untrimmed it would be (10+20+30+40+50+60)/6 = 35 — the trim is
	// load-bearing. Process CPU over ticks[2]=1100 .. ticks[5]=1250 in 3 s
	// = 150/100/3*100 = 50 % of one core.
	sA, eA := ScenarioWindow(utc(t, "2026-09-13T10:00:00Z"), utc(t, "2026-09-13T10:00:05Z"), warmup)
	a, st := WindowResources(samples, cpu, sA, eA)
	if st != WindowOK || a == nil {
		t.Fatalf("A: status=%q stats=%v", st, a)
	}
	assertF(t, "A mean_cpu_pct", a.Summary.MeanCPUPct, 45)
	assertF(t, "A mean_soft_pct", a.Summary.MeanSoftPct, 4.5)
	assertF(t, "A sut_process_cpu_pct", a.Summary.SUTProcessCPUPct, 50)
	if a.Window == nil || a.Window.CPUSamples != 4 || a.Window.ObserverSamples != 4 {
		t.Fatalf("A window=%+v want 4/4 samples", a.Window)
	}
	if a.Window.Start != sA || a.Window.End != eA {
		t.Errorf("A window bounds=%v..%v want %v..%v", a.Window.Start, a.Window.End, sA, eA)
	}
	if a.Summary.PeakRSSBytes == nil || *a.Summary.PeakRSSBytes != 105 {
		t.Errorf("A peak_rss=%v want 105 (rows 02..05 only)", a.Summary.PeakRSSBytes)
	}

	// Scenario B ran 10:00:06..10:00:09; trimmed to rows 08..09: busy
	// (90+96)/2 = 93, soft (9+10)/2 = 9.5. Process CPU: ticks[8]=1250+3*250
	// = 2000 .. ticks[9]=2250 over 1 s = 250 %.
	sB, eB := ScenarioWindow(utc(t, "2026-09-13T10:00:06Z"), utc(t, "2026-09-13T10:00:09Z"), warmup)
	b, st := WindowResources(samples, cpu, sB, eB)
	if st != WindowOK || b == nil {
		t.Fatalf("B: status=%q stats=%v", st, b)
	}
	assertF(t, "B mean_cpu_pct", b.Summary.MeanCPUPct, 93)
	assertF(t, "B mean_soft_pct", b.Summary.MeanSoftPct, 9.5)
	assertF(t, "B sut_process_cpu_pct", b.Summary.SUTProcessCPUPct, 250)
	if b.Window.CPUSamples != 2 || b.Window.ObserverSamples != 2 {
		t.Fatalf("B window=%+v want 2/2 samples", b.Window)
	}

	// The two slices differ from each other and from the column mean —
	// the whole point: the shared column-wide number cannot stand in for
	// either scenario.
	if *a.Summary.MeanCPUPct == *b.Summary.MeanCPUPct || abs(*a.Summary.MeanCPUPct-colMean) < 1 || abs(*b.Summary.MeanCPUPct-colMean) < 1 {
		t.Errorf("slices must differ: A=%v B=%v column=%v", *a.Summary.MeanCPUPct, *b.Summary.MeanCPUPct, colMean)
	}

	// NEGATIVE CONTROL: a scenario whose window lies entirely after the
	// sampler stopped (10:00:30..10:00:40) must come back as NO DATA —
	// nil stats and the no_data status — not a zero-valued summary.
	sC, eC := ScenarioWindow(utc(t, "2026-09-13T10:00:30Z"), utc(t, "2026-09-13T10:00:40Z"), warmup)
	c, st := WindowResources(samples, cpu, sC, eC)
	if st != WindowNoData || c != nil {
		t.Fatalf("negative control: want (nil, %q) got (%+v, %q)", WindowNoData, c, st)
	}

	// A record without runner timestamps (pre-v5.9 per-cell JSON) is
	// distinguishable from an empty window.
	if got, st := WindowResources(samples, cpu, time.Time{}, time.Time{}); st != WindowNoTimestamps || got != nil {
		t.Errorf("no timestamps: want (nil, %q) got (%+v, %q)", WindowNoTimestamps, got, st)
	}
	// A cell that died inside its warm-up: start trimmed past the end.
	sD, eD := ScenarioWindow(utc(t, "2026-09-13T10:00:00Z"), utc(t, "2026-09-13T10:00:01Z"), warmup)
	if got, st := WindowResources(samples, cpu, sD, eD); st != WindowEmpty || got != nil {
		t.Errorf("empty window: want (nil, %q) got (%+v, %q)", WindowEmpty, got, st)
	}
	// No sidecar at all.
	if got, st := WindowResources(nil, nil, sA, eA); st != WindowNoSidecar || got != nil {
		t.Errorf("no sidecar: want (nil, %q) got (%+v, %q)", WindowNoSidecar, got, st)
	}
}

// TestWindowResourcesLegacyObserverNoTicks: a DB written by a pre-v5.9
// observer (no cpu tick columns) still slices, with sut_process_cpu_pct
// left nil rather than computed from zeros.
func TestWindowResourcesLegacyObserverNoTicks(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.sqlite")
	base := utc(t, "2026-09-13T10:00:00Z").Unix()
	writeObserverDB(t, dbPath, []obsRow{
		{ts: base, fd: 10, rss: 100},
		{ts: base + 1, fd: 10, rss: 100},
		{ts: base + 2, fd: 10, rss: 100},
	})
	samples, err := ParseObserverDB(dbPath)
	if err != nil {
		t.Fatalf("ParseObserverDB: %v", err)
	}
	for i, s := range samples {
		if s.CPUTicksOK {
			t.Fatalf("sample %d: CPUTicksOK=true on a tick-less DB", i)
		}
	}
	got, st := WindowResources(samples, nil, time.Unix(base, 0), time.Unix(base+2, 0))
	if st != WindowOK || got == nil {
		t.Fatalf("status=%q stats=%v", st, got)
	}
	if got.Summary.SUTProcessCPUPct != nil {
		t.Errorf("sut_process_cpu_pct=%v want nil (no tick columns)", *got.Summary.SUTProcessCPUPct)
	}
	if got.Summary.MeanCPUPct != nil {
		t.Errorf("mean_cpu_pct=%v want nil (no cpu rows)", *got.Summary.MeanCPUPct)
	}
	if got.Window.ObserverSamples != 3 || got.Window.CPUSamples != 0 {
		t.Errorf("window=%+v want 3 observer / 0 cpu samples", got.Window)
	}
}

// TestWindowResourcesZeroTicksAreAbsent: the observer writes 0/0 ticks
// when /proc/<pid>/stat is unreadable (the respawn supervisor replaced
// the PID it pinned). A window over such rows must report
// sut_process_cpu_pct=nil — absent — never a false 0 %.
func TestWindowResourcesZeroTicksAreAbsent(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.sqlite")
	base := utc(t, "2026-09-13T10:00:00Z").Unix()
	writeObserverDBTicks(t, dbPath, []obsTickRow{
		{ts: base, rss: 100, utime: 0, stime: 0},
		{ts: base + 1, rss: 100, utime: 0, stime: 0},
		{ts: base + 2, rss: 100, utime: 0, stime: 0},
	})
	samples, err := ParseObserverDB(dbPath)
	if err != nil {
		t.Fatalf("ParseObserverDB: %v", err)
	}
	for i, s := range samples {
		if s.CPUTicksOK {
			t.Fatalf("sample %d: 0/0 ticks must read as absent", i)
		}
	}
	got, st := WindowResources(samples, nil, time.Unix(base, 0), time.Unix(base+2, 0))
	if st != WindowOK || got == nil {
		t.Fatalf("status=%q stats=%v", st, got)
	}
	if got.Summary.SUTProcessCPUPct != nil {
		t.Errorf("sut_process_cpu_pct=%v want nil (dead-PID zeros are not a measurement)", *got.Summary.SUTProcessCPUPct)
	}
}

// TestParseMPStatWallClockVariants pins the timestamp reconstruction
// across the layouts the parser accepts: 12 h AM/PM rows with the C
// locale MM/DD/YY banner, and a midnight rollover, plus a banner with no
// recognisable date leaving TSUnix at 0 (so windowing reports no_data
// instead of joining on garbage).
func TestParseMPStatWallClockVariants(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	const ampmRollover = `Linux 5.4.0 (host) 	12/31/26 	_x86_64_	(4 CPU)

11:59:59 PM  CPU    %usr   %nice    %sys %iowait    %irq   %soft  %steal  %guest   %idle
11:59:59 PM  all   10.00    0.00    5.00    0.00    0.00    0.50    0.00    0.00   85.00
12:00:00 AM  all   20.00    0.00   10.00    0.00    0.00    1.50    0.00    0.00   70.00
`
	p := filepath.Join(dir, "ampm.log")
	if err := os.WriteFile(p, []byte(ampmRollover), 0o644); err != nil {
		t.Fatal(err)
	}
	_, series, ok, err := ParseMPStat(p)
	if err != nil || !ok || len(series) != 2 {
		t.Fatalf("ampm: ok=%v err=%v n=%d", ok, err, len(series))
	}
	want0 := utc(t, "2026-12-31T23:59:59Z").Unix()
	if series[0].TSUnix != want0 {
		t.Errorf("row0 ts=%d want %d", series[0].TSUnix, want0)
	}
	if series[1].TSUnix != want0+1 {
		t.Errorf("row1 ts=%d want %d (midnight rollover)", series[1].TSUnix, want0+1)
	}
	if series[0].SoftPct != 0.5 || series[1].SoftPct != 1.5 {
		t.Errorf("soft=%v,%v want 0.5,1.5", series[0].SoftPct, series[1].SoftPct)
	}

	const noDate = `Linux 5.4.0 (host) 	nodate 	_x86_64_	(4 CPU)

12:00:01     CPU    %usr   %nice    %sys %iowait    %irq  %steal  %guest   %idle
12:00:01     all   10.00    0.00    5.00    0.00    0.00    0.00    0.00   85.00
`
	p = filepath.Join(dir, "nodate.log")
	if err := os.WriteFile(p, []byte(noDate), 0o644); err != nil {
		t.Fatal(err)
	}
	_, series, ok, err = ParseMPStat(p)
	if err != nil || !ok || len(series) != 1 {
		t.Fatalf("nodate: ok=%v err=%v n=%d", ok, err, len(series))
	}
	if series[0].TSUnix != 0 {
		t.Errorf("nodate ts=%d want 0", series[0].TSUnix)
	}
	if series[0].SoftPct != -1 {
		t.Errorf("no %%soft column: SoftPct=%v want -1", series[0].SoftPct)
	}
	// And such a series cannot be windowed: no_data, not zero.
	if got, st := WindowResources(nil, series, time.Unix(0, 0), time.Unix(1<<40, 0)); st != WindowNoData || got != nil {
		t.Errorf("unplaceable rows: want (nil, %q) got (%+v, %q)", WindowNoData, got, st)
	}
}

// TestReduceResourcesCarriesWindowFields: the across-runs reduction keeps
// the v5.9 fields (median of the new scalars, last run's window) and
// leaves them nil for column-wide inputs.
func TestReduceResourcesCarriesWindowFields(t *testing.T) {
	t.Parallel()
	w := &ResourceWindow{CPUSamples: 3}
	r1 := &ResourceStats{Summary: ResourceSummary{MeanSoftPct: ptrF64(2), SUTProcessCPUPct: ptrF64(40)}}
	r2 := &ResourceStats{Summary: ResourceSummary{MeanSoftPct: ptrF64(4), SUTProcessCPUPct: ptrF64(60)}, Window: w}
	got := ReduceResources([]*ResourceStats{r1, r2})
	assertF(t, "mean_soft_pct", got.Summary.MeanSoftPct, 3)
	assertF(t, "sut_process_cpu_pct", got.Summary.SUTProcessCPUPct, 50)
	if got.Window != w {
		t.Errorf("window not carried from the last run")
	}
	plain := ReduceResources([]*ResourceStats{{Summary: ResourceSummary{MeanCPUPct: ptrF64(1)}}})
	if plain.Summary.MeanSoftPct != nil || plain.Summary.SUTProcessCPUPct != nil || plain.Window != nil {
		t.Errorf("column-wide reduction grew window fields: %+v", plain)
	}
}

func assertF(t *testing.T, name string, got *float64, want float64) {
	t.Helper()
	if got == nil {
		t.Fatalf("%s: nil, want %v", name, want)
	}
	if abs(*got-want) > 1e-9 {
		t.Errorf("%s: got %v want %v", name, *got, want)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
