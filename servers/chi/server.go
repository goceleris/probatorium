// Command chi serves the probatorium contract endpoints with the Chi
// router. Two engine modes: h1 (plain) and h2c (h2c.NewHandler-wrapped).
//
// Chi expresses path params as "{id}" rather than the contract's
// ":id" — registerRoutes does that translation locally.
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

	"github.com/go-chi/chi/v5"

	"github.com/goceleris/probatorium/servers/common"
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	engine := flag.String("engine", "h1", "runtime engine: h1 or h2c")
	flag.Parse()

	r := chi.NewRouter()
	registerRoutes(r)

	srv := &http.Server{Addr: *bind, Handler: r}
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
		log.Fatalf("chi: listen %s: %v", *bind, err)
	}
	go func() {
		if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
			log.Fatalf("chi: serve: %v", err)
		}
	}()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	<-sig
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = srv.Shutdown(ctx)
}

func registerRoutes(r chi.Router) {
	r.Get("/", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Hello, World!"))
	})
	r.Get("/json", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"message":"Hello, World!"}`))
	})
	r.Get("/json-1k", func(w http.ResponseWriter, _ *http.Request) {
		common.WriteJSON1K(w)
	})
	r.Get("/json-64k", func(w http.ResponseWriter, _ *http.Request) {
		common.WriteJSON64K(w)
	})
	r.Get("/users/{id}", func(w http.ResponseWriter, r *http.Request) {
		id := chi.URLParam(r, "id")
		common.WritePath(w, id)
	})
	r.Post("/upload", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		common.WriteBody(w)
	})

	// Phase-2 capability routes. chi-h1/chi-h2 declare Drivers + Middleware
	// in the registry, so both are mounted unconditionally here; the driver
	// handlers self-gate to 503 when a backend is unconfigured.
	mountDriverHandlers(r)
	mountChainHandlers(r)
}
