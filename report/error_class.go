package report

import "sort"

// ErrorClass describes one of the cause buckets celeris#646 split
// EngineMetrics.ErrorCount into, and says whether the bucket is worth a
// column in the per-second series.
type ErrorClass struct {
	// Counts is what the bucket counts, in one sentence. Printed next to a
	// reading so a reader does not have to know the engine to act on it.
	Counts string
	// Series is true when the bucket also gets its own column in the
	// per-cell 1 Hz series, and false when the end-of-cell total in
	// Tier1Summary.EngineErrorClasses is the whole of what it can say.
	//
	// The split is a judgement, and Why records it. The series is written
	// once a second for the whole cell and already carries thirty-one
	// columns, so a bucket earns one only when the QUESTION asked of it is
	// "when", and the artifact carries something else timestamped to join
	// that against: the promotion instant (adaptive_switches), the
	// hand-off (engine_transplant_adopted), a walker's slow-read or
	// handshake-fail fire, or the per-second denominators a rate needs
	// (engine_requests_total, active). A bucket whose question is "how
	// many, over the cell" does not, and 3,600 rows of zeros and one step
	// is not a better answer than the total.
	Series bool
	// Why justifies this bucket's Series value. It is prose on purpose:
	// the next person to add a bucket has to make the same call, and the
	// reasoning is the only part of it that transfers.
	Why string
}

