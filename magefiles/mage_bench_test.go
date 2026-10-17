//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	hdr "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/goceleris/loadgen"
	"github.com/goceleris/probatorium/report"
)

// writeRawHost writes a raw/<host>.json payload in the shape
// aggregatePerCellResults produces (host / celeris_version / summary /
// cells[] where each cell's loadgen is a marshalled loadgen.Result).
// suffix is appended to the filename (use "" for the default
// <host>.json or a unique tag like "iter1" to write multiple raw
// files in the same raw/ — the production bench does this for every
// back-to-back iteration so the merge can fold them all together).
func writeRawHost(t *testing.T, rawDir, host string, cells []cellRecord, suffix ...string) {
	t.Helper()
	name := host
	if len(suffix) > 0 {
		name = host + "." + suffix[0]
	}
	payload := map[string]any{
		"host":            host,
		"celeris_version": "v1.4.12",
		"summary":         map[string]any{},
		"cells":           cells,
	}
	buf, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		t.Fatalf("marshal raw host: %v", err)
	}
	if err := os.WriteFile(filepath.Join(rawDir, name+".json"), buf, 0o644); err != nil {
		t.Fatalf("write raw host: %v", err)
	}
}

func mustMarshalLoadgen(t *testing.T, r loadgen.Result) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(r)
	if err != nil {
		t.Fatalf("marshal loadgen.Result: %v", err)
	}
	return b
}

// TestMergeBenchResultsHistogramSurvives is the regression guard for the
// double-base64 bug that left every published hdr_histogram_b64 empty
// (v1.5.2–v1.5.5). loadgen.Result.Histogram is ALREADY the hdr base64 wire
// form (hdrhistogram-go Encode base64-encodes); the merge must pass it
// through verbatim. The old code base64-re-encoded it, so report's hdr.Decode
// saw the ASCII "HIST" magic as the cookie ("only V2 is supported") and
// silently dropped it. This drives a REAL histogram through mergeBenchResults
// and asserts the merged blob is non-empty AND decodes back with the recorded
// sample count — it fails loudly if anyone re-introduces an extra encode.
func TestMergeBenchResultsHistogramSurvives(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	// A REAL V2 histogram, encoded exactly as loadgen emits it: Encode()
	// returns base64 bytes, and that string is what lands in Result.Histogram.
	const perRun = 1000
	mkWire := func(base int64) []byte {
		h := hdr.New(1, 60_000_000, 3)
		for i := 0; i < perRun; i++ {
			_ = h.RecordValue(base + int64(i))
		}
		wire, err := h.Encode(hdr.V2CompressedEncodingCookieBase)
		if err != nil {
			t.Fatalf("encode histogram: %v", err)
		}
		return wire
	}
	mkResult := func(base int64) loadgen.Result {
		return loadgen.Result{
			Requests:       perRun,
			Duration:       time.Minute,
			RequestsPerSec: 300000,
			ThroughputBPS:  300000 * 120,
			Latency:        loadgen.Percentiles{P50: 200 * time.Microsecond, P99: 4 * time.Millisecond},
			Histogram:      mkWire(base),
			CPUPctP95:      60, // self-CPU sampler value → loadgen_cpu_p95 (0.60 of a core)
		}
	}

	writeRawHost(t, rawDir, "msa2-server", []cellRecord{
		{RunIndex: 0, Competitor: "celeris-std-h1", Loadgen: mustMarshalLoadgen(t, mkResult(100))},
		{RunIndex: 1, Competitor: "celeris-std-h1", Loadgen: mustMarshalLoadgen(t, mkResult(200))},
	})

	out, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.12", Duration: "60s", Warmup: "30s", Conns: "256", Runs: "2", Seed: "42",
	})
	if err != nil {
		t.Fatalf("mergeBenchResults: %v", err)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read results.json: %v", err)
	}
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal results.json: %v", err)
	}
	if len(doc.Benchmarks) == 0 {
		t.Fatal("no benchmarks in merged document")
	}
	b := doc.Benchmarks[0]
	blob := b.HdrHistogramB64[clusterScenarioName]
	if blob == "" {
		t.Fatalf("hdr_histogram_b64[%q] is EMPTY — histogram dropped in merge (double-encode regression?)", clusterScenarioName)
	}
	h, derr := hdr.Decode([]byte(blob))
	if derr != nil || h == nil {
		t.Fatalf("published hdr_histogram_b64 does not decode: %v", derr)
	}
	if got := h.TotalCount(); got != int64(2*perRun) {
		t.Errorf("merged histogram TotalCount = %d, want %d (both runs merged)", got, 2*perRun)
	}
	// CPUPctP95=60 across runs → loadgen_cpu_p95 = 0.60 (fraction of one core).
	if got := b.LoadgenCPUP95[clusterScenarioName]; got < 0.59 || got > 0.61 {
		t.Errorf("loadgen_cpu_p95[%q] = %v, want ~0.60 (CPUPctP95 must flow through)", clusterScenarioName, got)
	}
}

