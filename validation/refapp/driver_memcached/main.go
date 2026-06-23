// Command driver_memcached is the memcached-driven validation refapp:
// celeris with the native memcached driver wired into both session
// and ratelimit stores, plus a small handful of routes that exercise
// Set / Get / CAS + read-after-write consistency.
//
// Coverage per probatorium#103 + #110:
//
//   - driver/memcached bounded pool → I-DRV pool-cap invariant.
//   - middleware/session/memcachedstore → session round-trip via MC.
//   - middleware/ratelimit/memcachedstore → CAS-loop token-bucket
//     atomicity under concurrent traffic.
//   - Set + Get happy path on the seeded demo-key.
//   - CAS update + read-after-write consistency (I-DRV-1).
//
// Address is taken from -mc-addr or PROBATORIUM_MEMCACHED_ADDR (the
// canonical var the bench adapters and ansible/validate.yml export).
//
// On startup the refapp prints the canonical ready line:
//
//	ready addr=<bind-addr>
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/middleware/ratelimit"
	rlmc "github.com/goceleris/celeris/middleware/ratelimit/memcachedstore"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/session"
	sessmc "github.com/goceleris/celeris/middleware/session/memcachedstore"
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
	addr := flag.String("mc-addr", envOr("PROBATORIUM_MEMCACHED_ADDR", "127.0.0.1:21211"),
		"memcached host:port; env: PROBATORIUM_MEMCACHED_ADDR")
	rps := flag.Float64("rps", 5000, "ratelimit RPS per key (permissive for walker traffic)")
	burst := flag.Int("burst", 1000, "ratelimit burst per key")
	flag.Parse()

	client, err := memcached.NewClient(*addr)
	if err != nil {
		log.Fatalf("driver_memcached: client init: %v", err)
	}
	defer client.Close()

	// Initial probe: a Get with a fresh key. Returning ErrCacheMiss is
	// a successful round-trip (server is up); any other error is
	// fatal.
	if _, err := client.Get(context.Background(), "__probatorium_probe__"); err != nil && !errors.Is(err, memcached.ErrCacheMiss) {
		log.Fatalf("driver_memcached: probe: %v", err)
	}

	sstore := sessmc.New(client)
	rlStore, err := rlmc.New(client, rlmc.Options{
		RPS:   *rps,
		Burst: *burst,
	})
	if err != nil {
		log.Fatalf("driver_memcached: ratelimit store init: %v", err)
	}

	srv := celeris.New(celeris.Config{
		Addr:            *bind,
		Engine:          resolveEngine(*engineFlag),
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
	srv.Use(recovery.New(recovery.Config{Logger: discardLog}))
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

	// /healthz — round-trip probe.
	srv.GET("/healthz", func(c *celeris.Context) error {
		if _, err := client.Get(c.Context(), "__healthz__"); err != nil && !errors.Is(err, memcached.ErrCacheMiss) {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"err": err.Error()})
		}
		return c.JSON(http.StatusOK, map[string]string{"memcached": "ok"})
	})

	// GET /kv/:key.
	srv.GET("/kv/:key", func(c *celeris.Context) error {
		v, err := client.Get(c.Context(), c.Param("key"))
		if errors.Is(err, memcached.ErrCacheMiss) {
			return c.String(http.StatusNotFound, "miss")
		}
		if err != nil {
			return c.String(http.StatusInternalServerError, "%s", "get: "+err.Error())
		}
		return c.JSON(http.StatusOK, map[string]any{
			"key":   c.Param("key"),
			"value": v,
			"len":   len(v),
		})
	})

	// POST /kv/:key — Set + read-after-write check.
	srv.POST("/kv/:key", func(c *celeris.Context) error {
		k := c.Param("key")
		v := c.Query("v")
		if v == "" {
			v = "tier1"
		}
		if err := client.Set(c.Context(), k, v, 5*time.Minute); err != nil {
			return c.String(http.StatusInternalServerError, "%s", "set: "+err.Error())
		}
		got, err := client.Get(c.Context(), k)
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

	// /store/:key — exercise the memcached session store directly
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
		log.Printf("driver_memcached: signal received, shutting down")
		shCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shCancel()
		_ = srv.Shutdown(shCtx)
	}()

	fmt.Printf("ready addr=%s\n", *bind)
	if err := srv.Start(); err != nil {
		log.Fatalf("driver_memcached: start: %v", err)
	}
}
