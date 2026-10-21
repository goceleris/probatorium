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
//     so a carried 0 would read as a measured rate (celeris removed the
//     field before v1.6.0; 5.19 records the key's removal). Nothing new is
//     gated: engine_transplant_stranded is documented must-stay-zero, but
//     moving it into ZeroWitnessMeaning is a gate change this version does
//     not make. Additive; older readers ignore every field.
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
//     reduced by their HIGHEST SAMPLED reading. A zero peak clears every
//     sampled instant (the property loop samples at 1 Hz); it does not prove
//     zero at an unsampled switch verdict, though a residue that stands after
//     a switch is sampled. Nothing new is gated: the six counters celeris
//     documents as must-stay-zero carry their meaning in EngineCounter.MustStayZero,
//     and moving any of them into ZeroWitnessMeaning is a gate change this
//     version does not make. Additive; older readers ignore every field.
//   - 5.17 — the bench SUT's CPU and egress become attributable
//     (celeris#585). Adds, on ResourceSummary (column-wide Resources and
//     every per-scenario ScenarioResources slice), MeanIOWaitPct and
//     MeanCPUExIOWaitPct -- mean_cpu_pct counts %iowait as busy, and an
//     io_uring worker waiting in io_uring_enter is charged as iowait, so
//     the 20260829 io_uring column's 96.3 % "busy" was 55.4 % executing --
//     and the engine egress deltas ZCSendsSubmitted, ZCNotifs,
//     InlineBytes, RingBytes, BytesWritten and RingBytesFraction, read
//     from the SUT's /debug/vars, which the bench server now serves on a
//     side listener (servers/celeris/debugvars.go) and cmd/observer
//     records per second. Without them a SEND_ZC on/off A/B could not
//     tell "zero-copy costs nothing" from "zero-copy never ran". Additive;
//     older readers ignore every field, mean_cpu_pct keeps its meaning,
//     and nothing is gated.
//   - 5.18 — the celeris#685 close-path counters (celeris 3e7abba, #793).
//     celeris added two EngineMetrics fields for its io_uring fd-lifetime
//     rule on the close paths: CloseFDDeferred (closes whose descriptor
//     stayed open until the last op the kernel owed on it completed, a
//     rate) and CloseFDForced (descriptors the 5 s release backstop closed
//     with an op still owed; documented must-stay-zero). Adds both to
//     Tier1Summary.EngineCounters and ONE per-cell series column,
//     engine_close_fd_forced, appended. Nothing new is gated:
//     engine_close_fd_forced carries its meaning in
//     EngineCounter.MustStayZero, and moving it into ZeroWitnessMeaning is
//     a gate change this version does not make (probatorium#416 holds that
//     decision for the celeris#657 counters). Additive; older readers
//     ignore every field.
//   - 5.19 — the celeris#812 held SEND_ZC send buffers (celeris e2508c7,
//     #813). celeris added five EngineMetrics fields for the io_uring
//     release backstop, which now holds a closed connection's send buffer
//     past its 5 s until the SEND_ZC's notification instead of reusing it:
//     CloseZCNotifHeld (holds started, a rate), the GAUGES
//     CloseZCNotifHeldNow and CloseZCNotifHeldBytes (buffers held right
//     now and their capacity, reduced by their highest sampled reading,
//     report.CounterPeakGauge), CloseZCNotifForced (a buffer given up with
//     a SEND_ZC still owed; documented must-stay-zero) and
//     ShutdownZCBufRetained (buffers a worker shutdown kept for the life of
//     the process). Adds all five to Tier1Summary.EngineCounters and THREE
//     per-cell series columns, appended: engine_close_zc_notif_held_now,
//     engine_close_zc_notif_held_bytes and engine_close_zc_notif_forced.
//     Nothing new is gated: engine_close_zc_notif_forced carries its
//     meaning in EngineCounter.MustStayZero, as engine_close_fd_forced does.
//     The same celeris (celeris#830, #894) removed EngineMetrics.Throughput,
//     so the refapps no longer publish celeris.engine_throughput, which no
//     artifact carried (probatorium#480). Additive; older readers ignore
//     every field.
//   - 5.20 — the informational twin of I-MEM-1 (probatorium#466). The
//     heap-slope predicate gained a scatter floor; the validator now also
//     runs the rule as it was before the floor beside the gated one on
//     the same series and records it, never gated. Adds
//     Tier1Summary.PropertyInformational (property_informational), keyed
//     by predicate ID, present only when the twin declared a violation
//     in the cell: its label, its violation count, the count of
//     evaluations the gated rule absolved, a flag for a cell it absolved
//     entirely, and its first message. Gate() does not read it. Additive;
//     older readers ignore the field.
//   - 5.21 — the NIC interrupt placement the bench ran under
//     (goceleris/celeris#1036). msr1 has one RX and one TX queue per
//     port and no RSS, and exploratory, post hoc runs saw a ~200 ms
//     whole-port TX stall when the TX-completion vector was serviced by a
//     CPU without a loop (a working explanation, not an established one).
//     The bench now pins that vector to CPU 1 before every bench run, a
//     host-tuning choice (ansible nic_irq_placement), and records it.
//     Adds Environment.NICIRQPlacement (nic_irq_placement): interface, vector name, CPU, and per IRQ the
//     smp_affinity_list / effective_affinity_list found before and read back
//     after, plus the end-of-run readback. Omitted when the host applies no
//     placement (every amd64 run). A changed placement changes
//     comparability. Nothing is gated. Additive; older readers ignore the
//     field.
//   - 5.22 — arch exclusion on a BENCH_TARGET=both run (probatorium#442).
//     Both arches are driven from msa2-client at the same time, and a
//     heavy cell of one arch that overlapped the other arch's heavy cell
//     read backlog clocks (ws-large-echo 6 of 10 overlapping x86 passes at
//     1 s or more, 0 of 42 others, run 33256654220). The runner now holds
//     a host-wide lock around every heavy cell and records the interval of
//     every saturation and rated pass. Adds Document.LoadWindows
//     (load_windows: competitor, scenario, kind, class, start, end, and the
//     target of a rated pass) and Environment.ArchExclusion
//     (arch_exclusion: the policy, this arch's cell order, the peer arch,
//     the overlap counts of the finished run and the lock wait). Both are
//     omitted on single-arch runs. A document from a both-arch run that
//     lacks arch_exclusion predates the fix: the arches ran concurrently
//     with nothing excluded. load_windows stays in results-<arch>.json; the
//     published summary.json drops it (SplitDocument) and env.json keeps
//     the verdict. The publish gate refuses a document whose verdict shows
//     a forbidden overlap. Additive; older readers ignore both fields.
//   - 5.23 — race tier observability (probatorium#481, celeris#871). A
//     failed race run (39560945814) could not say when its h2c_hang and
//     ws_handshake_fail events happened (the slow ring keeps only reads over
//     one second; those closed in 12 ms), what the refapp ran under, or how
//     many ws_echo fires were SLOW. Adds Tier1Summary.H2CHangSpan
//     (h2c_hang_span) and WSHandshakeFailSpan (ws_handshake_fail_span): the
//     first and last event of the kind with UTC instant, since_ready_ms,
//     cause, error and addresses; and Tier1Summary.RefappSched
//     (refapp_sched): the refapp's per-thread Cpus_allowed_list read from
//     /proc at ready, in every run, plus GOMAXPROCS when the stall-capture
//     side listener answered. Adds two ws_echo keys, ws_echo_slow_over_n and
//     ws_echo_slow_no_clean_fire: the SLOW_CLASS fires the race tier's guard
//     refused, a subset of ws_echo_timeout (which still fails and is still
//     the only one gated). Nothing new is gated. Additive; older readers
//     ignore every field.
//   - 5.24 — every rated pass is recorded (probatorium#441, refs; the validity
//     rule and the schedule lag remain open there). The runner kept
//     {target_rps, p99} for a pass that returned requests and dropped any
//     other with one stderr line, so a run could not say how much load
//     loadgen delivered, how many requests failed, or that a pass was
//     missing. Adds ServerResult.RatedPasses (rated_passes: scenario ->
//     one RatedPassResult per pass per run: fraction, target_rps,
//     achieved_rps, achieved_ratio, requests, errors, connect_errors,
//     dial_retries, close_aborts, shortfall_requests, duration, p50, p99,
//     p99_9, max, start, end, loadgen_version, and a status ok | skipped |
//     not_run with a reason and the error text for a pass that is not ok)
//     and ServerResult.RatedHeadlineTargetRPS (rated_headline_target_rps:
//     the target rated_mode_p99_at_target_rps was read at, the highest KEPT
//     target). On the runner's per-cell JSON and raw/<host>.json the passes
//     that are not kept are a separate array, rated_passes_not_kept, and
//     rated_passes keeps its pre-5.24 meaning (kept passes, now with the
//     added fields), so a reader that predates 5.24 sees the same
//     rated_passes entries it always did and ignores the rest.
//     latency_at_slo and rated_mode_p99_at_target_rps are built from the
//     kept passes exactly as before: nothing published changes value.
//     Additive; older readers ignore both fields.
const SchemaVersion = "5.24"

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

	// LoadWindows is every interval this arch's runner drove load through
	// the loadgen host: one per saturation pass and one per rated pass
	// (schema 5.22, probatorium#442). The cross-arch overlap check
	// (CheckArchOverlap) reads it. Omitted when the run recorded none
	// (single-arch runs, pre-5.22 runner output). It stays in the results
	// file and is not part of the published summary.json.
	LoadWindows []LoadWindow `json:"load_windows,omitempty"`
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

	// NICIRQPlacement records the standing NIC interrupt placement the
	// SUT host ran under (schema 5.21, goceleris/celeris#1036): which
	// vector was pinned to which CPU, what it was before, and what the
	// kernel reported after. Nil when the host applies none (amd64 hosts,
	// or msr1 with the placement variable emptied), so a run without one
	// is distinguishable from a run with one.
	NICIRQPlacement *NICIRQPlacement `json:"nic_irq_placement,omitempty"`

	// ArchExclusion is the arch-exclusion verdict of a BENCH_TARGET=both run
	// (schema 5.22, probatorium#442), computed at merge time from both
	// arches' load windows and stamped into both documents. Nil on a
	// single-arch run and on every document written before 5.22; on a
	// both-arch document nil means the arches ran concurrently with
	// nothing excluded.
	ArchExclusion *ArchExclusion `json:"arch_exclusion,omitempty"`
}

