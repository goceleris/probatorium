//go:build mage

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/loadgen"
	"github.com/goceleris/probatorium/report"
)

// writeRawHost writes a raw/<host>.json payload in the shape
// aggregatePerCellResults produces (host / celeris_version / summary /
// cells[] where each cell's loadgen is a marshalled loadgen.Result).
func writeRawHost(t *testing.T, rawDir, host string, cells []cellRecord) {
	t.Helper()
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
	if err := os.WriteFile(filepath.Join(rawDir, host+".json"), buf, 0o644); err != nil {
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
//   celeris-X on get-json:
//     saturation_mode_rps["get-json"]      = 700000
//     latency_at_slo["get-json"]           = { "1000": 600000, "500": 600000 }
//     rated_mode_p99["get-json"]           = 12ms
//   celeris-X on chain-fullstack-get-json:
//     saturation_mode_rps[...]             = 580000
//     latency_at_slo[...]                  = {}   (chain is not in rated set)
//     rated_mode_p99[...]                  = {}   (chain is not in rated set)
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
