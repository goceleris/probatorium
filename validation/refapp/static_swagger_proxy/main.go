// Command static_swagger_proxy is the static + swagger + proxy
// validation refapp: celeris with three middlewares mounted that
// cover the last untested middleware band per probatorium#103 +
// #112.
//
// Component coverage:
//
//   - middleware/static: serves a small in-binary embed.FS so the
//     refapp is portable across cluster hosts (no disk-mount
//     assumptions). Walker exercises happy-path + path-traversal
//     rejection.
//   - middleware/swagger: serves a tiny OpenAPI 3.0 spec at
//     /docs/spec + the Swagger UI at /docs/. Walker scrapes the
//     spec endpoint and asserts the response is valid JSON.
//   - middleware/proxy: X-Forwarded-For / X-Real-IP trust chain.
//     Routes echo back the resolved client IP so the walker can
//     check that untrusted upstream headers are correctly
//     rejected (the refapp configures 127.0.0.1 as the only
//     trusted proxy — loopback-only by design).
//
// On startup the refapp prints the canonical ready line:
//
//	ready addr=<bind-addr>
package main

import (
	"context"
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/proxy"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/static"
	"github.com/goceleris/celeris/middleware/swagger"
)

//go:embed static/*
var staticFS embed.FS

// openAPISpec is a minimal OpenAPI 3.0 spec the swagger middleware
// serves. Walker validates that the response is well-formed JSON
// and reachable at the configured BasePath/spec URL.
const openAPISpec = `{
  "openapi": "3.0.0",
  "info": {
    "title": "static_swagger_proxy validation refapp",
    "version": "1.0.0"
  },
  "paths": {
    "/api/whoami": {
      "get": {
        "summary": "Echo the resolved ClientIP from the proxy chain.",
        "responses": {"200": {"description": "client IP"}}
      }
    }
  }
}`

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	engineFlag := flag.String("engine", "auto", "engine: iouring | epoll | std | adaptive | auto")
	flag.Parse()

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

	srv.Use(recovery.New())
	srv.Use(requestid.New())

	// proxy middleware — trusts loopback only. Walker traffic from
	// outside 127.0.0.0/8 will be ignored (any X-Forwarded-For from
	// an untrusted client is dropped; ClientIP falls back to the
	// raw socket peer address). Tier 1 walker checks the
	// loopback-only path; the Tier 1 hostile-input slice fires
	// XFF with malformed values and asserts the middleware
	// doesn't panic.
	srv.Use(proxy.New(proxy.Config{
		TrustedProxies: []string{"127.0.0.0/8", "::1"},
		TrustedHeaders: []string{"x-forwarded-for", "x-real-ip"},
	}))

	// static middleware — serves the embedded FS at /static/*. The
	// embed prefix is "static/" (relative to this main.go) so the
	// Prefix stripping below maps /static/foo.txt → static/foo.txt
	// in the FS.
	subFS, err := fs.Sub(staticFS, "static")
	if err != nil {
		log.Fatalf("static_swagger_proxy: fs.Sub: %v", err)
	}
	srv.Use(static.New(static.Config{
		FS:     subFS,
		Prefix: "/static",
	}))

	// swagger middleware — serves UI at /docs/ + spec at /docs/spec.
	srv.Use(swagger.New(swagger.Config{
		BasePath:    "/docs",
		SpecContent: []byte(openAPISpec),
	}))

	// /healthz — I-CONN-1 sentinel.
	srv.GET("/healthz", func(c *celeris.Context) error {
		return c.JSON(http.StatusOK, map[string]string{"ok": "true"})
	})

	// /api/whoami — echo the resolved ClientIP. Walker sends
	// requests with and without X-Forwarded-For and asserts the
	// untrusted variant doesn't leak the spoofed IP into the
	// resolved value.
	srv.GET("/api/whoami", func(c *celeris.Context) error {
		return c.JSON(http.StatusOK, map[string]any{
			"client_ip":       c.ClientIP(),
			"x_forwarded_for": c.Header("x-forwarded-for"),
			"x_real_ip":       c.Header("x-real-ip"),
		})
	})

	// Placeholder routes so celeris's router doesn't 404 before
	// the static + swagger middlewares get a chance to intercept.
	// The middlewares short-circuit any matching path.
	srv.GET("/static/*filepath", func(c *celeris.Context) error {
		return c.String(http.StatusNotFound, "static middleware did not intercept: %s", c.Path())
	})
	srv.GET("/docs", func(c *celeris.Context) error {
		return c.String(http.StatusNotFound, "swagger middleware did not intercept: %s", c.Path())
	})
	srv.GET("/docs/*filepath", func(c *celeris.Context) error {
		return c.String(http.StatusNotFound, "swagger middleware did not intercept: %s", c.Path())
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("static_swagger_proxy: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// Suppress unused warning for strings — referenced by inline
	// path checks the static middleware does for traversal.
	_ = strings.TrimPrefix

	fmt.Printf("ready addr=%s\n", *bind)
	if err := srv.Start(); err != nil {
		log.Fatalf("static_swagger_proxy: start: %v", err)
	}
}