// TestMergeBenchResults drives the cluster merge path end to end: it
// writes a synthetic raw/<host>.json carrying real loadgen.Result-shaped
// cells (latency percentiles + a non-empty histogram), runs
// mergeBenchResults, and asserts the emitted results.json is a canonical
// v5.1 report.Document that validates against the schema, carries a
// populated Environment + BenchmarkConfig, and contains NONE of the
// retired loose "hosts"/top-level "version" keys.
func TestMergeBenchResults(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	// A tiny but valid V2-compressed HdrHistogram so the merge exercises
	// the histogram path rather than the median-of-percentiles fallback.
	hist := []byte("HISTFAAAAAx4nJNpmSzMwMDAyAABMBoAGzcCRQ==")

	mkResult := func(rps float64, p99 time.Duration) loadgen.Result {
		return loadgen.Result{
			Requests:       1_000_000,
			Errors:         0,
			Duration:       2 * time.Minute,
			RequestsPerSec: rps,
			ThroughputBPS:  rps * 120,
			Latency: loadgen.Percentiles{
				P50:   200 * time.Microsecond,
				P99:   p99,
				P999:  p99 * 2,
				P9999: p99 * 3,
				Max:   p99 * 4,
			},
			Histogram: hist,
		}
	}

	writeRawHost(t, rawDir, "msa2-server", []cellRecord{
		{RunIndex: 0, Competitor: "gin-h1", Loadgen: mustMarshalLoadgen(t, mkResult(120000, 9*time.Millisecond))},
		{RunIndex: 1, Competitor: "gin-h1", Loadgen: mustMarshalLoadgen(t, mkResult(122000, 11*time.Millisecond))},
		{RunIndex: 0, Competitor: "celeris-std-h1", Loadgen: mustMarshalLoadgen(t, mkResult(300000, 4*time.Millisecond))},
	})

	out, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.12",
		Duration:   "120s",
		Warmup:     "30s",
		Conns:      "256",
		Runs:       "5",
		Seed:       "42",
	})
	if err != nil {
		t.Fatalf("mergeBenchResults: %v", err)
	}

	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read results.json: %v", err)
	}

	// Must NOT carry the retired loose shape.
	var loose map[string]any
	if err := json.Unmarshal(data, &loose); err != nil {
		t.Fatalf("unmarshal results.json: %v", err)
	}
	if _, ok := loose["hosts"]; ok {
		t.Errorf("results.json carries retired loose key \"hosts\"")
	}
	if _, ok := loose["version"]; ok {
		t.Errorf("results.json carries retired top-level \"version\"")
	}

	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal into report.Document: %v", err)
	}
	if doc.SchemaVersion != report.SchemaVersion {
		t.Errorf("SchemaVersion: want %q got %q", report.SchemaVersion, doc.SchemaVersion)
	}
	if doc.BenchmarkConfig.Runs != 5 {
		t.Errorf("BenchmarkConfig.Runs: want 5 got %d", doc.BenchmarkConfig.Runs)
	}
	if doc.BenchmarkConfig.Duration != 2*time.Minute {
		t.Errorf("BenchmarkConfig.Duration: want 2m got %v", doc.BenchmarkConfig.Duration)
	}
	if doc.BenchmarkConfig.Warmup != 30*time.Second {
		t.Errorf("BenchmarkConfig.Warmup: want 30s got %v", doc.BenchmarkConfig.Warmup)
	}
	if doc.BenchmarkConfig.CelerisVer != "v1.4.12" {
		t.Errorf("BenchmarkConfig.CelerisVer: want v1.4.12 got %q", doc.BenchmarkConfig.CelerisVer)
	}
	if doc.Environment.LoadgenHost == "" || doc.Environment.Fabric == "" {
		t.Errorf("Environment not populated: %+v", doc.Environment)
	}
	if doc.Environment.KernelSysctlsApplied == nil {
		t.Errorf("Environment.KernelSysctlsApplied must be non-nil (schema requires the field)")
	}
	if doc.HostArchPair != "linux/amd64" {
		t.Errorf("HostArchPair: want linux/amd64 got %q", doc.HostArchPair)
	}

	// Two competitors, sorted by Name (celeris-std-h1 < gin-h1).
	if len(doc.Benchmarks) != 2 {
		t.Fatalf("Benchmarks: want 2 got %d", len(doc.Benchmarks))
	}
	if doc.Benchmarks[0].Name != "celeris-std-h1" || doc.Benchmarks[1].Name != "gin-h1" {
		t.Errorf("Benchmarks not sorted: %q, %q", doc.Benchmarks[0].Name, doc.Benchmarks[1].Name)
	}
	// gin-h1 metadata projected from servers.Registry.
	if doc.Benchmarks[1].Framework != "gin" {
		t.Errorf("gin-h1 Framework: want gin got %q", doc.Benchmarks[1].Framework)
	}
	if len(doc.Benchmarks[1].CompileOptions) == 0 {
		t.Errorf("gin-h1 CompileOptions: want non-empty for go adapter")
	}
	// The cluster scenario column must be present and carry an RPS value.
	if got := doc.Benchmarks[1].SaturationModeRPS[clusterScenarioName]; got == 0 {
		t.Errorf("SaturationModeRPS[%s]: want non-zero", clusterScenarioName)
	}
}

