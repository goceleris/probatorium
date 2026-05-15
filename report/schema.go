package report

import (
	"time"
)

// SchemaVersion is the on-disk JSON schema identifier emitted by every
// probatorium results file.
//
// History:
//   - 5.0 — first probatorium schema; additive over the v4 schema in
//     goceleris/benchmarks. Tier1Summary + Tier3Summary on the
//     top-level ValidationResults.
//   - 5.1 — per-cell breakdown for matrix runs
//     (probatorium#103). Adds ValidationResults.Cells; single-
//     cell runs unchanged. ValidationCellResult keys (refapp, engine,
//     arch) triple. Older readers ignore the Cells field and
//     fall back to top-level Tier1/Tier3 when present.
const SchemaVersion = "5.1"

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

	// Tier1 and Tier3 are the per-tier final tallies emitted by the
	// validator orchestrator for SINGLE-cell runs. Optional — pure-
	// bench runs leave both unset; single-cell validate runs always
	// populate Tier1 and (when a corpus is present) Tier3.
	//
	// For MULTI-cell matrix runs (refapp × engine × arch — see
	// probatorium#103), prefer the Cells slice below: each entry
	// keys the (refapp, engine, arch) triple and carries its own
	// Tier1Summary + Tier3Summary. The top-level Tier1/Tier3 are
	// left nil in multi-cell mode for back-compat: pre-v5.1 readers
	// won't see misleading top-level numbers when the truth is per
	// cell.
	Tier1 *Tier1Summary `json:"tier_1,omitempty"`
	Tier3 *Tier3Summary `json:"tier_3,omitempty"`

	// Cells is the per-cell breakdown for matrix runs (schema v5.1+).
	// Single-cell runs leave this empty and populate Tier1/Tier3 at
	// the top level instead.
	//
	// The cross-engine ValidateDiff in mage_diff.go walks this slice
	// to compute per-(refapp, arch) engine-divergence findings; the
	// existing cross-arch diff continues to work from the top-level
	// (or first Cells entry) snapshot.
	Cells []ValidationCellResult `json:"cells,omitempty"`
}

// ValidationCellResult is one (refapp × engine × arch) cell from a matrix
// validate run. Mirrors what a single-cell ValidationResults would
// carry, plus the keying fields that distinguish it.
//
// Added in schema v5.1 per probatorium#103.
type ValidationCellResult struct {
	Refapp string        `json:"refapp"`
	Engine string        `json:"engine"`
	Arch   string        `json:"arch"`
	Tier1  *Tier1Summary `json:"tier_1,omitempty"`
	Tier3  *Tier3Summary `json:"tier_3,omitempty"`
}

// Tier1Summary mirrors the validator's tier1TallySnapshot in the
// canonical v5 shape. The struct is duplicated (not imported from the
// validation package) to keep report/ a leaf node — it owns the wire
// shape, nothing else.
//
// New per-slice sub-tallies land as optional nested struct fields so
// older readers can ignore unknown keys.
type Tier1Summary struct {
	RequestsSent  int64 `json:"requests_sent"`
	Requests2xx   int64 `json:"requests_2xx"`
	Requests4xx   int64 `json:"requests_4xx"`
	Requests5xx   int64 `json:"requests_5xx"`
	RequestsError int64 `json:"requests_error"`

	// Per-slice sub-tallies (one per workload-mix slice from
	// validator-prod issue #55). Each is a plain `map[string]int64`
	// rather than a typed struct so this schema doesn't have to
	// re-version every time the validator adds a counter.
	//
	// Canonical keys (validator package writes these):
	//   adversarial  → adv_sent, adv_well_rejected, adv_wrong_accepted, adv_hang_until_timeout
	//   h2c_churn    → h2c_sent, h2c_upgraded, h2c_declined, h2c_crashed, h2c_hang
	//   ws_torture   → ws_sent, ws_upgraded, ws_handshake_fail, ws_closed_correctly, ws_accepted_bad_frame, ws_hang_no_close
	//   sse_kill     → sse_sent, sse_established, sse_events_read, sse_killed_mid_stream, sse_server_closed_early, sse_handshake_fail
	Adversarial map[string]int64 `json:"adversarial,omitempty"`
	H2CChurn    map[string]int64 `json:"h2c_churn,omitempty"`
	WSTorture   map[string]int64 `json:"ws_torture,omitempty"`
	SSEKill     map[string]int64 `json:"sse_kill,omitempty"`
}

// Tier3Summary mirrors the validator's tier3TallySnapshot.
type Tier3Summary struct {
	SeedsAttempted int64 `json:"seeds_attempted"`
	SeedsPassed    int64 `json:"seeds_passed"`
	SeedsFailed    int64 `json:"seeds_failed"`
	SeedsErrored   int64 `json:"seeds_errored"`
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
