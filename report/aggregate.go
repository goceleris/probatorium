// Package report aggregates per-cell loadgen samples and emits the v5.0
// JSON schema plus the markdown headline report.
//
// Aggregation strategy:
//
//   - Per-run scalars (RPS, errors, bytes/sec): we sort the per-run
//     values and take the sample median + 5th/95th percentiles as the
//     confidence bounds. Errors are summed across runs.
//   - Per-run latency histograms: we deserialise each per-run
//     HdrHistogram (V2-compressed base64 from loadgen) and merge them
//     into a single across-runs histogram before reading P50/P90/P99/
//     P99.9/P99.99/Max. This is exact — the median-of-percentiles
//     workaround the v4-era schema used (still available via the legacy
//     Percentiles fields below) is lossy whenever a run's tail
//     distribution is asymmetric.
//
// All statistics are stable under permutation of the input Samples
// slice, so running the same cells in a different order produces the
// same aggregate output.
package report

import (
	"errors"
	"io"
	"math"
	"sort"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/goceleris/loadgen"
)

// CellResult is the per-cell collection of samples produced by the
// orchestrator: one [loadgen.Result] per run, plus an optional parallel
// slice of HdrHistogram payloads (V2-compressed, base64-encoded).
//
// HistogramsB64 is a parallel slice — index i pairs with Samples[i] —
// of the HdrHistogram captured for that run. Empty strings mean the run
// did not produce a histogram (legacy loadgen build); aggregation falls
// back to the median-of-percentiles snapshot in that case. When at
// least one entry is non-empty, [Aggregate] decodes and merges them
// into a single across-runs histogram and reads exact percentiles off
// the merged result.
type CellResult struct {
	ScenarioName  string
	ServerName    string
	ServerKind    string
	Category      string
	Samples       []loadgen.Result
	HistogramsB64 []string
}

// Percentiles captures the latency percentile snapshot used by
// [CellAggregate]. Values are expressed as time.Duration to match the
// loadgen types and to keep the markdown formatter boundary-free.
type Percentiles struct {
	P50, P90, P99, P999, P9999 time.Duration
	Max                        time.Duration
}

// CellAggregate is the summary statistics for one (scenario, server)
// pair over every run.
type CellAggregate struct {
	ScenarioName string
	ServerName   string
	ServerKind   string
	Category     string
	N            int

	RPSMedian float64
	RPSP5     float64 // 5th percentile bound of the per-run RPS distribution
	RPSP95    float64 // 95th percentile bound of the per-run RPS distribution
	RPSStdDev float64

	// LatencyMedian is the legacy median-of-percentiles snapshot kept
	// for backward compatibility with v4-era consumers. Prefer
	// LatencyMerged for new code — it is computed from a merged
	// HdrHistogram across runs and is exact instead of approximate.
	LatencyMedian Percentiles

	// LatencyMerged is the percentile snapshot read off the
	// merged-across-runs HdrHistogram. Zero when no per-run histograms
	// were available (loadgen build < v5.0).
	LatencyMerged Percentiles

	// MergedHistogramB64 is the V2-compressed base64 encoding of the
	// merged-across-runs HdrHistogram. Stored verbatim in the v5.0 JSON
	// schema so downstream tools can re-merge across runs / hosts /
	// archs without re-running the bench.
	MergedHistogramB64 string

	Errors      int64
	BytesMedian float64
}

// ErrNotImplemented is returned by scaffold stubs that have not yet been
// filled in. Retained for API compatibility with earlier wave stubs.
var ErrNotImplemented = errors.New("probatorium/report: not yet implemented")

// CellID returns the canonical key used by Aggregate output maps.
func CellID(scenarioName, serverName string) string {
	return scenarioName + "/" + serverName
}

// Aggregate reduces per-cell samples to summary statistics keyed by
// "<scenarioName>/<serverName>". See the package documentation for the
// aggregation strategy.
func Aggregate(cells []CellResult) map[string]CellAggregate {
	out := make(map[string]CellAggregate, len(cells))
	for _, cell := range cells {
		agg := CellAggregate{
			ScenarioName: cell.ScenarioName,
			ServerName:   cell.ServerName,
			ServerKind:   cell.ServerKind,
			Category:     cell.Category,
			N:            len(cell.Samples),
		}
		if len(cell.Samples) == 0 {
			out[CellID(cell.ScenarioName, cell.ServerName)] = agg
			continue
		}

		rpsVals := make([]float64, 0, len(cell.Samples))
		bytesVals := make([]float64, 0, len(cell.Samples))
		var totalErrors int64
		for _, s := range cell.Samples {
			rpsVals = append(rpsVals, s.RequestsPerSec)
			bytesVals = append(bytesVals, s.ThroughputBPS)
			totalErrors += s.Errors
		}

		agg.RPSMedian = percentile(rpsVals, 50)
		agg.RPSP5 = percentile(rpsVals, 5)
		agg.RPSP95 = percentile(rpsVals, 95)
		agg.RPSStdDev = stddev(rpsVals)
		agg.BytesMedian = percentile(bytesVals, 50)
		agg.Errors = totalErrors
		agg.LatencyMedian = medianLatency(cell.Samples)

		// HdrHistogram merge — only emits LatencyMerged when at least one
		// sample carries a histogram payload. Otherwise we keep the
		// median-of-percentiles workaround.
		merged, b64 := mergeHistograms(cell.HistogramsB64)
		if merged != nil {
			agg.LatencyMerged = readPercentiles(merged)
			agg.MergedHistogramB64 = b64
		}

		out[CellID(cell.ScenarioName, cell.ServerName)] = agg
	}
	return out
}