// TestMergeRatedPassesThroughToDocument is the regression test for the
// v3.2 review: the rated sweep ran on the runner and was persisted to
// the per-cell JSON's `rated_passes` field, but readRunnerCellResults
// never propagated them to the cluster cellRecord, so mergeBenchResults
// never saw them, and the published summary.json's
// RatedModeP99AtTargetRPS / LatencyAtSLO maps stayed empty for every
// rated cell — which made the v1.4.15 "rated" cells effectively a
// duplicate saturation-grid cell. This test writes a synthetic raw cell
// with 3 rated passes and asserts the merged Document carries the
// typed RatedModeP99AtTargetRPS + LatencyAtSLO leaves on the correct
// benchmark row.
//
// The test pins three things at once:
//  1. readRunnerCellResults must read cf.RatedPasses into rec.RatedPasses
//  2. mergeBenchResults must read cell.RatedPasses and convert each
//     entry to a report.RatedSample via the wire mirror
//  3. report.Aggregate must reduce the RatedSamples into
//     LatencyAtSLO + RatedModeP99AtTargetRPS on the matching scenario
func TestMergeRatedPassesThroughToDocument(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	// A synthetic rated sweep at targets 100k, 200k, 300k RPS with the
	// p99 at each target (in nanoseconds, the loadgen wire format).
	// LatencyAtSLO at 50ms is 200k (300k would exceed), at 100ms is 300k,
	// at 500ms is 300k. The headline P99 (rated_mode_p99_at_target_rps)
	// is the p99 at the HIGHEST target — 12ms here (300k).
	ratedPasses := []ratedPassWire{
		{TargetRPS: 100000, P99: 8 * time.Millisecond},
		{TargetRPS: 200000, P99: 30 * time.Millisecond},
		{TargetRPS: 300000, P99: 12 * time.Millisecond},
	}
	satRPS := 600000.0 // saturation anchor (60% of which = 180k = ~rated mid)
	cell := cellRecord{
		RunIndex:          0,
		Competitor:        "celeris-epoll-h1-sync",
		Scenario:          "get-json",
		Status:            "ok",
		SaturationModeRPS: satRPS,
		RatedPasses:       ratedPasses,
		Loadgen: mustMarshalLoadgen(t, loadgen.Result{
			Requests: 1_000_000, Errors: 0, Duration: 2 * time.Minute,
			RequestsPerSec: satRPS, ThroughputBPS: satRPS * 120,
			Latency: loadgen.Percentiles{
				P50: 200 * time.Microsecond, P99: 9 * time.Millisecond,
				P999: 20 * time.Millisecond, P9999: 50 * time.Millisecond, Max: 80 * time.Millisecond,
			},
		}),
	}
	writeRawHost(t, rawDir, "msa2-server", []cellRecord{cell})

	merged, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.15", Duration: "40s", Warmup: "10s", Conns: "256", Runs: "1",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	data, err := os.ReadFile(merged)
	if err != nil {
		t.Fatalf("read results.json: %v", err)
	}
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks: want 1, got %d", len(doc.Benchmarks))
	}
	row := doc.Benchmarks[0]

	// 1. SaturationModeRPS reached the Document (this was the easy one).
	if got := row.SaturationModeRPS[cell.Scenario]; got != satRPS {
		t.Errorf("SaturationModeRPS[%q]: want %v, got %v", cell.Scenario, satRPS, got)
	}

	// 2. The rated pass made it through the cluster path: at least one
	//    target RPS reached the Document.
	if got := row.RatedModeP99AtTargetRPS[cell.Scenario]; got == 0 {
		t.Errorf("RatedModeP99AtTargetRPS[%q]: want non-zero (rated sweep was lost)", cell.Scenario)
	}
	// The headline P99 is the p99 at the highest target (300k → 12ms).
	if got := row.RatedModeP99AtTargetRPS[cell.Scenario]; got != 12*time.Millisecond {
		t.Errorf("RatedModeP99AtTargetRPS[%q]: want 12ms (P99 at 300k), got %v", cell.Scenario, got)
	}

	// 3. The LatencyAtSLO reduction fired. With 3 rated passes
	//    (8ms@100k, 30ms@200k, 12ms@300k), the per-bucket winners are:
	//      10ms  → 100k   (only 100k cleared 10ms; 30ms/12ms exceed 10ms? no,
	//                    12ms at 300k exceeds 10ms, 30ms at 200k exceeds 10ms,
	//                    only 8ms@100k ≤ 10ms)
	//      50ms  → 300k   (all three clear 50ms; 300k is the highest)
	//      100ms → 300k   (only 300k is the highest, all clear)
	slo := row.LatencyAtSLO[cell.Scenario]
	if slo == nil {
		t.Fatalf("LatencyAtSLO[%q]: want populated, got nil", cell.Scenario)
	}
	if got := slo[10]; got != 100000 {
		t.Errorf("LatencyAtSLO[%q][10ms]: want 100000 (only 8ms@100k clears), got %d", cell.Scenario, got)
	}
	if got := slo[50]; got != 300000 {
		t.Errorf("LatencyAtSLO[%q][50ms]: want 300000 (all three clear 50ms; 300k wins), got %d", cell.Scenario, got)
	}
	if got := slo[100]; got != 300000 {
		t.Errorf("LatencyAtSLO[%q][100ms]: want 300000, got %d", cell.Scenario, got)
	}
}

