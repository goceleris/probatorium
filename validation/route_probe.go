package validation

import (
	"bufio"
	"context"
	"net"
	"strings"
	"sync"
	"time"
)

// Streaming-route pre-flight (probatorium#300).
//
// The WS-torture and SSE-kill walkers used to fire at every cell in the
// matrix regardless of whether the refapp under test routes /ws or
// /events at all. Only auth_session_ratelimit does: across the v1.5.11
// 24h soak that meant 1,999,114 of 2,284,701 WS upgrades (87.5%) and
// 1,499,314 of 1,554,373 SSE GETs (96.5%) hit a 404 and were booked as
// *_endpoint_absent. Every ws_* / sse_* zero on the other seven refapps
// was therefore a statement about routing, not about robustness — the
// oracles reported clean because they were testing nothing.
//
// So the tier asks ONCE, per cell, before it decides how to spend the
// walker budget: does this endpoint exist here? An absent route means
// the slice does not run (its walker slots go back to the Markov mix)
// and the cell's document records that the route was probed and found
// absent, which is what makes a zero readable. A route that IS present
// is walked exactly as before.

// routeProbe is the verdict of one pre-flight probe.
//
// Probed=false means we never got a definite answer (the probe itself
// errored). The caller then runs the walker anyway: an unreachable
// probe must never silently cost us coverage on a refapp that does
// route the endpoint.
type routeProbe struct {
	Probed  bool
	Present bool
}

// absent reports a route we KNOW is not there. Deliberately not
// "!Present": an unprobed route is unknown, not missing.
func (p routeProbe) absent() bool { return p.Probed && !p.Present }

// routeProbeAttempts is how many times a probe retries before giving
// up and returning "unknown". The refapp has already announced ready
// (and, on any run longer than a minute, survived a 30s warm-up), so
// one retry covers a lost SYN rather than a slow boot.
const routeProbeAttempts = 2

// routeProbeTimeout bounds one probe attempt end to end.
const routeProbeTimeout = 2 * time.Second

// probeStreamingRoutes probes the WS and SSE endpoints on hostPort
// with the EXACT requests their walkers send, so the answer is the one
// the walker would have got rather than an approximation of it.
//
// The two run concurrently: the probes sit between "refapp ready" and
// "walkers start", and against a wedged server each costs its full
// retry budget. Serial probing would put that delay on the cell's
// clock twice for no extra information.
func probeStreamingRoutes(ctx context.Context, hostPort, wsPath, ssePath string) (ws, sse routeProbe) {
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		// The WS probe completes a real handshake on a refapp that
		// routes /ws and then hangs up. That is a legitimate — if
		// abrupt — client disconnect, and strictly gentler than the
		// torture frames the walker sends a moment later.
		ws = probeRoute(ctx, hostPort, wsUpgradeRequest(hostPort, wsPath, wsProbeKey))
	}()
	go func() {
		defer wg.Done()
		sse = probeRoute(ctx, hostPort, sseGetRequest(hostPort, ssePath))
	}()
	wg.Wait()
	return ws, sse
}

// wsProbeKey is the RFC 6455 example Sec-WebSocket-Key, same as the
// torture walker's — the handshake is deterministic across runs.
const wsProbeKey = "dGhlIHNhbXBsZSBub25jZQ=="

// probeRoute sends req to hostPort and classifies the status line:
// 404 is an absent route, any other reply (101, 200, 400, 401, 405…)
// means the path IS routed and the walker has something to test.
//
// A dial/read failure returns {Probed:false} — unknown, walk it anyway.
func probeRoute(ctx context.Context, hostPort string, req []byte) routeProbe {
	for attempt := 0; attempt < routeProbeAttempts; attempt++ {
		if ctx.Err() != nil {
			return routeProbe{}
		}
		status, err := probeStatusLine(ctx, hostPort, req)
		if err != nil {
			continue
		}
		return routeProbe{Probed: true, Present: !strings.HasPrefix(status, "HTTP/1.1 404") &&
			!strings.HasPrefix(status, "HTTP/1.0 404")}
	}
	return routeProbe{}
}

// probeStatusLine opens one conn, writes req, and returns the response
// status line. The conn is closed immediately — we only need the first
// line, never the body or any frame that follows it.
func probeStatusLine(ctx context.Context, hostPort string, req []byte) (string, error) {
	d := net.Dialer{Timeout: routeProbeTimeout}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return "", err
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(routeProbeTimeout))
	if _, err := conn.Write(req); err != nil {
		return "", err
	}
	return bufio.NewReader(conn).ReadString('\n')
}
