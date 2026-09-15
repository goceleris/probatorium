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
			// EngineMetrics.ErrorCount is bumped on the engine's
			// accept-side error paths (the epoll conn-table cap and
			// EMFILE/ENFILE drops, the io_uring listener re-creation).
			// Exported so the property loop's 1 Hz series shows it
			// STEP at the instant of a deaf-listener event, which is the
			// one class a walker's timeout cannot tell from a stall
			// (celeris#588). Not judged by any predicate.
			doc["celeris.engine_error_count"] = int64(info.Metrics.ErrorCount)
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