// mergeHistograms decodes each V2-compressed base64 payload in raws
// and merges them into one across-runs histogram. The merged histogram
// and its re-encoded V2-compressed base64 are returned. If no payload
// decodes, both return values are zero (nil + "").
//
// Best-effort: a payload that fails to decode is skipped rather than
// failing the whole cell. A partial merge (some runs missing histograms)
// is more useful in the report than dropping the cell entirely.
func mergeHistograms(raws []string) (*hdr.Histogram, string) {
	var merged *hdr.Histogram
	for _, raw := range raws {
		if raw == "" {
			continue
		}
		h, err := hdr.Decode([]byte(raw))
		if err != nil || h == nil {
			continue
		}
		if merged == nil {
			merged = hdr.New(h.LowestTrackableValue(), h.HighestTrackableValue(),
				int(h.SignificantFigures()))
		}
		// Merge() drops samples that fall outside merged's tracked
		// range. Surface the merged result either way — a partial
		// merge is more useful than dropping the whole cell.
		_ = merged.Merge(h)
	}
	if merged == nil {
		return nil, ""
	}
	encoded, err := merged.Encode(hdr.V2CompressedEncodingCookieBase)
	if err != nil {
		return merged, ""
	}
	return merged, string(encoded)
}

// readPercentiles snapshots the standard percentile set from a
// HdrHistogram. ValueAtQuantile returns the count's value at the given
// quantile in the histogram's native units (microseconds for loadgen);
// we widen to time.Duration via Microsecond.
func readPercentiles(h *hdr.Histogram) Percentiles {
	if h == nil {
		return Percentiles{}
	}
	return Percentiles{
		P50:   time.Duration(h.ValueAtQuantile(50.0)) * time.Microsecond,
		P90:   time.Duration(h.ValueAtQuantile(90.0)) * time.Microsecond,
		P99:   time.Duration(h.ValueAtQuantile(99.0)) * time.Microsecond,
		P999:  time.Duration(h.ValueAtQuantile(99.9)) * time.Microsecond,
		P9999: time.Duration(h.ValueAtQuantile(99.99)) * time.Microsecond,
		Max:   time.Duration(h.Max()) * time.Microsecond,
	}
}

// medianLatency computes the per-percentile median across runs (the v4
// fallback). Each run contributes one value per percentile; we sort
// those slices independently and take the median of each, yielding a
// "typical tail" that isn't pulled by a single outlier run.
func medianLatency(samples []loadgen.Result) Percentiles {
	n := len(samples)
	if n == 0 {
		return Percentiles{}
	}
	p50 := make([]int64, n)
	p90 := make([]int64, n)
	p99 := make([]int64, n)
	p999 := make([]int64, n)
	p9999 := make([]int64, n)
	maxv := make([]int64, n)
	for i, s := range samples {
		p50[i] = int64(s.Latency.P50)
		p90[i] = int64(s.Latency.P90)
		p99[i] = int64(s.Latency.P99)
		p999[i] = int64(s.Latency.P999)
		p9999[i] = int64(s.Latency.P9999)
		maxv[i] = int64(s.Latency.Max)
	}
	return Percentiles{
		P50:   time.Duration(medianInt64(p50)),
		P90:   time.Duration(medianInt64(p90)),
		P99:   time.Duration(medianInt64(p99)),
		P999:  time.Duration(medianInt64(p999)),
		P9999: time.Duration(medianInt64(p9999)),
		Max:   time.Duration(medianInt64(maxv)),
	}
}

// percentile returns the p-th percentile (0..100) of vals using linear
// interpolation. The input is not modified.
func percentile(vals []float64, p float64) float64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	if n == 1 {
		return vals[0]
	}
	sorted := make([]float64, n)
	copy(sorted, vals)
	sort.Float64s(sorted)

	if p <= 0 {
		return sorted[0]
	}
	if p >= 100 {
		return sorted[n-1]
	}
	rank := p / 100 * float64(n-1)
	lo := int(math.Floor(rank))
	hi := int(math.Ceil(rank))
	if lo == hi {
		return sorted[lo]
	}
	frac := rank - float64(lo)
	return sorted[lo] + frac*(sorted[hi]-sorted[lo])
}

// medianInt64 returns the median of a []int64 (copy-sorted, non-destructive).
func medianInt64(vals []int64) int64 {
	n := len(vals)
	if n == 0 {
		return 0
	}
	sorted := make([]int64, n)
	copy(sorted, vals)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	if n%2 == 1 {
		return sorted[n/2]
	}
	return (sorted[n/2-1] + sorted[n/2]) / 2
}

// stddev returns the sample standard deviation (divisor n-1). Returns 0
// for n < 2 so callers can safely format without guarding NaN.
func stddev(vals []float64) float64 {
	n := len(vals)
	if n < 2 {
		return 0
	}
	var mean float64
	for _, v := range vals {
		mean += v
	}
	mean /= float64(n)
	var sumSq float64
	for _, v := range vals {
		d := v - mean
		sumSq += d * d
	}
	return math.Sqrt(sumSq / float64(n-1))
}

// WriteBenchstat is retained for API compatibility with earlier wave
// scaffolds. Not implemented.
func WriteBenchstat(w io.Writer, cells []CellResult) error {
	_ = w
	_ = cells
	return ErrNotImplemented
}
