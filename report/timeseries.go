// timeseries.go carries the per-run 1 Hz request-rate series loadgen
// captures on [loadgen.Result.Timeseries] into a standalone, gzip-
// friendly sidecar document — kept entirely separate from the v5.x
// summary [Document] so results.json stays byte-stable.
//
// Two emit paths feed this:
//
//   - The in-process runner folds its []CellResult through
//     [BuildTimeseries] (cmd/runner), which reuses CellResult.Samples
//     (each a loadgen.Result already carrying .Timeseries).
//   - The cluster fan-in (mage_bench) has no CellResult — it walks raw
//     loadgen.json cells — so it calls [BuildScenarioSeries] directly on
//     a []loadgen.Result it unmarshals per competitor.
//
// Each scenario keeps every run's raw series PLUS a cross-run band: a
// per-elapsed-second min/p50/p99/max/mean envelope over the per-bucket
// RPS, windowed P99 latency and per-window error delta. Alignment is by
// ELAPSED second (TimestampSec), never wall clock, because
// interleave.Schedule time-multiplexes the runs.
package report

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"math"
	"sort"
	"time"

	"github.com/goceleris/loadgen"
)

// TimeseriesSchemaVersion is the on-disk identifier for the sidecar. It
// is intentionally distinct from [SchemaVersion] (the v5.x summary
// schema): the sidecar versions independently of results.json.
const TimeseriesSchemaVersion = "timeseries/1"

// TimeseriesDoc is the top-level sidecar shape, serialised gzip-
// compressed to timeseries.json.gz next to results.json.
type TimeseriesDoc struct {
	GeneratedAt   time.Time        `json:"generated_at"`
	SchemaVersion string           `json:"schema_version"`
	Scenarios     []ScenarioSeries `json:"scenarios"`
}

// ScenarioSeries is the per-(scenario, server) time-series block: each
// run's raw samples plus the cross-run percentile band.
type ScenarioSeries struct {
	Scenario string `json:"scenario"`
	Server   string `json:"server"`
	Category string `json:"category,omitempty"`

	Runs []RunSeries  `json:"runs"`
	Band []BucketBand `json:"band"`
}

// RunSeries is one run's ordered 1 Hz samples. Run is 1-based.
type RunSeries struct {
	Run     int         `json:"run"`
	Samples []SampleRow `json:"samples"`
}

// SampleRow mirrors [loadgen.TimeseriesPoint]: elapsed-second, RPS, the
// per-bucket P99 latency (ms) and the per-bucket error delta. loadgen
// (>= v1.4.5) fills all four on every tick. P99Ms/Errors keep omitempty
// so a measured 0 (no errors / sub-ms p99) shrinks the on-disk JSON.
type SampleRow struct {
	TSec   float64 `json:"t_s"`
	RPS    float64 `json:"rps"`
	P99Ms  float64 `json:"p99_ms,omitempty"`
	Errors int64   `json:"errors,omitempty"`
}

// BucketBand is the cross-run envelope for one elapsed-second bucket. RPS
// is always present; P99Ms and Errors are pointers so a bucket with no
// contributing points (an empty union slot) can omit them, but loadgen
// (>= v1.4.5) feeds every point's P99Ms/Errors so they populate in
// practice — see [mergeBand].
type BucketBand struct {
	TSec   int64     `json:"t_s"`
	RPS    BandStat  `json:"rps"`
	P99Ms  *BandStat `json:"p99_ms,omitempty"`
	Errors *BandStat `json:"errors,omitempty"`
}

// BandStat is the min/p50/p99/max/mean summary over the per-run values
// in one bucket. This is a small-sample band over per-bucket RPS — NOT
// an HDR-merged latency percentile (true latency tails come from the
// HdrHistogram path in aggregate.go). Named "band" so it cannot be
// mistaken for a latency p99.
type BandStat struct {
	Min  float64 `json:"min"`
	P50  float64 `json:"p50"`
	P99  float64 `json:"p99"`
	Max  float64 `json:"max"`
	Mean float64 `json:"mean"`
}

// bucketKey buckets a point by its 0-based elapsed second.
func bucketKey(p loadgen.TimeseriesPoint) int64 {
	return int64(math.Floor(p.TimestampSec))
}

// sampleRowFrom projects a loadgen point into a SampleRow, carrying all
// four fields loadgen (>= v1.4.5) emits per tick: elapsed-second, RPS,
// windowed P99 latency (ms) and the per-window error delta.
func sampleRowFrom(p loadgen.TimeseriesPoint) SampleRow {
	return SampleRow{
		TSec:   p.TimestampSec,
		RPS:    p.RequestsPerSec,
		P99Ms:  p.P99Ms,
		Errors: p.Errors,
	}
}

