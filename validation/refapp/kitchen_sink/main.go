// Command kitchen_sink is the kitchen-sink validation refapp:
// celeris with as many stateless middlewares wired as possible, so
// the Tier 1 walker traffic exercises every middleware's request/
// response path in a single soak.
//
// Coverage per probatorium#103:
//   - recovery       (always-on)
//   - requestid      (per-req X-Request-Id)
//   - secure         (HSTS / X-Frame-Options / X-Content-Type-Options)
//   - cors           (preflight + origin)
//   - bodylimit      (request body size cap)
//   - etag           (If-None-Match → 304)
//   - cache          (in-memory response cache)
//   - methodoverride (X-HTTP-Method-Override)
//   - rewrite        (path rewrites)
//   - redirect       (configured 30x)
//   - healthcheck    (/healthz, /livez)
//   - ratelimit      (token bucket; permissive)
//   - timeout        (per-handler deadline)
//   - circuitbreaker (downstream-fault gate)
//   - idempotency    (POST replay protection)
//   - singleflight   (dedupe concurrent identical requests)
//   - basicauth      (gated /admin/* — Tier 1 walker not authed here)
//
// Auth-cookie + WS/SSE are in the existing auth_session_ratelimit
// refapp; JWT in the (planned) auth_jwt_csrf refapp. Drivers (pg,
// redis, memcached) are in the driver_* refapps. Keeps each refapp
// focused on a coherent middleware slice while covering ~25 of the
// ~30 user-facing middlewares total.
//
// It is also the only refapp served over celeris Protocol Auto, which
// is what gives the Tier 1 h2c-churn slice an upgrade to complete —
// see the Config literal below and probatorium#279.
//
// On startup the refapp prints the canonical ready line:
//
//	ready addr=<bind-addr>
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/basicauth"
	"github.com/goceleris/celeris/middleware/bodylimit"
	"github.com/goceleris/celeris/middleware/cache"
	"github.com/goceleris/celeris/middleware/circuitbreaker"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/etag"
	"github.com/goceleris/celeris/middleware/healthcheck"
	"github.com/goceleris/celeris/middleware/idempotency"
	"github.com/goceleris/celeris/middleware/methodoverride"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/redirect"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/rewrite"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/singleflight"
	"github.com/goceleris/celeris/middleware/timeout"
	"github.com/goceleris/probatorium/validation/refapp/internal/debugvars"
)

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	rps := flag.Float64("rps", 5000, "ratelimit RPS per key (permissive for walker traffic)")
	burst := flag.Int("burst", 1000, "ratelimit burst per key")
	engineFlag := flag.String("engine", "auto", "engine: iouring | epoll | std | adaptive | auto")
	workersFlag := flag.Int("workers", 0, "io worker count (0 = celeris default GOMAXPROCS); celeris requires >=2 if set")
	flag.Parse()

	engineType := resolveEngine(*engineFlag)

	dv := debugvars.New() // /debug/vars + /debug/pprof for the validator's property loop
	srv := dv.NewServer(celeris.Config{
		Addr:    *bind,
		Engine:  engineType,
		Workers: *workersFlag,
		// Auto, not HTTP1 — kitchen_sink is the one refapp that answers
		// the HTTP/1.1 → h2c upgrade, so the Tier 1 h2c-churn slice has
		// a server to churn (probatorium#279). Under HTTP1 celeris
		// infers EnableH2Upgrade=false and all three churn modes
		// degenerate into a plain declined GET: the v1.5.11 nightly sent
		// 172,656 preambles across 48 cells for zero 101s, so h2c_hang —
		// the counter that caught celeris#470 — judged a path no engine
		// ever entered. Auto rather than HTTP1 + EnableH2Upgrade=true
		// because the std engine reads Protocol alone (it wraps the
		// handler in x/net/http2/h2c only for Auto/H2C), so the flag
		// would leave 1 of the 3 matrix engines still declining.
		//
		// Only this refapp switches: it mounts no WS/SSE routes
		// (auth_session_ratelimit owns those) so the upgrade can't
		// interact with the streaming detach paths, and the other seven
		// staying on HTTP1 keeps a control group — a new failure
		// confined to kitchen_sink cells is attributable on sight.
		Protocol:        celeris.Auto,
		AsyncHandlers:   true,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	})

	// Middleware order matters. Outer-most is the one wrapping the
	// whole chain; inner-most runs closest to the handler. Order
	// rationale below per pair.

	// recovery: outermost so a panic in ANY middleware below is
	// recovered. Same as auth_session_ratelimit.
	// Recovery middleware logger: explicit io.Discard sink, NOT
	// slog.Default(). The stdlib default routes through Go's text
	// handler whose defaultHandler mutex serializes a blocking
	// os.Stderr.Write across every conn + worker; under iouring/epoll's
	// per-conn async-dispatch model that stderr lock is held inside
	// cs.detachMu (around ProcessH1), gating the worker thread and
	// letting concurrent slowloris header-deadlines slip past.
	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv.Use(recovery.New(recovery.Config{Logger: dv.RecoveryLogger(discardLog)}))
	// requestid: stamp X-Request-Id before anything else logs.
	srv.Use(requestid.New())
	// secure: emit security headers on EVERY response.
	srv.Use(secure.New())
	// cors: preflight handling for every endpoint; permissive for
	// walker traffic (CheckOrigin in the actual middleware accepts
	// all since Tier 1 walker doesn't send Origin).
	srv.Use(cors.New())
	// bodylimit: reject bodies > 1 MiB. Walker rarely sends bodies
	// of that size; the limit exists so a bug ALLOWING oversized
	// bodies would surface.
	srv.Use(bodylimit.New(bodylimit.Config{MaxBytes: 1 << 20}))
	// methodoverride: allow X-HTTP-Method-Override on POST.
	srv.Use(methodoverride.New())
	// rewrite: rewrite `/v1/<path>` → `/<path>` so the walker can
	// hit either form. Exercises the path-mutation path.
	srv.Use(rewrite.New(rewrite.Config{
		Rules: []rewrite.Rule{
			{Pattern: "^/v1/(.*)$", Replacement: "/$1"},
		},
	}))
	// redirect: RemoveTrailingSlashRedirect strips trailing slashes
	// when present. The walker's paths don't end in `/` so this is
	// a no-op for normal traffic — exercises the middleware's
	// path-inspection path without 301'ing every request.
	srv.Use(redirect.RemoveTrailingSlashRedirect())
	// healthcheck: `/healthz` + `/livez` always return 200 +
	// short-circuit the rest of the chain.
	srv.Use(healthcheck.New())
	// ratelimit: token bucket. Permissive so the walker doesn't
	// saturate the budget. The key function is the middleware's own
	// default, spelled out so the shadow bucket below is keyed
	// identically by construction.
	rlKey := func(c *celeris.Context) string {
		if ip := c.ClientIP(); ip != "" {
			return ip
		}
		return c.RemoteAddr()
	}
	rlShadow := debugvars.NewTokenShadow(*rps, *burst)
	srv.Use(ratelimit.New(ratelimit.Config{
		RPS:     *rps,
		Burst:   *burst,
		KeyFunc: rlKey,
		ErrorHandler: func(c *celeris.Context, err error) error {
			dv.RateLimitRejected()
			return err // unchanged 429; the handler only counts
		},
	}))
	// Installed immediately after the limiter, so reaching it means the
	// limiter admitted the request. The shadow re-derives the bound the
	// token bucket promises (burst + RPS*elapsed, per key, with slack);
	// an admission it cannot pay for is I-MW-RATELIMIT's token violation.
	// Same oracle as auth_session_ratelimit, now in six more cells.
	srv.Use(func(c *celeris.Context) error {
		dv.RateLimitAdmitted()
		if !rlShadow.Admit(rlKey(c), time.Now()) {
			dv.RecordRateLimitTokenViolation()
		}
		return c.Next()
	})
	dv.Declare("I-MW-RATELIMIT")
	// timeout: per-handler deadline. Handlers below take <100ms
	// normally; if a future bug introduces a hang the middleware
	// returns 504.
	srv.Use(timeout.New(timeout.Config{Timeout: 5 * time.Second}))
	// circuitbreaker: opens after the error ratio exceeds Threshold
	// inside WindowSize. Permissive thresholds: the walker's mix
	// shouldn't trip it; if a downstream fault test does, the
	// breaker returns 503.
	srv.Use(circuitbreaker.New(circuitbreaker.Config{
		Threshold:      0.95, // 95% errors in window before tripping
		MinRequests:    1000,
		WindowSize:     30 * time.Second,
		CooldownPeriod: 5 * time.Second,
		HalfOpenMax:    10,
	}))
	// idempotency: POST replay protection keyed by Idempotency-Key
	// header. Walker doesn't currently send the header so this is
	// effectively a no-op for normal traffic, but the middleware
	// runs and exercises its hashing path.
	srv.Use(idempotency.New())
	// singleflight: dedupes concurrent identical requests. With
	// the walker's randomised RPC paths the dedup is rare but the
	// middleware runs on every request and exercises its key-
	// fingerprint path.
	srv.Use(singleflight.New())

	// Routes — small surface, intentionally varied so middlewares
	// have something to act on.

	// /api/echo — returns a small JSON body. Exercises etag (per-
	// route below), cache (per-route below), compress (when
	// negotiated by client), and the response-shaping middlewares.
	srv.GET("/api/echo",
		etag.New(),
		cache.New(),
		func(c *celeris.Context) error {
			return c.JSON(200, map[string]any{
				"msg":  "echo",
				"now":  time.Now().UTC().Format(time.RFC3339Nano),
				"path": c.Path(),
			})
		},
	)

	// /api/short — handler completes immediately. Exercises the
	// happy path through every middleware.
	srv.GET("/api/short", func(c *celeris.Context) error {
		return c.String(200, "ok")
	})

	// /api/post — accepts JSON, echoes. Exercises bodylimit on
	// requests + idempotency on writes.
	srv.POST("/api/post", func(c *celeris.Context) error {
		return c.JSON(200, map[string]any{"received": len(c.Body())})
	})

	// /admin/* — gated by basicauth. Walker is unauthed → 401.
	// Exercises the basicauth reject path.
	admin := srv.Group("/admin",
		basicauth.New(basicauth.Config{
			Users: map[string]string{"admin": "supersecret"},
		}),
	)
	admin.GET("/status", func(c *celeris.Context) error {
		return c.JSON(200, map[string]any{"admin": true})
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("kitchen_sink: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("kitchen_sink: listen: %v", err)
	}
	fmt.Printf("ready addr=%s\n", ln.Addr().String())
	if err := srv.StartWithListener(ln); err != nil {
		log.Fatalf("kitchen_sink: start: %v", err)
	}
}
