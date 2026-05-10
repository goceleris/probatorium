// Command stdhttp serves the probatorium contract endpoints with Go's
// net/http standard library.
//
// Three runtime modes are selected via -engine:
//
//   - h1     — plain HTTP/1.1.
//   - h2c    — HTTP/2 cleartext only (rejects HTTP/1.x).
//   - hybrid — HTTP/1.1 plus H2C upgrade. Falls through to H1 for
//     clients that don't speak H2.
//
// Modes map directly onto `http.Server.Protocols` (Go 1.24+):
// SetHTTP1 + SetUnencryptedHTTP2. The deprecated `h2c.NewHandler`
// wrapper is no longer needed.
//
// The contract endpoints are served from servers/common (Endpoints +
// helpers) so the body bytes match every other adapter exactly. Path
// parameters use ServeMux's 1.22+ pattern syntax ("/users/{id}") so
// the router stays inside the standard library.
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
	"strconv"
	"syscall"
	"time"

	"github.com/goceleris/probatorium/servers/common"
)

// runtimeEngine names the mode selected by -engine.
type runtimeEngine string

const (
	engineH1     runtimeEngine = "h1"
	engineH2C    runtimeEngine = "h2c"
	engineHybrid runtimeEngine = "hybrid"
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	engine := flag.String("engine", string(engineH1), "runtime engine: h1, h2c, or hybrid")
	flag.Parse()

	mux := http.NewServeMux()
	registerRoutes(mux)

	srv := &http.Server{
		Addr:           *bind,
		Handler:        mux,
		MaxHeaderBytes: 1 << 20,
	}
	p := new(http.Protocols)
	switch runtimeEngine(*engine) {
	case engineH1:
		p.SetHTTP1(true)
	case engineH2C:
		// h2c-only: clients that send HTTP/1.x get rejected at protocol
		// negotiation. This is the strict interpretation of the mode.
		p.SetUnencryptedHTTP2(true)
	case engineHybrid:
		p.SetHTTP1(true)
		p.SetUnencryptedHTTP2(true)
	default:
		log.Fatalf("stdhttp: unknown -engine %q (want h1|h2c|hybrid)", *engine)
	}
	srv.Protocols = p

	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("stdhttp: listen %s: %v", *bind, err)
	}

	errCh := make(chan error, 1)
	go func() { errCh <- srv.Serve(ln) }()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGTERM, syscall.SIGINT)

	select {
	case sig := <-sigCh:
		log.Printf("stdhttp: received %s, shutting down", sig)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	case err := <-errCh:
		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("stdhttp: serve: %v", err)
		}
	}
}

// registerRoutes wires every entry in common.Endpoints to a handler on
// mux. Static-body endpoints write the pre-baked bytes; /users/{id}
// echoes the path param; /upload drains the body and replies "OK".
func registerRoutes(mux *http.ServeMux) {
	for _, ep := range common.Endpoints {
		ep := ep // bind for closure
		switch ep.Path {
		case "/users/:id":
			mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
				id := r.PathValue("id")
				common.WritePath(w, id)
			})
		case "/upload":
			mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				common.WriteBody(w)
			})
		default:
			pattern := ep.Method + " " + ep.Path
			body := ep.ResponseBody
			contentType := ep.ResponseContentType
			mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", contentType)
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(body)
			})
		}
	}
}
