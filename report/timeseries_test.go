package report

import (
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/goceleris/loadgen"
)

// makeSeries attaches a synthetic 1 Hz time-series to a loadgen.Result.
// rps[i] is the RequestsPerSec for elapsed second i; p99[i], when the
// slice is non-nil, is the per-bucket P99Ms. A nil p99 leaves every
// point's P99Ms at 0 (a legitimate measured value loadgen can emit).
// Errors default to 0; use makeSeriesErr to set per-bucket error deltas.
func makeSeries(rps []float64, p99 []float64) loadgen.Result {
	return makeSeriesErr(rps, p99, nil)
}

// makeSeriesErr is makeSeries plus a per-bucket Errors slice (nil leaves
// every point's Errors at 0).
func makeSeriesErr(rps []float64, p99 []float64, errs []int64) loadgen.Result {
	pts := make([]loadgen.TimeseriesPoint, len(rps))
	for i, r := range rps {
		pts[i] = loadgen.TimeseriesPoint{
			TimestampSec:   float64(i),
			RequestsPerSec: r,
		}
		if p99 != nil {
			pts[i].P99Ms = p99[i]
		}
		if errs != nil {
			pts[i].Errors = errs[i]
		}
	}
	return loadgen.Result{
		RequestsPerSec: meanOf(rps),
		Timeseries:     pts,
	}
}

