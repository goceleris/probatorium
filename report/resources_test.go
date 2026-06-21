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

// rstat is a tiny constructor for a ResourceStats with a CPU + RSS scalar
// summary and one series point, for the aggregation/document flow tests.
func rstat(cpuPct float64, rssBytes, fdHWM int64) *ResourceStats {
	return &ResourceStats{
		Summary: ResourceSummary{
			MeanCPUPct:   ptrF64(cpuPct),
			PeakRSSBytes: ptrI64(rssBytes),
			FDHWM:        ptrI64(fdHWM),
		},
		Series: []ResourcePoint{{TSUnix: 1, CPUPct: ptrF64(cpuPct), RSSBytes: ptrI64(rssBytes)}},
	}
}

// TestReduceResourcesMediansAndSkipsNil pins the per-run reducer: medians
// each scalar across the runs that reported it, drops nil run entries, and
// keeps the last reporting run's series.
func TestReduceResourcesMediansAndSkipsNil(t *testing.T) {
	t.Parallel()
	runs := []*ResourceStats{
		rstat(40, 100, 10),
		nil, // a run with no observer sidecar — must be skipped, not panic
		rstat(60, 300, 30),
		rstat(50, 200, 20),
	}
	got := ReduceResources(runs)
	if got == nil {
		t.Fatal("ReduceResources returned nil for non-empty input")
	}
	if got.Summary.MeanCPUPct == nil || *got.Summary.MeanCPUPct != 50 {
		t.Errorf("MeanCPUPct=%v want 50 (median of 40,60,50)", got.Summary.MeanCPUPct)
	}
	if got.Summary.PeakRSSBytes == nil || *got.Summary.PeakRSSBytes != 200 {
		t.Errorf("PeakRSSBytes=%v want 200", got.Summary.PeakRSSBytes)
	}
	// Series is the LAST reporting run's (the 200/50 entry).
	if len(got.Series) != 1 || got.Series[0].CPUPct == nil || *got.Series[0].CPUPct != 50 {
		t.Errorf("Series drift: %+v", got.Series)
	}
	if ReduceResources(nil) != nil {
		t.Error("ReduceResources(nil) should be nil")
	}
	if ReduceResources([]*ResourceStats{nil, nil}) != nil {
		t.Error("ReduceResources of all-nil should be nil")
	}
}

// TestResourcesFlowAggregateToDocument is the regression guard for the bug
// the next bench round must not reship: server-side resources were captured
// per cell (observer.sqlite + cpu.log) but the merge → Aggregate →
// BuildDocument path dropped them, so every published Document carried an
// empty resources map. This drives a CellResult carrying per-run Resources
// all the way to ServerResult.Resources.
func TestResourcesFlowAggregateToDocument(t *testing.T) {
	t.Parallel()
	cell := CellResult{
		ScenarioName: "get-json-64k",
		ServerName:   "celeris-iouring-h1-async",
		// Two OK runs with real RPS so the cell is data-bearing.
		Samples: makeSamples([]float64{35000, 35200}, 0),
		Resources: []*ResourceStats{
			rstat(45, 100, 12),
			rstat(55, 120, 14),
		},
	}

	agg := Aggregate([]CellResult{cell})
	a, ok := agg[CellID(cell.ScenarioName, cell.ServerName)]
	if !ok {
		t.Fatal("aggregate missing cell")
	}
	if a.Resources == nil {
		t.Fatal("CellAggregate.Resources is nil — reduction dropped it")
	}
	if a.Resources.Summary.MeanCPUPct == nil || *a.Resources.Summary.MeanCPUPct != 50 {
		t.Errorf("aggregate MeanCPUPct=%v want 50", a.Resources.Summary.MeanCPUPct)
	}

	doc := BuildDocument(BuildInput{
		HostArchPair: "linux/amd64",
		Servers: map[string]ServerMeta{
			cell.ServerName: {Category: "celeris", Language: "go", Framework: "celeris"},
		},
		Agg: agg,
	})
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("benchmarks=%d want 1", len(doc.Benchmarks))
	}
	sr := doc.Benchmarks[0]
	if sr.Resources == nil {
		t.Fatal("ServerResult.Resources is nil — BuildDocument dropped resources")
	}
	r, ok := sr.Resources[cell.ScenarioName]
	if !ok || r == nil {
		t.Fatalf("no resources for scenario %q", cell.ScenarioName)
	}
	if r.Summary.MeanCPUPct == nil || *r.Summary.MeanCPUPct != 50 {
		t.Errorf("document MeanCPUPct=%v want 50", r.Summary.MeanCPUPct)
	}

	// And it must survive a JSON round-trip under the documented tag.
	b, err := json.Marshal(sr)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var back ServerResult
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if back.Resources[cell.ScenarioName] == nil {
		t.Error("resources lost across JSON round-trip")
	}
}
