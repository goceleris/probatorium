package report

import (
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// observationsDDL mirrors cmd/observer/main.go's schemaSQL so the test
// fixture is the exact table the parser reads in production.
const observationsDDL = `
CREATE TABLE IF NOT EXISTS observations (
	ts INTEGER PRIMARY KEY,
	host TEXT,
	pid INTEGER,
	fd_count INTEGER,
	rss_bytes INTEGER,
	goroutine_count INTEGER,
	heap_inuse_bytes INTEGER,
	gc_pause_p99_ns INTEGER,
	accepted_conn_total INTEGER,
	closed_conn_total INTEGER,
	panic_count INTEGER
);`

// obsRow is a minimal builder for fixture rows.
type obsRow struct {
	ts, fd, rss, goroutines, heap, gc int64
}

func writeObserverDB(t *testing.T, path string, rows []obsRow) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(observationsDDL); err != nil {
		t.Fatalf("ddl: %v", err)
	}
	stmt, err := db.Prepare(`INSERT INTO observations
		(ts, host, pid, fd_count, rss_bytes, goroutine_count, heap_inuse_bytes, gc_pause_p99_ns, accepted_conn_total, closed_conn_total, panic_count)
		VALUES (?, 'h', 1, ?, ?, ?, ?, ?, 0, 0, 0)`)
	if err != nil {
		t.Fatalf("prepare: %v", err)
	}
	defer func() { _ = stmt.Close() }()
	for _, r := range rows {
		if _, err := stmt.Exec(r.ts, r.fd, r.rss, r.goroutines, r.heap, r.gc); err != nil {
			t.Fatalf("insert: %v", err)
		}
	}
}

const cpuLogFixture = `Linux 6.8.0 (msa2-server) 	05/30/2026 	_x86_64_	(8 CPU)

12:00:01     CPU    %usr   %nice    %sys %iowait    %irq   %soft  %steal  %guest  %gnice   %idle
12:00:01     all   10.00    0.00    5.00    0.00    0.00    0.00    0.00    0.00    0.00   85.00
12:00:01       0   12.00    0.00    6.00    0.00    0.00    0.00    0.00    0.00    0.00   82.00
12:00:02     all   20.00    0.00   10.00    0.00    0.00    0.00    0.00    0.00    0.00   70.00
12:00:02       0   22.00    0.00   11.00    0.00    0.00    0.00    0.00    0.00    0.00   67.00
12:00:03     all   30.00    0.00   15.00    0.00    0.00    0.00    0.00    0.00    0.00   55.00
Average:     CPU    %usr   %nice    %sys %iowait    %irq   %soft  %steal  %guest  %gnice   %idle
Average:     all   20.00    0.00   10.00    0.00    0.00    0.00    0.00    0.00    0.00   70.00
`

// cpuLogNoGnice is an older sysstat layout without the %gnice column and
// with an AM/PM-style timestamp prefix; it must yield the same `all`
// means via header-driven column detection.
const cpuLogNoGnice = `Linux 5.4.0 (host) 	05/30/2026 	_x86_64_	(4 CPU)

12:00:01 AM  CPU    %usr   %nice    %sys %iowait    %irq   %soft  %steal  %guest   %idle
12:00:01 AM  all   10.00    0.00    5.00    0.00    0.00    0.00    0.00    0.00   85.00
12:00:02 AM  all   20.00    0.00   10.00    0.00    0.00    0.00    0.00    0.00   70.00
12:00:03 AM  all   30.00    0.00   15.00    0.00    0.00    0.00    0.00    0.00   55.00
`

func TestParseMPStat(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	for _, tc := range []struct {
		name, content string
	}{
		{"with_gnice", cpuLogFixture},
		{"no_gnice_ampm", cpuLogNoGnice},
	} {
		path := filepath.Join(dir, tc.name+".log")
		if err := os.WriteFile(path, []byte(tc.content), 0o644); err != nil {
			t.Fatalf("write %s: %v", tc.name, err)
		}
		mean, series, ok, err := ParseMPStat(path)
		if err != nil {
			t.Fatalf("%s: ParseMPStat: %v", tc.name, err)
		}
		if !ok {
			t.Fatalf("%s: ok=false, want true", tc.name)
		}
		// 100-idle over the three `all` rows: (15 + 30 + 45) / 3 = 30.
		if mean != 30 {
			t.Errorf("%s: mean=%v want 30", tc.name, mean)
		}
		if len(series) != 3 {
			t.Fatalf("%s: series len=%d want 3 (Average: block must be skipped)", tc.name, len(series))
		}
		if series[0].CPUPct != 15 || series[2].CPUPct != 45 {
			t.Errorf("%s: series=%v want [15 30 45]", tc.name, series)
		}
	}

	// Missing file => ok=false, err set.
	if _, _, ok, err := ParseMPStat(filepath.Join(dir, "nope.log")); ok || err == nil {
		t.Errorf("missing file: want ok=false err!=nil, got ok=%v err=%v", ok, err)
	}
}

