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
