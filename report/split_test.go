package report

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

// splitSampleDocument builds a Document with two servers across two
// scenarios, each carrying a fake HDR blob and a resource series, so the
// split exercises both the stripped (HDR) and retained (series) paths.
func splitSampleDocument() *Document {
	rss := int64(1 << 20)
	cpu := 42.5
	return &Document{
		SchemaVersion: SchemaVersion,
		HostArchPair:  "linux/amd64",
		Environment: Environment{
			KernelSysctlsApplied: []string{"net.core.somaxconn=65535"},
			LoadgenHost:          "msa2-client",
			Fabric:               "3-host LACP 20G",
		},
		BenchmarkConfig: BenchmarkConfig{
			StartedAt:  time.Date(2026, 5, 31, 7, 30, 0, 0, time.UTC),
			FinishedAt: time.Date(2026, 5, 31, 7, 52, 0, 0, time.UTC),
			Runs:       3,
			Duration:   30 * time.Second,
			Warmup:     5 * time.Second,
			CelerisVer: "v1.4.13",
		},
		Benchmarks: []ServerResult{
			{
				Name:                    "celeris",
				Category:                "go",
				Language:                "go",
				SaturationModeRPS:       map[string]float64{"get-json": 1842301.4, "plaintext": 2103880.0},
				RatedModeP99AtTargetRPS: map[string]time.Duration{"get-json": 842000},
				LatencyAtSLO:            map[string]map[int]int{"get-json": {1: 1500000}},
				HdrHistogramB64:         map[string]string{"get-json": "AAAAaGVqc29uLWhkcg==", "plaintext": "AAAAcGxhaW50ZXh0LWhkcg=="},
				LoadgenCPUP95:           map[string]float64{"get-json": 88.0},
				SentVsHandledDeltaPct:   map[string]float64{"get-json": 0.1},
				Resources: map[string]*ResourceStats{
					"get-json": {
						Summary: ResourceSummary{PeakRSSBytes: &rss, MeanCPUPct: &cpu},
						Series:  []ResourcePoint{{TSUnix: 1, RSSBytes: &rss}, {TSUnix: 2, CPUPct: &cpu}},
					},
				},
			},
			{
				Name:                    "fasthttp",
				Category:                "go",
				Language:                "go",
				SaturationModeRPS:       map[string]float64{"get-json": 1500000.0},
				RatedModeP99AtTargetRPS: map[string]time.Duration{},
				LatencyAtSLO:            map[string]map[int]int{},
				HdrHistogramB64:         map[string]string{"get-json": "AAAAZmFzdGh0dHAtaGRy"},
				LoadgenCPUP95:           map[string]float64{},
				SentVsHandledDeltaPct:   map[string]float64{},
			},
		},
	}
}

func splitTestMeta() SplitMeta {
	return SplitMeta{
		Version:        "v1.4.13",
		Arch:           "x86_64",
		Date:           "20260531",
		RunID:          "run-1",
		GitSHA:         "fb7e49c",
		GitRef:         "refs/tags/v1.4.13",
		CelerisVersion: "v1.4.13",
		LoadgenVersion: "v1.4.4",
		GeneratedAt:    time.Date(2026, 5, 31, 7, 55, 12, 0, time.UTC),
	}
}

// TestSplitMergeRoundTrip is the load-bearing guarantee: Split then
// Merge reproduces the original Document, and Split never mutates its
// input.
func TestSplitMergeRoundTrip(t *testing.T) {
	orig := splitSampleDocument()
	before := mustJSON(t, orig)

	summary, hist, env := SplitDocument(orig, splitTestMeta())

	if after := mustJSON(t, orig); !bytes.Equal(before, after) {
		t.Fatalf("SplitDocument mutated its input")
	}

	// Summary must carry no HDR blobs but keep everything else,
	// including the resource series.
	for _, sr := range summary.Benchmarks {
		if len(sr.HdrHistogramB64) != 0 {
			t.Errorf("summary server %q still has HDR blobs: %v", sr.Name, sr.HdrHistogramB64)
		}
	}
	if got := summary.Benchmarks[0].Resources["get-json"]; got == nil || len(got.Series) != 2 {
		t.Errorf("resource series dropped from summary: %+v", got)
	}

	merged := MergeSplit(summary, hist)
	if !reflect.DeepEqual(orig, merged) {
		t.Errorf("round-trip mismatch\n got: %s\nwant: %s", mustJSON(t, merged), before)
	}

	// env mirrors the meta + doc provenance.
	if env.SchemaVersion != EnvSchemaVersion || env.Version != "v1.4.13" || env.Arch != "x86_64" {
		t.Errorf("env provenance wrong: %+v", env)
	}
	if env.Environment.LoadgenHost != "msa2-client" {
		t.Errorf("env environment not copied: %+v", env.Environment)
	}
	if hist.SchemaVersion != HistogramSchemaVersion {
		t.Errorf("hist schema_version = %q, want %q", hist.SchemaVersion, HistogramSchemaVersion)
	}
	if hist.Histograms["celeris"]["plaintext"] == "" || hist.Histograms["fasthttp"]["get-json"] == "" {
		t.Errorf("hist missing expected blobs: %+v", hist.Histograms)
	}
}

