package report

import (
	"strconv"
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
//   - 5.7 — requests_panic_expected (designed panics netted out of I-PANIC).
//   - 5.6 — the in-process property loop (probatorium, after the
//     2026-09-04 soak reached an 18 GB heap with properties_passed=0).
//     Adds the loop's counters on Tier1Summary (property_evaluations,
//     property_skips, property_violations, property_violation_ids,
//     property_poll_errors, property_loop_skipped) and the per-cell
//     properties_passed / properties_failed / properties_not_instrumented
//     / properties_not_judged / failure_summaries on
//     ValidationCellResult. Before these, the I-* snapshot predicates
//     listed in plan.json were never evaluated by any run and
//     properties_passed/failed were always 0/0. Additive -- older readers
//     ignore the fields -- but the version bump is load-bearing for
//     mage ValidateGate: a 5.5 document has no property loop, so the
//     "loop never evaluated anything" check defaults off for it.
//   - 5.8 — property verdict COVERAGE (probatorium#299). Adds
//     ValidationCellResult.PropertiesNotJudgedByDesign: the subset of
//     properties_not_judged whose predicate needs a longer observation
//     window than this cell's property loop ran for. Without it the
//     gate cannot tell a 150 s nightly cell, which structurally cannot
//     judge a memory slope, from an hour-long soak cell whose slope
//     oracle stayed silent anyway -- so it said nothing about either
//     and a nightly PASS carried no opinion on memory growth at all.
//     Additive -- older readers ignore the field -- but load-bearing
//     for mage ValidateGate: a 5.6/5.7 document has the not-judged
//     list without the by-design one, so its short-cell oracles would
//     every one of them read as coverage failures, and the check
//     defaults off for it.
//   - 5.9 — per-fire capture for the h2c-churn and WS-torture walkers
//     (celeris#588). Adds, on Tier1Summary, the per-leg latency
//     histograms (h2c_latency / ws_latency), the bounded rings of slow
//     fires (h2c_slow_reads / ws_slow_reads: instant, per-leg elapsed,
//     outcome, verbatim error, both socket addresses), the refapp's
//     ready instant (ready_at) and the tail of its stderr
//     (refapp_stderr_tail). Before these the v1.5.11 soak's single
//     h2c_hang and single ws_handshake_fail were unattributable from
//     the artifact: the cell kept a cause class and one max elapsed.
//     Adds, in the same version, Tier1Summary.WSEcho (ws_echo_* keys):
//     64 KiB frames echoed through the refapp's /ws and verified
//     byte-for-byte, the wire-level oracle for the io_uring
//     SEND_ZC-vs-inline-write window no earlier tier reached
//     (celeris#587). All additive: older readers ignore every field, the
//     gate reads an absent map as all zeros, the gated totals keep their
//     meaning and the new keys are not gated.
//     Additive; older readers ignore every field. The gated totals keep
//     their meaning and the new keys are not gated.
//     Adds, in the same version, per-SCENARIO resource windows (celeris#585). Adds
//     ServerResult.ScenarioResources: scenario → the column's raw 1 Hz
//     mpstat + observer series SLICED to that scenario's own runner
//     window (started_at + warmup .. completed_at), plus
//     ResourceSummary.MeanSoftPct (mpstat %soft), SUTProcessCPUPct
//     (SUT utime+stime from /proc, in percent of ONE core), and
//     ResourceStats.Window (the slice bounds + sample counts). Until now
//     ServerResult.Resources carried ONE column-wide mean stamped onto
//     every scenario the column ran (the 27 scenarios of a celeris column
//     all reported the identical mean_cpu_pct), so no per-cell CPU
//     comparison — and no cpu-per-byte A/B — was possible. Resources is
//     unchanged (still the column-wide aggregate); the new map is the
//     per-scenario view. Also adds Environment.SUTEnv, the KEY=VALUE
//     overrides the bench passed into the SUT process, so an A/B arm is
//     identifiable from results.json alone. Additive — older readers
//     ignore every new field.
//   - 5.10 — the adaptive controller's own promotion signals
//     (celeris#580 follow-up). Adds, on Tier1Summary,
//     PeakConnsPerWorker and MeanBytesPerReq, and on the per-cell
//     series the four raw counters they reduce
//     (engine_workers, engine_requests_total, engine_bytes_read,
//     engine_bytes_written). Nightly 34876253223 reported seven
//     adaptive cells with adaptive_switches == 0 and the artifact
//     carried no way to tell a harness that under-loaded them from a
//     controller that deliberately suppressed a link-bound workload
//     from a celeris defect: it recorded the decision and none of its
//     inputs. Additive; older readers ignore every field and no new
//     key is gated.
//   - 5.11 — engine-side connection accounting and the must-stay-zero
//     defect witnesses. Adds, on Tier1Summary, EngineZeroWitness (the
//     peak each witness reached, keyed by its debugvars name),
//     EngineAcceptCount / EngineCloseCount / EngineTransplantDetached /
//     EngineTransplantAdopted / EngineAsyncPromotedConns /
//     PeakStandbyActiveConns / EngineRequestsTotal, and the matching
//     thirteen columns on the per-cell series. The gate gains two
//     unconditional checks: any nonzero witness fails its cell with the
//     defect it witnesses, and an engine whose own request count falls
//     more than a factor of ten below the walker's requests_sent fails
//     as a counter defect. Both exist because the artifact recorded one
//     side of a two-sided quantity: celeris#624's drift could not be
//     attributed without the engine's close count beside the hook's,
//     and celeris#626 left epoll reporting 3,089 requests against a
//     walker that sent 3,306,726 with nothing to compare it to.
//     Additive; older readers ignore every field.
//   - 5.12 — per-cell RUN OUTCOME (probatorium#359). Adds
//     ValidationCellResult.Status (ok | failed | not_run) and
//     FailureReason. A cell whose refapp never started and a cell whose
//     oracle fired both land as an all-zero tally, and until now the
//     only place that difference existed was the validator's run log:
//     nightly 34818888908 recorded eight failed cells and the document
//     said nothing about any of them. Additive -- an absent status
//     means "not recorded", never "ok" -- and no new key is gated: a
//     not_run cell already fails [Gate] as a dead cell.
//   - 5.13 — resume provenance (probatorium#376). Adds
//     ValidationResults.Resume and ValidationCellResult.ResumedFrom.
//     A run started with -matrix-resume-from now seeds its document
//     with the cells the interrupted run already made final, so the
//     absolute gate judges the whole soak rather than the remainder —
//     the weekend tier asks for 64 cells and a resume of the last ten
//     used to hand it ten. A document that claims cells the writing
//     process did not measure has to be auditable, so Resume records
//     the source directory, the prior run's window, the inherited /
//     ran split and every prior entry deliberately NOT carried over
//     (a cell that was mid-flight when the runner was lost is re-run,
//     never inherited), and each inherited cell carries ResumedFrom.
//     An inherited cell keeps the 5.12 Status the run that MEASURED it
//     recorded; ResumedFrom is what says that run was not this one.
//     Additive; older readers ignore both fields and neither is gated.
//   - 5.14 — the engine error-class split (celeris#645, celeris#646).
//     celeris#646 turned EngineMetrics.ErrorCount from one atomic a dozen
//     branches incremented into the derived SUM of eleven cause buckets,
//     plus StandbyErrorCount, the adaptive engine's share-by-sub-engine
//     split. Adds, on Tier1Summary, EngineErrorCount, EngineErrorClasses
//     (the end-of-cell total of each bucket, keyed by its debugvars name;
//     report.ErrorClasses says what each counts) and
//     EngineStandbyErrorCount, and SIX of the twelve as per-cell series
//     columns: engine_error_accept_fd_limit, engine_error_accept_cancelled,
//     engine_error_accept_other, engine_error_conn_table_cap,
//     engine_error_send_peer_gone and engine_standby_error_count. The
//     other six are end-of-cell totals only — report.ErrorClasses.Why
//     records the call bucket by bucket, and the short version is that a
//     bucket earns a 1 Hz column when the question asked of it is "when"
//     and the artifact carries something timestamped to join that
//     against. Nothing here is gated, unlike the 5.11 witnesses beside
//     it: these count things a correct engine does under load
//     (celeris#646 measured 88,010 ErrorSendPeerGone over 88,776 accepts
//     on a healthy io_uring load), so a threshold before a run has said
//     what normal looks like would be a number nobody measured.
//     Additive; older readers ignore every field.
//   - 5.15 — every published engine counter reaches the artifact
//     (probatorium#391). probatorium#386 made the refapps publish all
//     fifty-two engine.EngineMetrics fields, and twenty of them still
//     reached no artifact: ParseDebugVars, properties.Snapshot, the
//     per-cell series and this struct were each a hand-list of their own,
//     so celeris.engine_transplant_stranded was emitted by every refapp and
//     recorded nowhere. Adds, on Tier1Summary, EngineCounters — each
//     report.EngineCounters entry reduced over the samples that carried
//     the engine block by its declared Kind (the highest reading for a
//     running maximum, the last for every other kind, never a sum), keyed
//     by its debugvars name: the four celeris#647
//     hand-off outcomes (handoff_refused, drain_stopped, stranded,
//     adopt_refused), the eight celeris#533 / celeris#607 recv-stall
//     witnesses, detach and zero-copy accounting, the inline / ring egress
//     split and async_routes, plus four keys the series already carried
//     with no end-of-cell total (engine_standby_close_count,
//     engine_workers, engine_bytes_read, engine_bytes_written). Adds TEN
//     per-cell series columns: the four hand-off outcomes,
//     engine_recv_stall_nanos, engine_recv_stall_max_nanos,
//     engine_recv_linked_arms, engine_recv_linked_blocked_nanos,
//     engine_recv_linked_blocked_max_nanos and engine_detached_conns.
//     report.EngineCounters.Why records the column-or-total call key by
//     key, and .Kind how readings may be combined (two are running maxima
//     that must never be summed). celeris.engine_throughput is the one
//     published key carried nowhere: no engine assigns it (celeris#653),
//     so a carried 0 would read as a measured rate. Nothing new is gated:
//     engine_transplant_stranded is documented must-stay-zero, but moving
//     it into ZeroWitnessMeaning is a gate change this version does not
//     make. Additive; older readers ignore every field.
//   - 5.16 — the celeris#657 hand-off counters (probatorium#410). celeris
//     9f4d89b (celeris#676, #681, #687) added eighteen EngineMetrics
//     fields: the stale-recv loss witnesses, the fd-lifetime rule's holds,
//     reaps and refusals, and the post-switch sweep with its five residual
//     gauges. Adds all eighteen to Tier1Summary.EngineCounters and TWELVE
//     per-cell series columns (W1 = engine_stale_recv_data_transplanted +
//     _unattributed, W2 = engine_transplant_handoff_in_flight, the
//     must-stay-zero hold_rescued, double_claim and reap_failed, the
//     sweep's passes and the five engine_transplant_residual_* gauges).
//     Adds the report.CounterPeakGauge kind: the five residual gauges are
//     reduced by their HIGHEST reading, because celeris asks for them to be
//     read at every switch verdict and only a zero peak clears every switch
//     of the cell. Nothing new is gated: the six counters celeris documents
//     as must-stay-zero carry their meaning in EngineCounter.MustStayZero,
//     and moving any of them into ZeroWitnessMeaning is a gate change this
//     version does not make. Additive; older readers ignore every field.
const SchemaVersion = "5.16"

// SchemaAtLeast reports whether version (a "major.minor" string as
// emitted in SchemaVersion) is at least want. Malformed input is
// treated as older than anything.
func SchemaAtLeast(version, want string) bool {
	vm, vn, ok1 := parseSchema(version)
	wm, wn, ok2 := parseSchema(want)
	if !ok1 || !ok2 {
		return false
	}
	return vm > wm || (vm == wm && vn >= wn)
}

func parseSchema(v string) (major, minor int, ok bool) {
	parts := strings.SplitN(strings.TrimSpace(v), ".", 3)
	if len(parts) < 2 {
		return 0, 0, false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return 0, 0, false
	}
	minor, err = strconv.Atoi(parts[1])
	if err != nil {
		return 0, 0, false
	}
	return major, minor, true
}

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

	// SUTEnv records the KEY=VALUE environment overrides the bench merged
	// into the SUT process (benchmark-tier.yml `sut_env` → BENCH_SUT_ENV →
	// ansible bench_sut_env). Schema v5.9+ (celeris#585). Omitted when the
	// run passed none, so an A/B arm (e.g. CELERIS_IOURING_SEND_ZC=off) is
	// identifiable from results.json alone, next to the engine's own
	// server.log arm line.
	SUTEnv map[string]string `json:"sut_env,omitempty"`
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

	// ScenarioResources is the PER-SCENARIO slice of the same raw series
	// (schema v5.9+, celeris#585), keyed by Scenario.Name(): the column's
	// 1 Hz mpstat cpu.log and observer.sqlite rows windowed to the
	// scenario's own runner window (started_at + warmup .. completed_at)
	// and summarised on their own. Resources above is the COLUMN-WIDE
	// aggregate stamped onto every scenario (one mpstat run covers the
	// whole column pass); this map is what a per-cell CPU comparison must
	// read. Entries carry ResourceStats.Window with the slice bounds and
	// sample counts. A scenario whose window caught NO sample has no
	// entry here — "no data", never a zero — so absence is the signal.
	// Omitted when no scenario carried a windowed slice.
	ScenarioResources map[string]*ResourceStats `json:"scenario_resources,omitempty"`

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

	// Resume is set ONLY when this document is the union of an earlier,
	// interrupted run's cells and this run's (schema 5.12,
	// probatorium#376). Nil means every cell in Cells was measured by
	// the run that wrote the document.
	//
	// The gate counts entries in Cells, so a merged document is a
	// document that claims work this process did not do. That claim is
	// legitimate -- the earlier run did do it -- but only if it is
	// auditable, which is what this block and
	// [ValidationCellResult.ResumedFrom] are for.
	Resume *ResumeProvenance `json:"resume,omitempty"`
}

