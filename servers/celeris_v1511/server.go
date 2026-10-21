// Command celeris_v1511 is the BASELINE bench server: the same contract
// endpoints, driver / middleware / streaming routes and -engine flag values
// as servers/celeris, built against the RELEASED celeris v1.5.11 instead of
// the celeris main the harness pins. It exists so a bench-full run can put
// the "same-run interleaved bench vs the v1.5.x baseline" in ONE dispatch
// (maintainer decision U3: comparator = the v1.5.11 tag) instead of
// comparing against a run from another day.
//
// This module is a FROZEN fork of servers/celeris. go.mod pins
// github.com/goceleris/celeris v1.5.11 and must stay there: it is not
// listed in .github/dependabot.yml's gomod directories on purpose, and
// servers/baseline_test.go pins the version and its go.sum hash. Do not
// "repin to latest" here; the main module is servers/celeris. When
// servers/celeris gains a route, servers/baseline_test.go
// (TestBaselineRoutesMatchTheMainModule) fails until the same route is
// mirrored here (or the route is listed as unsupported by v1.5.11 with the
// reason).
//
// Differences from servers/celeris, all deliberate:
//
//   - this header; go.mod (module path, the celeris pin, no other change);
//   - debugvars_test.go does not require the SEND_ZC engine counters
//     (ZCSendsSubmitted, ZCNotifs, InlineBytes, RingBytes): v1.5.11's
//     engine.EngineMetrics does not have them, and the observer records
//     the absent fields as absent, not 0 (debugvars.go says so);
//   - nothing else: every Config field set below exists in v1.5.11, so the
//     main module's server.go compiles against the tag unchanged.
//
// Nine runtime configurations are selected by -engine, the same names the
// registry's baseline-celeris-v1511-* columns pass (servers/baseline.go registers
// the eight that bench-full benchmarks for the loop engines; celeris-std-h1
// has no baseline column). The handler set mirrors servers/common.Endpoints.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goceleris/celeris"

	"github.com/goceleris/probatorium/servers/common"
)

// engineSpec is the celeris-side knob set selected by -engine.
type engineSpec struct {
	engineType celeris.EngineType
	protocol   celeris.Protocol
	async      bool
}

// engineSpecs maps every probatorium cell-column name to its celeris
// runtime config. Adding a column means a new entry here AND a sister
// entry in the registry's Adapter map.
var engineSpecs = map[string]engineSpec{
	"iouring-h1-async": {
		engineType: celeris.IOUring,
		protocol:   celeris.HTTP1,
		async:      true,
	},
	"iouring-auto+upg-async": {
		engineType: celeris.IOUring,
		// Auto exposes both H1 and H2C; the engine accepts the H1→H2C
		// Upgrade handshake implicitly when Protocol=Auto.
		protocol: celeris.Auto,
		async:    true,
	},
	"epoll-h1-sync": {
		engineType: celeris.Epoll,
		protocol:   celeris.HTTP1,
		async:      false,
	},
	"std-h1": {
		engineType: celeris.Std,
		protocol:   celeris.HTTP1,
		async:      false,
	},
	// Adaptive meta-engine (the v1.5.x focus): starts epoll, promotes new
	// conns to io_uring under sustained load. h1 + auto(h2c) variants.
	"adaptive-h1-async": {
		engineType: celeris.Adaptive,
		protocol:   celeris.HTTP1,
		async:      true,
	},
	"adaptive-auto+upg-async": {
		engineType: celeris.Adaptive,
		protocol:   celeris.Auto,
		async:      true,
	},
	// Engine × sync/async grid completers so the matrix can isolate the
	// sync-vs-async axis on a fixed engine (and h2c on epoll, which the
	// original 4 columns could not express).
	"epoll-h1-async": {
		engineType: celeris.Epoll,
		protocol:   celeris.HTTP1,
		async:      true,
	},
	"epoll-auto+upg-async": {
		engineType: celeris.Epoll,
		protocol:   celeris.Auto,
		async:      true,
	},
	"iouring-h1-sync": {
		engineType: celeris.IOUring,
		protocol:   celeris.HTTP1,
		async:      false,
	},
}

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	engineName := flag.String("engine", "iouring-h1-async", "runtime engine (see source for the canonical 4)")
	flag.Parse()

	spec, ok := engineSpecs[*engineName]
	if !ok {
		log.Fatalf("celeris: unknown -engine %q", *engineName)
	}

	srv := celeris.New(celeris.Config{
		Addr:            *bind,
		Engine:          spec.engineType,
		Protocol:        spec.protocol,
		AsyncHandlers:   spec.async,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	})
	registerRoutes(srv)

	// lifetime bounds background goroutines owned by the chain middleware
	// (ratelimit's eviction sweeper); cancelled on shutdown so repeat
	// adapter processes don't leak goroutines.
	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()

	// Phase-2 routes: driver-backed (/db, /cache, /mc, /session), the four
	// middleware chains (/chain/<stack>/{json,upload}), and the streaming
	// surface (WS /ws, SSE /events). The streaming Hub/Broker background loops
	// are bounded by lifetime so repeat cells don't leak goroutines.
	clients := mountDriverHandlers(srv)
	mountChainHandlers(srv, lifetime)
	streaming := mountStreamingHandlers(srv, lifetime)

	// The observer's /debug/vars, on a side listener (debugvars.go).
	_, stopDebugVars := startDebugVars(srv)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("celeris: signal received, shutting down")
		stopDebugVars()
		cancelLifetime()
		streaming.close()
		clients.close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	if err := srv.Start(); err != nil {
		log.Fatalf("celeris: start: %v", err)
	}
}

func registerRoutes(srv *celeris.Server) {
	srv.GET("/", func(c *celeris.Context) error {
		return c.String(200, "Hello, World!")
	})
	srv.GET("/json", func(c *celeris.Context) error {
		return c.Blob(200, "application/json", []byte(`{"message":"Hello, World!"}`))
	})
	srv.GET("/json-1k", func(c *celeris.Context) error {
		return c.Blob(200, "application/json", common.JSON1KPayload())
	})
	srv.GET("/json-8k", func(c *celeris.Context) error {
		return c.Blob(200, "application/json", common.JSON8KPayload())
	})
	srv.GET("/json-16k", func(c *celeris.Context) error {
		return c.Blob(200, "application/json", common.JSON16KPayload())
	})
	srv.GET("/json-64k", func(c *celeris.Context) error {
		return c.Blob(200, "application/json", common.JSON64KPayload())
	})
	srv.GET("/users/:id", func(c *celeris.Context) error {
		id := c.Param("id")
		return c.String(200, "User ID: %s", id)
	})
	srv.POST("/upload", func(c *celeris.Context) error {
		_ = c.Body()
		return c.String(200, "OK")
	})
}
