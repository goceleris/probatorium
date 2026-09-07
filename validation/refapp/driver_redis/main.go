// Command driver_redis is the redis-driven validation refapp:
// celeris with the native redis driver wired into both session and
// ratelimit stores, plus a small handful of routes that exercise
// GET / SET / INCR + read-after-write consistency.
//
// Coverage per probatorium#103 + #110:
//
//   - driver/redis bounded pool → I-DRV pool-cap invariant.
//   - middleware/session/redisstore → session round-trip via Redis.
//   - middleware/ratelimit/redisstore → token-bucket atomicity under
//     concurrent traffic.
//   - middleware/recovery, requestid, secure → outermost chain.
//   - SET + GET happy path on the seeded demo-key.
//   - INCR + GET read-after-write consistency (I-DRV-1).
//
// Address is taken from -redis-addr or PROBATORIUM_REDIS_ADDR (the
// matrix runner / cluster orchestrator populates these from
// services.Handles.Redis.Addr).
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
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/ratelimit"
	rlredis "github.com/goceleris/celeris/middleware/ratelimit/redisstore"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/session"
	sessredis "github.com/goceleris/celeris/middleware/session/redisstore"
	"github.com/goceleris/probatorium/validation/refapp/internal/debugvars"
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	engineFlag := flag.String("engine", "auto", "engine: iouring | epoll | std | adaptive | auto")
	workersFlag := flag.Int("workers", 0, "io worker count (0 = celeris default GOMAXPROCS); celeris requires >=2 if set")
	addr := flag.String("redis-addr", envOr("PROBATORIUM_REDIS_ADDR", "127.0.0.1:63791"),
		"redis host:port; env: PROBATORIUM_REDIS_ADDR")
	rps := flag.Float64("rps", 5000, "ratelimit RPS per key (permissive for walker traffic)")
	burst := flag.Int("burst", 1000, "ratelimit burst per key")
	flag.Parse()

	client, err := redis.NewClient(*addr)
	if err != nil {
		log.Fatalf("driver_redis: client init: %v", err)
	}
	defer client.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := client.Ping(ctx); err != nil {
		log.Fatalf("driver_redis: ping: %v", err)
	}

	sstore := sessredis.New(client)
	rlStore, err := rlredis.New(ctx, client, rlredis.Options{
		RPS:   *rps,
		Burst: *burst,
	})
	if err != nil {
		log.Fatalf("driver_redis: ratelimit store init: %v", err)
	}

	dv := debugvars.New() // /debug/vars + /debug/pprof for the validator's property loop
	srv := dv.NewServer(celeris.Config{
		Addr:            *bind,
		Engine:          resolveEngine(*engineFlag),
		Workers:         *workersFlag,
		Protocol:        celeris.HTTP1,
		AsyncHandlers:   true,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	})

	// Recovery middleware logger: explicit io.Discard sink, NOT
	// slog.Default(). The stdlib default routes through Go's text
	// handler whose defaultHandler mutex serializes a blocking
	// os.Stderr.Write across every conn + worker; under iouring/epoll's
	// per-conn async-dispatch model that stderr lock is held inside
	// cs.detachMu (around ProcessH1), gating the worker thread and
	// letting concurrent slowloris header-deadlines slip past.
	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv.Use(recovery.New(recovery.Config{Logger: dv.RecoveryLogger(discardLog)}))
	srv.Use(requestid.New())
	srv.Use(secure.New())
	srv.Use(ratelimit.New(ratelimit.Config{
		Store: rlStore,
		RPS:   *rps,
		Burst: *burst,
	}))
	srv.Use(session.New(session.Config{
		Store:        sstore,
		CookieName:   "probatorium_sess",
		CookieMaxAge: session.IntPtr(86400),
	}))

	// /healthz — client health.
	srv.GET("/healthz", func(c *celeris.Context) error {
		if err := client.Ping(c.Context()); err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"err": err.Error()})
		}
		return c.JSON(http.StatusOK, map[string]string{"redis": "ok"})
	})

	// GET /kv/:key — GET happy path.
	srv.GET("/kv/:key", func(c *celeris.Context) error {
		v, err := client.DoString(c.Context(), "GET", c.Param("key"))
		if err != nil {
			return c.String(http.StatusNotFound, "miss")
		}
		return c.JSON(http.StatusOK, map[string]any{
			"key":   c.Param("key"),
			"value": v,
			"len":   len(v),
		})
	})

	// POST /kv/:key — SET + read-after-write check.
	srv.POST("/kv/:key", func(c *celeris.Context) error {
		k := c.Param("key")
		v := c.Query("v")
		if v == "" {
			v = "tier1"
		}
		if _, err := client.Do(c.Context(), "SET", k, v); err != nil {
			return c.String(http.StatusInternalServerError, "%s", "set: "+err.Error())
		}
		got, err := client.DoString(c.Context(), "GET", k)
		if err != nil {
			return c.String(http.StatusInternalServerError, "%s", "raw read: "+err.Error())
		}
		if got != v {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"err":           "read-after-write mismatch",
				"wrote":         v,
				"read":          got,
				"x-invariant":   "I-DRV-1",
				"x-invariant-h": "high",
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"key": k, "value": v})
	})

	// POST /incr/:key — INCR + read-after-write parity.
	srv.POST("/incr/:key", func(c *celeris.Context) error {
		k := c.Param("key")
		n, err := client.DoInt(c.Context(), "INCR", k)
		if err != nil {
			return c.String(http.StatusInternalServerError, "%s", "incr: "+err.Error())
		}
		got, err := client.DoString(c.Context(), "GET", k)
		if err != nil {
			return c.String(http.StatusInternalServerError, "%s", "raw read: "+err.Error())
		}
		// Read-after-write on a SHARED counter: concurrent walkers INCR
		// the same key, so GET may legitimately return MORE than this
		// request's INCR result. The sound invariant is monotonic: GET
		// must never be LESS than the value INCR returned -- that would
		// be a stale or misrouted read (a real I-DRV-1 hit). Demanding
		// equality produced a 12% false-positive rate at concurrency 30.
		gotN, perr := strconv.ParseInt(got, 10, 64)
		if perr != nil || gotN < n {
			return c.JSON(http.StatusInternalServerError, map[string]any{
				"err":           "incr read-after-write regression (GET < INCR return)",
				"incr_return":   n,
				"get_return":    got,
				"x-invariant":   "I-DRV-1",
				"x-invariant-h": "high",
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"key": k, "value": n})
	})

	// /store/:key — exercise the redis session store directly
	// (bypasses the session middleware's cookie machinery, which
	// the celeris auth_session_ratelimit refapp confirms is
	// "sid-in-body, not Set-Cookie" by design).
	srv.GET("/store/:key", func(c *celeris.Context) error {
		val, err := sstore.Get(c.Context(), c.Param("key"))
		if err != nil {
			return c.String(http.StatusNotFound, "miss")
		}
		return c.JSON(http.StatusOK, map[string]any{
			"key": c.Param("key"),
			"len": len(val),
		})
	})
	srv.POST("/store/:key", func(c *celeris.Context) error {
		k := c.Param("key")
		v := []byte(c.Query("v"))
		if len(v) == 0 {
			v = []byte("tier1")
		}
		if err := sstore.Set(c.Context(), k, v, 5*time.Minute); err != nil {
			return c.String(http.StatusInternalServerError, "%s", "set: "+err.Error())
		}
		got, err := sstore.Get(c.Context(), k)
		if err != nil {
			return c.String(http.StatusInternalServerError, "%s", "raw read: "+err.Error())
		}
		if string(got) != string(v) {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"err":           "store read-after-write mismatch",
				"x-invariant":   "I-DRV-1",
				"x-invariant-h": "high",
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"key": k, "len": len(v)})
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("driver_redis: signal received, shutting down")
		shCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shCancel()
		_ = srv.Shutdown(shCtx)
	}()

	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("driver_redis: listen: %v", err)
	}
	fmt.Printf("ready addr=%s\n", ln.Addr().String())
	if err := srv.StartWithListener(ln); err != nil {
		log.Fatalf("driver_redis: start: %v", err)
	}
}
