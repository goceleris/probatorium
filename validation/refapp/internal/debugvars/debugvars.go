// Package debugvars gives a validation refapp the /debug/vars document
// the validator's property loop polls (validation/checker.Poll), plus
// the /debug/pprof profiles the incident forensics fetch.
//
// Wiring (see any validation/refapp/<slug>/main.go):
//
//	dv := debugvars.New()
//	srv := dv.NewServer(celeris.Config{...}) // = Hook (OnConnect/OnDisconnect
//	                                         //   counters) + celeris.New +
//	                                         //   Mount (srv.Pre /debug/vars,
//	                                         //   /debug/pprof)
//	srv.Use(recovery.New(recovery.Config{Logger: dv.RecoveryLogger(sink)}))
//
// Both endpoints are mounted with Server.Pre, which runs BEFORE route
// lookup and bypasses every Use middleware: the 1 Hz poll never mints a
// session, burns a ratelimit token, or needs a placeholder route, and
// none of the benchmarked routes change. Only loopback peers are served.
//
// Document shape (flat keys; exactly what checker.Poll reads):
//
//	{
//	  "goroutines":                  runtime.NumGoroutine(),
//	  "celeris.accepted_conn_total": connections accepted (Config.OnConnect),
//	  "celeris.closed_conn_total":   connections closed   (Config.OnDisconnect),
//	  "celeris.active_conns":        EngineMetrics.ActiveConnections,
//	  "celeris.panic_count":         panics recovered by middleware/recovery,
//	  "celeris.adaptive_switches":   EngineMetrics.AdaptiveSwitches,
//	  "celeris.engine":              engine type name,
//
//	  // EngineMetrics.ErrorCount and the eleven cause buckets it is the
//	  // exact sum of (celeris#646), plus the adaptive engine's
//	  // share-by-sub-engine split. See report.ErrorClasses.
//	  "celeris.engine_error_count",
//	  "celeris.engine_error_accept_fd_limit",
//	  "celeris.engine_error_accept_cancelled",
//	  "celeris.engine_error_accept_other",
//	  "celeris.engine_error_conn_table_cap",
//	  "celeris.engine_error_conn_register",
//	  "celeris.engine_error_listener_recreate",
//	  "celeris.engine_error_transplant_adopt",
//	  "celeris.engine_error_send_peer_gone",
//	  "celeris.engine_error_send",
//	  "celeris.engine_error_request_body",
//	  "celeris.engine_error_handler",
//	  "celeris.engine_standby_error_count",
//	  "memstats":                    runtime.MemStats (cached, see MemStatsTTL)
//
//	  // Middleware oracles + this refapp's declaration of what it can
//	  // judge. Always present, moved only by the refapps that install the
//	  // matching middleware. See middleware.go.
//	  "celeris.session_owner_mismatches", "celeris.sessions_created_total",
//	  "celeris.sessions_expired_total",   "celeris.session_cookie_drops",
//	  "celeris.ratelimit_allowed",        "celeris.ratelimit_rejected",
//	  "celeris.ratelimit_token_violations",
//	  "celeris.jwt_validated_ok",         "celeris.jwt_validated_fail",
//	  "celeris.jwt_late_admits",          "celeris.instrumented_properties",
//	  "celeris.driver_writes_issued",     "celeris.driver_reads_issued",
//	  "celeris.driver_read_hits",         "celeris.driver_read_misses",
//	  "celeris.validation_build",         "celeris.iouring_sqe_corruptions",
//	  "celeris.race_build"
//	}
//
// Why the connection counters come from the refapp and not the engine:
// engine.EngineMetrics.AcceptCount/CloseCount are 0 on the std engine,
// dropped by the adaptive engine's aggregation, and drift across an
// io_uring->epoll demote (the detach counts as a close with no matching
// accept). Config.OnConnect/OnDisconnect fire on every engine for real
// accepts/closes only, so accepted - closed tracks ActiveConnections on
// all four engines, including across adaptive transplants.
//
// Why panics are counted here: celeris/validation.RecordPanic is a
// no-op unless celeris is built with -tags=validation, and the refapps
// are plain builds. middleware/recovery logs every recovered panic
// (message "panic recovered") through its Logger, so a counting slog
// handler in front of the refapp's sink sees each one exactly once.
// Broken-pipe panics (peer went away mid-write) are logged under a
// different message and are deliberately not counted. When the build IS
// validation-tagged, validation.Snapshot().PanicCount is folded in.
//
// Why EVERY engine.EngineMetrics field is published, including the ones
// no predicate reads: the projection below is by hand, and a hand-list
// silently loses whatever celeris adds next. It already has. celeris#647
// added TransplantStranded and TransplantAdoptRefused precisely so its
// own fix for celeris#624 would be falsifiable, race tier run
// 34961642523 was dispatched to test that prediction, and two of its
// four clauses could not be evaluated at all because this document did
// not carry the counters (probatorium#386). A key that is absent parses
// as zero and is indistinguishable from a counter that is clean, which
// is the same failure probatorium#297 cost two soaks.
//
// So the rule here is TOTAL PROJECTION: every scalar field of
// engine.EngineMetrics gets a key, whether or not anything reads it yet.
// TestDebugVarsPublishesEveryEngineMetricsField walks the struct by
// reflection and fails the moment celeris grows a field this file does
// not carry -- in the refapp that has to publish it, rather than in a
// nightly that quietly reports one counter short.
package debugvars

