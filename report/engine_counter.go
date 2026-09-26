package report

import "sort"

// EngineCounterKind says what one reading of an engine counter IS, which
// decides how readings may be combined. The end-of-cell tally combines a
// cell's samples by it -- a running maximum and a peak gauge by max, every
// other kind by its last reading, never by a sum (see
// Tier1Summary.EngineCounters) -- and it is recorded for the reader who
// combines further -- across samples of the series, or across cells and
// arches of a run -- because the wrong rule produces a plausible number
// nothing flags.
type EngineCounterKind string

const (
	// CounterCumulative is a running total since the engine started. The
	// cell's value is its last reading; a per-second rate is the delta
	// between adjacent rows of the series. Summing samples is always wrong;
	// summing cells is a valid run total.
	CounterCumulative EngineCounterKind = "cumulative"
	// CounterRunningMax is the longest single episode since the engine
	// started. Combine ONLY with max: never sum (celeris's adaptive engine
	// takes max across its two sub-engines for exactly this reason, because
	// two sub-second maxima added together fabricate a seconds-long episode
	// nobody observed), and never difference into a rate -- a step in the
	// series marks when a longer episode was recorded, not an amount accrued.
	CounterRunningMax EngineCounterKind = "running_max"
	// CounterGauge is a current level that can fall. Its last reading is one
	// instant, not a total; never sum it across samples or cells.
	CounterGauge EngineCounterKind = "gauge"
	// CounterStatic is fixed once the engine is listening. Any reading is the
	// value; never sum it.
	CounterStatic EngineCounterKind = "static"
	// CounterPeakGauge is a gauge -- a current level that can fall -- whose
	// question is whether it ever stood above zero, not where the cell
	// ended. celeris#687's residual gauges are the case: celeris asks for
	// them to be read at every switch verdict, and a cell makes several
	// switches. The cell's value is its HIGHEST SAMPLED reading, so a zero
	// means zero at every sampled instant. The property loop samples at
	// 1 Hz and verdicts fall between samples, so a zero does not prove zero
	// at an unsampled verdict; a residue that STANDS after a switch is still
	// sampled, which is the case that matters. A verdict-complete reading
	// would need the peak captured at the verdict source. CounterGauge's last
	// reading clears only the final instant and says
	// nothing about the switches before it. Never sum it across samples (a
	// level held for n samples is not n times the level), never difference it
	// into a rate, and combine cells with max. The peak can be a transient --
	// celeris#687's Busy is expected to be nonzero while a drain is in
	// progress -- so a nonzero peak is read against the series, which is why
	// every counter of this kind has a column.
	CounterPeakGauge EngineCounterKind = "peak_gauge"
)

// EngineCounter declares one engine.EngineMetrics counter carried in
// Tier1Summary.EngineCounters, and whether the per-cell series samples it.
type EngineCounter struct {
	// Kind is what a reading is; see [EngineCounterKind].
	Kind EngineCounterKind
	// Counts is what the counter counts, in a sentence a reader can act on
	// without knowing the engine.
	Counts string
	// Series is true when the counter also has a column in the per-cell 1 Hz
	// series, and false when its end-of-cell reading is the whole of what it
	// can say. Same bar as [ErrorClass.Series]: a column is written every
	// second of every cell, so it is earned only when the question asked of
	// the counter is WHEN, and the artifact carries something else
	// timestamped to join that against.
	Series bool
	// Why justifies Series. Prose, because the next counter celeris adds has
	// to make the same call and the reasoning is the only part that transfers.
	Why string
	// MustStayZero is set when celeris documents the counter as must-stay-zero,
	// and says what a nonzero reading witnesses, in the sentence
	// [ZeroWitnessMeaning] would carry for it. It is NOT a gate: nothing reads
	// it to fail a cell. Moving the entry into ZeroWitnessMeaning is the gate
	// decision, and it waits until a run on the cluster has said what normal
	// looks like for the counter; until then this is where the claim and its
	// meaning are written down, beside the reading the artifact carries.
	MustStayZero string
}

