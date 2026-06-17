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

	// RatedSamples is a parallel slice — index i pairs with Samples[i] —
	// of the rated (closed-loop, coordinated-omission-corrected) sweep for
	// that run: one (target RPS, P99) per rated pass. Nil when rated mode
	// was off, in which case LatencyAtSLO stays nil and the regression gate
	// sees no signal for this cell.
	RatedSamples [][]RatedSample

	// Resources is the per-run server-side resource aggregate (#154):
	// one [ResourceStats] per run that captured an observer.sqlite + cpu.log
	// sidecar (cluster path), reduced across runs by [Aggregate] into
	// CellAggregate.Resources. Unlike Samples this is NOT strictly parallel:
	// nil-resource runs are simply absent, so a run with loadgen samples but
	// no observer data contributes a sample without a resource entry. Nil
	// for the in-process loopback runner (no observer sidecar).
	Resources []*ResourceStats

	// Status is the per-cell outcome classification (schema v5.3+). The
	// zero value ("") is treated as [CellOK] when Samples are present —
	// [Aggregate] derives the effective status from ErrorMsg via
	// [ClassifyCellError] when Status is unset. A non-OK status means the
	// cell did not produce a real number and must not be ranked.
	Status CellStatus

	// ErrorMsg is the synthesised per-cell error string the orchestrator
	// recorded (e.g. "zero-request cell: …", "adapter start: …"). Empty
	// for an OK cell. [Aggregate] classifies it into Status when Status
	// is unset, so either path (in-process runner or cluster merge) can
	// hand a cell either pre-classified or as a raw error string.
	ErrorMsg string

	// RunStatuses is the per-run outcome sequence in execution order,
	// one entry per scheduled run of this cell (schema v5.4). Unlike
	// Status (the reduced cell-level outcome) it preserves the evidence
	// of every run, so an OK rerun cannot erase a prior crash. Nil when
	// the producer pre-dates v5.4.
	RunStatuses []CellStatus
}

