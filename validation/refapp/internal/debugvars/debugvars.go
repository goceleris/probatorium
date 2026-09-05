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

	mu       sync.Mutex
	cachedAt time.Time
	memstats runtime.MemStats
}

// New returns an empty Vars.
func New() *Vars { return &Vars{} }

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
		if prevConnect != nil {
			prevConnect(addr)
		}
	}
	cfg.OnDisconnect = func(addr string) {
		v.closed.Add(1)
		if prevDisconnect != nil {
			prevDisconnect(addr)
		}
	}
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
	}
	if srv := v.srv.Load(); srv != nil {
		if info := srv.EngineInfo(); info != nil {
			doc["celeris.active_conns"] = info.Metrics.ActiveConnections
			doc["celeris.adaptive_switches"] = int64(info.Metrics.AdaptiveSwitches)
			doc["celeris.engine"] = info.Type.String()
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