// ResumeProvenance records where a merged document's inherited cells came
// from, so a reader can tell which half of a 64-cell verdict this run
// actually measured (schema 5.13, probatorium#376).
//
// A 24 h soak has roughly a one-in-four history of losing its runner
// mid-run, and `resume_from` is the mitigation. A resumed run has to reach
// a whole-soak verdict -- the weekend tier's gate wants 64 cells -- without
// letting "the previous run measured this" become indistinguishable from
// "this run measured it".
type ResumeProvenance struct {
	// From is the results directory the inherited cells were read from,
	// exactly as it was passed to -matrix-resume-from.
	From string `json:"from"`
	// PriorStartedAt / PriorFinishedAt are the earlier run's own window.
	// They are NOT merged into StartedAt/FinishedAt: the merged
	// document's window is this run's, and the fact that the two halves
	// were measured hours apart is a property a reader must be able to
	// see rather than one the document smooths over.
	PriorStartedAt  time.Time `json:"prior_started_at,omitzero"`
	PriorFinishedAt time.Time `json:"prior_finished_at,omitzero"`
	// InheritedCells is how many entries in Cells carry ResumedFrom, and
	// RanCells how many this run measured. Their sum is len(Cells).
	InheritedCells int `json:"inherited_cells"`
	RanCells       int `json:"ran_cells"`
	// NotInherited names every cell the prior document recorded that was
	// deliberately NOT carried over, with the reason -- "<refapp>/<engine>:
	// <why>". The mid-flight cell an interrupted run leaves behind is the
	// entry that matters: it is re-run, not inherited, and this is where
	// that decision is on the record.
	NotInherited []string `json:"not_inherited,omitempty"`
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
	// Soak is the cell's soak block (soak mode only). The per-cell
	// orchestrator already wrote it into the cell's own document; carrying
	// it here lets the merged matrix document -- the only thing the gate
	// reads -- keep it (probatorium#281).
	Soak *SoakSummary `json:"soak_summary,omitempty"`

	// PropertiesPassed / PropertiesFailed are the cell's snapshot-predicate
	// verdicts from the in-process property loop: passed = instrumented
	// predicates that reached a verdict at least once with zero
	// violations; failed = distinct predicates that violated at least
	// once. Both 0 when the loop never observed a sample (see
	// Tier1Summary.PropertyEvaluations).
	PropertiesPassed int `json:"properties_passed"`
	PropertiesFailed int `json:"properties_failed"`
	// PropertiesNotInstrumented lists evaluated predicates whose inputs
	// have no data source in this cell (they pass vacuously and are
	// excluded from PropertiesPassed) -- either globally, or because this
	// refapp does not install the middleware the predicate judges and so
	// does not declare it.
	//
	// A predicate that appears here in EVERY cell verified nothing in the
	// whole run; [GateOptions.RequireInstrumented] fails on that unless the
	// hole is on the [WaivedUninstrumented] record. Before that check this
	// list carried 9 of 14 predicates through a PASSING nightly and nobody
	// read it (probatorium#297).
	PropertiesNotInstrumented []string `json:"properties_not_instrumented,omitempty"`
	// PropertiesNotJudged lists instrumented predicates whose every
	// evaluation was a skip: the slope oracles (I-MEM-1/3/4) need a
	// 5 min warm-up plus a 10 min window, so a ~150 s nightly cell never
	// judges them. They are excluded from PropertiesPassed -- a short
	// cell's passed count must not read as "the leak oracles passed".
	PropertiesNotJudged []string `json:"properties_not_judged,omitempty"`
	// PropertiesNotJudgedByDesign is the subset of PropertiesNotJudged
	// this cell COULD NOT have judged: the predicate needs a longer
	// observation window than the cell's property loop ran for (a 150 s
	// nightly cell against I-MEM-1's 5 min warm-up plus 10 min span).
	// It is what lets the absolute gate tell "cannot judge here, by
	// design" from "should have judged and did not" -- see
	// [Coverage] (schema 5.8, probatorium#299).
	PropertiesNotJudgedByDesign []string `json:"properties_not_judged_by_design,omitempty"`
	// FailureSummaries maps a failed predicate ID to its first violation
	// message.
	FailureSummaries map[string]string `json:"failure_summaries,omitempty"`

	// Status is the matrix runner's verdict on the cell as a unit of work:
	// did it run, and did it pass? Empty on documents written before
	// schema 5.11 and on any path that does not go through the matrix
	// runner -- readers must treat "" as "not recorded", never as ok.
	//
	// The distinction [ValidationCellNotRun] carries is the one the
	// artifact could not previously express: a cell whose refapp never
	// started says NOTHING about celeris under load, while a cell whose
	// oracle fired is the finding. Both land as an all-zero tally, and
	// telling them apart meant reading the run log (probatorium#359).
	Status ValidationCellStatus `json:"status,omitempty"`
	// FailureReason is the verbatim error that classified the cell as
	// failed or not-run: the predicate violation, or the reason the
	// refapp would not start. Empty for an ok cell. The fuller evidence
	// stays in the cell directory -- refapp_stderr_tail.txt for a cell
	// that never came up, incidents/ for one whose oracle fired.
	FailureReason string `json:"failure_reason,omitempty"`

	// ResumedFrom marks a cell this run did NOT measure: it was carried
	// over verbatim from the earlier, interrupted run whose results
	// directory this names (schema 5.13, probatorium#376). Empty on
	// every cell the writing run ran itself.
	//
	// Per-cell rather than only run-level, because the run-level
	// [ResumeProvenance] says how many were inherited and this says
	// WHICH -- and the tallies in a cell are indistinguishable from a
	// freshly measured one, which is the whole hazard.
	//
	// It pairs with [Status], which is NOT rewritten on the way across:
	// an inherited cell keeps the ok/failed verdict of the run that
	// actually measured it, because that verdict is true and blanking it
	// would throw away a real finding. What would be false is reading
	// "ok" as "this run measured it" -- which is what ResumedFrom is
	// here to prevent. [ValidationCellNotRun] cannot appear on an
	// inherited cell at all: the matrix runner classifies not_run with
	// the same predicate the resume uses to decide a cell need not be
	// repeated, so a cell that never ran is always re-run, never
	// carried over.
	ResumedFrom string `json:"resumed_from,omitempty"`
}