import (
	"context"
	"log/slog"
	"net"
	"runtime"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/pprof"
	"github.com/goceleris/celeris/validation"
)

// Path is the route the document is served on.
const Path = "/debug/vars"

// MemStatsTTL bounds how often runtime.ReadMemStats runs: every call is
// a stop-the-world pause, so the document caches the last reading for
// this long (the same floor middleware/debug uses). The checker polls
// at 1 Hz, so it always sees a fresh-enough reading.
const MemStatsTTL = time.Second

// recoveredPanicMessage is the slog message middleware/recovery emits
// once per recovered (non-broken-pipe) panic.
const recoveredPanicMessage = "panic recovered"

// Vars owns the counters behind the document. Construct with New.
type Vars struct {
	accepted atomic.Int64
	closed   atomic.Int64
	panics   atomic.Int64

	srv atomic.Pointer[celeris.Server]

	// mw carries the middleware-oracle counters and the refapp's
	// declaration of which property predicates it can judge. See
	// middleware.go.
	mw middleware

	// drv is the read-after-write tally behind I-DRV. See driver.go.
	drv driver

	// conns is the per-connection last-byte table behind I-CONN-1. See
	// conntable.go for why this lives here rather than in the engine.
	conns connTable

	mu       sync.Mutex
	cachedAt time.Time
	memstats runtime.MemStats
}

// New returns an empty Vars.
func New() *Vars {
	v := &Vars{}
	if checkptrBuild {
		v.Declare("I-CHECKPTR")
	}
	return v
}

// NewServer is Hook + celeris.New + Mount in one call: the refapp's
// `srv := celeris.New(cfg)` becomes `srv := dv.NewServer(cfg)`.
func (v *Vars) NewServer(cfg celeris.Config) *celeris.Server {
	v.Hook(&cfg)
	srv := celeris.New(cfg)
	v.Mount(srv)
	return srv
}

// Hook installs the connection counters on cfg, chaining any callbacks
// already set. Must run before celeris.New(cfg).
func (v *Vars) Hook(cfg *celeris.Config) {
	prevConnect, prevDisconnect := cfg.OnConnect, cfg.OnDisconnect
	cfg.OnConnect = func(addr string) {
		v.accepted.Add(1)
		v.conns.open(addr, time.Now().UnixNano())
		if prevConnect != nil {
			prevConnect(addr)
		}
	}
	cfg.OnDisconnect = func(addr string) {
		v.closed.Add(1)
		v.conns.close(addr)
		if prevDisconnect != nil {
			prevDisconnect(addr)
		}
	}
}

