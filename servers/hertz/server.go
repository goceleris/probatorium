// Command hertz serves the probatorium contract endpoints with the
// CloudWeGo Hertz framework (netpoll-backed transport on Linux). Two
// engine modes: h1 (plain) and h2c (Hertz native H2C via
// hertz-contrib/http2/factory).
//
// Hertz drives its own listener and signal handling via Spin(); the
// graceful-shutdown hook is registered before Spin so SIGTERM unwinds
// cleanly.
package main

import (
	"context"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/common/config"
	"github.com/hertz-contrib/http2/factory"

	"github.com/goceleris/probatorium/servers/common"
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	engine := flag.String("engine", "h1", "runtime engine: h1 or h2c")
	flag.Parse()

	useH2C := *engine == "h2c"

	opts := []config.Option{
		server.WithHostPorts(*bind),
		server.WithDisablePrintRoute(true),
	}
	if useH2C {
		opts = append(opts, server.WithH2C(true))
	}
	h := server.New(opts...)
	if useH2C {
		h.AddProtocol("h2", factory.NewServerFactory())
	}
	registerRoutes(h)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("hertz: signal received, shutting down")
		_ = h.Shutdown(context.Background())
	}()

	h.Spin()
}

func registerRoutes(h *server.Hertz) {
	h.GET("/", func(_ context.Context, ctx *app.RequestContext) {
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("Hello, World!")
	})
	h.GET("/json", func(_ context.Context, ctx *app.RequestContext) {
		ctx.SetContentType("application/json")
		ctx.Response.SetBody([]byte(`{"message":"Hello, World!"}`))
	})
	h.GET("/json-1k", func(_ context.Context, ctx *app.RequestContext) {
		ctx.SetContentType("application/json")
		ctx.Response.SetBody(common.JSON1KPayload())
	})
	h.GET("/json-64k", func(_ context.Context, ctx *app.RequestContext) {
		ctx.SetContentType("application/json")
		ctx.Response.SetBody(common.JSON64KPayload())
	})
	h.GET("/users/:id", func(_ context.Context, ctx *app.RequestContext) {
		id := ctx.Param("id")
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("User ID: " + id)
	})
	h.POST("/upload", func(_ context.Context, ctx *app.RequestContext) {
		_ = ctx.Request.Body()
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("OK")
	})
}