// TestSplitEmptyHistograms confirms a histogram-free run yields a
// non-nil but empty Histograms map and still round-trips.
func TestSplitEmptyHistograms(t *testing.T) {
	doc := splitSampleDocument()
	for i := range doc.Benchmarks {
		doc.Benchmarks[i].HdrHistogramB64 = map[string]string{}
	}
	want := mustJSON(t, doc)

	summary, hist, _ := SplitDocument(doc, splitTestMeta())
	if hist.Histograms == nil {
		t.Fatalf("Histograms must be non-nil even when empty")
	}
	if len(hist.Histograms) != 0 {
		t.Errorf("expected no histogram entries, got %v", hist.Histograms)
	}
	if got := mustJSON(t, MergeSplit(summary, hist)); !bytes.Equal(got, want) {
		t.Errorf("empty-hist round-trip mismatch\n got: %s\nwant: %s", got, want)
	}
}

// TestWriteTree exercises the on-disk layout: correct paths, valid gzip
// for the .gz files, and a histograms.json.gz that gunzips back to the
// split's HistogramDoc.
func TestWriteTree(t *testing.T) {
	doc := splitSampleDocument()
	meta := splitTestMeta()
	root := t.TempDir()

	// A real-looking timeseries sidecar to copy verbatim.
	ts := &TimeseriesDoc{
		GeneratedAt:   meta.GeneratedAt,
		SchemaVersion: TimeseriesSchemaVersion,
		Scenarios:     []ScenarioSeries{{Scenario: "get-json", Server: "celeris"}},
	}
	tsGz, err := ts.MarshalGzip()
	if err != nil {
		t.Fatalf("marshal timeseries: %v", err)
	}

	cellDir, err := WriteTree(root, doc, tsGz, meta)
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}

	wantDir := filepath.Join(root, "v1.4.13", "20260531", "x86_64")
	if cellDir != wantDir {
		t.Errorf("cell dir = %q, want %q", cellDir, wantDir)
	}

	for _, f := range []string{SummaryFile, HistogramsFile, TimeseriesFile, EnvFile} {
		if _, err := os.Stat(filepath.Join(cellDir, f)); err != nil {
			t.Errorf("missing %s: %v", f, err)
		}
	}

	// summary.json parses as a Document with no HDR.
	var summary Document
	readJSONFile(t, filepath.Join(cellDir, SummaryFile), &summary)
	if len(summary.Benchmarks[0].HdrHistogramB64) != 0 {
		t.Errorf("summary.json carried HDR blobs")
	}

	// histograms.json.gz gunzips to a HistogramDoc with the blobs.
	var hist HistogramDoc
	readGzipJSONFile(t, filepath.Join(cellDir, HistogramsFile), &hist)
	if hist.Histograms["celeris"]["get-json"] == "" {
		t.Errorf("histograms.json.gz missing celeris/get-json blob")
	}

	// timeseries.json.gz is the verbatim bytes we passed.
	got, err := os.ReadFile(filepath.Join(cellDir, TimeseriesFile))
	if err != nil {
		t.Fatalf("read timeseries: %v", err)
	}
	if !bytes.Equal(got, tsGz) {
		t.Errorf("timeseries.json.gz not copied verbatim")
	}

	// The on-disk summary + histograms reconstruct the original doc.
	if got := mustJSON(t, MergeSplit(&summary, &hist)); !bytes.Equal(got, mustJSON(t, doc)) {
		t.Errorf("on-disk round-trip mismatch")
	}
}

