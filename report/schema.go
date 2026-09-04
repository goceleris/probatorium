package report

import (
	"strings"
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
//   - 5.2 — per-(adapter, scenario) server resource aggregates +
//     time-series (probatorium#154). Adds ServerResult.Resources.
//     Additive and fully nullable: every metric leaf is a pointer
//     so non-Go competitors (no goroutine/GC/heap) serialize as
//     JSON null while RSS/CPU/FD stay populated. Older readers
//     ignore the field.
//   - 5.3 — per-cell OUTCOME classification (probatorium). Adds
//     ServerResult.CellStatuses: scenario → "not_applicable" | "dnf"
//     for cells that did not produce a real number. A cell that did
//     not run (route/protocol not implemented, or dial/port/crash)
//     no longer leaks into saturation_mode_rps / latency_at_slo as a
//     0-RPS also-ran; it is recorded here instead. Additive — older
//     readers ignore the field and a v5.2 document (no cell_statuses)
//     still decodes.
//   - 5.4 — per-run outcome evidence + "suspect" status (v3.9 harness
//     hardening). Adds ServerResult.CellRunStatuses: scenario → one
//     status per scheduled run (execution order), emitted only when at
//     least one run was non-OK, so a clean rerun can never erase a
//     prior crash from the record. Adds the "suspect" CellStatus for
//     completed cells whose loadgen error ratio exceeded the
//     scenario's error budget: the data exists (and stays in the
//     headline maps) but its integrity is questionable. Additive —
//     older readers ignore both and a v5.3 document still decodes.
//     Also additive within 5.4: ServerResult.ConnectErrors, the
//     per-scenario dial/handshake-failure subset of the loadgen error
//     total (loadgen Result.ConnectErrors), emitted only when nonzero.
//   - 5.5 — network-bound annotation for large-payload cells. Adds
//     Environment.FabricLineRateBitsPerSec (the fabric's theoretical
//     egress ceiling) and ServerResult.NetworkBound: scenario → true for
//     cells whose achieved bandwidth sat at/near the fabric line rate
//     while the loadgen still had CPU headroom — i.e. the NIC, not the
//     server, was the bottleneck, so raw RPS converges across fast
//     adapters and must NOT be read as a ranking. The CPU/RSS efficiency
//     in ServerResult.Resources (now populated, finally) is the
//     differentiator for those cells. Both additive and omitted when
//     absent: a Tailscale-overlay run (no known line rate) emits neither.
const SchemaVersion = "5.5"

// CellStatus classifies the OUTCOME of a single (scenario, server)
// cell. It is the single source of truth for whether a cell ran and
// produced a real number, and it travels with the cell through both
// result-merge paths (the in-process runner and the cluster mage Bench
// path) into the renderer.
//
// Only [CellOK] cells contribute a ranked datapoint to the headline
// maps (saturation_mode_rps / latency_at_slo / hdr_histogram_b64).
// [CellNotApplicable] and [CellDNF] cells are recorded in
// ServerResult.CellStatuses and rendered as "N/A" / "DNF" — never as a
// 0-RPS row, which would overstate the field of real competitors.
type CellStatus string

const (
	// CellOK is a cell that ran and produced a real measurement.
	CellOK CellStatus = "ok"
	// CellNotApplicable is a cell the adapter could not serve because
	// it does not implement the route or speak the protocol — a
	// capability the scheduler trusted but the adapter does not honour.
	CellNotApplicable CellStatus = "not_applicable"
	// CellDNF is a cell that failed to run for infrastructure reasons
	// (dial / port / crash / timeout). Loud by design: a real server
	// crash must surface, never be silently bucketed as not-applicable.
	CellDNF CellStatus = "dnf"
	// CellSuspect is a cell that ran and produced a real measurement
	// whose integrity is questionable: the loadgen error ratio exceeded
	// the scenario's error budget, or a sibling run of the same cell
	// failed against the server. The data is kept (it still appears in
	// the headline maps) but flagged — never silently promoted back to
	// OK, and never ranked as a leader. Schema v5.4+.
	CellSuspect CellStatus = "suspect"
)

// HasData reports whether cells with this status carry real measurement
// samples. The empty string is legacy-OK (producers that pre-date the
// v5.3 classification only ever hand over OK cells); suspect cells keep
// their data — surfacing the number next to the flag is the point.
func (s CellStatus) HasData() bool {
	return s == "" || s == CellOK || s == CellSuspect
}

// ClassifyCellError maps a per-cell error string to a [CellStatus].
// An empty error means the cell ran (CellOK).
//
// The split: "capability-lie" means the adapter does not implement the
// route (zero successes against a live server) → CellNotApplicable —
// EXCEPT the legacy ratio-fired form: pre-v3.9 runners emitted
// "capability-lie: ... got high error ratio ... (errors=N/requests=M)"
// and only with requests > 0, which under the zero-successes rule can
// never be a genuine gap (v3.8's io_uring crash cell, 4029 req / 33.1M
// err, wore exactly that string) → CellDNF, so stale artefacts cannot
// re-enter the skip list as N/A.
// "suspect:" is the runner's error-ratio gate — the cell completed with
// real data but its errors exceeded the scenario's budget → CellSuspect.
// "read server settings" (the H2 prior-knowledge preface going unanswered)
// is N/A ONLY when it TIMED OUT — the server simply never spoke H2; a
// reset / EOF / broken pipe on that handshake means the connection was
// actively torn down (an H2 server that crashed mid-handshake) and is a
// DNF, not N/A. Everything else — adapter start, ready-check,
// address-already-in-use, loadgen.New / loadgen.Run, dial / reset / EOF /
// timeout, plus the runner's synthesised "server-down:" /
// "server-died-mid-cell:" / "interrupted:" / "zero-request cell" reasons —
// is an infra failure → CellDNF. ("zero-request cell" was N/A before
// v5.4; the v3.8 run proved every dead-SUT and interrupted cell wears
// that string, and genuine capability gaps never reach loadgen — the
// scheduler skips them via featureSetFor — so zero requests is now loud.)
// Ambiguous errors default to CellDNF, never to CellNotApplicable: a real
// crash must not be silently excused as N/A.
func ClassifyCellError(errMsg string) CellStatus {
	switch {
	case errMsg == "":
		return CellOK
	case strings.HasPrefix(errMsg, "suspect:"):
		return CellSuspect
	case strings.Contains(errMsg, "read server settings"):
		if strings.Contains(errMsg, "i/o timeout") || strings.Contains(errMsg, "deadline exceeded") {
			return CellNotApplicable
		}
		return CellDNF
	case strings.Contains(errMsg, "capability-lie"):
		// Legacy ratio-fired guard (pre-v3.9) — requests were > 0, so
		// this cannot be a genuine capability gap under today's rule.
		if strings.Contains(errMsg, "got high error ratio") {
			return CellDNF
		}
		return CellNotApplicable
	default:
		return CellDNF
	}
}

// ReduceCellStatus folds a cell's per-run statuses into the cell-level
// status. All-OK stays OK. A cell with data whose only blemishes are
// harness-side interruptions (demoted=false) also stays OK — RunStatuses
// still carries the evidence. A cell with data plus any SUT-behaviour
// failure (demoted=true) is suspect: the data exists, but a sibling run
// crashed / lied / stormed, so an OK rerun can never erase the record
// into a clean "ok" (the v3.8 OK-promotion bug). With no data at all,
// any DNF run wins (loud) over not-applicable.
//
// Shared reduction for both result-merge paths. cmd/runner currently
// carries a private copy with the identical table (reduceCellStatus,
// pinned by cmd/runner/cellclassify_test.go); keep the two in sync
// until the runner delegates here.
func ReduceCellStatus(runs []CellStatus, hasData, demoted bool) CellStatus {
	allOK := true
	anyDNF := false
	for _, st := range runs {
		switch st {
		case CellOK:
		case CellDNF:
			allOK = false
			anyDNF = true
		default:
			allOK = false
		}
	}
	switch {
	case allOK:
		return CellOK
	case hasData && demoted:
		return CellSuspect
	case hasData:
		return CellOK
	case anyDNF:
		return CellDNF
	default:
		return CellNotApplicable
	}
}

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

	// FabricLineRateBitsPerSec is the fabric's theoretical egress ceiling
	// in bits/sec (e.g. 20e9 for the 2x10G LACP LAN). Used by BuildDocument
	// to flag large-payload cells whose achieved bandwidth sat at the NIC
	// ceiling (network-bound) rather than the server's CPU limit. Zero/
	// omitted when the line rate is unknown (the Tailscale overlay), in
	// which case no cell is flagged. Schema v5.5+.
	FabricLineRateBitsPerSec int64 `json:"fabric_line_rate_bits_per_sec,omitempty"`
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

	// ConnectErrors is the summed-across-runs dial/handshake-failure
	// subset of loadgen's error total, per scenario (additive within
	// schema v5.4; loadgen Result.ConnectErrors). Splits "server
	// unreachable" from "server answering with errors" next to the
	// headline number. Omitted when zero (including every pre-
	// ConnectErrors loadgen build). Older readers ignore it.
	ConnectErrors map[string]uint64 `json:"connect_errors,omitempty"`

	// Resources carries the server-side resource aggregate (RSS, CPU,
	// GC pause, goroutine / FD high-water) sampled at 1 Hz alongside the
	// run by cmd/observer + mpstat, keyed by Scenario.Name(). Schema
	// v5.2+ (probatorium#154). Nil/omitted when a run captured no
	// observer data (e.g. the local-loopback runner). Within an entry
	// every metric is a nullable pointer: non-Go competitors expose
	// RSS/CPU/FD only, leaving goroutine/GC/heap null.
	Resources map[string]*ResourceStats `json:"resources,omitempty"`

	// NetworkBound flags, per scenario, the cells whose achieved egress
	// bandwidth sat at/near the fabric line rate while the loadgen still
	// had CPU headroom — the NIC, not the server, capped throughput. For
	// these cells the saturation RPS converges across every fast adapter
	// and is NOT a ranking signal; compare ServerResult.Resources (CPU/RSS
	// at the shared ceiling) instead. Schema v5.5+. Omitted when no cell
	// for this adapter was network-bound (every small-payload run, and
	// every run on a fabric with no known line rate). Older readers ignore
	// it.
	NetworkBound map[string]bool `json:"network_bound,omitempty"`

	// CellStatuses records the non-OK outcome of every scenario this
	// adapter did NOT produce a clean number for, keyed by
	// Scenario.Name(); the value is "not_applicable" (route/protocol
	// unimplemented), "dnf" (dial/port/crash/timeout) or "suspect"
	// (v5.4+: data exists but its error ratio blew the scenario's
	// budget). Schema v5.3+. A not_applicable / dnf scenario is
	// deliberately absent from SaturationModeRPS / LatencyAtSLO /
	// HdrHistogramB64 — it did not run, so it is never ranked as a
	// 0-RPS row. A suspect scenario keeps its headline numbers next to
	// the flag. Omitted when every cell for this adapter ran (CellOK).
	// Older readers ignore it.
	CellStatuses map[string]string `json:"cell_statuses,omitempty"`

	// CellRunStatuses records, for any scenario where at least one run
	// came back non-OK, the per-run outcome sequence in execution order
	// (e.g. ["dnf","ok","ok"]). Schema v5.4+. Complements CellStatuses:
	// a cell that recovered on a later run still carries the earlier
	// failure here, so an OK rerun can never erase non-OK evidence (the
	// v3.8 celeris column crash vanished exactly this way). Omitted
	// when every run was OK. Older readers ignore it.
	CellRunStatuses map[string][]string `json:"cell_run_statuses,omitempty"`
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
	// Requests5xxExpected: 5xx from corpus states marked `expect: 5xx`
	// (designed-to-fail routes). Requests5xx above is UNEXPECTED only.
	Requests5xxExpected int64 `json:"requests_5xx_expected"`
	// InvariantHits: unexpected 5xx whose body carried a refapp
	// invariant marker (x-invariant) -- a self-reported invariant
	// violation surfaced as a first-class signal.
	InvariantHits int64 `json:"invariant_hits"`
	// RequestsCutAtDeadline: requests in flight when the tier budget
	// expired. Excluded from RequestsError, which is failures only.
	RequestsCutAtDeadline int64 `json:"requests_cut_at_deadline"`

	// Per-slice sub-tallies (one per workload-mix slice from
	// validator-prod issue #55). Each is a plain `map[string]int64`
	// rather than a typed struct so this schema doesn't have to
	// re-version every time the validator adds a counter.
	//
	// Canonical keys (validator package writes these):
	//   adversarial  → adv_sent, adv_well_rejected, adv_wrong_accepted, adv_hang_until_timeout
	//   h2c_churn    → h2c_sent, h2c_upgraded, h2c_declined, h2c_crashed, h2c_hang
	//   ws_torture   → ws_sent, ws_upgraded, ws_handshake_fail, ws_closed_correctly, ws_accepted_bad_frame, ws_hang_no_close, ws_endpoint_absent
	//   sse_kill     → sse_sent, sse_established, sse_events_read, sse_killed_mid_stream, sse_server_closed_early, sse_handshake_fail, sse_endpoint_absent
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

// ResourceStats is the server-side resource aggregate for one
// (adapter, scenario) cell: a scalar summary plus a downsampled
// time-series. Schema v5.2+ (probatorium#154). Built from one per-cell
// observer.sqlite (`observations` table) + one per-cell mpstat cpu.log.
//
// Every metric is a pointer so each can serialize as JSON null
// independently: non-Go competitors expose RSS/CPU/FD only, leaving the
// runtime-derived metrics (goroutine/GC/heap) null.
type ResourceStats struct {
	Summary ResourceSummary `json:"summary"`

	// Series is the downsampled (≤60-point) resource trajectory. Joined
	// positionally between the observer's unix-second rows and mpstat's
	// wall-clock rows; both sample at ~1 Hz over the same window.
	Series []ResourcePoint `json:"series,omitempty"`
}

// ResourceSummary is the scalar headline of a cell's resource usage.
// Pointers are nil when the metric was treated as absent (a competitor
// with no Go runtime, or no CPU sampler row).
type ResourceSummary struct {
	PeakRSSBytes   *int64   `json:"peak_rss_bytes,omitempty"`
	SteadyRSSBytes *int64   `json:"steady_rss_bytes,omitempty"`
	MeanCPUPct     *float64 `json:"mean_cpu_pct,omitempty"`
	GCPauseP99Ns   *int64   `json:"gc_pause_p99_ns,omitempty"`
	GoroutineHWM   *int64   `json:"goroutine_hwm,omitempty"`
	FDHWM          *int64   `json:"fd_hwm,omitempty"`
}

// ResourcePoint is one downsampled sample in a ResourceStats.Series.
// TSUnix is the observer's unix-second timestamp; the remaining metrics
// are nullable for the same reasons as ResourceSummary.
type ResourcePoint struct {
	TSUnix         int64    `json:"ts_unix"`
	RSSBytes       *int64   `json:"rss_bytes,omitempty"`
	CPUPct         *float64 `json:"cpu_pct,omitempty"`
	Goroutines     *int64   `json:"goroutines,omitempty"`
	HeapInuseBytes *int64   `json:"heap_inuse_bytes,omitempty"`
	FDCount        *int64   `json:"fd_count,omitempty"`
}