// ValidationCellStatus is a matrix cell's outcome as a unit of work
// (schema 5.11, probatorium#359). It is deliberately NOT the cell's
// verdict on celeris: a cell can be [ValidationCellOK] here and still
// carry gated counters that fail [Gate].
type ValidationCellStatus string

const (
	// ValidationCellOK: the cell ran and the run recorded no error for it.
	ValidationCellOK ValidationCellStatus = "ok"
	// ValidationCellFailed: the cell ran -- it sent traffic, its property
	// loop reached verdicts -- and then something failed it. The tally is
	// evidence and the failure is about celeris.
	ValidationCellFailed ValidationCellStatus = "failed"
	// ValidationCellNotRun: the cell produced no evidence at all (no
	// requests, no property verdicts). Its refapp would not start, its
	// binary was missing, or its orchestrator could not be built. The
	// cell measured nothing, so it carries no opinion on celeris -- but
	// it is still a hole in the matrix and still fails the gate.
	ValidationCellNotRun ValidationCellStatus = "not_run"
)

// Tier1Summary mirrors the validator's tier1TallySnapshot in the
// canonical v5 shape. The struct is duplicated (not imported from the
// validation package) to keep report/ a leaf node — it owns the wire
// shape, nothing else.
//
// New per-slice sub-tallies land as optional nested struct fields so
// older readers can ignore unknown keys.
type Tier1Summary struct {
	RequestsSent int64 `json:"requests_sent"`
	Requests2xx  int64 `json:"requests_2xx"`
	Requests4xx  int64 `json:"requests_4xx"`
	// Requests401 / Requests404 / Requests429 split Requests4xx by class.
	// The lump sum hid a months-long failure: auth_session_ratelimit ran at
	// ~96% 4xx because celeris never issued the session cookie, so every
	// /me was a 401 and the walker re-logged in on each one. A 4xx rate
	// alone cannot tell that apart from a healthy run that is mostly
	// rate-limited or probing absent routes (probatorium#292).
	Requests401 int64 `json:"requests_401,omitempty"`
	Requests404 int64 `json:"requests_404,omitempty"`
	Requests429 int64 `json:"requests_429,omitempty"`
	// WalkerLogins counts pre-walk logins (one per walker); WalkerRelogins
	// counts 401-triggered re-logins during the walk. A healthy run re-logs
	// in only after a deliberate logout, so relogins scaling with request
	// count is the signature of a server not honouring sessions at all.
	WalkerLogins   int64 `json:"walker_logins,omitempty"`
	WalkerRelogins int64 `json:"walker_relogins,omitempty"`
	// WalkerLogouts counts logouts the walk performed on purpose (the
	// matrix's `logout:` request). Each costs exactly one 401 and one
	// re-login, so it is the correct baseline for WalkerRelogins: the
	// auth_session_ratelimit matrix reaches its logout state from three
	// others, so a healthy run logs out thousands of times.
	WalkerLogouts int64 `json:"walker_logouts,omitempty"`
	Requests5xx   int64 `json:"requests_5xx"`
	RequestsError int64 `json:"requests_error"`
	// Requests5xxExpected: 5xx from corpus states marked `expect: 5xx`
	// (designed-to-fail routes). Requests5xx above is UNEXPECTED only.
	Requests5xxExpected int64 `json:"requests_5xx_expected"`
	// RequestsPanicExpected: 5xx from corpus states marked `expect: panic`
	// (designed-to-panic routes). Informational; the property loop nets it
	// out of the server's panic_count so I-PANIC judges only unexpected
	// panics (schema 5.7).
	RequestsPanicExpected int64 `json:"requests_panic_expected"`
	// InvariantHits: unexpected 5xx whose body carried a refapp
	// invariant marker (x-invariant) -- a self-reported invariant
	// violation surfaced as a first-class signal.
	InvariantHits int64 `json:"invariant_hits"`
	// RequestsCutAtDeadline: requests in flight when the tier budget
	// expired. Excluded from RequestsError, which is failures only.
	RequestsCutAtDeadline int64 `json:"requests_cut_at_deadline"`

	// Property-loop counters. The orchestrator polls the refapp's
	// /debug/vars once per second for the whole Tier 1 run and evaluates
	// the selected I-* snapshot predicates (validation/checker) against
	// each sample.
	//
	// PropertyEvaluations: predicate evaluations that reached a verdict
	// (pass or fail); skips are counted in PropertySkips. 0 means the
	// loop never got a sample -- the metrics endpoint was unreachable --
	// and the gate treats that as a failure when RequireProperties is
	// set (unless PropertyLoopSkipped explains it), because a run that
	// evaluated nothing has verified nothing.
	PropertyEvaluations int64 `json:"property_evaluations"`
	// PropertySkips: evaluations that reached no verdict (a slope
	// window not judgeable yet, an input not sampled). Informational.
	PropertySkips int64 `json:"property_skips"`
	// PropertyLoopSkipped, when non-empty, says why the loop did not run
	// at all for this cell (the ssh driver: the remote refapp serves
	// /debug/vars to loopback peers only). The gate waives the
	// zero-evaluations check for such a cell; the reason is on record.
	PropertyLoopSkipped string `json:"property_loop_skipped,omitempty"`
	// PropertyViolations: total failed evaluations (one per sample per
	// predicate while the violation persists). Gated: any nonzero value
	// fails the cell.
	PropertyViolations int64 `json:"property_violations"`
	// PropertyViolationIDs: distinct predicate IDs that failed at least
	// once, sorted.
	PropertyViolationIDs []string `json:"property_violation_ids,omitempty"`
	// PropertyPollErrors: polls that yielded no sample (transport error,
	// non-200, unparseable body). Informational.
	PropertyPollErrors int64 `json:"property_poll_errors"`
	// AdaptiveSwitches is the highest celeris.adaptive_switches the property
	// loop sampled. Meaningful only for an adaptive cell, where the gate's
	// ExpectAdaptiveSwitch requires it to be at least 1 (celeris#580).
	AdaptiveSwitches int64 `json:"adaptive_switches,omitempty"`
	// PeakConnsPerWorker and MeanBytesPerReq are the adaptive controller's
	// own two promotion signals as the property loop measured them: the
	// highest ActiveConns/Workers ratio sampled, and the average payload
	// bytes per request over the cell. An adaptive cell reporting
	// AdaptiveSwitches == 0 is only actionable alongside these two --
	// they separate "the load never reached the threshold" (a harness
	// sizing bug) from "the controller suppressed a link-bound workload"
	// (by design) from neither (a celeris defect).
	PeakConnsPerWorker float64 `json:"peak_conns_per_worker,omitempty"`
	MeanBytesPerReq    float64 `json:"mean_bytes_per_req,omitempty"`
	// EngineZeroWitness is the highest value each must-stay-zero engine
	// counter reached in this cell, keyed by its debugvars name. Each
	// counts an event that cannot happen in a correct engine, so the
	// gate fails a cell on any nonzero entry and prints the defect that
	// entry witnesses (report.ZeroWitnessMeaning). A map rather than
	// named fields because the set grows with every defect that earns a
	// witness, and celeris#627 is what a field-by-field literal does to
	// such a set.
	EngineZeroWitness map[string]int64 `json:"engine_zero_witness,omitempty"`
	// Engine-side connection accounting, against which
	// AcceptedConnTotal / ClosedConnTotal are the independent hook-side
	// witness. Their disagreement is what attributes an I-CONN-2 drift
	// rather than merely reporting it (celeris#624).
	EngineAcceptCount        int64 `json:"engine_accept_count,omitempty"`
	EngineCloseCount         int64 `json:"engine_close_count,omitempty"`
	EngineTransplantDetached int64 `json:"engine_transplant_detached,omitempty"`
	EngineTransplantAdopted  int64 `json:"engine_transplant_adopted,omitempty"`
	EngineAsyncPromotedConns int64 `json:"engine_async_promoted_conns,omitempty"`
	PeakStandbyActiveConns   int64 `json:"peak_standby_active_conns,omitempty"`
	// EngineRequestsTotal is the engine's own request counter at the end
	// of the cell. RequestsSent is the walker's independent count of what
	// it actually sent, so the two together catch an engine that stopped
	// counting: celeris#626 left epoll reporting 3,089 requests against a
	// walker that sent 3,306,726, and nothing noticed for as long as only
	// one side was recorded.
	EngineRequestsTotal int64 `json:"engine_requests_total,omitempty"`
	// EngineErrorCount is the engine's final ErrorCount and
	// EngineErrorClasses the eleven cause buckets celeris#646 derives it
	// from, keyed by their debugvars name (report.ErrorClasses says what
	// each one counts). celeris assigns the total from the buckets and
	// keeps no separate running total, so sum(EngineErrorClasses) ==
	// EngineErrorCount holds here and the split can be checked rather
	// than trusted.
	//
	// Diagnostic, not gated -- deliberately, and unlike EngineZeroWitness
	// directly above. Those counters each name an event that cannot
	// happen in a correct engine; these count things that legitimately
	// happen, and celeris#646 measured 88,010 ErrorSendPeerGone over
	// 88,776 accepts on a healthy io_uring abandon-churn load. Until a
	// run says what normal looks like per engine, a threshold would be a
	// number nobody measured.
	EngineErrorCount   int64            `json:"engine_error_count,omitempty"`
	EngineErrorClasses map[string]int64 `json:"engine_error_classes,omitempty"`
	// EngineStandbyErrorCount is the share of EngineErrorCount the
	// adaptive engine's STANDBY sub-engine contributed, the same split
	// PeakStandbyActiveConns applies to the live gauge. The buckets say
	// what went wrong; this says which sub-engine it went wrong on, and
	// celeris#645 needs both. Zero on every non-adaptive engine, and not
	// a member of EngineErrorClasses -- it cuts the same total along the
	// other axis, so summing it with the buckets would double count.
	EngineStandbyErrorCount int64 `json:"engine_standby_error_count,omitempty"`
	// EngineCounters is every published engine counter that has no named
	// field or registry of its own above, keyed by its debugvars name
	// (report.EngineCounters says what each counts, what KIND of reading it
	// is, and whether the per-cell series samples it). Each value reduces the
	// samples whose document carried the engine block by that kind: the
	// highest reading of a running maximum or of a peak gauge (the
	// celeris#687 residual gauges, schema 5.16), and the last reading of
	// everything else -- the cell's final total, gauge level or static count
	// -- never a sum over samples (schema 5.15, probatorium#391).
	//
	// A map for the reason EngineZeroWitness is one. Absent means no sample
	// carried engine metrics; present and zero means measured. Diagnostic,
	// not gated.
	EngineCounters map[string]int64 `json:"engine_counters,omitempty"`

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
	//   ws_echo      → ws_echo_fires, ws_echo_upgraded, ws_echo_sent, ws_echo_ok, ws_echo_corrupt (= ws_echo_egress_interleave + ws_echo_other_corrupt), ws_echo_reorder, ws_echo_missing, ws_echo_timeout
	Adversarial map[string]int64 `json:"adversarial,omitempty"`
	H2CChurn    map[string]int64 `json:"h2c_churn,omitempty"`
	WSTorture   map[string]int64 `json:"ws_torture,omitempty"`
	SSEKill     map[string]int64 `json:"sse_kill,omitempty"`
	// WSEcho is the WebSocket large-echo slice (schema 5.9, celeris#587).
	WSEcho map[string]int64 `json:"ws_echo,omitempty"`
	// SSEEarlyErrs carries the verbatim read errors behind sse_kill's
	// sse_server_closed_early / sse_peer_reset_early / sse_read_err_early.
	// Strings, so they cannot live in the int64 map above; a separate field
	// rather than a stringly-typed map value so the counters stay numeric
	// for the gate. Bounded at the source (validation/sse.go sseMaxEarlyErrs).
	SSEEarlyErrs []string `json:"sse_early_errs,omitempty"`

	// H2CLatency / WSLatency are the per-leg latency histograms of every
	// fire the h2c-churn and WS-torture walkers made (schema 5.9,
	// celeris#588). A stall shorter than the walker's read budget never
	// fails a fire -- it lands late as `declined` / `upgraded` -- so the
	// gated totals alone cannot see the 1.5-8 s once-a-minute class that
	// celeris#493 was; these can, as a burst in the 1-10 s read buckets.
	// Nil when the slice never ran.
	H2CLatency *WalkerLatency `json:"h2c_latency,omitempty"`
	WSLatency  *WalkerLatency `json:"ws_latency,omitempty"`
	// H2CSlowReads / WSSlowReads are the bounded rings (oldest first) of
	// every fire whose read leg exceeded one second, failed or not. Every
	// h2c_hang and every ws_handshake_fail_timeout is in here with its
	// timestamp, error string and addresses; h2c_slow_reads_total /
	// ws_slow_reads_total in the maps say how many the ring could not
	// hold. Omitted on a cell with no slow fire.
	H2CSlowReads []SlowFire `json:"h2c_slow_reads,omitempty"`
	WSSlowReads  []SlowFire `json:"ws_slow_reads,omitempty"`
	// ReadyAt is the UTC instant (RFC3339Nano) the refapp announced its
	// bound address, i.e. the origin of every SlowFire.SinceReadyMs and
	// of the mod-60 phase histogram the design computes offline.
	ReadyAt string `json:"ready_at,omitempty"`
	// RefappStderrTail is the last refappTailMaxLines (80) lines the refapp
	// wrote to its merged stdout+stderr after ready. The refapps log the
	// engine's Warn/Error lines there (fd-cap drops, EMFILE, listener
	// re-creation) and nothing else; before 5.9 those were kept only when
	// the process died. The same text is written to
	// <cell>/refapp_stderr_tail.txt and into every incident dossier.
	RefappStderrTail []string `json:"refapp_stderr_tail,omitempty"`
}

