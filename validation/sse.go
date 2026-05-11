package validation

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// sseTally aggregates per-walker outcomes for the SSE long-poll
// kill-mid-stream slice. The goal isn't to catch malformed responses
// — it's to repeatedly establish SSE streams, hold them for a
// randomised duration, then RST. A correct server's broker cleans up
// the client slot on each disconnect (no goroutine leak, no FD leak,
// no replay-queue retention). The I-CONN-2 invariant (accepted −
// closed − active == 0) is what predicate-tier code asserts against
// the metrics endpoint; this walker exists to GENERATE the
// disconnect events that I-CONN-2 catches a stuck broker on.
//
//   - sent             — fires attempted (dial + GET)
//   - established     — saw HTTP/1.1 200 with text/event-stream
//     Content-Type
//   - eventsRead      — total event-lines counted across all cells
//     (loose signal; the walker reads up to a
//     small bounded number per fire)
//   - killedMidStream — we RST'd the conn as planned (success)
//   - serverClosedEarly — server hung up before we killed (FINE,
//     just means broker closed naturally — record
//     for traceability but don't treat as bug)
//   - handshakeFail   — non-200 or no event-stream Content-Type
type sseTally struct {
	sent              atomic.Int64
	established       atomic.Int64
	eventsRead        atomic.Int64
	killedMidStream   atomic.Int64
	serverClosedEarly atomic.Int64
	handshakeFail     atomic.Int64
}

type sseSnapshot struct {
	Sent              int64 `json:"sse_sent"`
	Established       int64 `json:"sse_established"`
	EventsRead        int64 `json:"sse_events_read"`
	KilledMidStream   int64 `json:"sse_killed_mid_stream"`
	ServerClosedEarly int64 `json:"sse_server_closed_early"`
	HandshakeFail     int64 `json:"sse_handshake_fail"`
}

func (t *sseTally) snapshot() sseSnapshot {
	return sseSnapshot{
		Sent:              t.sent.Load(),
		Established:       t.established.Load(),
		EventsRead:        t.eventsRead.Load(),
		KilledMidStream:   t.killedMidStream.Load(),
		ServerClosedEarly: t.serverClosedEarly.Load(),
		HandshakeFail:     t.handshakeFail.Load(),
	}
}

// runSSEKillWalker dials hostPort + path per tickInterval, holds for
// a per-fire randomised hold duration, then RSTs. ctx-cancelled.
//
// path is the SSE endpoint (typically "/events"); the walker sends
// a canonical text/event-stream GET. seed is the per-walker PCG seed;
// same seed reproduces the same hold-duration sequence.
//
// Hold durations are sampled from a deterministic distribution in
// [50ms, 1.5s] — long enough to receive several events, short enough
// to RST while the broker still has the slot held. Predicate-tier
// I-CONN-2 catches the broker if it doesn't clean up.
func runSSEKillWalker(ctx context.Context, hostPort, path string,
	seed uint64, tickInterval time.Duration, tally *sseTally,
) {
	rng := rand.New(rand.NewPCG(seed, ^seed^0x5e5e_5e5e_dead_beef))
	if tickInterval <= 0 {
		tickInterval = 200 * time.Millisecond
	}
	if path == "" {
		path = "/events"
	}
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			// Sample a hold duration in [50ms, 1.5s] uniformly. The
			// distribution is deliberately wide so the broker sees a
			// mix of "barely connected" and "established session"
			// disconnects, each of which is a distinct cleanup path.
			//nolint:gosec
			hold := time.Duration(50+rng.IntN(1450)) * time.Millisecond
			fireSSEKill(ctx, hostPort, path, hold, tally)
		}
	}
}

