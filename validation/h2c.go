package validation

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// h2cChurnMode names one shape of HTTP/1.1 → HTTP/2 upgrade churn.
// Every mode sends a valid h2c upgrade preamble (RFC 9113 §3.4), then
// peels away from the handshake at a different point to exercise the
// PauseAccept/ResumeAccept race window in the engine.
//
// Bug-class targets per mode:
//   - ChurnRSTBeforeRead — dial, write preamble, close without reading
//     the 101. Exercises the "client RST during PauseAccept" race
//     (celeris commits ed55fb6 + bd675f9). The server is mid-pause for
//     H2 dial when the client disappears.
//   - ChurnRSTAfter101   — read the 101 Switching Protocols line, then
//     close BEFORE sending the H2 client preface. Server has emitted
//     SETTINGS but client never reciprocates — does the engine clean
//     up the half-upgraded conn state?
//   - ChurnPartialPreface — read 101, send first 12 bytes of the
//     "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" preface, close. Mid-preface
//     teardown — does the H2 frame parser handle truncated preface
//     without leaking a goroutine or panicking on the connect promise?
type h2cChurnMode int

const (
	ChurnRSTBeforeRead h2cChurnMode = iota
	ChurnRSTAfter101
	ChurnPartialPreface
	h2cChurnModeCount
)

func (m h2cChurnMode) String() string {
	switch m {
	case ChurnRSTBeforeRead:
		return "rst-before-read"
	case ChurnRSTAfter101:
		return "rst-after-101"
	case ChurnPartialPreface:
		return "partial-preface"
	}
	return "unknown"
}

// h2cTally aggregates outcomes across walkers. The classification is
// deliberately coarse — we don't care WHICH frame the server chose to
// send, only whether the conn was cleanly serviced (or visibly
// mishandled). Predicate-tier code maps non-zero `crashed` or `hang`
// to incidents; `upgraded` and `declined` are both healthy outcomes.
type h2cTally struct {
	sent atomic.Int64
	// upgraded — server replied 101 (the path we ARE trying to churn).
	upgraded atomic.Int64
	// declined — server replied 200 / 4xx without switching protocols.
	// Valid response; h2c is optional per RFC 9113 §3.4.
	declined atomic.Int64
	// crashed — server gave a malformed status line or 5xx in response
	// to a well-formed upgrade. Bug shape.
	crashed atomic.Int64
	// hang — server-side hang: we sent a valid upgrade request AND
	// tried to read the response, but no bytes returned within the
	// 2s walker timeout. Signals the server may be wedged inside
	// PauseAccept or otherwise stuck. Bug-adjacent.
	hang atomic.Int64
	// intentionalRST — walker-side close-without-read: the
	// ChurnRSTBeforeRead mode deliberately closes the conn after
	// writing the preamble, before reading any response. The server's
	// response (if any) is intentionally discarded by the walker.
	// NOT a bug signal — it's the workload itself. Tracked separately
	// so the `hang` counter retains its server-wedge semantics.
	//
	// Pre-split (before this counter existed), every ChurnRSTBeforeRead
	// fire either silently disappeared from outcome accounting OR was
	// later folded into `hang` — both wrong. The 3-day soak showed
	// h2c_hang=317K on amd64 which was almost entirely this workload
	// noise rather than real server hangs.
	intentionalRST atomic.Int64
}

// h2cSnapshot is the value-typed projection emitted into the tally
// JSON. Prefix `h2c_` keeps the keys unambiguous next to adversarial.
type h2cSnapshot struct {
	Sent           int64 `json:"h2c_sent"`
	Upgraded       int64 `json:"h2c_upgraded"`
	Declined       int64 `json:"h2c_declined"`
	Crashed        int64 `json:"h2c_crashed"`
	Hang           int64 `json:"h2c_hang"`
	IntentionalRST int64 `json:"h2c_intentional_rst"`
}

func (t *h2cTally) snapshot() h2cSnapshot {
	return h2cSnapshot{
		Sent:           t.sent.Load(),
		Upgraded:       t.upgraded.Load(),
		Declined:       t.declined.Load(),
		Crashed:        t.crashed.Load(),
		Hang:           t.hang.Load(),
		IntentionalRST: t.intentionalRST.Load(),
	}
}

