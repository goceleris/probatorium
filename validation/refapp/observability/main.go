// Command observability is the logger + metrics + otel validation
// refapp: celeris with all three observability middlewares mounted
// together, plus a small route surface that exercises happy / error /
// slow paths so the validator's Tier 1 walker can check the
// instrumentation invariants.
//
// Coverage per probatorium#103 + #111:
//
//   - middleware/logger: structured-log sink doesn't drop events
//     under load. The refapp wires an in-memory slog ring buffer
//     that the /metrics endpoint exposes the drop count of, so the
//     walker can scrape "obs_log_drops" and fail HIGH on non-zero.
//   - middleware/metrics: prometheus histogram buckets stay
//     monotonic across the run. The /metrics endpoint exposes the
//     standard celeris request histogram via promhttp.
//   - middleware/otel: tracer / meter providers attached. We use
//     the noop default providers — the invariant checked here is
//     "the middleware doesn't panic when noop providers are wired,
//     and spans don't orphan across async-handler goroutines".
//
// On startup the refapp prints the canonical ready line:
//
//	ready addr=<bind-addr>
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/logger"
	"github.com/goceleris/celeris/middleware/metrics"
	"github.com/goceleris/celeris/middleware/otel"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/prometheus/client_golang/prometheus"
)

// ringHandler is a bounded slog.Handler that records every log line
// in a fixed-size ring buffer. When the buffer overflows it bumps a
// drop counter — the validator scrapes that counter via /metrics to
// check the I-OBS-LOG-DROP invariant.
type ringHandler struct {
	mu     sync.Mutex
	buf    []slog.Record
	pos    int
	full   bool
	cap    int
	drops  atomic.Uint64
	writes atomic.Uint64
}

func newRingHandler(cap int) *ringHandler {
	return &ringHandler{buf: make([]slog.Record, cap), cap: cap}
}