func TestResourceParseAndSummarize_Go(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.sqlite")
	// RSS ramps then steadies; goroutine increasing; gc/fd varying. The
	// trailing-80% window of 6 samples starts at index 1, so steady RSS
	// is the median of [120,150,150,150,150]MB == 150MB.
	mb := int64(1024 * 1024)
	writeObserverDB(t, dbPath, []obsRow{
		{ts: 100, fd: 20, rss: 100 * mb, goroutines: 50, heap: 30 * mb, gc: 100_000},
		{ts: 101, fd: 30, rss: 120 * mb, goroutines: 60, heap: 32 * mb, gc: 200_000},
		{ts: 102, fd: 40, rss: 150 * mb, goroutines: 70, heap: 35 * mb, gc: 150_000},
		{ts: 103, fd: 45, rss: 150 * mb, goroutines: 80, heap: 35 * mb, gc: 300_000},
		{ts: 104, fd: 42, rss: 150 * mb, goroutines: 75, heap: 34 * mb, gc: 250_000},
		{ts: 105, fd: 48, rss: 150 * mb, goroutines: 90, heap: 36 * mb, gc: 400_000},
	})

	samples, err := ParseObserverDB(dbPath)
	if err != nil {
		t.Fatalf("ParseObserverDB: %v", err)
	}
	if len(samples) != 6 {
		t.Fatalf("samples=%d want 6", len(samples))
	}
	if samples[0].TSUnix != 100 || samples[5].Goroutines != 90 {
		t.Errorf("samples ordering/decoding drift: %+v", samples)
	}

	cpuPath := filepath.Join(dir, "cpu.log")
	if err := os.WriteFile(cpuPath, []byte(cpuLogFixture), 0o644); err != nil {
		t.Fatalf("write cpu.log: %v", err)
	}
	mean, cpuSeries, cpuOK, err := ParseMPStat(cpuPath)
	if err != nil {
		t.Fatalf("ParseMPStat: %v", err)
	}

	stats := SummarizeResources(samples, mean, cpuOK, cpuSeries)
	s := stats.Summary

	if s.PeakRSSBytes == nil || *s.PeakRSSBytes != 150*mb {
		t.Errorf("PeakRSSBytes=%v want %d", s.PeakRSSBytes, 150*mb)
	}
	if s.SteadyRSSBytes == nil || *s.SteadyRSSBytes != 150*mb {
		t.Errorf("SteadyRSSBytes=%v want %d", s.SteadyRSSBytes, 150*mb)
	}
	if s.MeanCPUPct == nil || *s.MeanCPUPct != 30 {
		t.Errorf("MeanCPUPct=%v want 30", s.MeanCPUPct)
	}
	if s.FDHWM == nil || *s.FDHWM != 48 {
		t.Errorf("FDHWM=%v want 48", s.FDHWM)
	}
	if s.GoroutineHWM == nil || *s.GoroutineHWM != 90 {
		t.Errorf("GoroutineHWM=%v want 90", s.GoroutineHWM)
	}
	// p99 (nearest-rank) over [100k,200k,150k,300k,250k,400k] sorted is
	// the last element, 400k.
	if s.GCPauseP99Ns == nil || *s.GCPauseP99Ns != 400_000 {
		t.Errorf("GCPauseP99Ns=%v want 400000", s.GCPauseP99Ns)
	}
	if len(stats.Series) == 0 || len(stats.Series) > maxSeriesPoints {
		t.Errorf("series len=%d want 1..%d", len(stats.Series), maxSeriesPoints)
	}
	// First series point joins CPU positionally (cpuSeries[0]==15).
	if stats.Series[0].CPUPct == nil || *stats.Series[0].CPUPct != 15 {
		t.Errorf("series[0].CPUPct=%v want 15", stats.Series[0].CPUPct)
	}
	if stats.Series[0].Goroutines == nil {
		t.Errorf("series[0].Goroutines: want non-nil for Go cell")
	}
}