// TestCellRelDir locks the layout the docs ingest (scripts/lib/results.mjs)
// expects: run-1 stays flat, additional back-to-back runs go in a /<run-id>
// subdirectory so they never overwrite each other. The rated pass's
// run-K-rated/ sibling subdirectory is the same shape — it must land
// in its own subdir (NEVER the flat path, which would clobber the
// saturation pass, and NEVER a different cell) so both panels of a
// back-to-back run survive intact. A regression here silently
// collapses a multi-run baseline onto one clobbered tree.
func TestCellRelDir(t *testing.T) {
	base := SplitMeta{Version: "v1.4.15", Date: "20260605", Arch: "x86_64"}
	cases := []struct {
		runID string
		want  string
	}{
		{"", filepath.Join("v1.4.15", "20260605", "x86_64")},                           // unset → flat
		{DefaultRunID, filepath.Join("v1.4.15", "20260605", "x86_64")},                 // run-1 → flat
		{"run-2", filepath.Join("v1.4.15", "20260605", "x86_64", "run-2")},             // run-K → subdir
		{"run-3", filepath.Join("v1.4.15", "20260605", "x86_64", "run-3")},             // run-K → subdir
		{"run-1-rated", filepath.Join("v1.4.15", "20260605", "x86_64", "run-1-rated")}, // rated subdir
		{"run-2-rated", filepath.Join("v1.4.15", "20260605", "x86_64", "run-2-rated")}, // rated subdir
		{"run-3-rated", filepath.Join("v1.4.15", "20260605", "x86_64", "run-3-rated")}, // rated subdir
		{"run-2.soak", filepath.Join("v1.4.15", "20260605", "x86_64", "run-2.soak")},   // future variant — append verbatim
	}
	for _, c := range cases {
		m := base
		m.RunID = c.runID
		if got := CellRelDir(m); got != c.want {
			t.Errorf("CellRelDir(run=%q) = %q, want %q", c.runID, got, c.want)
		}
	}

	// WriteTree must land files under the run-K subdirectory for run-2+.
	doc := splitSampleDocument()
	meta := splitTestMeta()
	meta.RunID = "run-2"
	root := t.TempDir()
	cellDir, err := WriteTree(root, doc, nil, meta)
	if err != nil {
		t.Fatalf("WriteTree: %v", err)
	}
	if want := filepath.Join(root, CellRelDir(meta)); cellDir != want {
		t.Errorf("run-2 cell dir = %q, want %q", cellDir, want)
	}
	if _, err := os.Stat(filepath.Join(cellDir, SummaryFile)); err != nil {
		t.Errorf("run-2 summary not written under subdir: %v", err)
	}
}

