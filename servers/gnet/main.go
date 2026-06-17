// Command gnet serves the probatorium static contract on top of
// panjf2000/gnet — the canonical Go event-loop networking library and the
// closest architectural peer to celeris's epoll engine. It is the single
// highest-value cross-engine comparison: both run a fixed pool of event loops
// over epoll/kqueue and parse HTTP off a zero-copy inbound ring buffer rather
// than one goroutine-per-connection on net/http.
//
// gnet ships no HTTP codec, so this adapter carries a minimal HTTP/1.1
// request framer (codec.go) and a hand-rolled dispatch switch over the six
// canonical [common.Endpoints]. It is HTTP/1.1-only and serves the static
// contract exclusively — no driver, middleware, WS, or SSE routes. The
// capability manifest (Static only) lives in the shared registry at
// servers/servers.go (the gnet-h1 Adapter entry), the single source of truth
// the runner gates scenario waves from.
//
// The -engine flag is accepted for symmetry with the other Go adapters; only
// "h1" is meaningful. Multicore is on (one event loop per CPU) with
// SO_REUSEPORT so the kernel load-balances accepts across loops, mirroring
// celeris's epoll worker fan-out.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/panjf2000/gnet/v2"

	"github.com/goceleris/probatorium/servers/common"
)

// preformatted full HTTP/1.1 responses for the static-bodied endpoints. Each
// carries an explicit Content-Length so keep-alive never falls back to chunked
// framing, matching the net/http adapters' wire output byte-for-byte.
var (
	respRoot    = buildResponse("text/plain", common.Endpoints[0].ResponseBody)
	respJSON    = buildResponse("application/json", common.Endpoints[1].ResponseBody)
	respJSON1K  = buildResponse("application/json", common.JSON1KPayload())
	respJSON8K  = buildResponse("application/json", common.JSON8KPayload())
	respJSON16K = buildResponse("application/json", common.JSON16KPayload())
	respJSON64K = buildResponse("application/json", common.JSON64KPayload())
	respUpload  = buildResponse("text/plain", []byte("OK"))
	respNotFnd  = buildStatusResponse(404, "Not Found", "text/plain", []byte("Not Found"))
)

// buildResponse renders a complete 200 OK HTTP/1.1 message (status line,
// Content-Type, Content-Length, terminator, body) once at startup so the hot
// path is a single Conn.Write of immutable bytes.
func buildResponse(contentType string, body []byte) []byte {
	return buildStatusResponse(200, "OK", contentType, body)
}

func buildStatusResponse(code int, reason, contentType string, body []byte) []byte {
	head := fmt.Sprintf(
		"HTTP/1.1 %d %s\r\nContent-Type: %s\r\nContent-Length: %d\r\n\r\n",
		code, reason, contentType, len(body),
	)
	out := make([]byte, 0, len(head)+len(body))
	out = append(out, head...)
	out = append(out, body...)
	return out
}

// httpServer is the gnet event handler. It embeds BuiltinEventEngine for the
// default no-op lifecycle callbacks and overrides OnBoot (readiness handshake)
// and OnTraffic (request framing + dispatch).
type httpServer struct {
	gnet.BuiltinEventEngine
	eng  gnet.Engine
	addr string
	// ready is closed once OnBoot has captured the engine handle; main blocks
	// on it before arming the signal-driven graceful shutdown.
	ready chan struct{}
	once  atomic.Bool
}

func (s *httpServer) OnBoot(eng gnet.Engine) gnet.Action {
	s.eng = eng
	// The runner scans child stdout for this exact prefix before dialing.
	// Printed to stdout (not the logger, which writes to stderr) so the
	// scanner keys on a clean line.
	fmt.Printf("ready addr=%s\n", s.addr)
	if s.once.CompareAndSwap(false, true) {
		close(s.ready)
	}
	return gnet.None
}

// OnTraffic drains every complete pipelined request currently buffered on the
// connection, writing each response in order. A partial trailing request is
// left in gnet's inbound buffer untouched and re-delivered on the next event.
func (s *httpServer) OnTraffic(c gnet.Conn) gnet.Action {
	closeAfter := false
	for {
		n := c.InboundBuffered()
		if n == 0 {
			break
		}
		buf, err := c.Peek(n)
		if err != nil {
			return gnet.Close
		}
		req, consumed, perr := parseRequest(buf)
		if perr == errPartial {
			break
		}
		if perr != nil {
			return gnet.Close
		}
		if _, err := c.Discard(consumed); err != nil {
			return gnet.Close
		}

		resp := route(req.method, req.target)
		if _, err := c.Write(resp); err != nil {
			return gnet.Close
		}
		if !req.keepAlive {
			closeAfter = true
			break
		}
	}
	if closeAfter {
		return gnet.Close
	}
	return gnet.None
}

// route resolves a parsed request to its preformatted response. The six
// contract endpoints are matched inline; everything else is 404. /users/:id
// and /upload are dynamic, so their responses are assembled per request rather
// than served from the static table.
func route(method, target []byte) []byte {
	path := target
	if i := indexByte(target, '?'); i >= 0 {
		path = target[:i]
	}

	if equal(method, "GET") {
		switch {
		case equal(path, "/"):
			return respRoot
		case equal(path, "/json"):
			return respJSON
		case equal(path, "/json-1k"):
			return respJSON1K
		case equal(path, "/json-8k"):
			return respJSON8K
		case equal(path, "/json-16k"):
			return respJSON16K
		case equal(path, "/json-64k"):
			return respJSON64K
		case hasPrefix(path, "/users/"):
			id := string(path[len("/users/"):])
			return buildResponse("text/plain", []byte("User ID: "+id))
		}
	}
	if equal(method, "POST") && equal(path, "/upload") {
		// The body has already been consumed by parseRequest's Discard, so
		// the parser cost is accounted for; we only ack.
		return respUpload
	}
	return respNotFnd
}

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	// -engine accepted for symmetry with the other adapters; only "h1" is
	// supported (gnet here speaks HTTP/1.1 only).
	_ = flag.String("engine", "h1", "runtime engine: h1 (only mode supported)")
	flag.Parse()

	srv := &httpServer{addr: *bind, ready: make(chan struct{})}

	go func() {
		<-srv.ready
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("gnet: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.eng.Stop(ctx)
	}()

	log.Printf("gnet: listening on %s (engine=h1)", *bind)
	err := gnet.Run(
		srv,
		"tcp://"+*bind,
		gnet.WithMulticore(true),
		gnet.WithReusePort(true),
		gnet.WithTCPKeepAlive(time.Minute),
	)
	if err != nil {
		log.Fatalf("gnet: run %s: %v", *bind, err)
	}
}
