// Command iris serves the probatorium contract endpoints with the Iris
// framework (kataras/iris/v12). Two engine modes: h1 (plain) and h2c
// (h2c.NewHandler-wrapped). Iris expresses path params as
// "{id:string}".
package main

import (
	"context"
	"flag"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/kataras/iris/v12"

	"github.com/goceleris/probatorium/servers/common"
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	engine := flag.String("engine", "h1", "runtime engine: h1 or h2c")
	flag.Parse()

	app := iris.New()
	app.Logger().SetLevel("warn")
	registerRoutes(app)

	// Phase-2 routes: driver round-trips (PG/Redis/memcached/session) and
	// the four middleware chains. Driver clients are opened lazily from
	// env-configured endpoints; unset services degrade to 503.
	dc := newDriverClients()
	defer closeDriverClients(dc)
	mountDriverHandlers(app, dc)
	mountChainHandlers(app)

	if err := app.Build(); err != nil {
		log.Fatalf("iris: build: %v", err)
	}

	srv := &http.Server{Addr: *bind, Handler: app}
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
		log.Fatalf("iris: listen %s: %v", *bind, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("iris: serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func registerRoutes(app *iris.Application) {
	app.Get("/", func(ctx iris.Context) {
		ctx.ContentType("text/plain")
		_, _ = ctx.WriteString("Hello, World!")
	})
	app.Get("/json", func(ctx iris.Context) {
		ctx.ContentType("application/json")
		_, _ = ctx.Write([]byte(`{"message":"Hello, World!"}`))
	})
	app.Get("/json-1k", func(ctx iris.Context) {
		ctx.ContentType("application/json")
		_, _ = ctx.Write(common.JSON1KPayload())
	})
	app.Get("/json-8k", func(ctx iris.Context) {
		ctx.ContentType("application/json")
		_, _ = ctx.Write(common.JSON8KPayload())
	})
	app.Get("/json-16k", func(ctx iris.Context) {
		ctx.ContentType("application/json")
		_, _ = ctx.Write(common.JSON16KPayload())
	})
	app.Get("/json-64k", func(ctx iris.Context) {
		ctx.ContentType("application/json")
		_, _ = ctx.Write(common.JSON64KPayload())
	})
	app.Get("/users/{id:string}", func(ctx iris.Context) {
		id := ctx.Params().Get("id")
		ctx.ContentType("text/plain")
		_, _ = ctx.WriteString("User ID: " + id)
	})
	app.Post("/upload", func(ctx iris.Context) {
		_, _ = ctx.GetBody()
		ctx.ContentType("text/plain")
		_, _ = ctx.WriteString("OK")
	})
}