// runSeriesFrom maps one run's points into a RunSeries, preserving order.
// run is 1-based.
func runSeriesFrom(run int, pts []loadgen.TimeseriesPoint) RunSeries {
	rows := make([]SampleRow, len(pts))
	for i, p := range pts {
		rows[i] = sampleRowFrom(p)
	}
	return RunSeries{Run: run, Samples: rows}
}

// mergeBand aligns every run's points by elapsed-second bucket and emits
// one BucketBand per bucket present in any run (ragged runs handled by
// union iteration). RPS, P99Ms and Errors bands all populate from the
// per-bucket values loadgen (>= v1.4.5) feeds on every tick; a measured
// 0 is a legitimate value, so the bands are emitted unconditionally for
// every bucket that has at least one contributing point.
func mergeBand(runs [][]loadgen.TimeseriesPoint) []BucketBand {
	rpsByBucket := map[int64][]float64{}
	p99ByBucket := map[int64][]float64{}
	errByBucket := map[int64][]float64{}
	for _, pts := range runs {
		for _, p := range pts {
			k := bucketKey(p)
			rpsByBucket[k] = append(rpsByBucket[k], p.RequestsPerSec)
			p99ByBucket[k] = append(p99ByBucket[k], p.P99Ms)
			errByBucket[k] = append(errByBucket[k], float64(p.Errors))
		}
	}
	if len(rpsByBucket) == 0 {
		return nil
	}

	keys := make([]int64, 0, len(rpsByBucket))
	for k := range rpsByBucket {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	out := make([]BucketBand, 0, len(keys))
	for _, k := range keys {
		p99 := bandStatOf(p99ByBucket[k])
		errs := bandStatOf(errByBucket[k])
		out = append(out, BucketBand{
			TSec:   k,
			RPS:    bandStatOf(rpsByBucket[k]),
			P99Ms:  &p99,
			Errors: &errs,
		})
	}
	return out
}

// bandStatOf computes the min/p50/p99/max/mean of vals, reusing the
// shared percentile() helper from aggregate.go.
func bandStatOf(vals []float64) BandStat {
	return BandStat{
		Min:  percentile(vals, 0),
		P50:  percentile(vals, 50),
		P99:  percentile(vals, 99),
		Max:  percentile(vals, 100),
		Mean: meanOf(vals),
	}
}

// meanOf returns the arithmetic mean of vals, or 0 for an empty slice.
func meanOf(vals []float64) float64 {
	if len(vals) == 0 {
		return 0
	}
	var sum float64
	for _, v := range vals {
		sum += v
	}
	return sum / float64(len(vals))
}

// BuildScenarioSeries assembles the per-(scenario, server) block from a
// slice of loadgen.Result — one per run, in run order. This is the
// public entry point the cluster fan-in (mage_bench) uses directly,
// because that pipeline has no [CellResult] to feed [BuildTimeseries].
//
// nil/empty Timeseries on every sample yields empty Runs/Band (no panic).
func BuildScenarioSeries(scenario, server, category string, samples []loadgen.Result) ScenarioSeries {
	ss := ScenarioSeries{Scenario: scenario, Server: server, Category: category}
	runs := make([][]loadgen.TimeseriesPoint, 0, len(samples))
	for i, s := range samples {
		ss.Runs = append(ss.Runs, runSeriesFrom(i+1, s.Timeseries))
		runs = append(runs, s.Timeseries)
	}
	ss.Band = mergeBand(runs)
	return ss
}

// MarshalGzip serialises the doc to compact JSON then gzip-compresses it.
// The result is what gets written to timeseries.json.gz.
func (d *TimeseriesDoc) MarshalGzip() ([]byte, error) {
	raw, err := json.Marshal(d)
	if err != nil {
		return nil, err
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(raw); err != nil {
		_ = zw.Close()
		return nil, err
	}
	if err := zw.Close(); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// UnmarshalGzip reverses MarshalGzip: gunzip then JSON-decode into d.
func (d *TimeseriesDoc) UnmarshalGzip(data []byte) error {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return err
	}
	defer func() { _ = zr.Close() }()
	raw, err := readAllGzip(zr)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, d)
}

// readAllGzip drains a gzip reader. Kept local to avoid pulling io into
// this file just for io.ReadAll.
func readAllGzip(zr *gzip.Reader) ([]byte, error) {
	var buf bytes.Buffer
	if _, err := buf.ReadFrom(zr); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