// TouchConn refreshes a connection's last-byte stamp from outside the
// request path. Detached streams -- WebSocket and SSE -- stop producing
// requests the moment they upgrade, so without this their entries age
// forever and I-CONN-1 fires on a perfectly healthy long-lived stream.
// Call it from the stream handler on each frame or event.
func (v *Vars) TouchConn(addr string) {
	v.conns.touch(addr, time.Now().UnixNano())
}

// RecordPanic bumps the panic counter. RecoveryLogger calls it; exposed
// for refapps that recover outside middleware/recovery.
func (v *Vars) RecordPanic() { v.panics.Add(1) }

// RecoveryLogger wraps sink so every "panic recovered" record bumps the
// panic counter before reaching sink. Pass the result as
// recovery.Config.Logger. A nil sink discards records.
func (v *Vars) RecoveryLogger(sink *slog.Logger) *slog.Logger {
	var inner slog.Handler
	if sink != nil {
		inner = sink.Handler()
	}
	return slog.New(&countingHandler{v: v, inner: inner})
}

// countingHandler is the slog.Handler behind RecoveryLogger.
type countingHandler struct {
	v     *Vars
	inner slog.Handler
}

func (h *countingHandler) Enabled(ctx context.Context, level slog.Level) bool {
	// Always enabled so the count is independent of the sink's level.
	return true
}

func (h *countingHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Message == recoveredPanicMessage {
		h.v.RecordPanic()
	}
	if h.inner == nil || !h.inner.Enabled(ctx, r.Level) {
		return nil
	}
	return h.inner.Handle(ctx, r)
}

func (h *countingHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if h.inner == nil {
		return h
	}
	return &countingHandler{v: h.v, inner: h.inner.WithAttrs(attrs)}
}

func (h *countingHandler) WithGroup(name string) slog.Handler {
	if h.inner == nil {
		return h
	}
	return &countingHandler{v: h.v, inner: h.inner.WithGroup(name)}
}

// Mount registers the /debug/vars and /debug/pprof pre-routing handlers
// on srv. Must run before srv.Start.
func (v *Vars) Mount(srv *celeris.Server) {
	v.srv.Store(srv)
	srv.Pre(v.Handler(), pprof.New())
}

// Handler is the /debug/vars pre-routing handler: serves the document
// to loopback GET/HEAD requests for Path and passes everything else on.
func (v *Vars) Handler() celeris.HandlerFunc {
	return func(c *celeris.Context) error {
		// Stamp every request, on every path, BEFORE the early return.
		//
		// This handler is mounted with srv.Pre, so it runs ahead of route
		// lookup for all traffic. Putting the stamp in ordinary middleware
		// instead would miss exactly one connection: the property loop's
		// own 1 Hz poller, because /debug/vars is served from Pre and calls
		// Abort, bypassing the Use chain. That keep-alive conn would then be
		// the oldest entry in every cell and I-CONN-1 would fire on the
		// checker itself within ~30s, everywhere.
		v.conns.touch(c.RemoteAddr(), time.Now().UnixNano())
		if c.Path() != Path {
			return c.Next()
		}
		c.Abort()
		if !isLoopback(c.RemoteAddr()) {
			return c.String(403, "forbidden")
		}
		if m := c.Method(); m != "GET" && m != "HEAD" {
			return c.String(405, "method not allowed")
		}
		return c.JSON(200, v.Document())
	}
}