// fireSSEKill opens one TCP conn, sends a canonical SSE GET, holds
// the stream for hold (reading whatever events arrive), then RSTs.
//
// The held conn is what the walker actually contributes to the test:
// the server's broker has a goroutine + client struct attached, and
// it must release both within seconds of the RST.
func fireSSEKill(ctx context.Context, hostPort, path string,
	hold time.Duration, tally *sseTally,
) {
	tally.sent.Add(1)
	// Per-fire overall deadline = hold + 1s grace for response headers
	// + handshake. If the server is wedged, the read deadline trips
	// rather than the goroutine accumulating forever.
	const handshakeBudget = 1500 * time.Millisecond
	overall := hold + handshakeBudget
	d := net.Dialer{Timeout: handshakeBudget}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(overall))

	req := sseGetRequest(hostPort, path)
	if _, err := conn.Write(req); err != nil {
		tally.handshakeFail.Add(1)
		return
	}

	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		tally.handshakeFail.Add(1)
		return
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 200") && !strings.HasPrefix(statusLine, "HTTP/1.0 200") {
		// Non-200 — server didn't accept the stream.
		tally.handshakeFail.Add(1)
		return
	}
	// Read headers, looking for Content-Type: text/event-stream. A
	// real SSE endpoint emits exactly that; anything else means we
	// got a normal HTTP response (e.g. 200 plain text), which
	// classifies as handshake-fail because we can't tell a "killed
	// mid-stream" event from a "completed normal request."
	gotSSE := false
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			tally.handshakeFail.Add(1)
			return
		}
		if strings.HasPrefix(strings.ToLower(line), "content-type:") &&
			strings.Contains(strings.ToLower(line), "text/event-stream") {
			gotSSE = true
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	if !gotSSE {
		tally.handshakeFail.Add(1)
		return
	}
	tally.established.Add(1)

	// Read events for `hold`, then RST. We don't buffer the whole
	// stream — we just consume whatever shows up, counting event
	// lines as a loose progress signal. ReadString returns when the
	// per-fire deadline trips, which gives us a deterministic
	// disconnect window.
	holdDeadline := time.Now().Add(hold)
	_ = conn.SetReadDeadline(holdDeadline)
	for time.Now().Before(holdDeadline) {
		line, err := br.ReadString('\n')
		if err != nil {
			// Read deadline / EOF. Two cases:
			//   - we hit our hold deadline → killedMidStream
			//   - server closed first → serverClosedEarly
			//
			// We disambiguate by checking how close we are to the
			// holdDeadline: within 50ms ≈ deadline tripped (ours);
			// well before → server closed early.
			if time.Until(holdDeadline) > 50*time.Millisecond {
				tally.serverClosedEarly.Add(1)
				return
			}
			break
		}
		// "event: " or "data: " lines are SSE primitives. Count both.
		if strings.HasPrefix(line, "event:") || strings.HasPrefix(line, "data:") {
			tally.eventsRead.Add(1)
		}
	}
	// Hit our hold deadline cleanly — deferred Close() RSTs the conn.
	tally.killedMidStream.Add(1)
}

// sseGetRequest builds a canonical SSE GET. The Accept header signals
// our intent; servers SHOULD respond with text/event-stream when this
// is present.
func sseGetRequest(hostPort, path string) []byte {
	var b strings.Builder
	b.WriteString("GET ")
	b.WriteString(path)
	b.WriteString(" HTTP/1.1\r\n")
	b.WriteString("Host: ")
	b.WriteString(hostPort)
	b.WriteString("\r\n")
	b.WriteString("Accept: text/event-stream\r\n")
	b.WriteString("Cache-Control: no-cache\r\n")
	b.WriteString("Connection: keep-alive\r\n")
	b.WriteString("\r\n")
	return []byte(b.String())
}

// summariseSSEKill formats a snapshot for the run summary log line.
func summariseSSEKill(s sseSnapshot) string {
	return fmt.Sprintf("sse_sent=%d sse_established=%d sse_events=%d sse_killed=%d sse_early_close=%d sse_handshake_fail=%d",
		s.Sent, s.Established, s.EventsRead, s.KilledMidStream, s.ServerClosedEarly, s.HandshakeFail)
}
