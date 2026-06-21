// Command gorilla_ws is the WS/SSE reference adapter: a net/http server whose
// streaming surface is hand-rolled on github.com/gorilla/websocket plus a
// flusher-based Server-Sent-Events broker. It exists so the celeris streaming
// numbers have a like-for-like baseline — the same connection-set + RWMutex
// broadcast pattern the celeris middleware/websocket Hub is designed to
// replace (see celeris test/benchcmp_ws's gorillaHub), and the same
// publish-to-N-subscribers SSE fan-out the celeris middleware/sse Broker
// replaces (see test/benchcmp_sse).
//
// It also serves the six canonical static endpoints so it is a fully valid
// adapter (conformance + static cells run against it too), but its reason for
// being is the WS / SSE comparison: this is the adapter the matrix pairs with
// the celeris streaming cell-columns.
//
// Routes:
//
//   - the six common.Endpoints (static contract)
//   - GET /ws      — WebSocket; ?mode= selects echo / large-echo / hub-receiver
//   - GET /events  — SSE fan-out; every subscriber joins the broker, a
//     background publisher pushes a steady event stream
//
// H1-only by design — the reference is for the WS upgrade + SSE long-poll,
// both of which ride HTTP/1.1. No H2C variant.
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
	"sync"
	"syscall"
	"time"

	gorillaws "github.com/gorilla/websocket"

	"github.com/goceleris/probatorium/servers/common"
)

// ?mode= selectors — mirror scenarios.StreamMode* and the celeris adapter so
// the loadgen client drives both adapters identically.
const (
	streamModeWSHub       = "ws-hub"
	streamModeWSLargeEcho = "ws-large-echo"

	largeEchoReadLimit int64 = 1 << 20 // 1 MiB, holds the 64 KiB large-echo payload

	broadcastPayload = "payload"
	sseEventData     = "hello"

	streamPublishInterval = time.Millisecond
)

func main() {
	bind := flag.String("bind", "0.0.0.0:8080", "address:port to listen on")
	flag.Parse()

	lifetime, cancelLifetime := context.WithCancel(context.Background())
	defer cancelLifetime()

	mux := http.NewServeMux()
	registerStatic(mux)
	hub := newGorillaHub()
	startBroadcastLoop(lifetime, hub)
	broker := newSSEBroker()
	startSSEPublishLoop(lifetime, broker)
	registerStreaming(mux, hub, broker)

	srv := &http.Server{
		Handler:      mux,
		ReadTimeout:  30 * time.Second,
		WriteTimeout: 0, // streaming responses are long-lived; no write deadline
		IdleTimeout:  120 * time.Second,
	}

	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("gorilla_ws: listen: %v", err)
	}

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("gorilla_ws: signal received, shutting down")
		cancelLifetime()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// ready marker on stdout, matching the native-adapter contract the runner
	// waits on (servers/start.go).
	log.Printf("ready addr=%s", ln.Addr().String())
	if err := srv.Serve(ln); err != nil && err != http.ErrServerClosed {
		log.Fatalf("gorilla_ws: serve: %v", err)
	}
}