// TestMergeMultipleScenariosPerCompetitor is the regression for the
// secondary v3.2 bug: mergeBenchResults keyed its `collected` map by
// competitor alone, collapsing all scenarios for a server into one
// CellResult (ScenarioName="bench", the legacy default). The published
// Document ended up with one SaturationModeRPS["bench"] entry per
// benchmark instead of a per-scenario grid. This test writes a
// competitor with TWO scenarios (different RPS + different rated sweeps)
// and asserts each scenario's data is preserved on the final Document
// under the right scenario key.
func TestMergeMultipleScenariosPerCompetitor(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	mkRes := func(rps float64) json.RawMessage {
		b, _ := json.Marshal(loadgen.Result{
			Requests: 1_000_000, Errors: 0, Duration: 2 * time.Minute,
			RequestsPerSec: rps, ThroughputBPS: rps * 120,
		})
		return b
	}

	cells := []cellRecord{
		// celeris-epoll-h1-sync on get-json: 600k RPS, rated {100k=8ms, 200k=30ms, 300k=12ms}
		{
			RunIndex: 0, Competitor: "celeris-epoll-h1-sync", Scenario: "get-json",
			Status: "ok", SaturationModeRPS: 600000,
			Loadgen: mkRes(600000),
			RatedPasses: []ratedPassWire{
				{TargetRPS: 100000, P99: 8 * time.Millisecond},
				{TargetRPS: 200000, P99: 30 * time.Millisecond},
				{TargetRPS: 300000, P99: 12 * time.Millisecond},
			},
		},
		// celeris-epoll-h1-sync on get-simple: 700k RPS, rated {100k=500us, 300k=2ms}
		{
			RunIndex: 0, Competitor: "celeris-epoll-h1-sync", Scenario: "get-simple",
			Status: "ok", SaturationModeRPS: 700000,
			Loadgen: mkRes(700000),
			RatedPasses: []ratedPassWire{
				{TargetRPS: 100000, P99: 500 * time.Microsecond},
				{TargetRPS: 300000, P99: 2 * time.Millisecond},
			},
		},
		// gin-h1 on get-json only (1 scenario) — saturation only, no rated
		{
			RunIndex: 0, Competitor: "gin-h1", Scenario: "get-json",
			Status: "ok", SaturationModeRPS: 400000,
			Loadgen: mkRes(400000),
		},
	}
	writeRawHost(t, rawDir, "msa2-server", cells)

	merged, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.15", Duration: "40s", Warmup: "10s", Conns: "256", Runs: "1",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	data, _ := os.ReadFile(merged)
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}

	if len(doc.Benchmarks) != 2 {
		t.Fatalf("Benchmarks: want 2 (celeris-epoll-h1-sync + gin-h1), got %d", len(doc.Benchmarks))
	}

	// Locate celeris-epoll-h1-sync.
	var cel *report.ServerResult
	for i := range doc.Benchmarks {
		if doc.Benchmarks[i].Name == "celeris-epoll-h1-sync" {
			cel = &doc.Benchmarks[i]
			break
		}
	}
	if cel == nil {
		t.Fatalf("celeris-epoll-h1-sync not in Document.Benchmarks")
	}

	// 1. The two scenarios must each carry their own SaturationModeRPS.
	//    Before the fix, both would have collapsed into a single "bench" key.
	if got := cel.SaturationModeRPS["get-json"]; got != 600000 {
		t.Errorf("SaturationModeRPS[get-json]: want 600000, got %v", got)
	}
	if got := cel.SaturationModeRPS["get-simple"]; got != 700000 {
		t.Errorf("SaturationModeRPS[get-simple]: want 700000, got %v", got)
	}
	if _, present := cel.SaturationModeRPS["bench"]; present {
		t.Errorf("SaturationModeRPS[bench]: want ABSENT (no default fallback), got present")
	}

	// 2. Each scenario carries its own rated headline P99.
	if got := cel.RatedModeP99AtTargetRPS["get-json"]; got != 12*time.Millisecond {
		t.Errorf("RatedModeP99AtTargetRPS[get-json]: want 12ms (P99 at 300k), got %v", got)
	}
	if got := cel.RatedModeP99AtTargetRPS["get-simple"]; got != 2*time.Millisecond {
		t.Errorf("RatedModeP99AtTargetRPS[get-simple]: want 2ms (P99 at 300k), got %v", got)
	}

	// 3. Each scenario's LatencyAtSLO is independently reduced.
	//    get-json: 10ms → 100k (8ms clears; 12ms at 300k doesn't)
	//    get-simple: 10ms → 300k (2ms clears)
	//    get-simple: 50ms → 300k
	if slo := cel.LatencyAtSLO["get-json"]; slo == nil {
		t.Errorf("LatencyAtSLO[get-json]: want populated")
	} else {
		if got := slo[10]; got != 100000 {
			t.Errorf("LatencyAtSLO[get-json][10ms]: want 100000, got %d", got)
		}
	}
	if slo := cel.LatencyAtSLO["get-simple"]; slo == nil {
		t.Errorf("LatencyAtSLO[get-simple]: want populated")
	} else {
		if got := slo[10]; got != 300000 {
			t.Errorf("LatencyAtSLO[get-simple][10ms]: want 300000 (only 2ms@300k ≤ 10ms), got %d", got)
		}
		if got := slo[50]; got != 300000 {
			t.Errorf("LatencyAtSLO[get-simple][50ms]: want 300000, got %d", got)
		}
	}

	// 4. gin-h1 had no rated pass — its rated maps should be empty.
	var gin *report.ServerResult
	for i := range doc.Benchmarks {
		if doc.Benchmarks[i].Name == "gin-h1" {
			gin = &doc.Benchmarks[i]
			break
		}
	}
	if gin == nil {
		t.Fatalf("gin-h1 not in Document.Benchmarks")
	}
	if _, present := gin.RatedModeP99AtTargetRPS["get-json"]; present {
		t.Errorf("gin-h1 RatedModeP99AtTargetRPS[get-json]: want ABSENT (no rated pass), got present")
	}
	if _, present := gin.LatencyAtSLO["get-json"]; present {
		t.Errorf("gin-h1 LatencyAtSLO[get-json]: want ABSENT (no rated pass), got present")
	}
}

// TestMergeCrashThenOKDemotesToSuspect is the merge-side mirror of the
// v3.8 OK-promotion bug (the runner-side half is pinned by
// cmd/runner/cellclassify_test.go): run 0 of a cell died with the real
// v3.8 crash signature, run 1 passed clean. The merged Document must
// keep the OK run's number but flag the cell suspect and carry the
// [dnf, ok] evidence in cell_run_statuses — never publish a clean "ok"
// that erases the crash. The raw cells are written OK-run-first to
// prove the reduction orders by RunIndex, not encounter order.
func TestMergeCrashThenOKDemotesToSuspect(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	crash := "server-died-mid-cell: post-cell probe: dial tcp 192.168.50.65:8080: connect: connection refused (requests=4029 errors=33140345)"
	cells := []cellRecord{
		// OK run written FIRST with the higher RunIndex.
		{
			RunIndex: 1, Competitor: "celeris-iouring-h1-async", Scenario: "get-json",
			Status: "ok", SaturationModeRPS: 500000,
			Loadgen: mustMarshalLoadgen(t, loadgen.Result{
				Requests: 1_000_000, Duration: 2 * time.Minute, RequestsPerSec: 500000,
			}),
		},
		{
			RunIndex: 0, Competitor: "celeris-iouring-h1-async", Scenario: "get-json",
			Status: "dnf", Error: crash,
		},
	}
	writeRawHost(t, rawDir, "msa2-server", cells)

	merged, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.15", Duration: "90s", Warmup: "20s", Conns: "256", Runs: "2",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	data, _ := os.ReadFile(merged)
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks: want 1, got %d", len(doc.Benchmarks))
	}
	row := doc.Benchmarks[0]

	// Demoted, not promoted: the OK rerun no longer erases the crash.
	if got := row.CellStatuses["get-json"]; got != string(report.CellSuspect) {
		t.Errorf("cell_statuses[get-json] = %q, want suspect (OK rerun must not erase the crash)", got)
	}
	// The OK run's data is kept — suspect means flagged, not dropped.
	if got := row.SaturationModeRPS["get-json"]; got != 500000 {
		t.Errorf("SaturationModeRPS[get-json] = %v, want 500000 (suspect keeps data)", got)
	}
	// Evidence in execution (RunIndex) order, despite reversed file order.
	if got := row.CellRunStatuses["get-json"]; len(got) != 2 || got[0] != "dnf" || got[1] != "ok" {
		t.Errorf("cell_run_statuses[get-json] = %v, want [dnf ok]", got)
	}
}

