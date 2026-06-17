// Command celeris serves the probatorium contract endpoints with the
// celeris HTTP engine. Four runtime configurations are exposed via
// -engine, matching the cell-columns the probatorium matrix
// distinguishes:
//
//   - iouring-h1-async        — celeris.IOUring + Protocol=HTTP1 + AsyncHandlers
//   - iouring-auto+upg-async  — celeris.IOUring + Protocol=Auto    + AsyncHandlers
//     (Auto includes implicit H1→H2C upgrade)
//   - epoll-h1-sync           — celeris.Epoll   + Protocol=HTTP1   + AsyncHandlers=false
//   - std-h1                  — celeris.Std     + Protocol=HTTP1   + AsyncHandlers=false
//
// The handler set mirrors servers/common.Endpoints; static-body cells
// write the pre-baked bytes via Context.Blob/String, /users/:id echoes
// the path param, /upload drains the body and replies "OK".
//
// Version policy: go.mod pins the celeris milestone/v1.5.0 branch by
// pseudo-version (the v1.4.15 release carries an io_uring heap-corruption
// bug class fixed only on that branch). Return to the always-latest
// release policy — re-running `go mod tidy` against the latest tag —
// once v1.5.0 ships.
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

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("celeris: signal received, shutting down")
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