func approxEq(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// TestMergeBand_RaggedRuns feeds three ragged runs (10/9/11 buckets) and
// checks the band union covers every elapsed second and that a fully-
// overlapping bucket's RPS band matches hand-computed values.
func TestMergeBand_RaggedRuns(t *testing.T) {
	// Bucket 0 is present in all three runs: values 100, 200, 300.
	r0 := make([]float64, 10)
	r1 := make([]float64, 9)
	r2 := make([]float64, 11)
	for i := range r0 {
		r0[i] = 100
	}
	for i := range r1 {
		r1[i] = 200
	}
	for i := range r2 {
		r2[i] = 300
	}
	r0[0], r1[0], r2[0] = 100, 200, 300

	runs := [][]loadgen.TimeseriesPoint{
		makeSeries(r0, nil).Timeseries,
		makeSeries(r1, nil).Timeseries,
		makeSeries(r2, nil).Timeseries,
	}
	band := mergeBand(runs)

	// Union of bucket keys is 0..10 (the longest run, 11 points).
	if len(band) != 11 {
		t.Fatalf("len(band) = %d, want 11 (union of 0..10)", len(band))
	}
	for i, b := range band {
		if b.TSec != int64(i) {
			t.Errorf("band[%d].TSec = %d, want %d", i, b.TSec, i)
		}
	}

	// Bucket 0 has all three runs: {100, 200, 300}.
	b0 := band[0].RPS
	if !approxEq(b0.Min, 100) {
		t.Errorf("bucket0 Min = %v, want 100", b0.Min)
	}
	if !approxEq(b0.Max, 300) {
		t.Errorf("bucket0 Max = %v, want 300", b0.Max)
	}
	if !approxEq(b0.Mean, 200) {
		t.Errorf("bucket0 Mean = %v, want 200", b0.Mean)
	}
	if !approxEq(b0.P50, 200) {
		t.Errorf("bucket0 P50 = %v, want 200", b0.P50)
	}
	// percentile(99) over {100,200,300} interpolates toward the max.
	if b0.P99 <= 200 || b0.P99 > 300 {
		t.Errorf("bucket0 P99 = %v, want in (200, 300]", b0.P99)
	}

	// Bucket 10 exists only in run2 (length 11): single value 300.
	b10 := band[10].RPS
	if !(approxEq(b10.Min, 300) && approxEq(b10.P50, 300) && approxEq(b10.Max, 300) && approxEq(b10.Mean, 300)) {
		t.Errorf("bucket10 (single run) band = %+v, want all 300", b10)
	}

	// loadgen always feeds P99Ms/Errors, so the bands are always emitted;
	// here every point carried 0, so every band is an all-zero BandStat.
	zero := BandStat{}
	for i, b := range band {
		if b.P99Ms == nil {
			t.Errorf("band[%d].P99Ms = nil, want populated", i)
		} else if *b.P99Ms != zero {
			t.Errorf("band[%d].P99Ms = %+v, want all-zero", i, *b.P99Ms)
		}
		if b.Errors == nil {
			t.Errorf("band[%d].Errors = nil, want populated", i)
		} else if *b.Errors != zero {
			t.Errorf("band[%d].Errors = %+v, want all-zero", i, *b.Errors)
		}
	}
}

// TestBuildScenarioSeries_PerRunRetention asserts every run's raw sample
// sequence + 1-based Run numbering survives into the ScenarioSeries.
func TestBuildScenarioSeries_PerRunRetention(t *testing.T) {
	samples := []loadgen.Result{
		makeSeries([]float64{10, 20, 30}, nil),
		makeSeries([]float64{40, 50}, nil),
	}
	ss := BuildScenarioSeries("get-json", "celeris-std-h1", "static", samples)

	if ss.Scenario != "get-json" || ss.Server != "celeris-std-h1" || ss.Category != "static" {
		t.Fatalf("keys drift: %+v", ss)
	}
	if len(ss.Runs) != 2 {
		t.Fatalf("len(Runs) = %d, want 2", len(ss.Runs))
	}
	if ss.Runs[0].Run != 1 || ss.Runs[1].Run != 2 {
		t.Errorf("Run numbering = %d,%d, want 1,2", ss.Runs[0].Run, ss.Runs[1].Run)
	}
	want0 := []float64{10, 20, 30}
	if len(ss.Runs[0].Samples) != len(want0) {
		t.Fatalf("run1 samples len = %d, want %d", len(ss.Runs[0].Samples), len(want0))
	}
	for i, row := range ss.Runs[0].Samples {
		if !approxEq(row.TSec, float64(i)) {
			t.Errorf("run1[%d].TSec = %v, want %v", i, row.TSec, float64(i))
		}
		if !approxEq(row.RPS, want0[i]) {
			t.Errorf("run1[%d].RPS = %v, want %v", i, row.RPS, want0[i])
		}
	}
}

// TestMergeBand_P99AndErrorsFed proves the cross-run P99Ms and Errors
// bands populate from the per-bucket values loadgen (>= v1.4.5) feeds on
// every tick, with the same min/p50/p99/max/mean envelope as the RPS band.
func TestMergeBand_P99AndErrorsFed(t *testing.T) {
	fed := []loadgen.Result{
		makeSeriesErr([]float64{100, 100}, []float64{1.0, 2.0}, []int64{10, 20}),
		makeSeriesErr([]float64{100, 100}, []float64{3.0, 4.0}, []int64{30, 40}),
	}
	ss := BuildScenarioSeries("s", "v", "", fed)
	if len(ss.Band) != 2 {
		t.Fatalf("len(band) = %d, want 2", len(ss.Band))
	}

	// Bucket 0 P99Ms values {1.0, 3.0}: min 1, max 3, mean 2.
	p := ss.Band[0].P99Ms
	if p == nil {
		t.Fatalf("band[0].P99Ms = nil, want populated")
	}
	if !approxEq(p.Min, 1.0) || !approxEq(p.Max, 3.0) || !approxEq(p.Mean, 2.0) || !approxEq(p.P50, 2.0) {
		t.Errorf("band[0].P99Ms = %+v, want min=1 p50=2 max=3 mean=2", *p)
	}

	// Bucket 0 Errors values {10, 30}: min 10, max 30, mean 20.
	e := ss.Band[0].Errors
	if e == nil {
		t.Fatalf("band[0].Errors = nil, want populated")
	}
	if !approxEq(e.Min, 10) || !approxEq(e.Max, 30) || !approxEq(e.Mean, 20) || !approxEq(e.P50, 20) {
		t.Errorf("band[0].Errors = %+v, want min=10 p50=20 max=30 mean=20", *e)
	}
}

// TestGzipRoundTrip checks MarshalGzip/UnmarshalGzip is lossless and that
// gzip actually shrinks a repetitive doc below its compact JSON.
func TestGzipRoundTrip(t *testing.T) {
	doc := &TimeseriesDoc{
		SchemaVersion: TimeseriesSchemaVersion,
		Scenarios: []ScenarioSeries{
			BuildScenarioSeries("get-json", "celeris-std-h1", "static",
				[]loadgen.Result{makeSeries([]float64{100, 200, 300}, nil)}),
			BuildScenarioSeries("get-text", "gin-h1", "static",
				[]loadgen.Result{makeSeries([]float64{50, 60, 70}, nil)}),
		},
	}

	gz, err := doc.MarshalGzip()
	if err != nil {
		t.Fatalf("MarshalGzip: %v", err)
	}

	var rt TimeseriesDoc
	if err := rt.UnmarshalGzip(gz); err != nil {
		t.Fatalf("UnmarshalGzip: %v", err)
	}
	// GeneratedAt is zero on both (we never set it); DeepEqual is safe.
	if !reflect.DeepEqual(doc, &rt) {
		t.Errorf("round-trip drift:\n got %+v\nwant %+v", rt, *doc)
	}
}

// TestEmptyAndSingleRun covers the degenerate inputs: empty Samples and a
// single run.
func TestEmptyAndSingleRun(t *testing.T) {
	empty := BuildScenarioSeries("s", "v", "", nil)
	if len(empty.Runs) != 0 || len(empty.Band) != 0 {
		t.Errorf("empty: Runs=%d Band=%d, want 0/0", len(empty.Runs), len(empty.Band))
	}

	single := BuildScenarioSeries("s", "v", "",
		[]loadgen.Result{makeSeries([]float64{42, 84}, nil)})
	if len(single.Band) != 2 {
		t.Fatalf("single: len(band) = %d, want 2", len(single.Band))
	}
	b := single.Band[0].RPS
	if !(approxEq(b.Min, 42) && approxEq(b.P50, 42) && approxEq(b.P99, 42) && approxEq(b.Max, 42) && approxEq(b.Mean, 42)) {
		t.Errorf("single-run bucket0 band = %+v, want all 42", b)
	}
}

// TestBuildTimeseries_FromCells is the integration over the runner path:
// []CellResult -> *TimeseriesDoc, deterministic (Scenario, Server) order.
func TestBuildTimeseries_FromCells(t *testing.T) {
	cells := []CellResult{
		{
			ScenarioName: "get-text", ServerName: "gin-h1", Category: "static",
			Samples: []loadgen.Result{
				makeSeries([]float64{50, 60}, nil),
				makeSeries([]float64{55, 65}, nil),
			},
		},
		{
			ScenarioName: "get-json", ServerName: "celeris-std-h1", Category: "static",
			Samples: []loadgen.Result{
				makeSeries([]float64{100, 200, 300}, nil),
				makeSeries([]float64{110, 210, 310}, nil),
				makeSeries([]float64{120, 220, 320}, nil),
			},
		},
	}
	doc := BuildTimeseries(cells)
	if doc.SchemaVersion != TimeseriesSchemaVersion {
		t.Errorf("SchemaVersion = %q, want %q", doc.SchemaVersion, TimeseriesSchemaVersion)
	}
	if len(doc.Scenarios) != 2 {
		t.Fatalf("len(Scenarios) = %d, want 2", len(doc.Scenarios))
	}
	// Sorted by (Scenario, Server): get-json < get-text.
	if doc.Scenarios[0].Scenario != "get-json" || doc.Scenarios[1].Scenario != "get-text" {
		t.Errorf("scenarios not sorted: %q, %q",
			doc.Scenarios[0].Scenario, doc.Scenarios[1].Scenario)
	}
	if len(doc.Scenarios[0].Runs) != 3 {
		t.Errorf("get-json runs = %d, want 3", len(doc.Scenarios[0].Runs))
	}
	if len(doc.Scenarios[0].Band) != 3 {
		t.Errorf("get-json band buckets = %d, want 3", len(doc.Scenarios[0].Band))
	}
}

// TestBuildScenarioSeries_FromLoadgenResults mirrors the cluster fan-in:
// a []loadgen.Result fed straight into BuildScenarioSeries yields the
// same Runs/Band the runner ([]CellResult) path produces.
func TestBuildScenarioSeries_FromLoadgenResults(t *testing.T) {
	samples := []loadgen.Result{
		makeSeries([]float64{100, 200, 300}, nil),
		makeSeries([]float64{110, 210, 310}, nil),
	}
	cluster := BuildScenarioSeries("get-json", "celeris", "msa2-server", samples)

	cells := []CellResult{{
		ScenarioName: "get-json", ServerName: "celeris", Category: "msa2-server",
		Samples: samples,
	}}
	runner := BuildTimeseries(cells).Scenarios[0]

	if !reflect.DeepEqual(cluster, runner) {
		t.Errorf("cluster vs runner path drift:\ncluster %+v\nrunner  %+v", cluster, runner)
	}
}

// TestMarkdownTimeseries covers the additive markdown section: nil and
// empty inputs render nothing; a populated doc renders the header + rows.
func TestMarkdownTimeseries(t *testing.T) {
	if got := MarkdownTimeseries(nil); got != "" {
		t.Errorf("MarkdownTimeseries(nil) = %q, want empty", got)
	}
	if got := MarkdownTimeseries(&TimeseriesDoc{}); got != "" {
		t.Errorf("MarkdownTimeseries(empty) = %q, want empty", got)
	}

	doc := BuildTimeseries([]CellResult{{
		ScenarioName: "get-json", ServerName: "celeris-std-h1", Category: "static",
		Samples: []loadgen.Result{makeSeries([]float64{100, 200, 300}, nil)},
	}})
	md := MarkdownTimeseries(doc)
	for _, want := range []string{"## Time-series", "get-json", "celeris-std-h1", "timeseries.json.gz"} {
		if !strings.Contains(md, want) {
			t.Errorf("MarkdownTimeseries: missing %q in:\n%s", want, md)
		}
	}
}