// NICIRQPlacement is the record ansible/files/nic_irq_pin.sh prints and
// ansible/tasks/nic_irq_placement.yml writes next to the run
// (nic-irq-placement.json). The pin is an explicit single-CPU write, verified
// by reading effective_affinity_list back; a run whose readback differed
// never gets this far, the play fails first.
type NICIRQPlacement struct {
	// Host is the SUT the placement was applied on (e.g. msr1).
	Host string `json:"host"`
	// Interface is the NIC the vector belongs to (e.g. enp1s0).
	Interface string `json:"interface"`
	// Vector is the interrupt NAME in /proc/interrupts (e.g. enp1s0-8);
	// the IRQ number is per boot, so the numbers live in IRQs.
	Vector string `json:"vector"`
	// Binding says which check tied the vector to Interface: "msi_irqs"
	// (every IRQ is listed under /sys/class/net/<iface>/device/msi_irqs) or
	// "name" (the vector name starts with "<iface>-"). Empty in records
	// written before the field existed.
	Binding string `json:"binding,omitempty"`
	// CPU is the one CPU the vector was pinned to.
	CPU int `json:"cpu"`
	// Changed is true when this run wrote the pin, false when the vector
	// was already there.
	Changed bool `json:"changed"`
	// Verified is true when every IRQ read back on CPU, both the
	// smp_affinity_list and the effective_affinity_list.
	Verified bool `json:"verified"`
	// IRQs holds one entry per /proc/interrupts line with that name.
	IRQs []NICIRQVector `json:"irqs"`
	// AtEnd is the readback taken after the last cell, before the artefacts
	// were collected. Nil when the end-of-run readback did not happen. Its
	// Verified is false when the vector had left its CPU during the run.
	AtEnd *NICIRQPlacement `json:"at_end,omitempty"`
	// AtEndError is the end-of-run readback's own message when it failed
	// (the vector left its CPU, or is gone); empty when it passed.
	AtEndError string `json:"at_end_error,omitempty"`
}

