package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/servers"
)

// Streaming scenarios (#159): WebSocket echo / large-echo / hub-broadcast and
// SSE fanout. These are the long-lived bidirectional / server-push workloads
// that the request/response static + chain + driver scenarios do not cover.
//
// SCOPE NOTE — the server side lives here; the loadgen WS/SSE *client* is a
// separate loadgen-repo change. loadgen v1.4.4 has a TLS client but no WS/SSE
// client (loadgen.Config carries no streaming knob). Until that lands these
// scenarios are forward-compatible definitions: they are only ever scheduled
// against adapters that declare WS / SSE (today: celeris only, plus a future
// gorilla-reference adapter), and the runner skips a streaming cell when the
// loadgen build lacks a streaming client rather than recording a bogus 0-RPS
// result. Registering them now keeps the cell-row taxonomy stable so the
// report and CLI filters do not shift when the loadgen client ships.
//
// WIRE CONTRACT the loadgen client must speak (the de-facto spec is the
// probatorium validation package, which already encodes both):
//
//   - WS  — RFC 6455 upgrade + masked client frames. See validation/ws.go
//     (wsUpgradeRequest / wsFrame). Path /ws. A "request" for reporting
//     purposes is one echo round-trip (client frame -> server echo frame).
//   - SSE — an HTTP GET with `Accept: text/event-stream`, server replies with
//     a `text/event-stream` body and pushes `data:` events. See
//     validation/sse.go (sseGetRequest). Path /events. A "request" is one
//     delivered event.

// StreamMode names the three WebSocket modes and the one SSE mode.
const (
	StreamModeWSEcho      = "ws-echo"       // echo each client frame back
	StreamModeWSLargeEcho = "ws-large-echo" // echo, with a raised read limit
	StreamModeWSHub       = "ws-hub"        // server fans a broadcast to all conns
	StreamModeSSEFanout   = "sse-fanout"    // server pushes events to all subscribers
)

// StreamScenario benches a streaming workload. WS modes target /ws; the SSE
// mode targets /events. N is the broadcast / fanout subscriber count for the
// hub and fanout modes — varied by scenario (one route per shape, N driven by
// the loadgen connection count), not by a distinct server route.
type StreamScenario struct {
	name     string
	category string // CategoryWS or CategorySSE
	mode     string // one of the StreamMode* constants
	path     string // "/ws" or "/events"

	// Connections is the number of concurrent streaming connections loadgen
	// should hold open. For hub / fanout modes it doubles as N (the subscriber
	// count the server broadcasts to).
	Connections int

	// MessageSize is the per-message payload size in bytes (echo modes). Zero
	// uses the loadgen default.
	MessageSize int
}

// NewStreamScenario constructs a [StreamScenario].
func NewStreamScenario(name, category, mode, path string, connections, messageSize int) *StreamScenario {
	return &StreamScenario{
		name:        name,
		category:    category,
		mode:        mode,
		path:        path,
		Connections: connections,
		MessageSize: messageSize,
	}
}

// Name implements [Scenario].
func (s *StreamScenario) Name() string { return s.name }

// Category implements [Scenario]. One of [CategoryWS] or [CategorySSE].
func (s *StreamScenario) Category() string { return s.category }

// Mode returns the streaming mode (one of the StreamMode* constants).
func (s *StreamScenario) Mode() string { return s.mode }

// Workload returns the loadgen.Config for this streaming scenario. Because
// loadgen has no streaming client yet, this sets the target URL and connection
// count only — forward-compatible with the loadgen WS/SSE client that will
// consume the path + mode. The runner skips the cell when the loadgen build
// cannot speak the streaming protocol.
func (s *StreamScenario) Workload(target string) loadgen.Config {
	conns := s.Connections
	if conns <= 0 {
		conns = 128
	}
	return loadgen.Config{
		URL:         target + s.path,
		Method:      "GET",
		Connections: conns,
	}
}

// Applicable requires the matching streaming capability and plain HTTP/1.1 on
// the wire (the WS upgrade and the SSE long-poll both ride H1). A server that
// only accepts H2C prior-knowledge is skipped.
func (s *StreamScenario) Applicable(fs servers.FeatureSet) bool {
	if !fs.HTTP1 {
		return false
	}
	switch s.category {
	case CategoryWS:
		return fs.WS
	case CategorySSE:
		return fs.SSE
	default:
		return false
	}
}

// Compile-time assertion that StreamScenario satisfies Scenario.
var _ Scenario = (*StreamScenario)(nil)

// StreamScenarioNames is the canonical ordered list of streaming cell-rows.
var StreamScenarioNames = []string{
	"ws-echo",
	"ws-large-echo",
	"ws-hub-broadcast-128",
	"ws-hub-broadcast-1024",
	"sse-fanout-128",
	"sse-fanout-1024",
}

func init() {
	Register(NewStreamScenario("ws-echo", CategoryWS, StreamModeWSEcho, "/ws", 128, 256))
	Register(NewStreamScenario("ws-large-echo", CategoryWS, StreamModeWSLargeEcho, "/ws", 64, 64*1024))
	Register(NewStreamScenario("ws-hub-broadcast-128", CategoryWS, StreamModeWSHub, "/ws", 128, 256))
	Register(NewStreamScenario("ws-hub-broadcast-1024", CategoryWS, StreamModeWSHub, "/ws", 1024, 256))
	Register(NewStreamScenario("sse-fanout-128", CategorySSE, StreamModeSSEFanout, "/events", 128, 256))
	Register(NewStreamScenario("sse-fanout-1024", CategorySSE, StreamModeSSEFanout, "/events", 1024, 256))
}
