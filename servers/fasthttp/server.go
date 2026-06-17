// Command fasthttp serves the probatorium contract endpoints with the
// valyala/fasthttp library. fasthttp speaks HTTP/1.1 only — no engine
// flag is honoured, the binary always runs in H1 mode.
//
// The adapter's capability manifest (Drivers + Middleware, no WS/TLS)
// lives in the shared registry at servers/servers.go — the fasthttp-h1
// Adapter entry — not in this package; the runner gates scenario waves
// from that single source of truth.
//
// fasthttp is router-less, so the contract endpoints are served by a
// hand-rolled dispatch switch (the hot path) and the Phase-2 driver /
// chain routes are registered on a small route table the dispatch
// consults on a miss. Exact paths match a map; dynamic-parameter routes
// (e.g. /db/user/:id) register a "/db/user/" prefix and the handler
// slices the parameter off itself.
//
// Tuning knobs are baked into the fasthttp.Server struct here:
//
//   - DisableHeaderNamesNormalizing — leaves header names exactly as
//     written; saves a per-header pass through the canonicaliser when
//     downstream code does its own case-insensitive lookup.
//   - ReadBufferSize / WriteBufferSize at 16 KiB — matches the loadgen
//     side defaults so a single 4 KiB POST fits without growth.
//   - MaxRequestBodySize at 100 MiB — generous so /upload and the chain
//     upload routes are bounded by the per-stack bodylimit middleware,
//     not by the transport ceiling.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"

	"github.com/valyala/fasthttp"

	"github.com/goceleris/probatorium/servers/common"
)

const (
	readBufferSize     = 16 * 1024
	writeBufferSize    = 16 * 1024
	maxRequestBodySize = 100 << 20
)

// Server holds the Phase-2 route table the chain / driver handlers
// register into. The six contract endpoints are served directly by
// dispatch; extras and extraPrefixes carry the driver and chain routes.
type Server struct {
	// extras maps "METHOD path" to a handler for exact-match routes.
	extras map[string]fasthttp.RequestHandler
	// extraPrefixes holds (method, prefix) routes for dynamic path
	// parameters; prefix MUST end in "/" and matches any path under it
	// once the exact lookup misses.
	extraPrefixes []routePrefix
}

// routePrefix pairs a method with a path prefix and its handler.
type routePrefix struct {
	method  string
	prefix  string
	handler fasthttp.RequestHandler
}

func newServer() *Server {
	return &Server{extras: make(map[string]fasthttp.RequestHandler)}
}

// MountNative registers a native fasthttp handler under (method, path).
// A path ending in "/" (other than "/") becomes a prefix route; any
// other path is an exact match. Registration happens once at startup
// before Serve, so no locking is needed.
func (s *Server) MountNative(method, path string, h fasthttp.RequestHandler) {
	m := strings.ToUpper(method)
	if strings.HasSuffix(path, "/") && path != "/" {
		s.extraPrefixes = append(s.extraPrefixes, routePrefix{method: m, prefix: path, handler: h})
		return
	}
	s.extras[m+" "+path] = h
}

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	// -engine accepted for symmetry with the other adapters; only "h1"
	// is supported (fasthttp is HTTP/1.1-only).
	_ = flag.String("engine", "h1", "runtime engine: h1 (only mode supported)")
	flag.Parse()

	s := newServer()
	mountChainHandlers(s)
	mountDriverHandlers(s)

	srv := &fasthttp.Server{
		Handler:                       s.dispatch,
		Name:                          "probatorium-fasthttp",
		DisableHeaderNamesNormalizing: true,
		ReadBufferSize:                readBufferSize,
		WriteBufferSize:               writeBufferSize,
		MaxRequestBodySize:            maxRequestBodySize,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("fasthttp: signal received, shutting down")
		_ = srv.Shutdown()
		closeDrivers()
	}()

	log.Printf("fasthttp: listening on %s (engine=h1)", *bind)
	if err := srv.ListenAndServe(*bind); err != nil {
		log.Fatalf("fasthttp: serve %s: %v", *bind, err)
	}
}

// dispatch serves the contract endpoints inline (the hot path) and falls
// through to the Phase-2 route table for the driver / chain routes.
func (s *Server) dispatch(ctx *fasthttp.RequestCtx) {
	path := string(ctx.Path())
	method := string(ctx.Method())

	switch {
	case method == "GET" && path == "/":
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("Hello, World!")
		return
	case method == "GET" && path == "/json":
		ctx.SetContentType("application/json")
		ctx.SetBodyString(`{"message":"Hello, World!"}`)
		return
	case method == "GET" && path == "/json-1k":
		ctx.SetContentType("application/json")
		ctx.SetBody(common.JSON1KPayload())
		return
	case method == "GET" && path == "/json-8k":
		ctx.SetContentType("application/json")
		ctx.SetBody(common.JSON8KPayload())
		return
	case method == "GET" && path == "/json-16k":
		ctx.SetContentType("application/json")
		ctx.SetBody(common.JSON16KPayload())
		return
	case method == "GET" && path == "/json-64k":
		ctx.SetContentType("application/json")
		ctx.SetBody(common.JSON64KPayload())
		return
	case method == "GET" && len(path) > 7 && path[:7] == "/users/":
		id := path[7:]
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("User ID: " + id)
		return
	case method == "POST" && path == "/upload":
		_ = ctx.PostBody()
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("OK")
		return
	}

	if h, ok := s.extras[method+" "+path]; ok {
		h(ctx)
		return
	}
	for _, p := range s.extraPrefixes {
		if p.method == method && strings.HasPrefix(path, p.prefix) {
			p.handler(ctx)
			return
		}
	}

	ctx.SetStatusCode(fasthttp.StatusNotFound)
	ctx.SetBodyString("Not Found")
}