// runH2CChurnWalker fires one h2c upgrade preamble per ~tickInterval
// until ctx is cancelled. Each request is sent over a fresh raw TCP
// socket — we can't use net/http here because the stdlib's
// http.Transport never closes mid-handshake the way we need to, and
// `golang.org/x/net/http2` would happily complete the upgrade rather
// than RST the partial one.
//
// hostPort is the target's host:port (NOT a URL). seed is the
// per-walker PCG seed; same seed reproduces the same mode sequence.
//
// tickInterval bounds the churn rate. The PauseAccept race we're
// hunting fires at H2-dial frequency, so we want a moderate rate
// (50–200ms tick) — not so fast that we synthesise our own backlog,
// not so slow that we miss the race window between conns.
func runH2CChurnWalker(ctx context.Context, hostPort string,
	seed uint64, tickInterval time.Duration, tally *h2cTally,
) {
	rng := rand.New(rand.NewPCG(seed, ^seed^0xdeadbabe_cafef00d))
	if tickInterval <= 0 {
		tickInterval = 100 * time.Millisecond
	}
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			//nolint:gosec // probability sampling, not crypto
			mode := h2cChurnMode(rng.IntN(int(h2cChurnModeCount)))
			fireH2CChurn(ctx, hostPort, mode, tally)
		}
	}
}

// h2cChurnDialTimeout and h2cChurnReadTimeout split what used to be a
// single 2s budget. Dial stays at 2s — refapps should accept quickly.
// Read budget is 10s — sized to celeris's default ReadTimeout (30s)
// minus generous slack, so a slow-but-correct refapp (observability,
// static_swagger_proxy under load) has time to respond before we
// declare h2c_hang. Pre-fix the 2s read budget produced ~18K false-
// positive hang events per soak on arm64 slow refapps, where the
// server WAS responding correctly but took 2-5s.
const (
	h2cChurnDialTimeout = 2 * time.Second
	h2cChurnReadTimeout = 10 * time.Second
)

// fireH2CChurn opens one raw TCP conn, writes a valid h2c upgrade
// preamble, then either RSTs immediately, RSTs after the 101 line, or
// sends a truncated H2 preface and RSTs — depending on mode.
//
// The point isn't to complete an HTTP/2 conversation. It's to leave
// the server holding a half-upgraded conn while the engine's
// PauseAccept/ResumeAccept dance is mid-flight, then disappear. A
// correct engine cleans up; a broken one leaks state or wedges.
func fireH2CChurn(ctx context.Context, hostPort string,
	mode h2cChurnMode, tally *h2cTally,
) {
	tally.sent.Add(1)
	d := net.Dialer{Timeout: h2cChurnDialTimeout}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		// Dial failure is infra (server down, port unreachable); don't
		// fold into the upgrade-outcome counters.
		return
	}
	defer func() { _ = conn.Close() }()
	// Dial-window deadline for the upgrade write; reset before the read.
	_ = conn.SetDeadline(time.Now().Add(h2cChurnDialTimeout))

	preamble := h2cUpgradePreamble(hostPort)
	if _, err := conn.Write(preamble); err != nil {
		// Server closed before we finished writing the upgrade headers.
		// Coarse heuristic: anything that prevents us reaching the read
		// step counts as "didn't upgrade." Not crashed — the server may
		// have legitimately closed (e.g. accept-queue backpressure).
		return
	}

	if mode == ChurnRSTBeforeRead {
		// Slam the socket shut without reading. Server's PauseAccept
		// path is mid-flight; we want it to see RST during the H2 dial.
		// The deferred Close() handles the RST.
		//
		// Tracked as intentionalRST — this is workload intent, NOT a
		// server-side hang. Keeping `hang` reserved for the case where
		// we DID attempt a read and got nothing (a real server-wedge
		// signal).
		tally.intentionalRST.Add(1)
		return
	}

	// Reset the deadline to a longer read budget — slow refapps
	// (observability /api/error, static_swagger_proxy proxy handler
	// on arm64 under load) take longer than the 2s dial budget to
	// respond. Without this, every slow-but-correct response counted
	// as h2c_hang.
	_ = conn.SetDeadline(time.Now().Add(h2cChurnReadTimeout))

	// Read up to the first 256 bytes to find the status line. We
	// intentionally don't drain — the 101 path produces SETTINGS
	// frames we ignore.
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		// Read EOF / timeout before any bytes. If we never saw bytes
		// after a valid upgrade request, that's the server hanging on
		// us — possible PauseAccept wedge.
		tally.hang.Add(1)
		return
	}
	statusLine := string(buf[:n])

	switch {
	case strings.HasPrefix(statusLine, "HTTP/1.1 101"):
		tally.upgraded.Add(1)
		if mode == ChurnPartialPreface {
			// Send the first 12 bytes of the H2 client preface, then
			// close. Full preface is 24 bytes:
			//   "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n"
			// We stop at "PRI * HTTP/2" so the server has read 12 bytes
			// of a 24-byte preface when we vanish.
			partial := []byte("PRI * HTTP/2")
			_, _ = conn.Write(partial)
		}
		// ChurnRSTAfter101 falls through — deferred Close() RSTs the
		// conn without ever sending preface bytes.
	case strings.HasPrefix(statusLine, "HTTP/1.0 ") || strings.HasPrefix(statusLine, "HTTP/1.1 "):
		// Any other 1.x status — server declined to upgrade. 200 / 400
		// / 426 are all valid responses to a refused h2c hint.
		tally.declined.Add(1)
	default:
		// Garbage status line in response to a well-formed upgrade.
		// Bug shape.
		tally.crashed.Add(1)
	}
}

