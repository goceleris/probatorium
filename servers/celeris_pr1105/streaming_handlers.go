// streaming_handlers.go mounts the Phase-2 streaming surface (#159) onto the
// celeris adapter, expressed with celeris's own in-tree middleware/websocket
// and middleware/sse packages — the idiomatic, perf-blessed path a celeris
// user would actually deploy, and the one the celeris reference benches use
// (test/benchcmp_ws, test/benchcmp_sse).
//
// Routes mounted:
//
//   - GET /ws        — WebSocket. The scenario "mode" (echo / large-echo /
//     hub-broadcast) is selected per connection by the ?mode= query param the
//     loadgen WS client sets, so a single upgrade route serves all three WS
//     cell-rows without colliding paths. Default (no/unknown mode) is echo.
//   - GET /events    — Server-Sent-Events fan-out. Every subscriber joins a
//     shared sse.Broker; a background publisher pushes a steady event stream
//     so the loadgen SSE client counts delivered events (sse-fanout-N).
//
// WIRE MODES (?mode=, must match scenarios.StreamMode* / the loadgen client):
//
//   - ""              -> echo            (StreamModeWSEcho)
//   - "ws-large-echo" -> echo, raised per-message read limit (64 KiB payloads)
//   - "ws-hub"        -> register with the shared Hub; the connection is a pure
//     receiver of the background broadcast (StreamModeWSHub)
//
// The Hub broadcast loop and the SSE publish loop are driven by background
// goroutines bounded by the server lifetime context, so repeat adapter
// processes (one per bench cell) never leak goroutines. streamingResources.
// close() releases the Hub and Broker on shutdown, wired into the existing
// signal handler in server.go.
package main

import (
	"context"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/sse"
	celerisws "github.com/goceleris/celeris/middleware/websocket"
)

// streamWSMode is the ?mode= query selector. The values mirror the
// scenarios.StreamMode* constants so the loadgen client and the adapter cannot
// drift on the wire.
const (
	streamModeWSEcho      = "ws-echo"
	streamModeWSLargeEcho = "ws-large-echo"
	streamModeWSHub       = "ws-hub"

	// largeEchoReadLimit raises the per-message read cap to comfortably hold
	// the 64 KiB large-echo payload (scenarios.ws-large-echo MessageSize) plus
	// framing slack.
	largeEchoReadLimit int64 = 1 << 20 // 1 MiB

	// broadcastPayload is the fixed text frame the Hub fans out. Kept small and
	// constant so the bench measures fan-out dispatch, not per-message
	// formatting (the prepared frame is encoded once).
	broadcastPayload = "payload"

	// sseEventData is the fixed SSE event body fanned out to every subscriber.
	sseEventData = "hello"

	// streamPublishInterval paces the background Hub broadcast and SSE publish
	// loops. Tight enough that loadgen sees a continuous push stream, loose
	// enough that the server stays push-bound (fan-out cost) rather than
	// busy-spinning a single CPU.
	streamPublishInterval = time.Millisecond
)

// streamingResources owns the long-lived Hub + Broker so the signal handler can
// release them on shutdown.
type streamingResources struct {
	hub    *celerisws.Hub
	broker *sse.Broker
}

// mountStreamingHandlers wires GET /ws and GET /events onto srv and starts the
// background broadcast / publish loops, all bounded by lifetime. The returned
// *streamingResources lets the caller close the Hub and Broker on shutdown.
func mountStreamingHandlers(srv *celeris.Server, lifetime context.Context) *streamingResources {
	hub := celerisws.NewHub(celerisws.HubConfig{})
	broker := sse.NewBroker(sse.BrokerConfig{SubscriberBuffer: 1024})

	// WebSocket: one upgrade route, behaviour selected per-connection by ?mode=.
	// Echo modes run the read->echo loop on the handler goroutine; hub mode
	// registers the connection as a broadcast receiver and parks until the peer
	// (or the engine) closes it.
	srv.GET("/ws", celerisws.New(celerisws.Config{
		Handler: func(c *celerisws.Conn) {
			switch c.Query("mode") {
			case streamModeWSHub:
				unreg := hub.Register(c)
				defer unreg()
				<-c.Context().Done()
			case streamModeWSLargeEcho:
				c.SetReadLimit(largeEchoReadLimit)
				echoLoop(c)
			default: // streamModeWSEcho or unset
				echoLoop(c)
			}
		},
	}))

	// SSE: every subscriber joins the shared broker and drains until its
	// request context is cancelled. HeartbeatInterval=-1 disables the
	// middleware's own keep-alive comments so the only bytes on the wire are the
	// fan-out events the background publisher emits.
	srv.GET("/events", sse.New(sse.Config{
		HeartbeatInterval: -1,
		Handler: func(c *sse.Client) {
			unsub := broker.Subscribe(c)
			defer unsub()
			<-c.Context().Done()
		},
	}))

	startBroadcastLoop(lifetime, hub)
	startSSEPublishLoop(lifetime, broker)

	return &streamingResources{hub: hub, broker: broker}
}

// echoLoop reads frames and echoes each back with the same message type,
// returning when the peer closes or the read errors. This is the WS echo /
// large-echo terminal — identical to the celeris reference bench loop.
func echoLoop(c *celerisws.Conn) {
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

// startBroadcastLoop fans the fixed broadcast frame to every Hub-registered
// connection on a ticker, until lifetime is cancelled. The frame is prepared
// once (encoded once, masked never — server frames are unmasked) so the bench
// measures Hub dispatch cost, not per-tick framing.
func startBroadcastLoop(lifetime context.Context, hub *celerisws.Hub) {
	pm, err := celerisws.NewPreparedMessage(celerisws.OpText, []byte(broadcastPayload))
	if err != nil {
		return
	}
	go func() {
		t := time.NewTicker(streamPublishInterval)
		defer t.Stop()
		for {
			select {
			case <-lifetime.Done():
				return
			case <-t.C:
				_, _ = hub.BroadcastPrepared(pm)
			}
		}
	}()
}

// startSSEPublishLoop publishes the fixed event to every broker subscriber on a
// ticker, until lifetime is cancelled. Broker.Publish encodes the event once
// and fans the prepared bytes out, so the bench measures fan-out cost.
func startSSEPublishLoop(lifetime context.Context, broker *sse.Broker) {
	ev := sse.Event{Data: sseEventData}
	go func() {
		t := time.NewTicker(streamPublishInterval)
		defer t.Stop()
		for {
			select {
			case <-lifetime.Done():
				return
			case <-t.C:
				_ = broker.Publish(ev)
			}
		}
	}()
}

// close releases the Hub and Broker. Safe to call with a nil receiver or nil
// fields.
func (r *streamingResources) close() {
	if r == nil {
		return
	}
	if r.hub != nil {
		r.hub.Close()
	}
	if r.broker != nil {
		r.broker.Close()
	}
}
