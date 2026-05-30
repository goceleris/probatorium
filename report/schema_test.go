package report

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

// loadSchema compiles report/schema_v5.json once per test. We keep the
// compiler scoped to each call (rather than caching at package level)
// so the suite stays parallel-safe.
func loadSchema(t *testing.T) *jsonschema.Schema {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join("schema_v5.json"))
	if err != nil {
		t.Fatalf("read schema: %v", err)
	}
	doc, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("unmarshal schema: %v", err)
	}
	c := jsonschema.NewCompiler()
	if err := c.AddResource("schema_v5.json", doc); err != nil {
		t.Fatalf("add schema resource: %v", err)
	}
	sch, err := c.Compile("schema_v5.json")
	if err != nil {
		t.Fatalf("compile schema: %v", err)
	}
	return sch
}

func validateAny(t *testing.T, sch *jsonschema.Schema, raw []byte) {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("unmarshal doc: %v", err)
	}
	if err := sch.Validate(v); err != nil {
		t.Fatalf("schema validation failed:\n%v", err)
	}
}

// TestSchemaRoundTrip builds a synthetic v5.0 Document in code, JSON-
// encodes it, validates the encoded bytes against schema_v5.json, then
// JSON-decodes back into a Document and confirms every load-bearing
// field survived intact. Catches drift between the Go struct tags and
// the formal schema in either direction.
func TestSchemaRoundTrip(t *testing.T) {
	t.Parallel()
	sch := loadSchema(t)

	started := time.Date(2026, 5, 10, 12, 0, 0, 0, time.UTC)
	finished := started.Add(2 * time.Minute)
	doc := Document{
		SchemaVersion: SchemaVersion,
		HostArchPair:  "linux_amd64",
		Environment: Environment{
			KernelSysctlsApplied: []string{
				"net.core.rmem_max=16777216",
				"net.core.somaxconn=65535",
			},
			LoadgenHost: "msa2-client",
			Fabric:      "3-host LACP 20G",
		},
		BenchmarkConfig: BenchmarkConfig{
			StartedAt:  started,
			FinishedAt: finished,
			Runs:       3,
			Duration:   2 * time.Minute,
			Warmup:     10 * time.Second,
			GitRef:     "main@deadbeef",
			LoadgenVer: "v1.4.3",
			CelerisVer: "v1.4.3",
		},
		Benchmarks: []ServerResult{
			{
				Name:             "celeris-std-h1",
				Category:         "celeris",
				Language:         "go",
				LanguageVersion:  "1.26.3",
				Framework:        "celeris",
				FrameworkVersion: "v1.4.3",
				Engine:           "std",
				CompileOptions:   []string{"GOAMD64=v3", "CGO_ENABLED=0"},
				SaturationModeRPS: map[string]float64{
					"get-json": 312000,
				},
				RatedModeP99AtTargetRPS: map[string]time.Duration{
					"get-json": 420 * time.Microsecond,
				},
				LatencyAtSLO: map[string]map[int]int{
					"get-json": {10: 180000, 50: 270000, 100: 305000},
				},
				HdrHistogramB64: map[string]string{
					"get-json": "",
				},
				LoadgenCPUP95: map[string]float64{
					"get-json": 0.78,
				},
				SentVsHandledDeltaPct: map[string]float64{
					"get-json": 0.001,
				},
				Resources: map[string]*ResourceStats{
					"get-json": {
						Summary: ResourceSummary{
							PeakRSSBytes:   ptrI64(150 * 1024 * 1024),
							SteadyRSSBytes: ptrI64(140 * 1024 * 1024),
							MeanCPUPct:     ptrF64(42.5),
							GCPauseP99Ns:   ptrI64(300000),
							GoroutineHWM:   ptrI64(512),
							FDHWM:          ptrI64(48),
						},
						Series: []ResourcePoint{
							{
								TSUnix:         100,
								RSSBytes:       ptrI64(150 * 1024 * 1024),
								CPUPct:         ptrF64(42.5),
								Goroutines:     ptrI64(512),
								HeapInuseBytes: ptrI64(36 * 1024 * 1024),
								FDCount:        ptrI64(48),
							},
						},
					},
				},
			},
		},
		Validation: &ValidationResults{
			StartedAt:        started.Add(-time.Hour),
			FinishedAt:       started,
			PropertiesPassed: 47,
			PropertiesFailed: 0,
		},
		Soak: &SoakSummary{
			Duration:           6 * time.Hour,
			RestartedProcesses: 0,
			HeapGrowthMB:       1.4,
			PerHourErrorRate:   0.0008,
		},
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	validateAny(t, sch, raw)

	var rt Document
	if err := json.Unmarshal(raw, &rt); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if rt.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion: want %q got %q", SchemaVersion, rt.SchemaVersion)
	}
	if rt.HostArchPair != doc.HostArchPair {
		t.Errorf("HostArchPair: drift")
	}
	if len(rt.Benchmarks) != 1 || rt.Benchmarks[0].Name != "celeris-std-h1" {
		t.Errorf("Benchmarks: drift, got %+v", rt.Benchmarks)
	}
	if got := rt.Benchmarks[0].RatedModeP99AtTargetRPS["get-json"]; got != 420*time.Microsecond {
		t.Errorf("RatedModeP99: want 420µs got %v", got)
	}
	if rt.Validation == nil || rt.Validation.PropertiesPassed != 47 {
		t.Errorf("Validation: drift, got %+v", rt.Validation)
	}
	if rt.Soak == nil || rt.Soak.Duration != 6*time.Hour {
		t.Errorf("Soak: drift, got %+v", rt.Soak)
	}
	// Resources (#154): the nullable metric pointers and the series must
	// survive the JSON round-trip and the schema validation above.
	res := rt.Benchmarks[0].Resources["get-json"]
	if res == nil {
		t.Fatal("Resources[get-json]: dropped on decode")
	}
	if res.Summary.PeakRSSBytes == nil || *res.Summary.PeakRSSBytes != 150*1024*1024 {
		t.Errorf("Resources PeakRSSBytes: drift, got %v", res.Summary.PeakRSSBytes)
	}
	if res.Summary.GoroutineHWM == nil || *res.Summary.GoroutineHWM != 512 {
		t.Errorf("Resources GoroutineHWM: drift, got %v", res.Summary.GoroutineHWM)
	}
	if len(res.Series) != 1 || res.Series[0].TSUnix != 100 {
		t.Errorf("Resources Series: drift, got %+v", res.Series)
	}
}

