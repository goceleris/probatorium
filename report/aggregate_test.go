package report

import (
	"bytes"
	"math"
	"strings"
	"testing"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/goceleris/loadgen"
)

// TestAggregateMedianAndCI feeds a known distribution and asserts the
// median + 5th/95th percentiles come back at the expected values.
func TestAggregateMedianAndCI(t *testing.T) {
	samples := []float64{150, 140, 160, 130, 170, 120, 180, 110, 190, 100, 200}
	cell := CellResult{
		ScenarioName: "get-json",
		ServerName:   "celeris-std-h1",
		ServerKind:   "celeris",
		Category:     "static",
		Samples:      makeSamples(samples, 0),
	}
	agg := Aggregate([]CellResult{cell})
	if len(agg) != 1 {
		t.Fatalf("Aggregate: want 1 entry, got %d", len(agg))
	}
	c := agg[CellID("get-json", "celeris-std-h1")]
	if c.N != 11 {
		t.Errorf("N: want 11, got %d", c.N)
	}
	if math.Abs(c.RPSMedian-150) > 0.01 {
		t.Errorf("RPSMedian: want 150, got %.2f", c.RPSMedian)
	}
	if math.Abs(c.RPSP5-105) > 0.5 {
		t.Errorf("RPSP5: want ~105, got %.2f", c.RPSP5)
	}
	if math.Abs(c.RPSP95-195) > 0.5 {
		t.Errorf("RPSP95: want ~195, got %.2f", c.RPSP95)
	}
	if math.Abs(c.RPSStdDev-33.17) > 0.5 {
		t.Errorf("RPSStdDev: want ~33.17, got %.2f", c.RPSStdDev)
	}
}

// TestAggregateLatencyMedianOfMedians checks that per-run P99s come
// through as a median-of-P99s in the legacy snapshot when no histogram
// payload is supplied.
func TestAggregateLatencyMedianOfMedians(t *testing.T) {
	samples := []loadgen.Result{
		{RequestsPerSec: 100, Latency: loadgen.Percentiles{
			P50: 100 * time.Microsecond, P99: 1 * time.Millisecond, P999: 2 * time.Millisecond, P9999: 3 * time.Millisecond,
		}},
		{RequestsPerSec: 100, Latency: loadgen.Percentiles{
			P50: 100 * time.Microsecond, P99: 2 * time.Millisecond, P999: 4 * time.Millisecond, P9999: 6 * time.Millisecond,
		}},
		{RequestsPerSec: 100, Latency: loadgen.Percentiles{
			P50: 100 * time.Microsecond, P99: 3 * time.Millisecond, P999: 6 * time.Millisecond, P9999: 9 * time.Millisecond,
		}},
	}
	agg := Aggregate([]CellResult{{ScenarioName: "sc", ServerName: "sv", Samples: samples}})
	c := agg[CellID("sc", "sv")]
	if c.LatencyMedian.P99 != 2*time.Millisecond {
		t.Errorf("P99 median: want 2ms, got %s", c.LatencyMedian.P99)
	}
	if c.LatencyMedian.P999 != 4*time.Millisecond {
		t.Errorf("P999 median: want 4ms, got %s", c.LatencyMedian.P999)
	}
	if c.LatencyMedian.P9999 != 6*time.Millisecond {
		t.Errorf("P9999 median: want 6ms, got %s", c.LatencyMedian.P9999)
	}
}

// TestAggregateErrorsSummed verifies errors are summed across runs.
func TestAggregateErrorsSummed(t *testing.T) {
	samples := []loadgen.Result{
		{RequestsPerSec: 10, Errors: 1},
		{RequestsPerSec: 10, Errors: 2},
		{RequestsPerSec: 10, Errors: 3},
	}
	agg := Aggregate([]CellResult{{ScenarioName: "s", ServerName: "v", Samples: samples}})
	c := agg[CellID("s", "v")]
	if c.Errors != 6 {
		t.Errorf("Errors: want 6, got %d", c.Errors)
	}
}

// TestAggregateEmpty ensures an empty samples slice produces a zero
// aggregate rather than a NaN one.
func TestAggregateEmpty(t *testing.T) {
	cell := CellResult{ScenarioName: "a", ServerName: "b"}
	agg := Aggregate([]CellResult{cell})
	c := agg[CellID("a", "b")]
	if c.N != 0 || c.RPSMedian != 0 || c.RPSStdDev != 0 {
		t.Errorf("empty cell: want zeroed aggregate, got %+v", c)
	}
}

