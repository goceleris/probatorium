package report

import (
	"time"
)

// SchemaVersion is the on-disk JSON schema identifier emitted by every
// probatorium results file. Kept additive over the v4 schema in
// goceleris/benchmarks so older readers can fall back to the fields
// they recognise.
const SchemaVersion = "5.0"

// Document is the top-level v5.0 results JSON shape. One file per
// probatorium run; emit by JSON-encoding from the orchestrator.
type Document struct {
	SchemaVersion   string          `json:"schema_version"`
	HostArchPair    string          `json:"host_arch_pair"`
	Environment     Environment     `json:"environment"`
	BenchmarkConfig BenchmarkConfig `json:"benchmark_config"`
	Benchmarks      []ServerResult  `json:"benchmarks"`

	// Validation, when non-nil, attaches the property-test / RESTler /
	// fault-injection summary from the validation tier. Optional —
	// pure-bench runs leave it unset.
	Validation *ValidationResults `json:"validation_results,omitempty"`

	// Soak, when non-nil, attaches the long-running soak summary
	// produced by the validator-replay harness. Optional.
	Soak *SoakSummary `json:"soak_summary,omitempty"`
}

// Environment captures the host fabric and the kernel/sysctl knobs the
// ansible playbooks applied before the run. Persisted alongside results
// so a regression two months later can be debugged against the exact
// kernel state of the producing run.
type Environment struct {
	// KernelSysctlsApplied is the canonical list of sysctl knobs the
	// ansible role wrote (sorted by key). Each entry is "key=value" so
	// the file is grep-friendly and stable across runs.
	KernelSysctlsApplied []string `json:"kernel_sysctls_applied"`

	// LoadgenHost is the hostname of the box that ran loadgen
	// (msa2-client in the standard 3-host fabric).
	LoadgenHost string `json:"loadgen_host"`

	// Fabric describes the wire fabric (e.g. "3-host LACP 20G", or
	// "loopback" for a single-host smoke run).
	Fabric string `json:"fabric"`
}

// BenchmarkConfig records the orchestrator flags + tunables that
// describe HOW the run was driven. Serves as the "what was the input?"
// half of the result; the per-cell numbers are the "what came out?"
// half.
type BenchmarkConfig struct {
	StartedAt  time.Time     `json:"started_at"`
	FinishedAt time.Time     `json:"finished_at"`
	Runs       int           `json:"runs"`
	Duration   time.Duration `json:"duration"`
	Warmup     time.Duration `json:"warmup"`
	GitRef     string        `json:"git_ref"`
	LoadgenVer string        `json:"loadgen_version"`
	CelerisVer string        `json:"celeris_version"`

	// ScenariosFilter is the comma-separated -scenarios CLI argument,
	// or empty for "all". Echoed back so a partial-matrix run is
	// distinguishable from a full one in the JSON.
	ScenariosFilter string `json:"scenarios_filter,omitempty"`

	// AdaptersFilter is the comma-separated -adapters CLI argument, or
	// empty for "all".
	AdaptersFilter string `json:"adapters_filter,omitempty"`
}

// ServerResult is the per-(server, scenario)... wait, no — this is the
// per-server section. Each Benchmarks entry covers ALL scenarios for ONE
// adapter. Per-scenario detail lives in ScenarioResults inside.
type ServerResult struct {
	Name             string `json:"name"`
	Category         string `json:"category"`
	Language         string `json:"language"`
	LanguageVersion  string `json:"language_version"`
	Framework        string `json:"framework"`
	FrameworkVersion string `json:"framework_version"`
	Engine           string `json:"engine,omitempty"`

	// CompileOptions lists the build-time knobs (build tags, GOAMD64,
	// CGO_ENABLED, json library choice, …) that produced the binary
	// under test. Sorted by key, "key=value" form so a two-line diff
	// between runs is readable.
	CompileOptions []string `json:"compile_options"`

	// SaturationModeRPS is the peak sustained RPS measured under the
	// "blast as hard as you can" profile (no closed-loop pacing). One
	// value per scenario, keyed by Scenario.Name().
	SaturationModeRPS map[string]float64 `json:"saturation_mode_rps"`

	// RatedModeP99AtTargetRPS records the P99 latency observed when the
	// rated-load profile drives the server at the per-scenario target
	// RPS. The key is Scenario.Name(); the value is the merged-across-
	// runs P99 in nanoseconds (time.Duration's wire encoding).
	RatedModeP99AtTargetRPS map[string]time.Duration `json:"rated_mode_p99_at_target_rps"`

	// LatencyAtSLO is the headline metric: per-scenario, the maximum
	// sustained RPS at which the merged-across-runs P99 stays under the
	// given SLO threshold (in milliseconds).
	//
	// Outer key: Scenario.Name(). Inner key: SLO threshold in
	// milliseconds (10, 50, 100, 500, 1000). Inner value: max sustained
	// RPS, rounded to integer for stability across reruns.
	LatencyAtSLO map[string]map[int]int `json:"latency_at_slo"`

	// HdrHistogramB64 carries the merged-across-runs HdrHistogram for
	// every scenario the adapter served. Keyed by Scenario.Name();
	// value is V2-compressed base64. Downstream tools can re-merge
	// across hosts / archs / git refs without re-running the bench.
	HdrHistogramB64 map[string]string `json:"hdr_histogram_b64"`

	// LoadgenCPUP95 is the 95th-percentile loadgen CPU usage observed
	// during the run (per scenario). Anchors the read: a number where
	// loadgen-side CPU was saturated cannot be claimed as a server
	// bottleneck.
	LoadgenCPUP95 map[string]float64 `json:"loadgen_cpu_p95"`

	// SentVsHandledDeltaPct records the delta between requests sent by
	// loadgen and requests acknowledged by the server, expressed as a
	// percentage of sent. Per scenario. >2% indicates the server is
	// dropping connections / replies — release-gate signal.
	SentVsHandledDeltaPct map[string]float64 `json:"sent_vs_handled_delta_pct"`
}

// ValidationResults captures the fixture-graph property tests and the
// RESTler-style stateful fuzzing summary. Fields kept loose (string
// counters keyed by name) so the validator can grow new properties
// without an v6 bump.
type ValidationResults struct {
	StartedAt        time.Time `json:"started_at"`
	FinishedAt       time.Time `json:"finished_at"`
	PropertiesPassed int       `json:"properties_passed"`
	PropertiesFailed int       `json:"properties_failed"`

	// FailureSummaries maps property name → human-readable summary of
	// the violation (1-line each). Empty when every property passed.
	FailureSummaries map[string]string `json:"failure_summaries,omitempty"`

	// FaultInjectionSeed is the deterministic seed used by the replay
	// harness. Empty when the fault-injection tier was disabled for
	// this run.
	FaultInjectionSeed string `json:"fault_injection_seed,omitempty"`
}

// SoakSummary captures the long-running soak metrics. Populated only
// when the orchestrator runs in `mage Validate` mode with
// VALIDATE_DURATION ≥ 6h.
type SoakSummary struct {
	Duration              time.Duration `json:"duration"`
	RestartedProcesses    int           `json:"restarted_processes"`
	GoroutineLeakDetected bool          `json:"goroutine_leak_detected"`
	HeapGrowthMB          float64       `json:"heap_growth_mb"`

	// PerHourErrorRate is the average non-2xx rate observed during the
	// soak, expressed as a percentage of total requests.
	PerHourErrorRate float64 `json:"per_hour_error_rate"`
}