// ErrorClasses declares the eleven cause buckets behind
// EngineMetrics.ErrorCount (celeris#646), keyed by the debugvars name the
// refapps publish them under, minus the "celeris." prefix — the same
// convention [ZeroWitnessMeaning] uses.
//
// The eleven partition ErrorCount exactly: celeris derives the total as
// their sum in engine.FillErrorClasses and keeps no separate running
// total, and the adaptive engine sums both sub-engines bucket by bucket,
// so sum(ErrorClasses) == EngineErrorCount holds inside any single
// /debug/vars document on every engine. The standby split is NOT in here:
// StandbyErrorCount cuts the same total along the other axis (which
// sub-engine, not which cause) and putting it in this map would make the
// sum wrong. It travels as Tier1Summary.EngineStandbyErrorCount, next to
// PeakStandbyActiveConns.
//
// NONE of these is gated, and that is deliberate. [ZeroWitnessMeaning]'s
// counters each name an event that cannot happen in a correct engine, so
// one is a failure. These count things that legitimately happen: a client
// that abandoned a response, an accept cancelled by the PauseAccept a
// promotion performs, a handler that returned an error. celeris#646
// measured 88,010 SendPeerGone over 88,776 accepts on a healthy io_uring
// abandon-churn load. Until a run says what normal looks like on each
// engine, any threshold here would be a number nobody measured.
//
// It lives in report rather than in the checker that populates it for the
// same reason ZeroWitnessMeaning does: two copies of the set would drift,
// and a bucket declared on one side and recorded on the other is silent in
// both directions.
var ErrorClasses = map[string]ErrorClass{
	"engine_error_accept_fd_limit": {
		Counts: "accepts refused for want of a descriptor, EMFILE (per-process) or ENFILE (system-wide); the connection was never accepted (celeris#646)",
		Series: true,
		Why:    "a connection refused at accept leaves no other trace anywhere in the artifact — no conn-table entry, no close hook, no request — so a walker's handshake-fail instant has nothing but this column to join against (celeris#588)",
	},
	"engine_error_accept_cancelled": {
		Counts: "accept failures that mean the accept went away rather than the host running out of something: ECANCELED and EBADF (what a PauseAccept does), ECONNABORTED, EINTR. io_uring counts all four; epoll retries ECONNABORTED and EINTR in place and counts neither (celeris#646)",
		Series: true,
		Why:    "it steps deterministically at the promotion — celeris#646 measured exactly two per io_uring worker per PauseAccept — so joined against adaptive_switches in the same row it is what separates a switch transient from sustained accept-side loss",
	},
	"engine_error_accept_other": {
		Counts: "accept failures that are neither an fd limit nor a cancellation (celeris#646)",
		Series: true,
		Why:    "same class as accept_fd_limit: a connection that was never answered and is recorded nowhere else. Kept separate from it so the step is attributed rather than narrowed to one of two",
	},
	"engine_error_conn_table_cap": {
		Counts: "descriptors dropped because they fall outside the worker's flat connection table; the descriptor is closed and the connection is lost, so a nonzero value is a worker at its per-worker connection limit (celeris#646)",
		Series: true,
		Why:    "it fires on two different paths at two different instants — accept, and transplant adoption — and only the timestamp, joined against engine_transplant_adopted, says which one this cell hit",
	},
	"engine_error_conn_register": {
		Counts: "descriptors dropped because registering them with the event loop failed (epoll_ctl ADD), on both the accept and the adoption path; epoll only (celeris#646)",
		Series: false,
		Why:    "a kernel refusal of a single syscall, not a load-shaped rate, and it is co-timed with either an accept or an adoption — both already timestamped in the series. The cell total is what a reader would act on",
	},
	"engine_error_listener_recreate": {
		Counts: "failures to re-create a listen socket after a ResumeAccept; the loop or worker that hits one shuts itself down, so the engine has permanently lost accept capacity on that worker (celeris#646)",
		Series: false,
		Why:    "celeris's own comment at both increment sites: \"the bump is not a rate: it is the one record that the engine lost a listener\". At most one per loop, and the loop returns immediately after. Note the series cannot show the capacity loss either way — EngineMetrics.Workers is len(loops), a static slice length that does not drop when a loop dies — which makes the tally entry the only record there is, not a reason to sample it 3,600 times",
	},
	"engine_error_transplant_adopt": {
		Counts: "adoptions refused because the target's conn-table slot for that descriptor was already occupied; tracks EngineMetrics.TransplantAdoptSlotOccupied one for one (celeris#624, celeris#646)",
		Series: false,
		Why:    "engine_transplant_adopt_slot_occupied is ALREADY a series column and a gated zero witness, and celeris derives this bucket from the same event. A second column carrying provably identical values buys no timestamp. In the tally it earns its keep as the cross-check: this and the zero witness disagreeing means one of the two accountings is broken",
	},
	"engine_error_send_peer_gone": {
		Counts: "send completions that failed because the peer was already gone (EPIPE, ECONNRESET, ECONNABORTED, ENOTCONN) — one per connection whose client stopped reading before its response flushed. io_uring only; epoll's write path has never fed ErrorCount at all (celeris#645, celeris#646)",
		Series: true,
		Why:    "the bucket celeris#645 has to size, and \"proportionate to the load\" is a rate: it needs the per-second denominators only the series carries in the same row (engine_requests_total, active). It is also the bucket that drowns the total — 88,010 of 88,776 accepts on io_uring — so without its own column no step in engine_error_count can be read at all",
	},
	"engine_error_send": {
		Counts: "send completions that failed for any other reason — a genuine transmit fault rather than a client that left. io_uring only (celeris#646)",
		Series: false,
		Why:    "nonzero at all is a defect, and when one fires the walker's slow-read ring (schema 5.9) already carries the instant, both socket addresses and the verbatim error, at far higher fidelity than a 1 Hz column. Promote it if a run ever shows it moving in bulk",
	},
	"engine_error_request_body": {
		Counts: "requests rejected before the handler ran because the body would not read or exceeded MaxRequestBodySize; std only (celeris#646)",
		Series: false,
		Why:    "request-shaped and decided before the handler: every instance is a request the walker sent and got a status back for, so the walker's own record timestamps it already and at per-request resolution",
	},
	"engine_error_handler": {
		Counts: "handler invocations that returned an error; std only, the native engines do not fold a handler error into ErrorCount (celeris#646)",
		Series: false,
		Why:    "same as request_body. And the standing question about it — why every std cell in nightly 34918161309 sat at exactly 45 — is a question about a total, not about a trajectory",
	},
}

// SeriesErrorClasses returns, sorted, the bucket names [ErrorClasses] marks
// as worth a per-second column. validation/series.go writes the columns in
// its own explicit order (a CSV row is positional, and the order is part of
// the file format); this is what its test compares that order's membership
// against, so a bucket promoted here and forgotten there fails loudly.
func SeriesErrorClasses() []string {
	out := make([]string, 0, len(ErrorClasses))
	for k, c := range ErrorClasses {
		if c.Series {
			out = append(out, k)
		}
	}
	sort.Strings(out)
	return out
}