// TestAggregateMergesHdrHistograms verifies that the V2-compressed
// HdrHistogram payloads passed in HistogramsB64 are merged into the
// aggregate's LatencyMerged field. We synthesise three histograms with
// different value distributions and check the merge surfaces a P99
// dominated by the heaviest tail.
func TestAggregateMergesHdrHistograms(t *testing.T) {
	mk := func(values []int64) string {
		h := hdr.New(1, 60_000_000, 3) // 1µs - 60s, 3 sig figs
		for _, v := range values {
			_ = h.RecordValue(v)
		}
		b, err := h.Encode(hdr.V2CompressedEncodingCookieBase)
		if err != nil {
			t.Fatalf("encode: %v", err)
		}
		return string(b)
	}

	// Histogram A: most weight at 100µs, a couple at 1ms.
	a := mk(append(repeatN(100, 9990), repeatN(1000, 10)...))
	// Histogram B: most weight at 200µs, a couple at 5ms.
	b := mk(append(repeatN(200, 9990), repeatN(5000, 10)...))
	// Histogram C: most weight at 300µs, a couple at 10ms.
	c := mk(append(repeatN(300, 9990), repeatN(10_000, 10)...))

	cell := CellResult{
		ScenarioName: "merge-test",
		ServerName:   "sv",
		Samples: []loadgen.Result{
			{RequestsPerSec: 100},
			{RequestsPerSec: 100},
			{RequestsPerSec: 100},
		},
		HistogramsB64: []string{a, b, c},
	}
	agg := Aggregate([]CellResult{cell})
	got := agg[CellID("merge-test", "sv")]

	if got.LatencyMerged == (Percentiles{}) {
		t.Fatalf("LatencyMerged is zero, expected populated from histograms")
	}
	if got.MergedHistogramB64 == "" {
		t.Errorf("MergedHistogramB64 is empty, expected re-encoded payload")
	}
	// P50 should be in the bulk (~200µs ± a tick); P99.9 should be
	// pulled by the 10ms tail. The exact numbers come from the
	// HdrHistogram bucket boundaries — assert ranges, not exact values.
	if got.LatencyMerged.P50 < 100*time.Microsecond || got.LatencyMerged.P50 > 400*time.Microsecond {
		t.Errorf("LatencyMerged.P50 = %s, want 100µs..400µs", got.LatencyMerged.P50)
	}
	if got.LatencyMerged.P9999 < 5*time.Millisecond {
		t.Errorf("LatencyMerged.P9999 = %s, want >= 5ms", got.LatencyMerged.P9999)
	}
}

// TestAggregateLegacyFallbackWhenNoHistograms ensures that a CellResult
// without any HistogramsB64 keeps LatencyMerged zeroed and the legacy
// LatencyMedian remains the only signal — no NaNs, no synthesised
// histograms.
func TestAggregateLegacyFallbackWhenNoHistograms(t *testing.T) {
	samples := makeSamples([]float64{100, 200, 300}, 50*time.Microsecond)
	cell := CellResult{
		ScenarioName: "legacy",
		ServerName:   "sv",
		Samples:      samples,
	}
	agg := Aggregate([]CellResult{cell})
	c := agg[CellID("legacy", "sv")]
	if c.LatencyMerged != (Percentiles{}) {
		t.Errorf("LatencyMerged unexpectedly populated: %+v", c.LatencyMerged)
	}
	if c.MergedHistogramB64 != "" {
		t.Errorf("MergedHistogramB64 unexpectedly populated: %q", c.MergedHistogramB64)
	}
	if c.LatencyMedian.P50 == 0 {
		t.Errorf("LatencyMedian.P50 unexpectedly zero")
	}
}

// TestWriteMarkdownRoundTrip validates the headline render produces the
// expected sections and bolds the SLO winner per scenario.
func TestWriteMarkdownRoundTrip(t *testing.T) {
	samples := makeSamples([]float64{100, 200, 300}, 50*time.Microsecond)
	a := CellResult{
		ScenarioName: "get-json", ServerName: "celeris-std-h1",
		ServerKind: "celeris", Category: "static", Samples: samples,
	}
	b := CellResult{
		ScenarioName: "get-json", ServerName: "fiber-h1",
		ServerKind: "fiber", Category: "static", Samples: makeSamples([]float64{90, 180, 270}, 60*time.Microsecond),
	}
	agg := Aggregate([]CellResult{a, b})

	doc := &Document{
		SchemaVersion: SchemaVersion,
		Benchmarks: []ServerResult{
			{
				Name: "celeris-std-h1",
				LatencyAtSLO: map[string]map[int]int{
					"get-json": {10: 200000, 50: 250000, 100: 250000, 500: 250000, 1000: 250000},
				},
			},
			{
				Name: "fiber-h1",
				LatencyAtSLO: map[string]map[int]int{
					"get-json": {10: 100000, 50: 150000, 100: 180000, 500: 180000, 1000: 180000},
				},
			},
		},
	}
	meta := Meta{
		GitRef: "testref",
		StartedAt:  time.Unix(1700000000, 0).UTC(),
		FinishedAt: time.Unix(1700001000, 0).UTC(),
		Host:       "unit-test",
		Runs:       3,
		Duration:   10 * time.Second,
		TotalCells: 2,
	}
	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, doc, agg, meta); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	md := buf.String()
	for _, want := range []string{
		"# probatorium report — testref",
		"## Latency at SLO",
		"### get-json",
		"≤10ms",
		"≤1000ms",
		"celeris-std-h1",
		"fiber-h1",
		"## Per-scenario detail",
		"### Static scenarios",
		"## Tail latency",
	} {
		if !strings.Contains(md, want) {
			t.Errorf("WriteMarkdown: missing %q in output:\n%s", want, md)
		}
	}
	// The celeris row carries the higher RPS at every SLO; expect the
	// number to be bolded somewhere in the headline table.
	if !strings.Contains(md, "**200.0k**") && !strings.Contains(md, "**250.0k**") {
		t.Errorf("WriteMarkdown: winner not bolded in latency_at_slo section:\n%s", md)
	}
}

// makeSamples synthesises loadgen.Result samples from a set of RPS
// values and a constant latency. Useful for quickly building test
// fixtures.
func makeSamples(rps []float64, latency time.Duration) []loadgen.Result {
	out := make([]loadgen.Result, len(rps))
	for i, r := range rps {
		out[i] = loadgen.Result{
			RequestsPerSec: r,
			ThroughputBPS:  r * 1024,
			Duration:       10 * time.Second,
			Latency: loadgen.Percentiles{
				P50:   latency,
				P90:   latency * 2,
				P99:   latency * 4,
				P999:  latency * 8,
				P9999: latency * 16,
				Max:   latency * 32,
			},
		}
	}
	return out
}

func repeatN(v int64, n int) []int64 {
	out := make([]int64, n)
	for i := range out {
		out[i] = v
	}
	return out
}