func (h *ringHandler) Enabled(_ context.Context, _ slog.Level) bool { return true }
func (h *ringHandler) WithAttrs(_ []slog.Attr) slog.Handler         { return h }
func (h *ringHandler) WithGroup(_ string) slog.Handler              { return h }
func (h *ringHandler) Handle(_ context.Context, r slog.Record) error {
	h.writes.Add(1)
	h.mu.Lock()
	if h.full {
		// Overwriting an unread slot — counts as a drop. In a
		// well-behaved system the validator scrapes /metrics
		// between bursts so drops stay zero.
		h.drops.Add(1)
	}
	h.buf[h.pos] = r
	h.pos++
	if h.pos >= h.cap {
		h.pos = 0
		h.full = true
	}
	h.mu.Unlock()
	return nil
}

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	engineFlag := flag.String("engine", "auto", "engine: iouring | epoll | std | adaptive | auto")
	workersFlag := flag.Int("workers", 0, "io worker count (0 = celeris default GOMAXPROCS); celeris requires >=2 if set")
	ringCap := flag.Int("log-ring-cap", 16384, "capacity of the in-memory log ring buffer")
	flag.Parse()

	ring := newRingHandler(*ringCap)
	reqLog := slog.New(ring)

	// Register the celeris request-histogram middleware against an
	// isolated registry so /metrics doesn't carry stray globals.
	reg := prometheus.NewRegistry()

	// Register an obs_log_drops counter against the same registry
	// so the walker can scrape it alongside the celeris histograms.
	logDropsGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "obs_log_drops_total",
		Help: "Number of slog records the in-memory ring dropped (write rate exceeded read rate).",
	}, func() float64 { return float64(ring.drops.Load()) })
	logWritesGauge := prometheus.NewGaugeFunc(prometheus.GaugeOpts{
		Name: "obs_log_writes_total",
		Help: "Number of slog records the in-memory ring accepted.",
	}, func() float64 { return float64(ring.writes.Load()) })
	reg.MustRegister(logDropsGauge, logWritesGauge)

	// Per-handler async (celeris #300): this refapp uses a SYNC server
	// default and opts the I/O-shaped routes (slow, induced-panic) into
	// async dispatch via Route.Async(), while the fast scrape/health/ok
	// routes stay sync (inline on the worker). Exercises the
	// "sync-default + per-route .Async()" direction across the matrix.
	// (The engine still stands up the async dispatch infrastructure
	// because some routes are async, so slowloris/close behavior is
	// unchanged from the prior all-async config.)
	srv := celeris.New(celeris.Config{
		Addr:            *bind,
		Engine:          resolveEngine(*engineFlag),
		Workers:         *workersFlag,
		Protocol:        celeris.HTTP1,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	})

	// Recovery middleware logger: route through the same in-memory
	// ringHandler the logger middleware uses, NOT slog.Default(). The
	// stdlib default routes through Go's text handler whose
	// defaultHandler mutex serializes a blocking os.Stderr.Write across
	// every conn + worker; under iouring/epoll's per-conn async-dispatch
	// model that stderr lock is held inside cs.detachMu (around
	// ProcessH1), which gates the entire worker thread and lets
	// concurrent slowloris-conn header deadlines slip past. Diagnosed
	// from nightly 26438393561 (~14× throughput regression vs std on
	// this refapp + ~18 slowloris hangs/cell concentrated here).
	srv.Use(recovery.New(recovery.Config{Logger: reqLog}))
	srv.Use(requestid.New())
	srv.Use(logger.New(logger.Config{Output: reqLog}))
	srv.Use(metrics.New(metrics.Config{Registry: reg}))
	srv.Use(otel.New())

	// /healthz — sentinel for I-CONN-1.
	srv.GET("/healthz", func(c *celeris.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"obs": "ok"})
	})

	// /api/ok — happy path. Logger emits one record, metrics records
	// one observation in the success-bucket histogram, otel opens +
	// closes a span.
	srv.GET("/api/ok", func(c *celeris.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"status": "ok"})
	})

	// /api/error — panic path. recovery catches it, logger records
	// the panic, metrics observes the 500 in the error-bucket
	// histogram. Used by the walker to check that error responses
	// don't break monotonic histogram bucket counts.
	srv.GET("/api/error", func(_ *celeris.Context) error {
		panic("observability refapp: induced panic")
	}).Async() // async: exercises recovery on the async dispatch path

	// /api/slow — 5 ms sleep so the histogram bucket span includes
	// a sample > 1 ms. The walker scrapes /metrics afterwards and
	// asserts the bucket monotonicity (sum of le_X buckets ≥ sum of
	// le_X' for X' < X).
	srv.GET("/api/slow", func(c *celeris.Context) error {
		time.Sleep(5 * time.Millisecond)
		return c.JSON(http.StatusOK, map[string]string{"status": "slow"})
	}).Async() // async: slow/blocking handler runs off the worker

	// /metrics needs a registered route or celeris's router 404s
	// before the metrics-middleware chain runs. The handler body
	// is unreachable in practice — the middleware short-circuits
	// any request whose c.Path() matches Config.Path and serves
	// the gathered text-format payload itself.
	srv.GET("/metrics", func(c *celeris.Context) error {
		return c.String(http.StatusInternalServerError, "metrics middleware did not intercept")
	})

	// /obs/log-stats — direct exposure of the ring writes/drops so
	// the walker can sanity-check the Prometheus values against the
	// raw counters. Always-zero drops on a clean run.
	srv.GET("/obs/log-stats", func(c *celeris.Context) error {
		return c.JSON(http.StatusOK, map[string]any{
			"writes": ring.writes.Load(),
			"drops":  ring.drops.Load(),
		})
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("observability: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("observability: listen: %v", err)
	}
	fmt.Printf("ready addr=%s\n", ln.Addr().String())
	if err := srv.StartWithListener(ln); err != nil {
		log.Fatalf("observability: start: %v", err)
	}
}
