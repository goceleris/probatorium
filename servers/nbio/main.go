// Command nbio serves the probatorium static contract on top of
// lesismal/nbio's nbhttp engine — a pure-Go, non-blocking, event-driven HTTP
// server (epoll/kqueue poller pool, not goroutine-per-connection on net/http).
// It is the second Go event-loop peer alongside gnet: where gnet hands the
// adapter a raw inbound ring buffer and the adapter framures HTTP itself,
// nbhttp ships its own non-blocking HTTP/1.x parser and drives a standard
// http.Handler, so this adapter reuses the same mux + servers/common payload
// path as the net/http adapters while the I/O underneath is fully event-loop.
//
// nbhttp speaks HTTP/1.x only on the server side (its HTTP/2 surface is
// client-oriented and not a cheap cleartext-h2c prior-knowledge server), so
// this adapter is HTTP/1.1-only and serves the static contract exclusively —
// no driver, middleware, WS, or SSE routes. The capability manifest
// (Static only) lives in the shared registry at servers/servers.go (the
// nbio-h1 Adapter entry), the single source of truth the runner gates
// scenario waves from.
//
// The -engine flag is accepted for symmetry with the other Go adapters; only
// "h1" is meaningful.
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
	"sync"
	"syscall"
	"time"

	"github.com/lesismal/nbio/nbhttp"

	"github.com/goceleris/probatorium/servers/common"
)

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	// -engine accepted for symmetry with the other adapters; only "h1" is
	// supported (nbhttp serves HTTP/1.x only on the server side).
	_ = flag.String("engine", "h1", "runtime engine: h1 (only mode supported)")
	flag.Parse()

	mux := http.NewServeMux()
	registerStatic(mux)

	// nbhttp calls Config.Listen once per address (defaulting to net.Listen
	// when nil). We wrap it so the actual bound address — crucially for the
	// ":0" kernel-assigned case — is captured from the returned listener and
	// reported on the ready line the runner scans for. Only the first
	// listener's address is recorded (there is a single Addrs entry here).
	var (
		boundOnce sync.Once
		boundAddr string
	)
	listen := func(network, addr string) (net.Listener, error) {
		ln, err := net.Listen(network, addr)
		if err != nil {
			return nil, err
		}
		boundOnce.Do(func() { boundAddr = ln.Addr().String() })
		return ln, nil
	}

	engine := nbhttp.NewEngine(nbhttp.Config{
		Network: "tcp",
		Addrs:   []string{*bind},
		Handler: mux,
		Listen:  listen,
	})

	if err := engine.Start(); err != nil {
		log.Fatalf("nbio: start %s: %v", *bind, err)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("nbio: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = engine.Shutdown(ctx)
	}()

	// The runner scans child stdout for this exact prefix before dialing.
	// Printed via the logger like the other Go adapters; the runner's scanner
	// matches the "ready addr=" token anywhere on the line.
	log.Printf("ready addr=%s", boundAddr)

	// Engine.Start returns immediately (the poller pool runs in the
	// background), so block until shutdown closes the process. Shutdown ->
	// os.Exit happens via the signal goroutine path; we park here.
	select {}
}

// registerStatic mounts the eight canonical contract endpoints from
// servers/common so this adapter satisfies the static contract byte-for-byte
// with every other adapter.
func registerStatic(mux *http.ServeMux) {
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// "GET /" matches every otherwise-unmatched path; guard so only the
		// root serves the hello body and everything else 404s.
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		writeBlob(w, "text/plain", []byte("Hello, World!"))
	})
	mux.HandleFunc("GET /json", func(w http.ResponseWriter, r *http.Request) {
		writeBlob(w, "application/json", []byte(`{"message":"Hello, World!"}`))
	})
	mux.HandleFunc("GET /json-1k", func(w http.ResponseWriter, r *http.Request) {
		writeBlob(w, "application/json", common.JSON1KPayload())
	})
	mux.HandleFunc("GET /json-8k", func(w http.ResponseWriter, r *http.Request) {
		writeBlob(w, "application/json", common.JSON8KPayload())
	})
	mux.HandleFunc("GET /json-16k", func(w http.ResponseWriter, r *http.Request) {
		writeBlob(w, "application/json", common.JSON16KPayload())
	})
	mux.HandleFunc("GET /json-64k", func(w http.ResponseWriter, r *http.Request) {
		writeBlob(w, "application/json", common.JSON64KPayload())
	})
	mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
		writeBlob(w, "text/plain", []byte("User ID: "+r.PathValue("id")))
	})
	mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		writeBlob(w, "text/plain", []byte("OK"))
	})
}

func writeBlob(w http.ResponseWriter, contentType string, body []byte) {
	h := w.Header()
	h.Set("Content-Type", contentType)
	// Set Content-Length explicitly so nbhttp emits a fixed-length body
	// instead of falling back to Transfer-Encoding: chunked. The net/http
	// adapters get Content-Length for free (their ResponseWriter buffers the
	// single Write before flushing); matching it here keeps the wire output
	// byte-for-byte identical across adapters, so a throughput delta reflects
	// engine cost, not a framing artefact.
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}