// Document builds the current /debug/vars document.
func (v *Vars) Document() map[string]any {
	doc := map[string]any{
		"goroutines":                  int64(runtime.NumGoroutine()),
		"celeris.accepted_conn_total": v.accepted.Load(),
		"celeris.closed_conn_total":   v.closed.Load(),
		"celeris.panic_count":         v.PanicCount(),
		"memstats":                    v.MemStats(),
		// I-CONN-1's input. Always present, even at zero, so the document
		// keeps a fixed shape and a missing key is distinguishable from a
		// clean reading.
		"celeris.oldest_open_conn_last_byte_age_ms": v.conns.oldestAgeMs(time.Now().UnixNano()),
		"celeris.open_conns_tracked":                v.conns.liveConns(),
	}
	v.middlewareDocument(doc)
	v.driverDocument(doc)
	// True only in a -tags=validation build, where celeris's own assertion
	// counters exist. The property loop declares I-ENG-IOURING on it (for
	// an io_uring cell), so a plain build never reports the predicate as
	// covered when its counter is merely the zero the stub returns.
	doc["celeris.validation_build"] = validationBuild
	doc["celeris.race_build"] = raceBuild
	doc["celeris.iouring_sqe_corruptions"] = int64(validation.Snapshot().IouringSQECorruptions)
	if srv := v.srv.Load(); srv != nil {
		if info := srv.EngineInfo(); info != nil {
			doc["celeris.active_conns"] = info.Metrics.ActiveConnections
			doc["celeris.adaptive_switches"] = int64(info.Metrics.AdaptiveSwitches)
			// EngineMetrics.ErrorCount, and since celeris#646 the
			// eleven cause buckets it is the exact sum of. Exported so
			// the property loop's 1 Hz series shows it STEP at the
			// instant of a deaf-listener event, which is the one class
			// a walker's timeout cannot tell from a stall
			// (celeris#588). Not judged by any predicate.
			//
			// The total alone could not answer celeris#645: the
			// adaptive engine recorded 421 engine errors in a
			// 112-second cell against io_uring's 63 and epoll's 0, and
			// one number can bound the cause but never name it. Every
			// bucket is published, even the ones the per-cell series
			// does not sample, because the end-of-cell tally reads
			// this document and nothing else.
			doc["celeris.engine_error_count"] = int64(info.Metrics.ErrorCount)
			doc["celeris.engine_error_accept_fd_limit"] = int64(info.Metrics.ErrorAcceptFDLimit)
			doc["celeris.engine_error_accept_cancelled"] = int64(info.Metrics.ErrorAcceptCancelled)
			doc["celeris.engine_error_accept_other"] = int64(info.Metrics.ErrorAcceptOther)
			doc["celeris.engine_error_conn_table_cap"] = int64(info.Metrics.ErrorConnTableCap)
			doc["celeris.engine_error_conn_register"] = int64(info.Metrics.ErrorConnRegister)
			doc["celeris.engine_error_listener_recreate"] = int64(info.Metrics.ErrorListenerRecreate)
			doc["celeris.engine_error_transplant_adopt"] = int64(info.Metrics.ErrorTransplantAdopt)
			doc["celeris.engine_error_send_peer_gone"] = int64(info.Metrics.ErrorSendPeerGone)
			doc["celeris.engine_error_send"] = int64(info.Metrics.ErrorSend)
			doc["celeris.engine_error_request_body"] = int64(info.Metrics.ErrorRequestBody)
			doc["celeris.engine_error_handler"] = int64(info.Metrics.ErrorHandler)
			doc["celeris.engine"] = info.Type.String()
			// The four inputs to the adaptive controller's promotion
			// decision, published so a cell that never promoted can say
			// WHY instead of only that it did not (celeris#580 proved the
			// switch counter; this proves the load that was offered).
			// The controller divides ActiveConnections by Workers to get
			// conns/worker, and derives bytes/req from the per-interval
			// delta of (BytesRead+BytesWritten)/RequestCount -- so at the
			// property loop's 1 Hz these four reconstruct both signals
			// offline, per interval, from the series alone.
			doc["celeris.engine_workers"] = int64(info.Metrics.Workers)
			doc["celeris.engine_requests_total"] = int64(info.Metrics.RequestCount)
			doc["celeris.engine_bytes_read"] = int64(info.Metrics.BytesRead)
			doc["celeris.engine_bytes_written"] = int64(info.Metrics.BytesWritten)
			// Engine-side connection accounting, against which the
			// refapp's own OnConnect/OnDisconnect tallies are the
			// independent witness. I-CONN-2 fires on
			// accepted - closed - active, and celeris#624 showed a
			// cell lose two connections from `active` with the hooks
			// unmoved -- which the artifact could not attribute,
			// because it carried only the hook side. With both sides
			// recorded the drift forks itself: engine_close_count
			// running AHEAD of the hook count means a close skipped
			// its hook, and the two agreeing while `active` is short
			// means nothing closed and a connection was detached
			// without being re-adopted.
			doc["celeris.engine_accept_count"] = int64(info.Metrics.AcceptCount)
			doc["celeris.engine_close_count"] = int64(info.Metrics.CloseCount)
			// How much of the cell's traffic took the async dispatch
			// path. celeris#626 was invisible for as long as it was
			// because nothing recorded this: the epoll request counter
			// stopped advancing on exactly these connections, and a
			// cell could not say how many it had.
			doc["celeris.engine_async_promoted_conns"] = int64(info.Metrics.AsyncPromotedConns)
			// The adaptive engine's two halves, separately. Its
			// Metrics() sums the sub-engines, so a connection lost on
			// one side is invisible in the total (celeris#627). Zero
			// on every non-adaptive engine.
			doc["celeris.engine_standby_active_conns"] = info.Metrics.StandbyActiveConnections
			doc["celeris.engine_standby_close_count"] = int64(info.Metrics.StandbyCloseCount)
			// The same split applied to the error total (celeris#646).
			// The buckets above say WHAT went wrong; this says which
			// sub-engine it went wrong on, and celeris#645 needs both
			// at the same second to distinguish "the standby's accepts
			// were cancelled at the promotion" from "the promoted
			// engine is failing sends".
			doc["celeris.engine_standby_error_count"] = int64(info.Metrics.StandbyErrorCount)
			// The transplant hand-off, both directions. The epoll side
			// decrements its live count and fires NO hook by design,
			// and the io_uring side increments on adoption; a residual
			// between them is a connection that left one engine and
			// never arrived at the other.
			doc["celeris.engine_transplant_detached"] = int64(info.Metrics.TransplantDetached)
			doc["celeris.engine_transplant_adopted"] = int64(info.Metrics.TransplantAdopted)
			// MUST-STAY-ZERO witnesses. Each names one specific
			// defect, and a nonzero value is that defect firing, not a
			// note: the adoption path finding its slot already
			// occupied (celeris#624 hypothesis A), a close decrementing
			// the gauge with no connection state so the hook is
			// skipped (#624 hypothesis B), a second recv armed while
			// one is already in flight (#484's stream corruption), a
			// recv completion that accounts to nothing (#484), and the
			// two #607 guards -- a submission queue that filled and a
			// recv that stalled behind it. The #607 pair read zero
			// across the whole investigation that produced them, which
			// is exactly why they ship: a guard that has never fired is
			// only worth keeping if it is still watching.
			doc["celeris.engine_transplant_adopt_slot_occupied"] = int64(info.Metrics.TransplantAdoptSlotOccupied)
			doc["celeris.engine_close_missing_conn_state"] = int64(info.Metrics.CloseMissingConnState)
			doc["celeris.engine_recv_double_armed"] = int64(info.Metrics.RecvDoubleArmed)
			doc["celeris.engine_recv_cqe_unaccounted"] = int64(info.Metrics.RecvCQEUnaccounted)
			doc["celeris.engine_recv_sq_full"] = int64(info.Metrics.RecvSQFull)
			doc["celeris.engine_recv_stall_episodes"] = int64(info.Metrics.RecvStallEpisodes)
			// The duration half of that same stall. The episode count
			// says how OFTEN a connection was passed over while it was
			// owed a recv arm; only the nanos say whether an episode was
			// submission-queue pressure clearing inside a pass (normal)
			// or a connection that received nothing for seconds with its
			// peer's bytes already sitting in the kernel. celeris#607
			// turns on exactly that distinction, so the count without
			// the durations cannot reproduce the finding.
			doc["celeris.engine_recv_stall_nanos"] = int64(info.Metrics.RecvStallNanos)
			doc["celeris.engine_recv_stall_max_nanos"] = int64(info.Metrics.RecvStallMaxNanos)
			// The IOSQE_IO_LINK chain celeris#607 was actually solved on:
			// a RECV linked behind a SEND does not start until the SEND
			// completes, so a peer that stops reading blocks the send and
			// takes the recv down with it. `arms` is the exposure
			// denominator -- the reading the fix was confirmed against
			// was this counter at zero, and a cell cannot report "zero"
			// for a counter it never published.
			doc["celeris.engine_recv_linked_arms"] = int64(info.Metrics.RecvLinkedArms)
			doc["celeris.engine_recv_linked_blocked_nanos"] = int64(info.Metrics.RecvLinkedBlockedNanos)
			doc["celeris.engine_recv_linked_blocked_max_nanos"] = int64(info.Metrics.RecvLinkedBlockedMaxNanos)
			// The celeris#484 resume window and the #560 guard standing
			// in it. `cancel_pending` counts resumes processed while the
			// pause's ASYNC_CANCEL was still in flight; `recv_in_flight`
			// is the subset where the cancelled recv was still armed --
			// the only state in which a second recv could be placed on
			// top of a kernel-held one. Those two are the exposure
			// witnesses for engine_recv_double_armed above: at zero, a
			// clean double-armed count is unearned rather than
			// reassuring, because the window was never reached
			// (celeris#586). `arm_declined` is the guard itself firing.
			doc["celeris.engine_recv_resume_while_cancel_pending"] = int64(info.Metrics.RecvResumeWhileCancelPending)
			doc["celeris.engine_recv_resume_while_recv_in_flight"] = int64(info.Metrics.RecvResumeWhileRecvInFlight)
			doc["celeris.engine_recv_arm_declined"] = int64(info.Metrics.RecvArmDeclined)
			// The four remaining hand-off outcomes, so a nonzero
			// detached - adopted residual can be READ rather than
			// inferred. Every one of them used to be a silent close: the
			// source had already dropped the descriptor from its loop,
			// live set and conn table with no OnDisconnect, and the
			// branch closed it with no counter moving -- the
			// unattributable step celeris#624 chased.
			//
			// `stranded` MUST STAY ZERO: the two flags it sits between
			// are mutually exclusive by construction, and the counter is
			// what makes that argument checkable instead of asserted.
			// The other three are recoveries, not faults --
			// `handoff_refused` and `drain_stopped` re-adopt onto the
			// source, so each is paired with a TransplantAdopted on the
			// SAME engine and the residual returns to zero, and
			// `adopt_refused` closes the descriptor AND fires
			// OnDisconnect, so accepted - closed - active stays balanced.
			// Which of the three moved is how an adopted count that rose
			// without a hand-off is told apart from one that arrived.
			doc["celeris.engine_transplant_handoff_refused"] = int64(info.Metrics.TransplantHandoffRefused)
			doc["celeris.engine_transplant_drain_stopped"] = int64(info.Metrics.TransplantDrainStopped)
			doc["celeris.engine_transplant_stranded"] = int64(info.Metrics.TransplantStranded)
			doc["celeris.engine_transplant_adopt_refused"] = int64(info.Metrics.TransplantAdoptRefused)
			// celeris#657: the io_uring -> epoll hand-off that lost
			// requests while the #624 ledger above kept balancing. A
			// recv armed before the hand-off outlived it, read the
			// client's next request off the socket epoll now owned, and
			// was dropped as stale with nothing counting it.
			//
			// The loss witnesses (celeris#676). `stale_recv_data_*` count
			// recv completions that READ BYTES for an identity that no
			// longer owns the descriptor, split by what the engine still
			// holds for it: `transplanted` is the #657 loss itself,
			// `unattributed` a gap in attribution, `closed` usually a
			// client's bytes racing a server-side close.
			// `handoff_in_flight` is a hand-off made with an op still
			// outstanding, the precondition of every transplanted loss.
			// celeris#681's gate is transplanted + unattributed (W1) and
			// handoff_in_flight (W2) reading zero.
			doc["celeris.engine_stale_recv_data_closed"] = int64(info.Metrics.StaleRecvDataClosed)
			doc["celeris.engine_stale_recv_data_transplanted"] = int64(info.Metrics.StaleRecvDataTransplanted)
			doc["celeris.engine_stale_recv_data_unattributed"] = int64(info.Metrics.StaleRecvDataUnattributed)
			doc["celeris.engine_transplant_handoff_in_flight"] = int64(info.Metrics.TransplantHandoffInFlight)
			// The fd-lifetime rule's counters (celeris#681): a connection
			// leaves io_uring only when no read can still resolve its
			// descriptor. `held`, `reaps`, `reap_misses`,
			// `claim_deferred` and `reap_unsupported` are rates of the
			// mechanism; `hold_rescued`, `double_claim` and
			// `reap_failed` celeris documents MUST STAY ZERO.
			doc["celeris.engine_transplant_held"] = int64(info.Metrics.TransplantHeld)
			doc["celeris.engine_transplant_reaps"] = int64(info.Metrics.TransplantReaps)
			doc["celeris.engine_transplant_reap_misses"] = int64(info.Metrics.TransplantReapMisses)
			doc["celeris.engine_transplant_hold_rescued"] = int64(info.Metrics.TransplantHoldRescued)
			doc["celeris.engine_transplant_double_claim"] = int64(info.Metrics.TransplantDoubleClaim)
			doc["celeris.engine_transplant_claim_deferred"] = int64(info.Metrics.TransplantClaimDeferred)
			doc["celeris.engine_transplant_reap_failed"] = int64(info.Metrics.TransplantReapFailed)
			doc["celeris.engine_transplant_reap_unsupported"] = int64(info.Metrics.TransplantReapUnsupported)
			// The post-switch sweep (celeris#687). `sweep_passes` is a
			// rate, the sweep's cost. The five `residual_*` are GAUGES,
			// not totals: the connections a draining engine still holds,
			// by the reason the hand-off refused them. A standing
			// nonzero `busy` after a switch settles is the placement bug
			// itself; the other four are the expected residue.
			doc["celeris.engine_transplant_sweep_passes"] = int64(info.Metrics.TransplantSweepPasses)
			doc["celeris.engine_transplant_residual_detached"] = int64(info.Metrics.TransplantResidualDetached)
			doc["celeris.engine_transplant_residual_h2"] = int64(info.Metrics.TransplantResidualH2)
			doc["celeris.engine_transplant_residual_pinned"] = int64(info.Metrics.TransplantResidualPinned)
			doc["celeris.engine_transplant_residual_unstarted"] = int64(info.Metrics.TransplantResidualUnstarted)
			doc["celeris.engine_transplant_residual_busy"] = int64(info.Metrics.TransplantResidualBusy)
			// Detach accounting, the input to I-ENG-DETACH
			// (probatorium#352). `detached_conns` is a GAUGE, not a
			// total: the live number of connections handed to a detached
			// middleware goroutine (WebSocket / SSE), summed over the
			// io_uring workers, so a drift between it and the number of
			// live streams is the celeris#549 accounting bug made visible
			// (celeris#584). `detach_window_closes` counts the closes
			// that landed between the middleware's Detach and the
			// worker's deferred increment; before celeris#551 each of
			// those decremented with no matching increment, so it is the
			// proof the window was entered at all.
			doc["celeris.engine_detached_conns"] = info.Metrics.DetachedConnections
			doc["celeris.engine_detach_window_closes"] = int64(info.Metrics.DetachWindowCloses)
			// The io_uring egress split. `zc_sends_submitted` is the
			// exposure witness for the zero-copy send path: a bench or
			// soak that reports a clean SEND_ZC result with this at 0
			// never ran the branch (celeris#585/#587/#591), and
			// submitted - notifs is the number of ZC sends whose buffer
			// the kernel still holds pinned, which bounds how long the
			// cycle stayed open. `inline_bytes` vs `ring_bytes` splits
			// BytesWritten into the raw unix.Write(2) a detached stream
			// takes -- bytes that can never be zero-copy -- and the bytes
			// that went through the ring, which is the denominator any
			// SEND_ZC A/B needs before a throughput delta means anything.
			doc["celeris.engine_zc_sends_submitted"] = int64(info.Metrics.ZCSendsSubmitted)
			doc["celeris.engine_zc_notifs"] = int64(info.Metrics.ZCNotifs)
			doc["celeris.engine_inline_bytes"] = int64(info.Metrics.InlineBytes)
			doc["celeris.engine_ring_bytes"] = int64(info.Metrics.RingBytes)
			// Static after Listen: how many of this refapp's routes were
			// registered with .Async(true), i.e. how many handlers CAN
			// take the per-conn dispatch goroutine. It is the
			// denominator for engine_async_promoted_conns above --
			// promotions counted against a refapp with no async routes
			// at all is a different reading from the same number against
			// one that has them.
			doc["celeris.engine_async_routes"] = int64(info.Metrics.AsyncRoutes)
			// Published as a float, because it is the one non-integer
			// field on EngineMetrics. No shipped engine assigns it --
			// std, epoll and io_uring leave it at zero and the adaptive
			// engine sums two zeros -- so it reads 0 everywhere today and
			// a nonzero value means celeris started populating it. It is
			// here anyway: a field that exists and is never published
			// cannot be told apart from one that is published and never
			// moves, and removing that ambiguity is what this document
			// is for (probatorium#297).
			doc["celeris.engine_throughput"] = info.Metrics.Throughput
		}
	}
	return doc
}