func TestResourceParseAndSummarize_NonGo(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "observer.sqlite")
	mb := int64(1024 * 1024)
	// goroutine/heap/gc all 0 — the observer ran with -metrics-url '' (a
	// non-Go competitor, or the current cluster config). RSS/FD populated.
	writeObserverDB(t, dbPath, []obsRow{
		{ts: 200, fd: 15, rss: 80 * mb, goroutines: 0, heap: 0, gc: 0},
		{ts: 201, fd: 18, rss: 90 * mb, goroutines: 0, heap: 0, gc: 0},
		{ts: 202, fd: 22, rss: 95 * mb, goroutines: 0, heap: 0, gc: 0},
	})
	samples, err := ParseObserverDB(dbPath)
	if err != nil {
		t.Fatalf("ParseObserverDB: %v", err)
	}
	cpuPath := filepath.Join(dir, "cpu.log")
	if err := os.WriteFile(cpuPath, []byte(cpuLogFixture), 0o644); err != nil {
		t.Fatalf("write cpu.log: %v", err)
	}
	mean, cpuSeries, cpuOK, err := ParseMPStat(cpuPath)
	if err != nil {
		t.Fatalf("ParseMPStat: %v", err)
	}

	stats := SummarizeResources(samples, mean, cpuOK, cpuSeries)
	s := stats.Summary

	// RSS / CPU / FD must be present.
	if s.PeakRSSBytes == nil || *s.PeakRSSBytes != 95*mb {
		t.Errorf("PeakRSSBytes=%v want %d", s.PeakRSSBytes, 95*mb)
	}
	if s.MeanCPUPct == nil {
		t.Errorf("MeanCPUPct: want non-nil")
	}
	if s.FDHWM == nil || *s.FDHWM != 22 {
		t.Errorf("FDHWM=%v want 22", s.FDHWM)
	}
	// Runtime metrics must be null for a non-Go cell.
	if s.GoroutineHWM != nil {
		t.Errorf("GoroutineHWM=%v want nil for non-Go cell", *s.GoroutineHWM)
	}
	if s.GCPauseP99Ns != nil {
		t.Errorf("GCPauseP99Ns=%v want nil for non-Go cell", *s.GCPauseP99Ns)
	}
	// Series points must carry FD/CPU but no goroutine/heap.
	for i, p := range stats.Series {
		if p.Goroutines != nil {
			t.Errorf("series[%d].Goroutines: want nil for non-Go cell", i)
		}
		if p.HeapInuseBytes != nil {
			t.Errorf("series[%d].HeapInuseBytes: want nil for non-Go cell", i)
		}
		if p.FDCount == nil {
			t.Errorf("series[%d].FDCount: want non-nil", i)
		}
	}
}

func TestResourceParseObserverDB_Missing(t *testing.T) {
	t.Parallel()
	if _, err := ParseObserverDB(filepath.Join(t.TempDir(), "nope.sqlite")); err == nil {
		t.Errorf("missing DB: want error, got nil")
	}
}

// TestResourceJSONRoundTrip confirms a ServerResult carrying a Resources
// map round-trips through JSON with nil metric pointers staying nil
// (omitempty) and present values surviving.
func TestResourceJSONRoundTrip(t *testing.T) {
	t.Parallel()
	peak := int64(150 * 1024 * 1024)
	cpu := 42.5
	fd := int64(48)
	sr := ServerResult{
		Name: "celeris-std-h1",
		Resources: map[string]*ResourceStats{
			"get-json": {
				Summary: ResourceSummary{
					PeakRSSBytes: &peak,
					MeanCPUPct:   &cpu,
					FDHWM:        &fd,
					// GoroutineHWM / GCPauseP99Ns / SteadyRSSBytes left nil.
				},
				Series: []ResourcePoint{
					{TSUnix: 100, RSSBytes: &peak, CPUPct: &cpu, FDCount: &fd},
				},
			},
		},
	}

	raw, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var rt ServerResult
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	got := rt.Resources["get-json"]
	if got == nil {
		t.Fatal("Resources[get-json] dropped on decode")
	}
	if got.Summary.PeakRSSBytes == nil || *got.Summary.PeakRSSBytes != peak {
		t.Errorf("PeakRSSBytes drift: %v", got.Summary.PeakRSSBytes)
	}
	if got.Summary.MeanCPUPct == nil || *got.Summary.MeanCPUPct != cpu {
		t.Errorf("MeanCPUPct drift: %v", got.Summary.MeanCPUPct)
	}
	if got.Summary.GoroutineHWM != nil {
		t.Errorf("GoroutineHWM: want nil after round-trip, got %v", *got.Summary.GoroutineHWM)
	}
	if got.Summary.GCPauseP99Ns != nil {
		t.Errorf("GCPauseP99Ns: want nil after round-trip, got %v", *got.Summary.GCPauseP99Ns)
	}
	if len(got.Series) != 1 || got.Series[0].TSUnix != 100 {
		t.Errorf("Series drift: %+v", got.Series)
	}
}