// TestMergeInterruptedThenOKStaysOK asserts a harness-side interruption
// (the ansible hang-guard SIGTERM, error prefix "interrupted:") does NOT
// demote a cell whose rerun produced clean data — it says nothing about
// the SUT — while the [dnf, ok] evidence still lands in
// cell_run_statuses so the interruption stays on the record.
func TestMergeInterruptedThenOKStaysOK(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	cells := []cellRecord{
		{
			RunIndex: 0, Competitor: "gin-h1", Scenario: "get-json",
			Status: "dnf", Error: "interrupted: run cancelled before cell start",
		},
		{
			RunIndex: 1, Competitor: "gin-h1", Scenario: "get-json",
			Status: "ok", SaturationModeRPS: 120000,
			Loadgen: mustMarshalLoadgen(t, loadgen.Result{
				Requests: 1_000_000, Duration: 2 * time.Minute, RequestsPerSec: 120000,
			}),
		},
	}
	writeRawHost(t, rawDir, "msa2-server", cells)

	merged, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.15", Duration: "90s", Warmup: "20s", Conns: "256", Runs: "2",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	data, _ := os.ReadFile(merged)
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	row := doc.Benchmarks[0]

	// Interruptions never demote a cell with clean data...
	if _, present := row.CellStatuses["get-json"]; present {
		t.Errorf("cell_statuses[get-json] = %v, want absent (interruption must not demote)", row.CellStatuses)
	}
	if got := row.SaturationModeRPS["get-json"]; got != 120000 {
		t.Errorf("SaturationModeRPS[get-json] = %v, want 120000", got)
	}
	// ...but the evidence is preserved.
	if got := row.CellRunStatuses["get-json"]; len(got) != 2 || got[0] != "dnf" || got[1] != "ok" {
		t.Errorf("cell_run_statuses[get-json] = %v, want [dnf ok]", got)
	}
}

// TestMergeSuspectCellKeepsData asserts a suspect record (the v3.8
// churn-close shape: errors 24x requests, published status=ok back then)
// travels the cluster path with its loadgen payload intact: the Document
// flags it suspect AND keeps the headline number, and the raw summary
// (summarizeCells) includes its totals.
func TestMergeSuspectCellKeepsData(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	res := loadgen.Result{
		Requests: 12081484, Errors: 290204598,
		Duration: 90 * time.Second, RequestsPerSec: 134231.93,
	}
	cells := []cellRecord{{
		RunIndex: 0, Competitor: "ntex", Scenario: "churn-close",
		Status:  "suspect",
		Error:   "suspect: error ratio 0.960 exceeds budget 0.50 (errors=290204598 requests=12081484)",
		Loadgen: mustMarshalLoadgen(t, res),
	}}
	writeRawHost(t, rawDir, "msa2-server", cells)

	merged, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.15", Duration: "90s", Warmup: "20s", Conns: "256", Runs: "1",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	data, _ := os.ReadFile(merged)
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	row := doc.Benchmarks[0]
	if got := row.CellStatuses["churn-close"]; got != string(report.CellSuspect) {
		t.Errorf("cell_statuses[churn-close] = %q, want suspect", got)
	}
	if got := row.SaturationModeRPS["churn-close"]; got != 134231.93 {
		t.Errorf("SaturationModeRPS[churn-close] = %v, want 134231.93 (suspect keeps data)", got)
	}

	// summarizeCells shares the HasData inclusion rule: the suspect
	// cell's totals appear in the raw summary instead of vanishing.
	sum, err := summarizeCells(cells)
	if err != nil {
		t.Fatalf("summarizeCells: %v", err)
	}
	st, ok := sum[summaryKey("ntex", "churn-close")]
	if !ok {
		t.Fatalf("summary missing suspect cell bucket: %v", sum)
	}
	if st.TotalErrors != 290204598 || st.TotalRequests != 12081484 {
		t.Errorf("summary totals = req %d err %d, want req 12081484 err 290204598", st.TotalRequests, st.TotalErrors)
	}
}

// TestDiffBenchResultsTypedShape proves the regression gate reads the
// typed Benchmarks[].LatencyAtSLO (not the retired loose "hosts" walk):
// it writes a baseline and a current canonical Document where one cell
// regresses past the threshold and asserts diffBenchResults flags it.
func TestDiffBenchResultsTypedShape(t *testing.T) {
	dir := t.TempDir()

	writeDoc := func(name string, rps int) string {
		doc := report.Document{
			SchemaVersion:   report.SchemaVersion,
			HostArchPair:    "linux/amd64",
			Environment:     report.Environment{KernelSysctlsApplied: []string{}, LoadgenHost: "h", Fabric: "loopback"},
			BenchmarkConfig: report.BenchmarkConfig{Runs: 1, LoadgenVer: "v1", CelerisVer: "v1"},
			Benchmarks: []report.ServerResult{
				{
					Name:                    "celeris-std-h1",
					Category:                "celeris",
					Language:                "go",
					Framework:               "celeris",
					CompileOptions:          []string{},
					SaturationModeRPS:       map[string]float64{},
					RatedModeP99AtTargetRPS: map[string]time.Duration{},
					LatencyAtSLO:            map[string]map[int]int{clusterScenarioName: {100: rps}},
					HdrHistogramB64:         map[string]string{},
					LoadgenCPUP95:           map[string]float64{},
					SentVsHandledDeltaPct:   map[string]float64{},
				},
			},
		}
		buf, err := json.MarshalIndent(&doc, "", "  ")
		if err != nil {
			t.Fatalf("marshal %s: %v", name, err)
		}
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, buf, 0o644); err != nil {
			t.Fatalf("write %s: %v", name, err)
		}
		return p
	}

	base := writeDoc("baseline.json", 300000)
	curr := writeDoc("current.json", 240000) // -20%, past the 5% threshold

	regressed, rep, err := diffBenchResults(base, curr, "0.05")
	if err != nil {
		t.Fatalf("diffBenchResults: %v", err)
	}
	if !regressed {
		t.Errorf("expected regression (20%% drop > 5%% threshold); report:\n%s", rep)
	}

	// A non-regressing pair (1% drop) must pass.
	curr2 := writeDoc("current2.json", 297000)
	regressed2, _, err := diffBenchResults(base, curr2, "0.05")
	if err != nil {
		t.Fatalf("diffBenchResults (no-regress): %v", err)
	}
	if regressed2 {
		t.Errorf("did not expect regression for a 1%% drop")
	}
}