// TestSchemaFixtureMinimal validates testdata/v5_minimal.json against
// the schema and parses it into a Document. The minimal fixture is the
// smallest legal v5.0 document — used as the lower bound for what
// downstream consumers may see in the wild.
func TestSchemaFixtureMinimal(t *testing.T) {
	t.Parallel()
	sch := loadSchema(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "v5_minimal.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	validateAny(t, sch, raw)

	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if doc.SchemaVersion != "5.0" {
		t.Errorf("SchemaVersion: want 5.0 got %q", doc.SchemaVersion)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks: want 1, got %d", len(doc.Benchmarks))
	}
	if doc.Benchmarks[0].Name != "celeris-std-h1" {
		t.Errorf("Benchmarks[0].Name: drift, got %q", doc.Benchmarks[0].Name)
	}
}

// TestSchemaFixtureFull validates testdata/v5_full.json against the
// schema and parses it into a Document. The full fixture exercises
// every optional field — validation_results, soak_summary, multi-
// scenario maps, non-empty hdr_histogram_b64.
func TestSchemaFixtureFull(t *testing.T) {
	t.Parallel()
	sch := loadSchema(t)
	raw, err := os.ReadFile(filepath.Join("testdata", "v5_full.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	validateAny(t, sch, raw)

	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal fixture: %v", err)
	}
	if doc.Validation == nil {
		t.Fatal("Validation: nil, want populated")
	}
	if doc.Validation.PropertiesPassed != 47 {
		t.Errorf("PropertiesPassed: want 47 got %d", doc.Validation.PropertiesPassed)
	}
	if doc.Soak == nil {
		t.Fatal("Soak: nil, want populated")
	}
	if doc.Soak.Duration != 6*time.Hour {
		t.Errorf("Soak.Duration: want 6h got %v", doc.Soak.Duration)
	}
	if len(doc.Benchmarks) != 2 {
		t.Fatalf("Benchmarks: want 2, got %d", len(doc.Benchmarks))
	}
	// hdr_histogram_b64 must round-trip non-empty
	if got := doc.Benchmarks[0].HdrHistogramB64["get-json"]; got == "" {
		t.Errorf("HdrHistogramB64[get-json] dropped on decode")
	}
	// latency_at_slo nested map must round-trip
	if got := doc.Benchmarks[0].LatencyAtSLO["get-json"][100]; got != 305000 {
		t.Errorf("LatencyAtSLO[get-json][100]: want 305000 got %d", got)
	}
}

// TestSchemaBackCompatV4 confirms a v4.0 document still parses cleanly
// under the v5.0 reader. The v5 schema is additive — every v4 field is
// preserved verbatim — so a v4 producer's output must round-trip
// through the Document struct without errors and without losing any
// field the reader cares about.
//
// Note: we do NOT validate v4 docs against schema_v5.json because the
// schema_version pattern is identical (^[0-9]+\.[0-9]+$) so a v4 doc
// passes; the back-compat guarantee that matters is the Go decoder.
func TestSchemaBackCompatV4(t *testing.T) {
	t.Parallel()
	raw, err := os.ReadFile(filepath.Join("testdata", "v4_legacy.json"))
	if err != nil {
		t.Fatalf("read fixture: %v", err)
	}
	var doc Document
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("v4 doc must parse under v5 reader: %v", err)
	}
	if doc.SchemaVersion != "4.0" {
		t.Errorf("SchemaVersion: want 4.0 got %q", doc.SchemaVersion)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("Benchmarks: want 1 got %d", len(doc.Benchmarks))
	}
	b := doc.Benchmarks[0]
	if b.Name != "celeris-std-h1" {
		t.Errorf("Benchmarks[0].Name: drift, got %q", b.Name)
	}
	if got := b.SaturationModeRPS["get-json"]; got != 240000 {
		t.Errorf("SaturationModeRPS drift: got %v", got)
	}
	// v4 docs have no validation_results / soak_summary; the omitempty
	// pointers must remain nil after decode.
	if doc.Validation != nil {
		t.Errorf("Validation: want nil for v4 doc, got %+v", doc.Validation)
	}
	if doc.Soak != nil {
		t.Errorf("Soak: want nil for v4 doc, got %+v", doc.Soak)
	}
}

// TestBuildDocument feeds a synthetic aggregate + environment +
// benchmark config + server-meta map into BuildDocument and asserts the
// returned *Document marshals, validates against schema_v5.json, is
// sorted by Name, carries the canonical SchemaVersion, and emits NONE of
// the retired loose v5.0 keys ("hosts" / top-level "version").
func TestBuildDocument(t *testing.T) {
	t.Parallel()
	sch := loadSchema(t)

	started := time.Date(2026, 5, 30, 9, 0, 0, 0, time.UTC)
	agg := map[string]CellAggregate{
		CellID("get-json", "celeris-std-h1"): {
			ScenarioName:  "get-json",
			ServerName:    "celeris-std-h1",
			Category:      "static",
			N:             3,
			RPSMedian:     312000,
			LatencyMerged: Percentiles{P50: 200 * time.Microsecond, P99: 8 * time.Millisecond},
			// Measured rated sweep (probatorium#156): LatencyAtSLO comes ONLY
			// from real rated probing now, not slid off the saturation P99. A
			// single 312000-RPS rated pass at P99 8ms clears every SLO budget.
			RatedP99ByTarget: map[int]time.Duration{312000: 8 * time.Millisecond},
			LatencyAtSLO:     map[int]int{10: 312000, 50: 312000, 100: 312000, 500: 312000, 1000: 312000},
		},
		CellID("get-text", "celeris-std-h1"): {
			ScenarioName:  "get-text",
			ServerName:    "celeris-std-h1",
			Category:      "static",
			N:             3,
			RPSMedian:     410000,
			LatencyMerged: Percentiles{P50: 150 * time.Microsecond, P99: 40 * time.Millisecond},
			// 410000-RPS rated pass at P99 40ms clears 50/100/500/1000 ms but
			// NOT the 10ms budget.
			RatedP99ByTarget: map[int]time.Duration{410000: 40 * time.Millisecond},
			LatencyAtSLO:     map[int]int{50: 410000, 100: 410000, 500: 410000, 1000: 410000},
		},
		// Second server so Benchmarks-sorting is observable.
		CellID("get-json", "gin-h1"): {
			ScenarioName:  "get-json",
			ServerName:    "gin-h1",
			Category:      "static",
			N:             3,
			RPSMedian:     120000,
			LatencyMedian: Percentiles{P99: 120 * time.Millisecond},
		},
	}

	doc := BuildDocument(BuildInput{
		HostArchPair: "linux/amd64",
		Environment: Environment{
			KernelSysctlsApplied: []string{},
			LoadgenHost:          "msa2-client",
			Fabric:               "3-host LACP 20G",
		},
		BenchmarkConfig: BenchmarkConfig{
			StartedAt:  started,
			FinishedAt: started.Add(2 * time.Minute),
			Runs:       3,
			Duration:   2 * time.Minute,
			Warmup:     10 * time.Second,
			GitRef:     "v1.4.12",
			LoadgenVer: "v1.4.4",
			CelerisVer: "v1.4.12",
		},
		Servers: map[string]ServerMeta{
			"celeris-std-h1": {
				Category:        "celeris",
				Language:        "go",
				LanguageVersion: "go1.26.3",
				Framework:       "celeris",
				Engine:          "std-h1",
				CompileOptions:  CompileOptionsFor("go", "amd64"),
			},
			"gin-h1": {
				Category:       "go-net-http",
				Language:       "go",
				Framework:      "gin",
				Engine:         "h1",
				CompileOptions: CompileOptionsFor("go", "amd64"),
			},
		},
		Agg: agg,
	})

	if doc.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion: want %q got %q", SchemaVersion, doc.SchemaVersion)
	}
	if doc.SchemaVersion != "5.2" {
		t.Errorf("SchemaVersion drift: want 5.2 got %q", doc.SchemaVersion)
	}
	if len(doc.Benchmarks) != 2 {
		t.Fatalf("Benchmarks: want 2 got %d", len(doc.Benchmarks))
	}
	// Sorted by Name: "celeris-std-h1" < "gin-h1".
	if doc.Benchmarks[0].Name != "celeris-std-h1" || doc.Benchmarks[1].Name != "gin-h1" {
		t.Errorf("Benchmarks not sorted by Name: %q, %q",
			doc.Benchmarks[0].Name, doc.Benchmarks[1].Name)
	}
	// Per-server metadata projected from ServerMeta.
	if got := doc.Benchmarks[0].CompileOptions; len(got) == 0 {
		t.Errorf("CompileOptions: want non-empty for go adapter")
	}
	if got := doc.Benchmarks[0].LanguageVersion; got != "go1.26.3" {
		t.Errorf("LanguageVersion: want go1.26.3 got %q", got)
	}
	// LatencyAtSLO measured (probatorium#156): the rated sweep's get-json
	// pass (P99 8ms) clears every SLO; get-text (P99 40ms) clears
	// 50/100/500/1000 ms but NOT 10 ms.
	cel := doc.Benchmarks[0]
	if got := cel.LatencyAtSLO["get-json"][10]; got != 312000 {
		t.Errorf("LatencyAtSLO[get-json][10]: want 312000 got %d", got)
	}
	if _, present := cel.LatencyAtSLO["get-text"][10]; present {
		t.Errorf("LatencyAtSLO[get-text][10]: want absent (P99=40ms > 10ms)")
	}
	if got := cel.LatencyAtSLO["get-text"][50]; got != 410000 {
		t.Errorf("LatencyAtSLO[get-text][50]: want 410000 got %d", got)
	}

	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	validateAny(t, sch, raw)

	// The retired loose v5.0 shape must NOT appear in the canonical output.
	s := string(raw)
	if strings.Contains(s, `"hosts"`) {
		t.Errorf("output carries retired loose key \"hosts\"")
	}
	if strings.Contains(s, `"version":"5.0"`) || strings.Contains(s, `"version": "5.0"`) {
		t.Errorf("output carries retired top-level \"version\":\"5.0\"")
	}
}