// LatencyBuckets is a fixed-edge histogram of one leg (dial, write or
// read) of a walker fire. Timeout counts legs that ended with the walker's
// own deadline, whatever their elapsed; the elapsed buckets classify every
// other leg. Ge20s is structurally zero (no walker budget exceeds 20 s)
// and exists so nothing is silently folded.
type LatencyBuckets struct {
	Lt100ms int64 `json:"lt_100ms"`
	Lt1s    int64 `json:"lt_1s"`
	Lt2s    int64 `json:"lt_2s"`
	Lt5s    int64 `json:"lt_5s"`
	Lt10s   int64 `json:"lt_10s"`
	Lt20s   int64 `json:"lt_20s"`
	Ge20s   int64 `json:"ge_20s"`
	Timeout int64 `json:"timeout"`
}

// Total is the number of legs the histogram observed.
func (b LatencyBuckets) Total() int64 {
	return b.Lt100ms + b.Lt1s + b.Lt2s + b.Lt5s + b.Lt10s + b.Lt20s + b.Ge20s + b.Timeout
}

// WalkerLatency is the three legs of one walker's fires.
type WalkerLatency struct {
	Dial  LatencyBuckets `json:"dial"`
	Write LatencyBuckets `json:"write"`
	Read  LatencyBuckets `json:"read"`
}