// EngineCounters declares every published engine counter the end-of-cell
// tally carries that has no named field or registry of its own, keyed by its
// debugvars name without the "celeris." prefix -- the convention
// [ZeroWitnessMeaning] and [ErrorClasses] use.
//
// Most of it is what probatorium#386 published and probatorium#391 found
// reaching no artifact: nineteen of the twenty keys it added (the twentieth,
// celeris.engine_throughput, is carried nowhere; checker.EngineKeysNotParsed
// says why). The other four were already series columns with no end-of-cell
// total, which the completeness guard found on its first run.
//
// The eighteen celeris#657 counters celeris 9f4d89b added (celeris#676, #681,
// #687) joined in schema 5.16.
//
// Nothing here is gated. The counters celeris documents as must-stay-zero --
// engine_transplant_stranded, and six of the celeris#657 counters --
// qualify for [ZeroWitnessMeaning], but moving one there fails a cell on a
// nonzero value, and that is a gate decision, not a reporting one: a
// diagnostic counter is not gated until a run has said what normal looks
// like. Each carries its would-be meaning in [EngineCounter.MustStayZero]
// instead.
//
// It lives in report, not in the checker that populates it, for the reason
// ZeroWitnessMeaning does: two copies of the set would drift, and a counter
// declared on one side and recorded on the other is silent both ways.
var EngineCounters = map[string]EngineCounter{
	// --- The celeris#647 hand-off outcomes (celeris#624). Nonzero only on
	// the adaptive engine, the only one that transplants.
	"engine_transplant_handoff_refused": {
		Kind:   CounterCumulative,
		Counts: "hand-offs the target refused after the source had already dropped the descriptor from its loop, live set and conn table; since celeris#624 the source re-adopts it, so each is paired with an engine_transplant_adopted on the SAME engine and the detached - adopted residual stays zero. A recovery, not a fault",
		Series: true,
		Why:    "celeris#647 added it to make the celeris#624 fix falsifiable, and the prediction is about an instant: a step in engine_transplant_adopted that this column explains is a re-adoption onto the source, and the same step with this column flat is a connection that arrived from the other engine. Two end-of-cell totals cannot say which step was which; only the row can",
	},
	"engine_transplant_drain_stopped": {
		Kind:   CounterCumulative,
		Counts: "deferred hand-offs that found the drain already stopped (the adaptive engine reverted) between the detach and the hand-off; the connection is re-adopted onto the source. A recovery, not a fault",
		Series: true,
		Why:    "a revert racing a hand-off happens within seconds of a switch, so the reading that matters is this column stepping in the same few rows as adaptive_switches; a total says it happened, the row says it was that switch",
	},
	"engine_transplant_stranded": {
		Kind:         CounterCumulative,
		Counts:       "transplant-pending connections the source's detach-queue drain dropped without handing off, closing or firing a hook. celeris documents it MUST STAY ZERO: the two flags it sits between are mutually exclusive by construction, and this counter is what makes that argument checkable",
		Series:       true,
		Why:          "a nonzero reading is a defect, and the first question it raises is when, relative to adaptive_switches and engine_transplant_detached in the same row: the drain that can strand a connection runs only around a switch. Not gated in this schema version; see the note on EngineCounters",
		MustStayZero: "a transplant-pending connection was dropped by the source's detach-queue drain without being handed off, closed or reported: it left one engine and arrived at neither, with no hook fired (celeris#647)",
	},
	"engine_transplant_adopt_refused": {
		Kind:   CounterCumulative,
		Counts: "adoptions the target refused for a reason other than an occupied slot (a descriptor outside its conn table, or a failed event-loop registration); the target closes the descriptor AND fires OnDisconnect, so accepted - closed - active stays balanced",
		Series: true,
		Why:    "because it fires OnDisconnect it moves the hook-side `closed` column in the same second, and the row is what attributes that step to a refused adoption rather than to a client that disconnected",
	},

	// --- The celeris#657 hand-off loss witnesses (celeris#676). io_uring
	// only, cumulative, and summed over the adaptive engine's two sub-engines
	// by celeris itself: the stale completions arrive on the sub-engine that
	// made the hand-off, which after a switch is the standby.
	"engine_stale_recv_data_transplanted": {
		Kind:         CounterCumulative,
		Counts:       "io_uring recv completions that READ BYTES for a connection this engine had handed to the other sub-engine, and were dropped as stale: each is a client request taken off a socket and discarded. The celeris#657 loss; with engine_stale_recv_data_unattributed it is W1, which celeris#681 takes to zero",
		Series:       true,
		Why:          "the loss happens only at a revert hand-off, so the first question a nonzero reading raises is which switch: the row it steps in, against adaptive_switches and engine_transplant_detached, names it. celeris#676 joined this counter to client read errors one for one; the column is what lets a cluster run make that join against the walker",
		MustStayZero: "a recv armed before an io_uring -> epoll hand-off outlived it and read a request meant for the new owner, or through a reused descriptor number for another connection; the request was dropped as stale and its client never answered (celeris#657, W1)",
	},
	"engine_stale_recv_data_unattributed": {
		Kind:         CounterCumulative,
		Counts:       "io_uring recv completions that read bytes for a (fd, generation) the engine had registered nothing for -- neither closed, hijacked nor handed off -- dropped as stale. The attribution gap of the celeris#676 split, and the other half of W1",
		Series:       true,
		Why:          "the same question as engine_stale_recv_data_transplanted, which it forms W1 with: a nonzero reading is a lost request, and the row is what places it against a switch",
		MustStayZero: "a recv completion read a request for a connection identity the engine had no record of, and dropped it with no attribution (celeris#657, W1)",
	},
	"engine_stale_recv_data_closed": {
		Kind:   CounterCumulative,
		Counts: "io_uring recv completions that read bytes for a connection this engine had closed or hijacked, dropped as stale. Usually a client's bytes racing a server-side close, and that client sees its connection end; celeris documents it is not always benign -- after a Hijack, or when a recv resolves a reused descriptor number, a live client's request can land here",
		Series: false,
		Why:    "it moves with ordinary close traffic rather than with switches, so its question is how large the close race was over the cell, which the total answers. The switch-time loss is engine_stale_recv_data_transplanted, which has a column",
	},
	"engine_transplant_handoff_in_flight": {
		Kind:         CounterCumulative,
		Counts:       "io_uring -> epoll hand-offs that detached a connection while an operation was still outstanding on it (a recv armed, a kernel op in flight or a SEND_ZC notification pending), counted whether or not the recv then read anything. The precondition of every engine_stale_recv_data_transplanted; celeris calls it W2 and celeris#681 takes it to zero",
		Series:       true,
		Why:          "it can only step at a hand-off, in the seconds after a switch, so the row against engine_transplant_detached says which switch broke the fd-lifetime rule and for how many of that switch's hand-offs",
		MustStayZero: "an io_uring connection was handed to epoll while a read could still resolve its descriptor: the precondition of every celeris#657 lost request, which celeris#681's fd-lifetime rule forbids (W2)",
	},

	// --- The celeris#681 fd-lifetime rule: a connection leaves io_uring only
	// when no read can still resolve its descriptor. io_uring only,
	// cumulative, summed over both sub-engines; the rates read zero while no
	// drain runs.
	"engine_transplant_held": {
		Kind:   CounterCumulative,
		Counts: "responses flushed with the connection's next recv held back because a drain was set, so the hand-off at that send's completion finds nothing in flight: the route most celeris#681 hand-offs of a keep-alive connection take. A rate, zero while no drain runs",
		Series: false,
		Why:    "which route the cell's hand-offs took, held against reaped, is a ratio of totals; the instant of each hand-off is already engine_transplant_detached's column",
	},
	"engine_transplant_reaps": {
		Kind:   CounterCumulative,
		Counts: "cancels submitted for an already-armed recv so the connection can be handed off at that recv's cancellation: the other celeris#681 route. A rate, zero while no drain runs",
		Series: false,
		Why:    "the other half of the same ratio of totals as engine_transplant_held",
	},
	"engine_transplant_reap_misses": {
		Kind:   CounterCumulative,
		Counts: "reaps that matched nothing because the recv had completed or was not issued yet; each is retried and none is followed by a hand-off. celeris measured up to one miss per reap under continuous load, every one safe. Within one document it is at most engine_transplant_reaps",
		Series: false,
		Why:    "only meaningful against engine_transplant_reaps, which is itself a total",
	},
	"engine_transplant_hold_rescued": {
		Kind:         CounterCumulative,
		Counts:       "connections whose recv was held for a hand-off that did not happen and that no completion released, found by the timeout sweep with their response sent and no recv armed, and re-armed. celeris documents it MUST STAY ZERO: it is a belt under the release at every send completion",
		Series:       true,
		Why:          "a nonzero reading is a connection that could not read until the sweep came by, and a hold exists only around a drain, so the question is when, against adaptive_switches in the same row",
		MustStayZero: "a connection's recv was held for a hand-off that never happened and nothing released it, so the connection sat unable to read until the timeout sweep re-armed it (celeris#681)",
	},
	"engine_transplant_double_claim": {
		Kind:         CounterCumulative,
		Counts:       "io_uring hand-offs refused because the connection had already left its descriptor slot when its async dispatch goroutine's claim was acted on. In async mode only a second hand-off of the same connection vacates the slot that way, so celeris documents it MUST STAY ZERO and says a release gate can require it",
		Series:       true,
		Why:          "as engine_transplant_stranded: a nonzero reading is a defect, and the drain that can produce one runs only around a switch, so its row against adaptive_switches is the first thing to read",
		MustStayZero: "an io_uring connection was claimed for a second hand-off after the first had already moved it; without the refusal it would have been handed off twice, the second time as whatever socket then held the descriptor number (celeris#681)",
	},
	"engine_transplant_claim_deferred": {
		Kind:   CounterCumulative,
		Counts: "hand-off attempts on the worker's own path that found the connection's async dispatch goroutine had already claimed the hand-off, and left it to that claim. Ordering, not a fault: a completion of the connection landed between the goroutine's park and the worker's drain of the claim. A rate",
		Series: false,
		Why:    "a rate of a benign ordering; the total says how often async hand-offs raced their own completions in the cell, and nothing it could explain needs its instant",
	},
	"engine_transplant_reap_failed": {
		Kind:         CounterCumulative,
		Counts:       "reaps whose completion was neither a hit nor a miss, e.g. -EINVAL from a kernel that rejects the cancel flags the startup probe found accepted; not retried, never followed by a hand-off, so the connection stays until its recv completes on its own. celeris documents it MUST STAY ZERO",
		Series:       true,
		Why:          "as engine_transplant_double_claim: a nonzero reading is a defect that can only occur during a drain, and its row against adaptive_switches is the first thing to read",
		MustStayZero: "a hand-off recv cancel failed outright -- the running kernel refused a cancel the engine's startup probe had found it accepts -- so the probe and the kernel disagree and the connection could not be handed off (celeris#681)",
	},
	"engine_transplant_reap_unsupported": {
		Kind:   CounterCumulative,
		Counts: "hand-off recv cancels not placed because the io_uring startup probe did not find the IORING_ASYNC_CANCEL flags (Linux 5.19) accepted; such a connection stays on io_uring until its recv completes on its own. Placement only, never a lost request, and zero wherever the probe finds the flags",
		Series: false,
		Why:    "a property of the host's kernel rather than of any second: a nonzero total says the arch ran a kernel without the flags, and the residue it leaves is engine_transplant_residual_pinned, which has a column",
	},

	// --- The celeris#687 post-switch sweep, on whichever engine is draining.
	// The five residual gauges are summed over the adaptive engine's two
	// sub-engines by celeris, correctly: the two hold disjoint connections.
	"engine_transplant_sweep_passes": {
		Kind:   CounterCumulative,
		Counts: "passes of the post-switch sweep, which moves a connection the drain would otherwise reach only at that connection's own next event (never, for one idle at the switch): the sweep's cost. It stops climbing once the drain ends, once nothing is left to move, and once every connection left is of a permanent class",
		Series: true,
		Why:    "the question is whether the sweep stops: a column still climbing long after an adaptive_switches step is a sweep that never went dormant, which is how celeris#687 found its unstarted-connection defect, and a total cannot tell that from a cell that switched often",
	},
	"engine_transplant_residual_busy": {
		Kind:   CounterPeakGauge,
		Counts: "connections a draining engine still holds because they are mid-request, their response is not flushed, or a dispatch goroutine is still running: the transient class, and the only one that can clear with no event of the connection's own. celeris#687: a standing nonzero value after a switch settles IS the placement bug; nonzero while a drain is in progress is expected",
		Series: true,
		Why:    "the placement question is its level at a verdict -- a few seconds after an adaptive_switches step, once the sweep has settled -- and only the row places a reading against the step. The tally keeps the peak, an upper bound: zero proves no sample, and so no verdict, ever held Busy residue; nonzero sends the reader to this column to tell a standing residue from a drain in progress",
	},
	"engine_transplant_residual_detached": {
		Kind:   CounterPeakGauge,
		Counts: "WebSocket or SSE connections a draining engine still holds; they do not move, so this is expected residue that falls as those streams close",
		Series: true,
		Why:    "residue is read at the verdict after each adaptive_switches step, as engine_transplant_residual_busy is, and the column also shows it decaying as the streams close, which the tally's peak cannot",
	},
	"engine_transplant_residual_h2": {
		Kind:   CounterPeakGauge,
		Counts: "H2 or h2c connections, or H1 connections mid-upgrade, a draining engine still holds: expected residue",
		Series: true,
		Why:    "as engine_transplant_residual_detached: residue read at each switch verdict, and its decay, are rows",
	},
	"engine_transplant_residual_pinned": {
		Kind:   CounterPeakGauge,
		Counts: "connections a draining engine holds that cannot be handed over at all: an io_uring fixed-file connection, or one whose recv only a reap could clear on a worker whose kernel lacks IORING_ASYNC_CANCEL. Expected residue; placement, never loss",
		Series: true,
		Why:    "as engine_transplant_residual_detached; and a nonzero row beside a nonzero engine_transplant_reap_unsupported is the no-cancel-flags kernel made visible",
	},
	"engine_transplant_residual_unstarted": {
		Kind:   CounterPeakGauge,
		Counts: "accepted connections a draining engine holds that have not yet sent the byte protocol detection needs, which the hand-off refuses at its first gate. Permanent for the sweep, transient for the connection; celeris#687 split it out of Busy",
		Series: true,
		Why:    "as engine_transplant_residual_detached: residue read at each switch verdict, and its decay, are rows",
	},

	// --- The celeris#484 resume window and its guard. io_uring only.
	"engine_recv_resume_while_cancel_pending": {
		Kind:   CounterCumulative,
		Counts: "WebSocket backpressure resumes processed while the pause's ASYNC_CANCEL was still in flight: the window in which celeris#484 armed a second recv. A load that never moves it has not exercised the #560 guard (celeris#586)",
		Series: false,
		Why:    "an exposure denominator: its question is whether this cell's load reached the celeris#484 window at all, and the total answers that. The defect it is the denominator for, engine_recv_double_armed, is already a series column and a gated witness, so a firing already has its instant",
	},
	"engine_recv_resume_while_recv_in_flight": {
		Kind:   CounterCumulative,
		Counts: "the subset of engine_recv_resume_while_cancel_pending in which the cancelled recv was still armed: the only state in which a second recv could land on a kernel-held one, and so the witness that the celeris#484 window was actually reached. Within one document it is at most engine_recv_resume_while_cancel_pending",
		Series: false,
		Why:    "same as engine_recv_resume_while_cancel_pending, of which it is the sharper half: a total is the exposure reading, and the instant of any double arm is already in engine_recv_double_armed's column",
	},
	"engine_recv_arm_declined": {
		Kind:   CounterCumulative,
		Counts: "recv arms the io_uring engine declined because a recv was already armed on that connection: the #560 guard itself firing, from any caller",
		Series: false,
		Why:    "a guard that worked leaves no damage to time: how often it fired over the cell is the reading. A double arm it failed to stop would appear in engine_recv_double_armed, which does have a column",
	},

	// --- The celeris#607 recv-stall ledger. io_uring only.
	"engine_recv_stall_nanos": {
		Kind:   CounterCumulative,
		Counts: "total wall time of the episodes engine_recv_stall_episodes counts: connections owed a recv arm but passed over because a send was outstanding, from the first skipped pass to the arm",
		Series: true,
		Why:    "celeris#607 turned on duration and on when, not on count: the delta of this column between two rows is the stall time accrued in that second, which is what joins against a walker's slow-read fire (h2c_slow_reads / ws_slow_reads carry the instant). Divided by the delta of engine_recv_stall_episodes it is the mean episode in that second",
	},
	"engine_recv_stall_max_nanos": {
		Kind:   CounterRunningMax,
		Counts: "the longest single recv-stall episode. The discriminating one: submission-queue pressure clears inside a pass, while an episode measured in seconds is a connection that received nothing with its peer's bytes already in the kernel (celeris#607)",
		Series: true,
		Why:    "a step marks the second a longer episode than any before it was recorded, and that timestamp is what to hold against a slow-read fire. A running maximum: read it with max, never difference it into a rate, never sum it",
	},
	"engine_recv_linked_arms": {
		Kind:   CounterCumulative,
		Counts: "RECVs chained behind a SEND with IOSQE_IO_LINK, the single-shot request/response fast path: the exposure denominator for the mechanism celeris#607 was solved on",
		Series: true,
		Why:    "the per-second denominator engine_recv_linked_blocked_nanos needs: blocked nanoseconds per linked arm, second by second, is the mean linked wait, and a rate needs both deltas from the same row",
	},
	"engine_recv_linked_blocked_nanos": {
		Kind:   CounterCumulative,
		Counts: "total time chained recvs spent waiting for their send to complete -- time the connection could not receive, because the kernel does not start a linked operation until its predecessor finishes",
		Series: true,
		Why:    "celeris#607 was solved on this mechanism (a peer that stops reading blocks the SEND and takes the linked RECV with it), and its signature is time accrued in specific seconds, which only the series can line up against the walker's slow-read ring",
	},
	"engine_recv_linked_blocked_max_nanos": {
		Kind:   CounterRunningMax,
		Counts: "the longest single linked-recv wait: microseconds for a request/response cycle, seconds for a peer that has stopped reading (celeris#607)",
		Series: true,
		Why:    "as engine_recv_stall_max_nanos: the step's timestamp is the reading. A running maximum: max only, never a difference, never a sum",
	},

	// --- Detach accounting (celeris#549, celeris#584). io_uring only.
	"engine_detached_conns": {
		Kind:   CounterGauge,
		Counts: "the live number of connections handed to a detached middleware goroutine (WebSocket / SSE), summed over the io_uring workers; a drift between it and the live streams is the celeris#549 accounting bug made visible",
		Series: true,
		Why:    "a gauge's end-of-cell value is one instant, and the drift question is whether it tracks the live streams over the whole cell, which only its trajectory shows. The tally's last reading keeps a persistent drift visible in either direction (a peak would hide a negative one), but it is not a drained residual: the property loop's last sample is taken while load is still being offered",
	},
	"engine_detach_window_closes": {
		Kind:   CounterCumulative,
		Counts: "async-mode connections that closed between the middleware's Detach and the worker's deferred increment; each is a close the pre-#551 accounting decremented with no matching increment",
		Series: false,
		Why:    "the exposure proof that the celeris#549 window was entered at all, which is a question about the cell, not a second. The consequence it would explain is in engine_detached_conns, which has a column",
	},

	// --- The io_uring egress split (celeris#585, celeris#591).
	"engine_zc_sends_submitted": {
		Kind:   CounterCumulative,
		Counts: "IORING_OP_SEND_ZC submissions: the exposure witness for the zero-copy send path. Zero on other engines and whenever CELERIS_IOURING_SEND_ZC disables it",
		Series: false,
		Why:    "its question is whether the zero-copy branch ran in this cell at all, which the total answers; with engine_zc_notifs beside it, submitted - notifs is the sends whose buffer the kernel still held pinned at the end",
	},
	"engine_zc_notifs": {
		Kind:   CounterCumulative,
		Counts: "SEND_ZC notification completions, the ones that release the kernel-pinned buffer. Within one document it is at most engine_zc_sends_submitted",
		Series: false,
		Why:    "only meaningful against engine_zc_sends_submitted, which is itself a total",
	},
	"engine_inline_bytes": {
		Kind:   CounterCumulative,
		Counts: "payload bytes written with a raw write(2) from a detached middleware goroutine (WebSocket / SSE inline egress) instead of through the ring; these can never be zero-copy",
		Series: false,
		Why:    "one half of the egress split of engine_bytes_written, which is already a column; the question the split answers is what fraction of the cell's egress could never be zero-copy, a ratio of totals",
	},
	"engine_ring_bytes": {
		Kind:   CounterCumulative,
		Counts: "payload bytes flushed through ring SEND / SEND_ZC / WRITEV completions: the complement of engine_inline_bytes within engine_bytes_written",
		Series: false,
		Why:    "the other half of the same ratio of totals",
	},

	// --- Denominators.
	"engine_async_routes": {
		Kind:   CounterStatic,
		Counts: "routes registered with .Async(true), which can take the per-connection dispatch goroutine; fixed after Listen, and the adaptive engine reports one sub-engine's value rather than the sum",
		Series: false,
		Why:    "static, so a column would repeat one number every row. The reading is the denominator for engine_async_promoted_conns: promotions against a refapp with no async routes are a different finding from the same number against one that has them",
	},

	// --- Already series columns, with no end-of-cell total until 5.15.
	"engine_standby_close_count": {
		Kind:   CounterCumulative,
		Counts: "the share of engine_close_count the adaptive engine's standby sub-engine contributed; zero on every other engine",
		Series: true,
		Why:    "a column since 5.11: which sub-engine a close landed on, at the second of a live-gauge step (celeris#624). It had no end-of-cell total, the one parsed engine counter in that state, until the probatorium#391 guard found it",
	},
	"engine_workers": {
		Kind:   CounterGauge,
		Counts: "I/O workers or event loops the engine runs. Static after Listen on a single engine; on the adaptive engine it is the sum of both sub-engines and steps up once, when the lazy standby is built",
		Series: true,
		Why:    "a column since 5.10: the adaptive controller divides active by it per interval, so conns/worker can only be rebuilt offline with both in the same row. The tally keeps its last reading, the worker count the cell ended with, and that is NOT necessarily the divisor behind peak_conns_per_worker: the evaluator divides each sample's active by that same sample's engine_workers and keeps the highest ratio, so on the adaptive engine a peak sampled before the lazy standby was built was computed against the smaller count. The divisor of the peak is in the series row where active / engine_workers is highest",
	},
	"engine_bytes_read": {
		Kind:   CounterCumulative,
		Counts: "payload bytes received across all connections",
		Series: true,
		Why:    "a column since 5.10 (the controller's bytes/request signal per interval). In the tally because mean_bytes_per_req keeps the ratio and discards both totals",
	},
	"engine_bytes_written": {
		Kind:   CounterCumulative,
		Counts: "payload bytes sent across all connections",
		Series: true,
		Why:    "a column since 5.10, as engine_bytes_read. In the tally because the io_uring egress split, engine_inline_bytes + engine_ring_bytes, can only be checked against this total if the total is in the same document",
	},
}

// SeriesEngineCounters returns, sorted, the counters [EngineCounters] marks
// as worth a per-second column. validation/series.go writes columns in its
// own explicit order (a CSV row is positional, and the order is part of the
// file format); its test compares that order's membership against this.
func SeriesEngineCounters() []string {
	out := make([]string, 0, len(EngineCounters))
	for k, c := range EngineCounters {
		if c.Series {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
