// Command fasthttp serves the probatorium contract endpoints with the
// valyala/fasthttp library. fasthttp speaks HTTP/1.1 only — no engine
// flag is honoured, the binary always runs in H1 mode.
//
// Tuning knobs are baked into the fasthttp.Server struct here:
//
//   - DisableHeaderNamesNormalizing — leaves header names exactly as
//     written; saves a per-header pass through the canonicaliser when
//     downstream code does its own case-insensitive lookup.
//   - ReadBufferSize / WriteBufferSize at 16 KiB — matches the loadgen
//     side defaults so a single 4 KiB POST fits without growth.
package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/valyala/fasthttp"

	"github.com/goceleris/probatorium/servers/common"
)

const (
	readBufferSize  = 16 * 1024
	writeBufferSize = 16 * 1024
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	// -engine accepted for symmetry with the other adapters; only "h1"
	// is supported (fasthttp is HTTP/1.1-only).
	_ = flag.String("engine", "h1", "runtime engine: h1 (only mode supported)")
	flag.Parse()

	srv := &fasthttp.Server{
		Handler:                       handler,
		Name:                          "probatorium-fasthttp",
		DisableHeaderNamesNormalizing: true,
		ReadBufferSize:                readBufferSize,
		WriteBufferSize:               writeBufferSize,
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("fasthttp: signal received, shutting down")
		_ = srv.Shutdown()
	}()

	if err := srv.ListenAndServe(*bind); err != nil {
		log.Fatalf("fasthttp: serve %s: %v", *bind, err)
	}
}

func handler(ctx *fasthttp.RequestCtx) {
	path := string(ctx.Path())
	method := string(ctx.Method())

	switch {
	case method == "GET" && path == "/":
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("Hello, World!")
	case method == "GET" && path == "/json":
		ctx.SetContentType("application/json")
		ctx.SetBodyString(`{"message":"Hello, World!"}`)
	case method == "GET" && path == "/json-1k":
		ctx.SetContentType("application/json")
		ctx.SetBody(common.JSON1KPayload())
	case method == "GET" && path == "/json-64k":
		ctx.SetContentType("application/json")
		ctx.SetBody(common.JSON64KPayload())
	case method == "GET" && len(path) > 7 && path[:7] == "/users/":
		id := path[7:]
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("User ID: " + id)
	case method == "POST" && path == "/upload":
		_ = ctx.PostBody()
		ctx.SetContentType("text/plain")
		ctx.SetBodyString("OK")
	default:
		ctx.SetStatusCode(fasthttp.StatusNotFound)
		ctx.SetBodyString("Not Found")
	}
}