// registerStatic mounts the six canonical contract endpoints from
// servers/common so this adapter satisfies the static contract byte-for-byte.
func registerStatic(mux *http.ServeMux) {
	mux.HandleFunc("GET /", func(w http.ResponseWriter, r *http.Request) {
		// net/http's "GET /" matches every unmatched path; guard so only the
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
	w.Header().Set("Content-Type", contentType)
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(body)
}

// --- WebSocket: hand-rolled hub (the pattern the celeris Hub replaces) ---

var upgrader = gorillaws.Upgrader{
	CheckOrigin:     func(*http.Request) bool { return true },
	ReadBufferSize:  4096,
	WriteBufferSize: 4096,
}

// gorillaHub is a connection set guarded by an RWMutex with a serialized
// broadcast loop — deliberately the naive baseline (no prepared-frame caching,
// no sharded fan-out) so the comparison against the celeris Hub is honest.
type gorillaHub struct {
	mu    sync.RWMutex
	conns map[*gorillaws.Conn]struct{}
}

func newGorillaHub() *gorillaHub {
	return &gorillaHub{conns: make(map[*gorillaws.Conn]struct{})}
}

func (h *gorillaHub) register(c *gorillaws.Conn) {
	h.mu.Lock()
	h.conns[c] = struct{}{}
	h.mu.Unlock()
}

func (h *gorillaHub) unregister(c *gorillaws.Conn) {
	h.mu.Lock()
	delete(h.conns, c)
	h.mu.Unlock()
}

func (h *gorillaHub) broadcast(messageType int, msg []byte) {
	h.mu.RLock()
	conns := make([]*gorillaws.Conn, 0, len(h.conns))
	for c := range h.conns {
		conns = append(conns, c)
	}
	h.mu.RUnlock()
	for _, c := range conns {
		_ = c.WriteMessage(messageType, msg)
	}
}

func registerStreaming(mux *http.ServeMux, hub *gorillaHub, broker *sseBroker) {
	mux.HandleFunc("GET /ws", func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer c.Close()
		switch r.URL.Query().Get("mode") {
		case streamModeWSHub:
			hub.register(c)
			defer hub.unregister(c)
			// Pure receiver: drain reads (control frames, eventual close) until
			// the peer goes away; the background loop drives the writes.
			for {
				if _, _, err := c.ReadMessage(); err != nil {
					return
				}
			}
		case streamModeWSLargeEcho:
			c.SetReadLimit(largeEchoReadLimit)
			echoLoop(c)
		default:
			echoLoop(c)
		}
	})

	mux.HandleFunc("GET /events", func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			http.Error(w, "streaming unsupported", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Connection", "keep-alive")
		w.WriteHeader(http.StatusOK)
		flusher.Flush()

		sub := broker.subscribe()
		defer broker.unsubscribe(sub)
		for {
			select {
			case <-r.Context().Done():
				return
			case data, open := <-sub.ch:
				if !open {
					return
				}
				if _, err := w.Write(data); err != nil {
					return
				}
				flusher.Flush()
			}
		}
	})
}

func echoLoop(c *gorillaws.Conn) {
	for {
		mt, msg, err := c.ReadMessage()
		if err != nil {
			return
		}
		if err := c.WriteMessage(mt, msg); err != nil {
			return
		}
	}
}

func startBroadcastLoop(lifetime context.Context, hub *gorillaHub) {
	payload := []byte(broadcastPayload)
	go func() {
		t := time.NewTicker(streamPublishInterval)
		defer t.Stop()
		for {
			select {
			case <-lifetime.Done():
				return
			case <-t.C:
				hub.broadcast(gorillaws.TextMessage, payload)
			}
		}
	}()
}

// --- SSE: hand-rolled broker (the pattern the celeris sse.Broker replaces) ---

type sseSubscriber struct {
	ch chan []byte
}

// sseBroker fans a published event to every subscriber over a buffered channel.
// Slow subscribers drop events (non-blocking send) so one stalled consumer
// cannot wedge the publish loop — the baseline equivalent of the celeris
// broker's drop policy.
type sseBroker struct {
	mu   sync.RWMutex
	subs map[*sseSubscriber]struct{}
}

func newSSEBroker() *sseBroker {
	return &sseBroker{subs: make(map[*sseSubscriber]struct{})}
}

func (b *sseBroker) subscribe() *sseSubscriber {
	s := &sseSubscriber{ch: make(chan []byte, 1024)}
	b.mu.Lock()
	b.subs[s] = struct{}{}
	b.mu.Unlock()
	return s
}

func (b *sseBroker) unsubscribe(s *sseSubscriber) {
	b.mu.Lock()
	delete(b.subs, s)
	b.mu.Unlock()
}

func (b *sseBroker) publish(event []byte) {
	b.mu.RLock()
	for s := range b.subs {
		select {
		case s.ch <- event:
		default: // slow subscriber: drop rather than block the fan-out
		}
	}
	b.mu.RUnlock()
}

func startSSEPublishLoop(lifetime context.Context, broker *sseBroker) {
	// Pre-format the SSE wire frame once: "data: hello\n\n".
	frame := []byte("data: " + sseEventData + "\n\n")
	go func() {
		t := time.NewTicker(streamPublishInterval)
		defer t.Stop()
		for {
			select {
			case <-lifetime.Done():
				return
			case <-t.C:
				broker.publish(frame)
			}
		}
	}()
}