// RatedSample is one rated pass: a target offered load and the
// coordinated-omission-corrected P99 measured at that load.
type RatedSample struct {
	TargetRPS float64
	P99       time.Duration
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

	// Status is the per-cell outcome (schema v5.3+). Only Status==CellOK
	// cells carry headline numbers (RPS / latency / histogram); for a
	// non-OK cell those fields are left zero and BuildDocument records
	// the cell in ServerResult.CellStatuses instead of ranking it.
	Status CellStatus

	// ErrorMsg is the per-cell error string that produced a non-OK
	// Status, surfaced for JSON detail / debugging. Empty for CellOK.
	ErrorMsg string

	// RunStatuses is the per-run outcome sequence carried through from
	// [CellResult] (schema v5.4). Nil for pre-v5.4 producers.
	RunStatuses []CellStatus

	RPSMedian float64
	RPSP5     float64 // 5th percentile bound of the per-run RPS distribution
	RPSP95    float64 // 95th percentile bound of the per-run RPS distribution
	RPSStdDev float64

	// LoadgenCPUP95 is the median (across runs) of the loadgen's
	// self-CPU P95 reading for this cell, expressed as a fraction of
	// one core (0.0–1.0+; >1.0 means the loadgen process was using
	// more than one core's worth of CPU). Anchors the read: a number
	// close to or above 1.0 here means the loadgen — not the server —
	// was the bottleneck and the saturation RPS is a loadgen ceiling,
	// not a server ceiling. Zero when no run reported a sample (the
	// loadgen build did not include a self-CPU sampler).
	LoadgenCPUP95 float64

	// SentVsHandledDeltaPct is the loadgen-side error rate as a proxy
	// for the (sent − handled) gap: total loadgen Errors ÷ total
	// Requests × 100. A value >2% is a release-gate signal that the
	// server is dropping connections or returning non-2xx replies; 0
	// means the loadgen saw no errors. Computed from the loadgen
	// counter pair (Requests, Errors) without needing a server-side
	// /metrics endpoint. Conservative upper bound — server-side
	// handled could be higher if some "errors" are 4xx the server
	// still processed. Zero when no Requests were recorded.
	SentVsHandledDeltaPct float64

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

	Errors int64

	// ConnectErrors is the summed-across-runs dial/handshake-failure
	// subset of Errors (loadgen Result.ConnectErrors, additive within
	// schema v5.4). Errors ≈ ConnectErrors reads "server unreachable",
	// not "server misbehaving". Zero for pre-ConnectErrors loadgen
	// builds.
	ConnectErrors uint64

	BytesMedian float64

	// RatedP99ByTarget maps an integer target-RPS bucket to the median
	// (across runs) coordinated-omission-corrected P99 measured at that
	// offered load. Nil when rated mode was off.
	RatedP99ByTarget map[int]time.Duration

	// LatencyAtSLO maps each SLO budget (ms, from SLOThresholds) to the
	// maximum target RPS whose median P99 stayed under that budget. Bigger
	// is better — this is the leaf the regression gate keys on. Nil when
	// rated mode was off, so a non-rated run emits no fake gate signal.
	LatencyAtSLO map[int]int

	// Resources is the across-runs reduction of the cell's server-side
	// resource sampling (#154): median of each scalar across the runs that
	// reported it, plus the last run's downsampled series. Nil when no run
	// carried observer data (e.g. the in-process loopback path, or a cell
	// whose observer sidecar produced nothing). BuildDocument surfaces it on
	// ServerResult.Resources so the report can rank by CPU/RSS efficiency —
	// the key lever for the network-bound large-payload cells, where raw RPS
	// converges at the NIC ceiling but CPU cost per byte still differs.
	Resources *ResourceStats
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
		// Effective status: honour a pre-classified Status when the
		// caller set one, otherwise derive it from the error string. An
		// unset Status on a cell with samples and no error is CellOK.
		status := cell.Status
		if status == "" {
			status = ClassifyCellError(cell.ErrorMsg)
		}

		agg := CellAggregate{
			ScenarioName: cell.ScenarioName,
			ServerName:   cell.ServerName,
			ServerKind:   cell.ServerKind,
			Category:     cell.Category,
			N:            len(cell.Samples),
			Status:       status,
			ErrorMsg:     cell.ErrorMsg,
			RunStatuses:  cell.RunStatuses,
		}
		// A cell that did not run (or ran but produced no real number) is
		// never emitted as a ranked datapoint: leave every headline field
		// zero so BuildDocument records it in CellStatuses instead.
		// Suspect cells DO carry data — integrity questionable, but the
		// number exists — so they fall through to the math below exactly
		// like OK cells; their non-OK Status still travels on the
		// aggregate so BuildDocument flags them (schema v5.4).
		if !status.HasData() {
			out[CellID(cell.ScenarioName, cell.ServerName)] = agg
			continue
		}
		if len(cell.Samples) == 0 {
			out[CellID(cell.ScenarioName, cell.ServerName)] = agg
			continue
		}

		rpsVals := make([]float64, 0, len(cell.Samples))
		bytesVals := make([]float64, 0, len(cell.Samples))
		cpuVals := make([]float64, 0, len(cell.Samples))
		var totalErrors, totalSent int64
		var totalConnectErrors uint64
		for _, s := range cell.Samples {
			rpsVals = append(rpsVals, s.RequestsPerSec)
			bytesVals = append(bytesVals, s.ThroughputBPS)
			cpuVals = append(cpuVals, s.CPUPctP95)
			totalErrors += s.Errors
			totalSent += s.Requests
			totalConnectErrors += s.ConnectErrors
		}

		agg.RPSMedian = percentile(rpsVals, 50)
		agg.RPSP5 = percentile(rpsVals, 5)
		agg.RPSP95 = percentile(rpsVals, 95)
		agg.RPSStdDev = stddev(rpsVals)
		agg.BytesMedian = percentile(bytesVals, 50)
		agg.Errors = totalErrors
		agg.ConnectErrors = totalConnectErrors
		// Validity telemetry. The loadgen's CPUPctP95 is a percent
		// (0–100+, normalised by available cores) — divide by 100 so
		// the on-wire unit is a fraction of one core, matching the
		// v5_minimal fixture convention. 0.0 means the loadgen
		// build did not sample self-CPU, which is itself a signal
		// (the consumer should treat the cell as
		// loadgen-bottleneck-untested).
		agg.LoadgenCPUP95 = percentile(cpuVals, 50) / 100.0
		if totalSent > 0 {
			agg.SentVsHandledDeltaPct = 100.0 * float64(totalErrors) / float64(totalSent)
		}
		agg.LatencyMedian = medianLatency(cell.Samples)

		// HdrHistogram merge — only emits LatencyMerged when at least one
		// sample carries a histogram payload. Otherwise we keep the
		// median-of-percentiles workaround.
		merged, b64 := mergeHistograms(cell.HistogramsB64)
		if merged != nil {
			agg.LatencyMerged = readPercentiles(merged)
			agg.MergedHistogramB64 = b64
		}

		reduceRated(cell.RatedSamples, &agg)

		// Server-side resource reduction (#154): median each scalar across
		// the runs that captured an observer sidecar. Nil when none did.
		agg.Resources = ReduceResources(cell.Resources)

		out[CellID(cell.ScenarioName, cell.ServerName)] = agg
	}
	return out
}

