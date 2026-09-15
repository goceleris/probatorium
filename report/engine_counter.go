package report

import "sort"

// EngineCounterKind says what one reading of an engine counter IS, which
// decides how readings may be combined. The harness itself combines nothing:
// the tally keeps the last reading of each cell (see
// Tier1Summary.EngineCounters). The kind is recorded for the reader who does
// combine -- across samples of the series, or across cells and arches of a
// run -- because the wrong rule produces a plausible number nothing flags.
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
// Nothing here is gated. engine_transplant_stranded is documented by celeris
// as must-stay-zero and so qualifies for [ZeroWitnessMeaning], but moving it
// there fails a cell on a nonzero value, and that is a gate decision, not a
// reporting one.
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
		Kind:   CounterCumulative,
		Counts: "transplant-pending connections the source's detach-queue drain dropped without handing off, closing or firing a hook. celeris documents it MUST STAY ZERO: the two flags it sits between are mutually exclusive by construction, and this counter is what makes that argument checkable",
		Series: true,
		Why:    "a nonzero reading is a defect, and the first question it raises is when, relative to adaptive_switches and engine_transplant_detached in the same row: the drain that can strand a connection runs only around a switch. Not gated in this schema version; see the note on EngineCounters",
	},
	"engine_transplant_adopt_refused": {
		Kind:   CounterCumulative,
		Counts: "adoptions the target refused for a reason other than an occupied slot (a descriptor outside its conn table, or a failed event-loop registration); the target closes the descriptor AND fires OnDisconnect, so accepted - closed - active stays balanced",
		Series: true,
		Why:    "because it fires OnDisconnect it moves the hook-side `closed` column in the same second, and the row is what attributes that step to a refused adoption rather than to a client that disconnected",
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
		Why:    "a column since 5.10: the adaptive controller divides active by it per interval, so conns/worker can only be rebuilt offline with both in the same row. In the tally so peak_conns_per_worker can be read against the divisor it was computed with",
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