// TestWriteTreeRatedPassIsSeparateSubdir is the end-to-end sanity
// check that the rated pass actually lands in its OWN subdir (so a
// back-to-back run's saturation grid at run-K is never overwritten)
// AND that the rated subdir carries the rated fields (LatencyAtSLO,
// RatedModeP99AtTargetRPS) the headline markdown needs. Together with
// TestCellRelDir (which locks the path layout) and the docs
// selftest.mjs multi-run case (which locks the index enumeration),
// this is the probatorium side of the rated-suffix contract.
func TestWriteTreeRatedPassIsSeparateSubdir(t *testing.T) {
	// Build a synthetic rated Document: LatencyAtSLO + RatedModeP99 +
	// a non-zero LoadgenCPUP95 + a non-zero SentVsHandledDeltaPct.
	ratedDoc := BuildDocument(BuildInput{
		HostArchPair: "linux/amd64",
		Environment:  Environment{KernelSysctlsApplied: []string{}, LoadgenHost: "h", Fabric: "loopback"},
		BenchmarkConfig: BenchmarkConfig{
			StartedAt: time.Unix(0, 0).UTC(), FinishedAt: time.Unix(0, 0).UTC(),
			Runs: 1, Duration: time.Second, Warmup: 0,
			GitRef: "v1", LoadgenVer: "v1", CelerisVer: "v1",
		},
		Servers: map[string]ServerMeta{
			"celeris-iouring-auto+upg-async": {Category: "celeris", Language: "go", Framework: "celeris", Engine: "iouring-auto+upg-async", CompileOptions: CompileOptionsFor("go", "amd64")},
		},
		Agg: map[string]CellAggregate{
			CellID("get-json", "celeris-iouring-auto+upg-async"): {
				ScenarioName: "get-json", ServerName: "celeris-iouring-auto+upg-async",
				N:         3,
				RPSMedian: 350000,
				RatedP99ByTarget: map[int]time.Duration{
					100000: 1 * time.Millisecond,
					300000: 8 * time.Millisecond,
				},
				LatencyAtSLO:          map[int]int{10: 350000, 50: 350000, 100: 350000},
				MergedHistogramB64:    "AAAA",
				LoadgenCPUP95:         0.62,
				SentVsHandledDeltaPct: 0.3,
			},
		},
	})

	root := t.TempDir()
	baseMeta := SplitMeta{
		Version: "v1.4.15", Date: "20260605", Arch: "x86_64",
		GeneratedAt: time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
	}

	// 1. Saturation pass at run-2 (the default back-to-back slot).
	satMeta := baseMeta
	satMeta.RunID = "run-2"
	satDir, err := WriteTree(root, ratedDoc, nil, satMeta)
	if err != nil {
		t.Fatalf("saturation WriteTree: %v", err)
	}

	// 2. Rated pass at run-2-rated (the new sibling sub-resource).
	ratedMeta := baseMeta
	ratedMeta.RunID = "run-2-rated"
	ratedDir, err := WriteTree(root, ratedDoc, nil, ratedMeta)
	if err != nil {
		t.Fatalf("rated WriteTree: %v", err)
	}

	// The two cells must be DIFFERENT directories (rated didn't
	// clobber the saturation grid at run-2).
	if satDir == ratedDir {
		t.Fatalf("rated pass wrote to the same dir as saturation pass: %s", satDir)
	}
	if want := filepath.Join(root, "v1.4.15", "20260605", "x86_64", "run-2"); satDir != want {
		t.Errorf("saturation dir = %q, want %q", satDir, want)
	}
	if want := filepath.Join(root, "v1.4.15", "20260605", "x86_64", "run-2-rated"); ratedDir != want {
		t.Errorf("rated dir = %q, want %q", ratedDir, want)
	}

	// 3. The rated cell's summary carries the rated fields (and the
	//    validity telemetry) — proves the rated pass is publishing a
	//    real, distinct panel rather than a no-op rerun.
	var satSum, ratedSum Document
	readJSONFile(t, filepath.Join(satDir, SummaryFile), &satSum)
	readJSONFile(t, filepath.Join(ratedDir, SummaryFile), &ratedSum)
	if got := ratedSum.Benchmarks[0].LatencyAtSLO["get-json"][100]; got != 350000 {
		t.Errorf("rated summary LatencyAtSLO[get-json][100]: want 350000, got %d", got)
	}
	if got, ok := ratedSum.Benchmarks[0].LoadgenCPUP95["get-json"]; !ok || got != 0.62 {
		t.Errorf("rated summary LoadgenCPUP95[get-json]: want 0.62, got %v (present=%v)", got, ok)
	}
	if got, ok := ratedSum.Benchmarks[0].SentVsHandledDeltaPct["get-json"]; !ok || got != 0.3 {
		t.Errorf("rated summary SentVsHandledDeltaPct[get-json]: want 0.3, got %v (present=%v)", got, ok)
	}

	// 4. env.json in each subdir carries the matching run_id (the
	//    dispatch-pointer field the docs workflow validates against).
	var satEnv, ratedEnv EnvDoc
	readJSONFile(t, filepath.Join(satDir, EnvFile), &satEnv)
	readJSONFile(t, filepath.Join(ratedDir, EnvFile), &ratedEnv)
	if satEnv.RunID != "run-2" {
		t.Errorf("saturation env.run_id: want run-2, got %q", satEnv.RunID)
	}
	if ratedEnv.RunID != "run-2-rated" {
		t.Errorf("rated env.run_id: want run-2-rated, got %q", ratedEnv.RunID)
	}
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return raw
}

func readJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if err := json.Unmarshal(raw, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}

func readGzipJSONFile(t *testing.T, path string, v any) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	zr, err := gzip.NewReader(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("gzip reader %s: %v", path, err)
	}
	defer func() { _ = zr.Close() }()
	dec, err := io.ReadAll(zr)
	if err != nil {
		t.Fatalf("gunzip %s: %v", path, err)
	}
	if err := json.Unmarshal(dec, v); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
}
