// Command echo serves the probatorium contract endpoints with the Echo
// framework (labstack/echo/v4). Two engine modes: h1 (plain) and h2c
// (h2c.NewHandler-wrapped).
package main

import (
	"context"
	"flag"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/labstack/echo/v4"

	"github.com/goceleris/probatorium/servers/common"
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	engine := flag.String("engine", "h1", "runtime engine: h1 or h2c")
	pgDSN := flag.String("postgres-dsn", envOr("PROBATORIUM_PG_DSN", ""), "postgres DSN for the driver-pg route")
	redisAddr := flag.String("redis-addr", envOr("PROBATORIUM_REDIS_ADDR", ""), "redis host:port for the driver-redis / session routes")
	mcAddr := flag.String("mc-addr", envOr("PROBATORIUM_MC_ADDR", ""), "memcached host:port for the driver-mc route")
	flag.Parse()

	e := echo.New()
	e.HideBanner = true
	e.HidePort = true

	dc := newDriverClients(*pgDSN, *redisAddr, *mcAddr)
	defer dc.close()

	registerRoutes(e)
	registerDriverHandlers(e, dc)
	registerChainHandlers(e)

	srv := &http.Server{Addr: *bind, Handler: e}
	// h2c mode accepts HTTP/1.1 AND HTTP/2-over-cleartext (matches the
	// pre-Go 1.24 `h2c.NewHandler` semantics).
	if *engine == "h2c" {
		p := new(http.Protocols)
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
		srv.Protocols = p
	}
	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("echo: listen %s: %v", *bind, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("echo: serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func registerRoutes(e *echo.Echo) {
	e.GET("/", func(c echo.Context) error {
		return c.String(http.StatusOK, "Hello, World!")
	})
	e.GET("/json", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/json", []byte(`{"message":"Hello, World!"}`))
	})
	e.GET("/json-1k", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/json", common.JSON1KPayload())
	})
	e.GET("/json-8k", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/json", common.JSON8KPayload())
	})
	e.GET("/json-16k", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/json", common.JSON16KPayload())
	})
	e.GET("/json-64k", func(c echo.Context) error {
		return c.Blob(http.StatusOK, "application/json", common.JSON64KPayload())
	})
	e.GET("/users/:id", func(c echo.Context) error {
		id := c.Param("id")
		return c.String(http.StatusOK, "User ID: "+id)
	})
	e.POST("/upload", func(c echo.Context) error {
		_, _ = io.Copy(io.Discard, c.Request().Body)
		return c.String(http.StatusOK, "OK")
	})
}