// SlowFire is one walker fire whose read leg exceeded one second.
type SlowFire struct {
	// TS is the UTC instant (RFC3339Nano) the read leg ended.
	TS string `json:"ts"`
	// SinceReadyMs is TS minus the refapp's ready instant; 0 when the
	// walker ran without one (unit tests).
	SinceReadyMs int64 `json:"since_ready_ms,omitempty"`
	DialMs       int64 `json:"dial_ms"`
	WriteMs      int64 `json:"write_ms"`
	ReadMs       int64 `json:"read_ms"`
	// Outcome is the walker's classification: for h2c one of upgraded,
	// declined, crashed, hang-timeout, hang-eof, hang-reset, hang-other;
	// for WS one of upgraded, endpoint-absent, handshake-fail-timeout,
	// handshake-fail-eof, handshake-fail-reset, handshake-fail-status,
	// handshake-fail-other.
	Outcome string `json:"outcome"`
	// Err is the verbatim error that ended the read leg; empty when the
	// read completed.
	Err string `json:"err,omitempty"`
	// Status is the non-101 status line a WS handshake was answered with.
	Status string `json:"status,omitempty"`
	// NRead is the bytes the read leg returned before it ended.
	NRead int `json:"n_read"`
	// LocalAddr / RemoteAddr are the walker's socket addresses: the
	// engine<->client join key (celeris#562), and what an `ss -tnp`
	// snapshot on the server side is matched against.
	LocalAddr  string `json:"local_addr,omitempty"`
	RemoteAddr string `json:"remote_addr,omitempty"`
	// ValidatorSkewMs is the longest validator heartbeat gap (>500 ms)
	// observed while this read was in flight: nonzero means the validator
	// process itself was starved or frozen, so the elapsed is not the
	// server's alone.
	ValidatorSkewMs int64 `json:"validator_skew_ms,omitempty"`
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

	// Window is set ONLY on a per-scenario slice (schema v5.9+,
	// ServerResult.ScenarioResources): the [start, end] the raw series was
	// cut to and how many samples of each kind fell inside. Nil on the
	// column-wide aggregate.
	Window *ResourceWindow `json:"window,omitempty"`
}