// NICIRQVector is one interrupt's affinity before and after the pin.
type NICIRQVector struct {
	IRQ    int            `json:"irq"`
	Before NICIRQAffinity `json:"before"`
	After  NICIRQAffinity `json:"after"`
}

// NICIRQAffinity is a smp_affinity_list / effective_affinity_list reading.
type NICIRQAffinity struct {
	Affinity  string `json:"affinity"`
	Effective string `json:"effective"`
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

	// RatedHeadlineTargetRPS is, per scenario, the target the entry of
	// RatedModeP99AtTargetRPS was measured at: the highest target among the
	// KEPT passes (schema 5.24, probatorium#441). It is the cell's top
	// fraction only if that pass was kept; when the 0.9 pass was skipped the
	// headline is the 0.75 pass's, and this says so. Omitted for a cell with
	// no kept pass.
	RatedHeadlineTargetRPS map[string]int `json:"rated_headline_target_rps,omitempty"`

	// RatedPasses lists, per scenario, every rated pass of every run of the
	// cell, whatever became of it (schema 5.24, probatorium#441): target,
	// the rate loadgen achieved, its errors, and for a pass that was skipped
	// or never ran a status and a reason, so no pass is missing without a
	// record. latency_at_slo and the headline are built from the passes with
	// status "ok" only. Omitted for a cell with no rated record.
	RatedPasses map[string][]RatedPassResult `json:"rated_passes,omitempty"`

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

// PropertyInformational is what an informational twin of a gated
// predicate recorded in one cell (schema 5.20). See
// [Tier1Summary.PropertyInformational].
type PropertyInformational struct {
	// Label names the twin rule.
	Label string `json:"label"`
	// Violations: samples on which the twin declared a violation.
	Violations int64 `json:"violations"`
	// AbsolvedEvaluations: the part of Violations on which the gated
	// predicate declared none.
	AbsolvedEvaluations int64 `json:"absolved_evaluations"`
	// Absolved: the twin tripped and the gated predicate never did.
	Absolved bool `json:"absolved"`
	// FirstMessage is the twin's first violation message.
	FirstMessage string `json:"first_message,omitempty"`
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
	// PropertyInformational is the informational twin of a gated
	// predicate (schema 5.20), keyed by the predicate's ID, present only
	// when the twin declared a violation in this cell. NOT gated: Gate
	// never reads it. For I-MEM-1 it is the rule before the scatter floor,
	// so a reader sees every trip the floor absolved.
	PropertyInformational map[string]PropertyInformational `json:"property_informational,omitempty"`
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
	// celeris#687 residual gauges, schema 5.16, and the celeris#812 held
	// send-buffer gauges, schema 5.19), and the last reading of
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
	//   ws_echo      → ws_echo_fires, ws_echo_upgraded, ws_echo_sent, ws_echo_ok, ws_echo_corrupt (= ws_echo_egress_interleave + ws_echo_other_corrupt), ws_echo_reorder, ws_echo_missing, ws_echo_timeout, ws_echo_timeout_slow
	//                  ws_echo_timeout_slow is the race tier's recorded latency: timed-out fires whose
	//                  two-instant capture said SLOW_CLASS. Not in ws_echo_timeout, not in the cross-arch
	//                  diff, tolerated by Gate only with GateOptions.ToleratesWSEchoSlow (probatorium#481).
	//                  ws_echo_slow_over_n / ws_echo_slow_no_clean_fire (schema 5.23) split, by guard
	//                  reason, the SLOW_CLASS fires the guard refused; a subset of ws_echo_timeout, not gated.
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
	// H2CHangSpan / WSHandshakeFailSpan place the cell's h2c_hang and
	// ws_handshake_fail events in time (schema 5.23, probatorium#481): the
	// first and the last one, with their cause and verbatim error. The slow
	// ring above keeps only reads over one second, so a hang that closed in
	// 12 ms (the shape of race run 39560945814) had no instant anywhere.
	// Record only; Gate() does not read them. Omitted when the cell had none.
	H2CHangSpan         *EventSpan `json:"h2c_hang_span,omitempty"`
	WSHandshakeFailSpan *EventSpan `json:"ws_handshake_fail_span,omitempty"`
	// RefappSched is the refapp's CPU view, read by the validator from
	// /proc when the refapp is ready (schema 5.23, probatorium#481), in every
	// run and not only a stall-capture one. Omitted when no refapp pid was
	// seen. See RefappSched for what it can and cannot say.
	RefappSched *RefappSched `json:"refapp_sched,omitempty"`
	// LoadShaping records that the cell's walkers were paced below their
	// closed-loop rate, and by how much (probatorium#481, celeris#871). Omitted
	// when nothing was shaped, which is every cell outside the race tier's
	// adaptive cell. Additive WITHOUT a schema bump, on purpose: the bump would
	// re-stamp schema_version in every nightly, checkptr, weekend and bench
	// document, which this change is meant to leave byte-identical; the field's
	// presence is what tells a shaped cell from an unshaped one, in any 5.23
	// document. See LoadShaping.
	LoadShaping *LoadShaping `json:"load_shaping,omitempty"`
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

// EventStamp is one event of a rare kind (an h2c_hang, a ws_handshake_fail)
// with the instant it happened. TS is UTC (RFC3339Nano), the same clock as
// SlowFire.TS, Tier1Summary.ReadyAt and the epoch seconds of the per-cell
// properties_series.csv, so it joins with all three.
type EventStamp struct {
	TS string `json:"ts"`
	// SinceReadyMs is TS minus the refapp's ready instant; 0 when the
	// walker ran without one.
	SinceReadyMs int64 `json:"since_ready_ms,omitempty"`
	// Cause is the walker's classification: eof, reset, timeout, status or
	// other (the same words as the ws_handshake_fail_* and h2c_hang_*
	// counters).
	Cause string `json:"cause"`
	// Err is the verbatim error that ended the read; empty for a non-101
	// status or a zero-length read.
	Err        string `json:"err,omitempty"`
	LocalAddr  string `json:"local_addr,omitempty"`
	RemoteAddr string `json:"remote_addr,omitempty"`
}

// EventSpan is the earliest and the latest event of one kind in a cell, and
// how many were stamped. Count equals the counter it sits beside.
type EventSpan struct {
	Count int64      `json:"count"`
	First EventStamp `json:"first"`
	Last  EventStamp `json:"last"`
}

// LoadShaping is the pacing a cell's Markov walkers ran under. It exists so a
// reader of a race-tier cell can see that its load was shaped, and by how much,
// without reading the validator's source.
//
// Walkers is the whole fleet's count (Config.ConcurrencyFloor applied) and
// MarkovWalkers the part of it that was paced; both are the counts the cell ran
// with, so a shaped cell can be compared walker for walker with an unshaped one.
// MarkovMinPeriodUs is the pacing, in microseconds: the least time between the
// STARTS of two requests of one Markov walker (open loop: a slow response does
// not make the next start earlier, and nothing is caught up afterwards). The
// fleet therefore offers at most MarkovWalkers / MarkovMinPeriodUs requests per
// microsecond, whatever the response time.
type LoadShaping struct {
	Reason            string `json:"reason"`
	Walkers           int    `json:"walkers"`
	MarkovWalkers     int    `json:"markov_walkers"`
	MarkovMinPeriodUs int64  `json:"markov_min_period_us"`
}

// RefappSched is what the validator read of the refapp's CPU placement.
//
// ThreadMasks counts the refapp's threads by their own Cpus_allowed_list.
// LeaderCPUsAllowedList is the thread-group leader's alone, which is NOT the
// process's mask: celeris pins each engine loop's thread to one CPU and the
// main thread can be one of them (race run 38410499633 read "0" and was taken
// for a one-CPU confinement). Read ThreadMasks.
//
// GOMAXPROCS is in no /proc file. It is recorded only when the refapp's own
// /debug/sched answered (a stall-capture run, which starts the side
// listener); otherwise it is 0 and GOMAXPROCSNote says why. Unavailable, when
// set, is why nothing could be read at all.
type RefappSched struct {
	At                    string         `json:"at,omitempty"`
	Unavailable           string         `json:"unavailable,omitempty"`
	Threads               int            `json:"threads,omitempty"`
	LeaderCPUsAllowedList string         `json:"leader_cpus_allowed_list,omitempty"`
	ThreadMasks           map[string]int `json:"thread_masks,omitempty"`
	GOMAXPROCS            int            `json:"gomaxprocs,omitempty"`
	GOMAXPROCSNote        string         `json:"gomaxprocs_note,omitempty"`
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

	// MeanIOWaitPct is the mean mpstat %iowait over the rows whose
	// %iowait parsed (a non-negative value), and MeanCPUExIOWaitPct the
	// mean of (100 - %idle - %iowait) over those same rows: host CPU that
	// was actually executing (schema v5.17, celeris#585). MeanCPUPct stays
	// the mean over every valid CPU row, iowait column or not. It counts
	// %iowait as busy, and io_uring charges a worker waiting in
	// io_uring_enter as iowait, so an io_uring column's MeanCPUPct is not
	// comparable with an epoll column's; this pair is. Nil when no row
	// carried a %iowait value.
	MeanIOWaitPct      *float64 `json:"mean_iowait_pct,omitempty"`
	MeanCPUExIOWaitPct *float64 `json:"mean_cpu_ex_iowait_pct,omitempty"`

	// The SUT engine's egress over the window (schema v5.17, celeris#585),
	// each the delta of the cumulative engine.EngineMetrics counter
	// between the first and last observer sample that carried it -- nil
	// unless at least two samples carried it and it never decreased
	// between them (summarizeEgress). They are
	// the exposure witnesses a SEND_ZC A/B cannot be read without:
	// ZCSendsSubmitted / ZCNotifs say whether the zero-copy arm ran at all
	// (0 in an OFF arm; 0 in an ON arm means the treatment never fired),
	// and InlineBytes / RingBytes split BytesWritten into the bytes a
	// detached stream wrote with a raw write(2) -- never zero-copy -- and
	// the bytes the io_uring ring sent. RingBytesFraction = ring / (inline
	// + ring). Nil when the SUT published no such counter (non-celeris,
	// pre-#585 bench server, celeris before the field existed); 0 is a
	// reading.
	ZCSendsSubmitted  *int64   `json:"zc_sends_submitted,omitempty"`
	ZCNotifs          *int64   `json:"zc_notifs,omitempty"`
	InlineBytes       *int64   `json:"inline_bytes,omitempty"`
	RingBytes         *int64   `json:"ring_bytes,omitempty"`
	BytesWritten      *int64   `json:"bytes_written,omitempty"`
	RingBytesFraction *float64 `json:"ring_bytes_fraction,omitempty"`
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