// h2cUpgradePreamble builds the canonical client-side h2c upgrade
// request per RFC 9113 §3.4. The HTTP2-Settings value is a base64-url-
// encoded SETTINGS frame body advertising INITIAL_WINDOW_SIZE=100,
// MAX_CONCURRENT_STREAMS=100 — values the server is free to ignore
// but must not reject. Connection: Upgrade, HTTP2-Settings is the
// REQUIRED header set; missing either makes the server treat this as
// a plain HTTP/1.1 request.
//
// We deliberately don't randomise the path or method. The point is to
// exercise the engine-level upgrade lifecycle, not the request router.
// GET / is the universal "any server will route this somewhere"
// shape — even a router that returns 404 will have parsed the upgrade
// headers and gone through the PauseAccept dance first.
func h2cUpgradePreamble(hostPort string) []byte {
	// SETTINGS frame body: 2 settings = 12 bytes raw, then base64-url
	// encoded. We pre-compute the encoding rather than recompute every
	// call:
	//   id=4 (INITIAL_WINDOW_SIZE) value=65535
	//   id=3 (MAX_CONCURRENT_STREAMS) value=100
	// → base64url("\x00\x04\x00\x00\xff\xff\x00\x03\x00\x00\x00\x64")
	// = "AAQAAP__AAMAAABk"
	const settingsB64 = "AAQAAP__AAMAAABk"
	var b strings.Builder
	b.WriteString("GET / HTTP/1.1\r\n")
	b.WriteString("Host: ")
	b.WriteString(hostPort)
	b.WriteString("\r\n")
	b.WriteString("Connection: Upgrade, HTTP2-Settings\r\n")
	b.WriteString("Upgrade: h2c\r\n")
	b.WriteString("HTTP2-Settings: ")
	b.WriteString(settingsB64)
	b.WriteString("\r\n")
	b.WriteString("\r\n")
	return []byte(b.String())
}

// h2cChurnModeFor exposes the seed→mode mapping for tests.
func h2cChurnModeFor(seed uint64, step int) h2cChurnMode {
	rng := rand.New(rand.NewPCG(seed, ^seed^0xdeadbabe_cafef00d))
	var m h2cChurnMode
	for i := 0; i <= step; i++ {
		//nolint:gosec
		m = h2cChurnMode(rng.IntN(int(h2cChurnModeCount)))
	}
	return m
}

// summariseH2CChurn formats a snapshot for the run summary log line.
func summariseH2CChurn(s h2cSnapshot) string {
	return fmt.Sprintf("h2c_sent=%d h2c_upgraded=%d h2c_declined=%d h2c_crashed=%d h2c_hang=%d",
		s.Sent, s.Upgraded, s.Declined, s.Crashed, s.Hang)
}