// ResourceWindow describes the slice a per-scenario ResourceStats was
// computed over (schema v5.9+, celeris#585). Start is the scenario's
// runner started_at plus the warm-up, End its completed_at; both
// inclusive at 1 s resolution. The counts are the samples that fell
// inside — the denominators behind every mean in the summary.
type ResourceWindow struct {
	Start           time.Time `json:"start"`
	End             time.Time `json:"end"`
	CPUSamples      int       `json:"cpu_samples"`
	ObserverSamples int       `json:"observer_samples"`
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

	// MeanSoftPct is the mean mpstat %soft (softirq) over the window
	// (schema v5.9+). Nil when the cpu.log carried no %soft column or the
	// stats were not windowed per scenario.
	MeanSoftPct *float64 `json:"mean_soft_pct,omitempty"`

	// SUTProcessCPUPct is the SUT process's own CPU over the window
	// (schema v5.9+): (utime+stime delta from /proc/<pid>/stat) / wall
	// time, in percent of ONE core (so 3200 means every one of 32 cores),
	// the same convention as top/pidstat. Separates the server's CPU from
	// host-wide softirq/kernel work that mean_cpu_pct folds in. Nil when
	// the observer wrote no cpu tick columns (pre-v5.9 observer) or fewer
	// than two ticks samples fell in the window.
	SUTProcessCPUPct *float64 `json:"sut_process_cpu_pct,omitempty"`
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