// PanicCount is the larger of the recovery-logger count and the
// validation-build counter (0 in plain builds).
func (v *Vars) PanicCount() int64 {
	n := v.panics.Load()
	if vc := int64(validation.Snapshot().PanicCount); vc > n {
		n = vc
	}
	return n
}

// Accepted and Closed expose the connection counters.
func (v *Vars) Accepted() int64 { return v.accepted.Load() }

// Closed is the number of connections closed so far.
func (v *Vars) Closed() int64 { return v.closed.Load() }

// MemStats returns a copy of runtime.MemStats no older than MemStatsTTL.
// ReadMemStats runs outside the lock; two concurrent cache misses both
// pay the stop-the-world, which is benign for a single 1 Hz poller.
func (v *Vars) MemStats() runtime.MemStats {
	v.mu.Lock()
	if !v.cachedAt.IsZero() && time.Since(v.cachedAt) < MemStatsTTL {
		ms := v.memstats
		v.mu.Unlock()
		return ms
	}
	v.mu.Unlock()

	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)

	v.mu.Lock()
	v.memstats = ms
	v.cachedAt = time.Now()
	v.mu.Unlock()
	return ms
}

// isLoopback reports whether a host:port (or bare host) is a loopback
// address. Anything unparseable is rejected.
func isLoopback(remote string) bool {
	host, _, err := net.SplitHostPort(remote)
	if err != nil {
		host = remote
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
