// Command fiber serves the probatorium contract endpoints with the
// gofiber/fiber/v2 framework (a fasthttp-based router). HTTP/1.1 only
// — fiber inherits fasthttp's no-H2 limitation. -engine is accepted
// for symmetry but only "h1" is honoured.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2"

	"github.com/goceleris/probatorium/servers/common"
)

const (
	fiberReadBufferSize  = 16 * 1024
	fiberWriteBufferSize = 16 * 1024
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	_ = flag.String("engine", "h1", "runtime engine: h1 (only mode supported)")
	flag.Parse()

	app := fiber.New(fiber.Config{
		ServerHeader:          "probatorium-fiber",
		DisableStartupMessage: true,
		Prefork:               false,
		ReadBufferSize:        fiberReadBufferSize,
		WriteBufferSize:       fiberWriteBufferSize,
	})
	registerRoutes(app)

	// Phase-2 routes. The fiber-h1 adapter declares Drivers + Middleware in
	// the registry, so the driver (/db,/cache,/mc,/session) and chain
	// (/chain/<stack>/{json,upload}) routes are always mounted; an
	// unconfigured backend answers 503 rather than 404 so the runner's
	// cell guard sees a served-but-degraded route, not a missing one.
	drivers := buildDriverState()
	mountDriverHandlers(app, drivers)
	mountChainHandlers(app)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("fiber: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = app.ShutdownWithContext(ctx)
		shutdownDriverState(drivers)
	}()

	if err := app.Listen(*bind); err != nil {
		log.Fatalf("fiber: listen %s: %v", *bind, err)
	}
}

func registerRoutes(app *fiber.App) {
	app.Get("/", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "text/plain")
		return c.SendString("Hello, World!")
	})
	app.Get("/json", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send([]byte(`{"message":"Hello, World!"}`))
	})
	app.Get("/json-1k", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send(common.JSON1KPayload())
	})
	app.Get("/json-64k", func(c *fiber.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send(common.JSON64KPayload())
	})
	app.Get("/users/:id", func(c *fiber.Ctx) error {
		id := c.Params("id")
		c.Set("Content-Type", "text/plain")
		return c.SendString("User ID: " + id)
	})
	app.Post("/upload", func(c *fiber.Ctx) error {
		_ = c.Body()
		c.Set("Content-Type", "text/plain")
		return c.SendString("OK")
	})
}