// TestMergeUnifiedPanelOnOneRow is the regression for the user-visible
// "I have to look in two folders" complaint: a saturation grid and a
// rated grid for the same (server, scenario) pair must land on the
// SAME benchmarks[] row in a single Document, with both maps keyed
// by scenario. The split-tree design (run-K/ + run-K-rated/) is gone
// — every back-to-back iteration produces exactly one summary.json
// per arch, with the per-scenario SaturationModeRPS and the
// per-scenario LatencyAtSLO+RatedModeP99AtTargetRPS living side by
// side on the same ServerResult.
//
// This test pins that contract: a synthetic cell with both
// saturation AND rated data, plus a second cell with saturation
// only, plus a third cell with rated only (so the per-scenario
// presence pattern is mixed), and the Document must carry:
//
//	celeris-X on get-json:
//	  saturation_mode_rps["get-json"]      = 700000
//	  latency_at_slo["get-json"]           = { "1000": 600000, "500": 600000 }
//	  rated_mode_p99["get-json"]           = 12ms
//	celeris-X on chain-fullstack-get-json:
//	  saturation_mode_rps[...]             = 580000
//	  latency_at_slo[...]                  = {}   (chain is not in rated set)
//	  rated_mode_p99[...]                  = {}   (chain is not in rated set)
//
// The two-panel-on-one-row invariant is the headline the dashboard
// reads — breaking it forces consumers to re-merge two folders.
func TestMergeUnifiedPanelOnOneRow(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	mkRes := func(rps float64) json.RawMessage {
		b, _ := json.Marshal(loadgen.Result{
			Requests: 1_000_000, Errors: 0, Duration: 2 * time.Minute,
			RequestsPerSec: rps, ThroughputBPS: rps * 120,
		})
		return b
	}

	cells := []cellRecord{
		// get-json: BOTH panels populated on the same row.
		{
			RunIndex: 0, Competitor: "celeris-X", Scenario: "get-json",
			Status: "ok", SaturationModeRPS: 700000,
			Loadgen: mkRes(700000),
			RatedPasses: []ratedPassWire{
				{TargetRPS: 100000, P99: 8 * time.Millisecond},
				{TargetRPS: 300000, P99: 12 * time.Millisecond},
			},
		},
		// chain-fullstack-get-json: only saturation (chain is not in
		// the rated set; the per-cell rated sweep leaves the maps empty
		// for this scenario).
		{
			RunIndex: 0, Competitor: "celeris-X", Scenario: "chain-fullstack-get-json",
			Status: "ok", SaturationModeRPS: 580000,
			Loadgen: mkRes(580000),
		},
		// post-4k: BOTH panels populated on the same row (post-4k IS in
		// the rated set).
		{
			RunIndex: 0, Competitor: "celeris-X", Scenario: "post-4k",
			Status: "ok", SaturationModeRPS: 430000,
			Loadgen: mkRes(430000),
			RatedPasses: []ratedPassWire{
				{TargetRPS: 100000, P99: 200 * time.Microsecond},
			},
		},
	}
	writeRawHost(t, rawDir, "msa2-server", cells)

	merged, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.15", Duration: "40s", Warmup: "10s", Conns: "256", Runs: "1",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	data, _ := os.ReadFile(merged)
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks: want 1 (celeris-X), got %d", len(doc.Benchmarks))
	}
	row := doc.Benchmarks[0]

	// get-json: both panels on the same row.
	if got := row.SaturationModeRPS["get-json"]; got != 700000 {
		t.Errorf("SaturationModeRPS[get-json]: want 700000, got %v", got)
	}
	if got := row.RatedModeP99AtTargetRPS["get-json"]; got != 12*time.Millisecond {
		t.Errorf("RatedModeP99AtTargetRPS[get-json]: want 12ms (P99 at 300k), got %v", got)
	}
	if slo := row.LatencyAtSLO["get-json"]; slo == nil {
		t.Errorf("LatencyAtSLO[get-json]: want populated")
	} else {
		if got := slo[1000]; got != 300000 {
			t.Errorf("LatencyAtSLO[get-json][1000ms]: want 300000, got %d", got)
		}
	}

	// chain-fullstack-get-json: only saturation, rated maps empty.
	if got := row.SaturationModeRPS["chain-fullstack-get-json"]; got != 580000 {
		t.Errorf("SaturationModeRPS[chain-fullstack-get-json]: want 580000, got %v", got)
	}
	if _, present := row.LatencyAtSLO["chain-fullstack-get-json"]; present {
		t.Errorf("LatencyAtSLO[chain-fullstack-get-json]: want ABSENT (chain not in rated set), got present")
	}
	if _, present := row.RatedModeP99AtTargetRPS["chain-fullstack-get-json"]; present {
		t.Errorf("RatedModeP99AtTargetRPS[chain-fullstack-get-json]: want ABSENT, got present")
	}

	// post-4k: both panels on the same row.
	if got := row.SaturationModeRPS["post-4k"]; got != 430000 {
		t.Errorf("SaturationModeRPS[post-4k]: want 430000, got %v", got)
	}
	if got := row.RatedModeP99AtTargetRPS["post-4k"]; got != 200*time.Microsecond {
		t.Errorf("RatedModeP99AtTargetRPS[post-4k]: want 200us (P99 at 100k), got %v", got)
	}
	if slo := row.LatencyAtSLO["post-4k"]; slo == nil {
		t.Errorf("LatencyAtSLO[post-4k]: want populated")
	} else {
		// 200us @ 100k → clears 10ms (well under) → bucket {10: 100000}
		// also clears 50/100/500/1000ms → all buckets = 100000
		if got := slo[10]; got != 100000 {
			t.Errorf("LatencyAtSLO[post-4k][10ms]: want 100000, got %d", got)
		}
	}
}