// reduceRated folds the per-run rated sweeps into RatedP99ByTarget (median
// P99 per integer target RPS, across runs) and LatencyAtSLO (the max
// sustained target RPS whose median P99 stays under each SLO budget). Both
// maps stay nil when no rated samples are present, so a non-rated cell emits
// no latency_at_slo leaf and the regression gate sees nothing to compare.
//
// LatencyAtSLO is a throughput-at-SLO metric (bigger is better) — never a
// raw latency — which is what keeps the gate's bigger-is-better sign correct.
func reduceRated(runs [][]RatedSample, agg *CellAggregate) {
	byTarget := map[int][]int64{}
	for _, run := range runs {
		for _, rs := range run {
			t := int(rs.TargetRPS + 0.5)
			byTarget[t] = append(byTarget[t], int64(rs.P99))
		}
	}
	if len(byTarget) == 0 {
		return
	}

	medByTarget := make(map[int]time.Duration, len(byTarget))
	for t, ps := range byTarget {
		medByTarget[t] = time.Duration(medianInt64(ps))
	}
	agg.RatedP99ByTarget = medByTarget

	slo := make(map[int]int, len(SLOThresholds))
	for _, ms := range SLOThresholds {
		budget := time.Duration(ms) * time.Millisecond
		best := 0
		for t, p99 := range medByTarget {
			if p99 <= budget && t > best {
				best = t
			}
		}
		if best > 0 {
			slo[ms] = best
		}
	}
	if len(slo) > 0 {
		agg.LatencyAtSLO = slo
	}
}

// BuildTimeseries folds the SAME []CellResult that [Aggregate] consumes
// into a standalone time-series sidecar [TimeseriesDoc]. CellResult
// already carries the per-run loadgen.Timeseries on its Samples, so no
// struct change is needed and the summary Document is untouched.
//
// Scenarios are sorted by (Scenario, Server) so the sidecar (modulo its
// GeneratedAt stamp) is byte-stable across permutations of the input.
func BuildTimeseries(cells []CellResult) *TimeseriesDoc {
	out := &TimeseriesDoc{
		GeneratedAt:   time.Now().UTC(),
		SchemaVersion: TimeseriesSchemaVersion,
	}
	for _, c := range cells {
		// Skip no-data cells (not_applicable / dnf): they carry no samples, so
		// they would only append empty ScenarioSeries entries and bloat the
		// sidecar — matching Aggregate / the markdown reducers (schema v5.3).
		// Suspect cells carry real samples and stay in (schema v5.4).
		if !c.Status.HasData() {
			continue
		}
		out.Scenarios = append(out.Scenarios,
			BuildScenarioSeries(c.ScenarioName, c.ServerName, c.Category, c.Samples))
	}
	sort.Slice(out.Scenarios, func(i, j int) bool {
		if out.Scenarios[i].Scenario != out.Scenarios[j].Scenario {
			return out.Scenarios[i].Scenario < out.Scenarios[j].Scenario
		}
		return out.Scenarios[i].Server < out.Scenarios[j].Server
	})
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
