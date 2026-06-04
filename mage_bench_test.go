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