// TestMergeBackToBackRatedPersistsAcrossIterations is the regression
// for the v3.4-rated-loss bug: the BenchTier tier loop runs N
// back-to-back iterations, each of which should publish a summary
// that carries the rated data on every row. The earlier bug was that
// setBenchEnvFromProfile's else branch called os.Unsetenv(BENCH_RATED),
// so iterations 2..N produced summaries with empty
// latency_at_slo + rated_mode_p99_at_target_rps maps while iteration
// 1 looked fine.
//
// This test writes THREE raw/<host>.json files — one per iteration
// — each carrying a single (competitor, scenario) cell. The rated
// sweep reuses the same TargetRPS across all 3 iterations with
// distinct P99s, so the reduction collapses 3 P99 samples into one
// target bucket and the median reduction is deterministic. The
// assertion is that the Document has non-empty rated data on the
// row, and the non-rated scenario (chain-fullstack-get-json) stays
// empty, pinning the "rated set only" invariant.
func TestMergeBackToBackRatedPersistsAcrossIterations(t *testing.T) {
	resultsDir := t.TempDir()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}

	mkRes := func(rps float64) json.RawMessage {
		b, _ := json.Marshal(loadgen.Result{
			Requests: 1_000_000, Errors: 0, Duration: 2 * time.Minute,
			RequestsPerSec: rps, ThroughputBPS: rps * 120,
			Latency: loadgen.Percentiles{
				P50: 200 * time.Microsecond, P99: 1 * time.Millisecond,
				P999: 2 * time.Millisecond, P9999: 3 * time.Millisecond,
				Max: 4 * time.Millisecond,
			},
		})
		return b
	}

	// Three iterations, each with its own raw/ payload. The bench
	// code in production writes raw/<host>.json after each
	// back-to-back iteration, then mergeBenchResults folds them all
	// into one Document. Reusing the same 200k TargetRPS across all
	// 3 iterations means the 3 P99s collapse into one bucket.
	iterP99s := []time.Duration{2 * time.Millisecond, 4 * time.Millisecond, 6 * time.Millisecond}
	for iter, p99 := range iterP99s {
		cell := cellRecord{
			RunIndex: iter, Competitor: "celeris-epoll-h1-sync", Scenario: "get-json",
			Status: "ok", SaturationModeRPS: 250000,
			Loadgen: mkRes(250000),
			RatedPasses: []ratedPassWire{
				{TargetRPS: 200000, P99: p99},
			},
		}
		writeRawHost(t, rawDir, "msa2-server", []cellRecord{cell}, fmt.Sprintf("iter%d", iter))
	}

	merged, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.4.15", Duration: "40s", Warmup: "10s", Conns: "256", Runs: "1",
	})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}

	data, _ := os.ReadFile(merged)
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks: want 1, got %d", len(doc.Benchmarks))
	}
	row := doc.Benchmarks[0]

	// Saturation landed (median of 250k/250k/250k → 250k).
	if got := row.SaturationModeRPS["get-json"]; got != 250000 {
		t.Errorf("SaturationModeRPS[get-json]: want 250000 (median), got %v", got)
	}

	// THE key assertion: rated data is populated, not silently empty.
	// If setBenchEnvFromProfile is ever broken again, the rated
	// sweep never runs on iter 2/3 and these two maps come back nil.
	if got := row.RatedModeP99AtTargetRPS["get-json"]; got != 4*time.Millisecond {
		t.Errorf("RatedModeP99AtTargetRPS[get-json]: want 4ms (median of {2,4,6}ms at 200k target), got %v", got)
	}
	if slo := row.LatencyAtSLO["get-json"]; slo == nil {
		t.Fatalf("LatencyAtSLO[get-json]: want populated, got nil (rated data lost)")
	} else {
		// Median 4ms @ 200k clears every SLO threshold in
		// {10, 50, 100, 500, 1000}ms, so all 5 buckets collapse
		// to the only target with a P99 below the threshold: 200k.
		for _, ms := range []int{10, 50, 100, 500, 1000} {
			if got := slo[ms]; got != 200000 {
				t.Errorf("LatencyAtSLO[get-json][%dms]: want 200000, got %d", ms, got)
			}
		}
	}

	// Non-rated scenario (chain-fullstack-get-json) must stay empty
	// even after the back-to-back loop. We only wrote get-json
	// cells, so chain-fullstack-get-json has no raw payload, but
	// the invariant matters: a future code change that mistakenly
	// promotes chain into the rated set would silently leak here.
	if _, present := row.RatedModeP99AtTargetRPS["chain-fullstack-get-json"]; present {
		t.Errorf("RatedModeP99AtTargetRPS[chain-fullstack-get-json]: want ABSENT, got present")
	}
	if _, present := row.LatencyAtSLO["chain-fullstack-get-json"]; present {
		t.Errorf("LatencyAtSLO[chain-fullstack-get-json]: want ABSENT, got present")
	}
}

// TestMaxDryRunCellsPerServer pins the parse of the runner's -dry-run
// schedule print ("run0 <scenario>/<server>"), which sizes the ansible
// per-column hang guard. Server slugs with '+' (celeris-iouring-
// auto+upg-async) must count correctly, columns outside the slug scope
// must not contribute, and garbage lines must be ignored.
func TestMaxDryRunCellsPerServer(t *testing.T) {
	out := `run0 get-json/celeris-epoll-h1-sync
run0 get-json/celeris-iouring-auto+upg-async
run0 auto-mix-111/celeris-iouring-auto+upg-async
run0 chain-api-get-json/celeris-iouring-auto+upg-async
run0 get-json/gin-h1

not-a-schedule-line
`
	cases := []struct {
		name  string
		slugs []string
		want  int
	}{
		{"max across in-scope columns", []string{"celeris-epoll-h1-sync", "celeris-iouring-auto+upg-async"}, 3},
		{"scope excludes the busiest column", []string{"celeris-epoll-h1-sync", "gin-h1"}, 1},
		{"no matching column", []string{"axum"}, 0},
		{"empty schedule", nil, 0},
	}
	for _, tc := range cases {
		if got := maxDryRunCellsPerServer(out, tc.slugs); got != tc.want {
			t.Errorf("%s: maxDryRunCellsPerServer = %d, want %d", tc.name, got, tc.want)
		}
	}
}

// TestDurationSeconds pins the Go-side duration→seconds conversion that
// replaced the playbook's unit-stripping regex (which read "1m30s" as
// 130 seconds).
func TestDurationSeconds(t *testing.T) {
	cases := []struct {
		in      string
		want    int
		wantErr bool
	}{
		{"90s", 90, false},
		{"1m30s", 90, false}, // the regex hack returned 130 for this
		{"2h", 7200, false},
		{"500ms", 1, false}, // sub-second rounds UP, never to 0
		{"0s", 0, true},     // zero would disable `timeout`
		{"-5s", 0, true},
		{"five", 0, true},
	}
	for _, tc := range cases {
		got, err := durationSeconds(tc.in)
		if (err != nil) != tc.wantErr {
			t.Errorf("durationSeconds(%q): err = %v, wantErr = %v", tc.in, err, tc.wantErr)
			continue
		}
		if !tc.wantErr && got != tc.want {
			t.Errorf("durationSeconds(%q) = %d, want %d", tc.in, got, tc.want)
		}
	}
}

// TestCheckDataCompleteness is the regression guard for the publish-time
// completeness gate — the protection that was ABSENT while hdr_histogram_b64,
// loadgen_cpu_p95, and framework_version silently shipped empty across four
// releases. A complete doc passes; each individual silent-drop must fail the
// gate; BENCH_PUBLISH_FORCE=1 bypasses.
func TestCheckDataCompleteness(t *testing.T) {
	mk := func() *report.Document {
		return &report.Document{
			BenchmarkConfig: report.BenchmarkConfig{
				StartedAt:  time.Now().Add(-time.Hour),
				FinishedAt: time.Now(),
			},
			Benchmarks: []report.ServerResult{{
				Name: "celeris-std-h1", Framework: "celeris", Language: "go", Category: "celeris",
				FrameworkVersion:  "v1.5.6",
				SaturationModeRPS: map[string]float64{"get-json": 100000},
				HdrHistogramB64:   map[string]string{"get-json": "HISTFAAA"},
				LoadgenCPUP95:     map[string]float64{"get-json": 0.6},
			}},
		}
	}
	if err := checkDataCompleteness(mk()); err != nil {
		t.Fatalf("a complete document must pass the gate: %v", err)
	}
	defects := map[string]func(*report.Document){
		"empty histogram":   func(d *report.Document) { d.Benchmarks[0].HdrHistogramB64 = map[string]string{"get-json": ""} },
		"empty cpu":         func(d *report.Document) { d.Benchmarks[0].LoadgenCPUP95 = map[string]float64{} },
		"empty fwk version": func(d *report.Document) { d.Benchmarks[0].FrameworkVersion = "" },
		"zero started_at":   func(d *report.Document) { d.BenchmarkConfig.StartedAt = time.Time{} },
		"empty saturation":  func(d *report.Document) { d.Benchmarks[0].SaturationModeRPS = nil },
		"no celeris column": func(d *report.Document) { d.Benchmarks[0].Framework = "gin" },
	}
	for name, mut := range defects {
		d := mk()
		mut(d)
		if err := checkDataCompleteness(d); err == nil {
			t.Errorf("%s: gate should FAIL but passed", name)
		}
	}
	// FORCE override ships a known-incomplete run anyway.
	t.Setenv("BENCH_PUBLISH_FORCE", "1")
	d := mk()
	d.Benchmarks[0].FrameworkVersion = ""
	if err := checkDataCompleteness(d); err != nil {
		t.Errorf("BENCH_PUBLISH_FORCE=1 must bypass the gate: %v", err)
	}
}

// TestClusterServerMetaFrameworkVersion pins the cluster-path framework_version
// fix: celeris columns carry the threaded benched version, competitors carry
// their registry-pinned version (the field was 0/52 before — never assigned).
func TestClusterServerMetaFrameworkVersion(t *testing.T) {
	cells := map[string]*report.CellResult{
		"celeris-std-h1|get-json": {},
		"gin-h1|get-json":         {},
	}
	meta := clusterServerMeta(cells, "v1.5.9", "amd64")
	if got := meta["celeris-std-h1"].FrameworkVersion; got != "v1.5.9" {
		t.Errorf("celeris FrameworkVersion = %q, want v1.5.9 (threaded benched version)", got)
	}
	if got := meta["gin-h1"].FrameworkVersion; got == "" {
		t.Error("gin-h1 FrameworkVersion is empty; want the registry-pinned version")
	}
}

// TestClusterServerMetaCompileOptionsArch pins the published half of the
// arch-blind compile_options bug (v1.5.5-v1.5.8 shipped GOARM64=v8.0 on
// every amd64 document): the merge runs on the arm64 Actions runner, so
// CompileOptions must come from the bench TARGET, never runtime.GOARCH.
func TestClusterServerMetaCompileOptionsArch(t *testing.T) {
	cells := map[string]*report.CellResult{"gin-h1|get-json": {}}
	for _, tc := range []struct{ arch, want, notWant string }{
		{"amd64", "GOAMD64=v3", "GOARM64=v8.0"},
		{"arm64", "GOARM64=v8.0", "GOAMD64=v3"},
	} {
		opts := clusterServerMeta(cells, "v1.5.9", tc.arch)["gin-h1"].CompileOptions
		var hasWant, hasNot bool
		for _, o := range opts {
			if o == tc.want {
				hasWant = true
			}
			if o == tc.notWant {
				hasNot = true
			}
		}
		if !hasWant {
			t.Errorf("target arch %s: CompileOptions %v missing %q", tc.arch, opts, tc.want)
		}
		if hasNot {
			t.Errorf("target arch %s: CompileOptions %v leaked %q from the merge host", tc.arch, opts, tc.notWant)
		}
	}
}
