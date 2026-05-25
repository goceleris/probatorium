package validation

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net"
	"os"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// isCloseObservation reports whether err — returned from a Read on a
// drip-mode slowloris conn — definitively indicates the server has
// closed the connection. Used by the slowloris walker to detect close
// via the read path rather than waiting for write-side buffer pressure.
//
// Includes io.EOF (graceful FIN), ECONNRESET (RST), EPIPE (broken pipe).
// Excludes timeout errors — the walker calls SetReadDeadline(now) for
// non-blocking semantics, so a deadline-exceeded result means "no data
// AND not closed (yet)" which is the keep-dripping case.
func isCloseObservation(err error) bool {
	if errors.Is(err, io.EOF) {
		return true
	}
	if errors.Is(err, syscall.ECONNRESET) || errors.Is(err, syscall.EPIPE) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) && ne.Timeout() {
		return false
	}
	// Wrapped os.PathError or net.OpError around any of the above.
	var oe *os.SyscallError
	if errors.As(err, &oe) {
		if errors.Is(oe.Err, syscall.ECONNRESET) || errors.Is(oe.Err, syscall.EPIPE) {
			return true
		}
	}
	return false
}

// adversarialMode names one shape of bad request the walker generates.
// Each is engineered to find a specific RFC-violation bug class:
//   - ModeBadChunks       — Transfer-Encoding: chunked with garbage
//     framing (negative length, missing terminator,
//     CRLF in chunk size).
//   - ModeOversizedHeader — single header value > 64KiB. Catches naive
//     strncpy / fixed-buffer parsers.
//   - ModeNULInHeader     — embedded \x00 in a header name or value.
//     Must be rejected per RFC 9110.
//   - ModeCRLFInjection   — extra "\r\nX-Smuggled: yes" injected into
//     a header value. Catches response-splitting
//     bugs.
//   - ModeSlowloris       — partial request with arbitrarily slow
//     subsequent reads. Validates the read timeout
//     is bounded.
//   - ModeDoubleCL        — two Content-Length headers with different
//     values. RFC 9110 §6.4 — must reject.
type adversarialMode int

const (
	ModeBadChunks adversarialMode = iota
	ModeOversizedHeader
	ModeNULInHeader
	ModeCRLFInjection
	ModeSlowloris
	ModeDoubleCL
	adversarialModeCount
)

func (m adversarialMode) String() string {
	switch m {
	case ModeBadChunks:
		return "bad-chunks"
	case ModeOversizedHeader:
		return "oversized-header"
	case ModeNULInHeader:
		return "nul-in-header"
	case ModeCRLFInjection:
		return "crlf-injection"
	case ModeSlowloris:
		return "slowloris"
	case ModeDoubleCL:
		return "double-content-length"
	}
	return "unknown"
}

// adversarialTally aggregates per-mode counters across walkers. The
// "well-rejected" counter increments when the server returns ANY 4xx
// response or closes the connection — both are correct under RFC 9110
// for the listed pathologies. "Accepted" is the bug signal — we sent
// CRLF-in-header and got 200 back. Predicate-tier code surfaces a
// non-zero accepted count as an incident.
type adversarialTally struct {
	sent       atomic.Int64
	wellReject atomic.Int64
	accepted   atomic.Int64
	hang       atomic.Int64 // connection lasted > timeout without server close
}

// adversarialSnapshot is the value-typed projection.
type adversarialSnapshot struct {
	Sent             int64 `json:"adv_sent"`
	WellRejected     int64 `json:"adv_well_rejected"`
	WrongAccepted    int64 `json:"adv_wrong_accepted"`
	HangUntilTimeout int64 `json:"adv_hang_until_timeout"`
}

func (t *adversarialTally) snapshot() adversarialSnapshot {
	return adversarialSnapshot{
		Sent:             t.sent.Load(),
		WellRejected:     t.wellReject.Load(),
		WrongAccepted:    t.accepted.Load(),
		HangUntilTimeout: t.hang.Load(),
	}
}

// runAdversarialWalker fires one adversarial request per ~tickInterval
// against the host:port until ctx is cancelled. Each request is sent
// over a raw TCP socket — we can't use net/http for these because
// the stdlib enforces RFC 9110 framing on the way out, which would
// turn every adversarial request into a well-formed one before it
// hit the wire.
//
// hostPort is the bench target's `host:port` (NOT a URL — adversarial
// traffic doesn't speak http://). seed is the per-walker PCG seed;
// same seed reproduces the same mode sequence.
//
// tickInterval bounds how fast we fire: a ~10ms tick on a single
// walker × 4 walkers = ~400 adversarial requests/second, well below
// the Markov walker's per-walker rate. We don't WANT adversarial to
// dominate the workload — 20% mix budget already accounts for that.
func runAdversarialWalker(ctx context.Context, hostPort string,
	seed uint64, tickInterval time.Duration, tally *adversarialTally,
) {
	rng := rand.New(rand.NewPCG(seed, ^seed^0xdead_beef_cafe_babe))
	if tickInterval <= 0 {
		tickInterval = 10 * time.Millisecond
	}
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			//nolint:gosec // probability sampling, not crypto
			mode := adversarialMode(rng.IntN(int(adversarialModeCount)))
			fireAdversarial(ctx, hostPort, mode, tally)
		}
	}
}

// slowlorisDripBudget caps how long the slowloris drip-walker keeps a
// conn alive before declaring "server didn't close = bug". Must be
// longer than celeris's default ReadHeaderTimeout (10s) so a correctly-
// configured server has time to enforce its own header-read timeout
// before the walker gives up.
//
// Pre-fix this was 2s (the same timeout used for dial + non-slowloris
// reads), which meant the walker counted any refapp with the celeris
// default config as buggy: the conn was indeed still open at 2s, but
// only because celeris's ReadHeaderTimeout hadn't fired yet (it's
// scheduled for ~10s). Surfaced by soak 26333090001 as 664K false-
// positive adv_hang_until_timeout events across 24h.
//
// 12s gives headroom over the 10s default; refapps that set a longer
// ReadHeaderTimeout will still legitimately trip the hang counter,
// which is correct (they disabled their slowloris defence).
const slowlorisDripBudget = 12 * time.Second

// fireAdversarial opens one raw TCP conn, writes the mode's bad-bytes
// payload, reads up to one short response, and classifies the result.
// timeout caps how long any individual victim request can stall.
func fireAdversarial(ctx context.Context, hostPort string,
	mode adversarialMode, tally *adversarialTally,
) {
	tally.sent.Add(1)
	const timeout = 2 * time.Second
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		// Dial failure is an infra problem (refapp down, port blocked)
		// — NOT a server-side adversarial-handling result. Don't fold
		// it into wellRejected or accepted.
		return
	}
	defer func() { _ = conn.Close() }()
	deadline := time.Now().Add(timeout)
	_ = conn.SetDeadline(deadline)

	payload := adversarialPayload(mode)
	if _, err := conn.Write(payload); err != nil {
		// Write failed mid-flight — server's likely already closed
		// the conn, which IS a rejection.
		tally.wellReject.Add(1)
		return
	}

	// Slowloris has a special read pattern: we WANT the conn to
	// stay open while we slow-drip subsequent bytes. The server's
	// read-header-timeout should kick in within slowlorisDripBudget
	// (longer than celeris's default 10s ReadHeaderTimeout).
	if mode == ModeSlowloris {
		// Extend the conn deadline beyond the dial/read 2s budget —
		// otherwise the SetDeadline above would fire 10s before we
		// give celeris its full 10s ReadHeaderTimeout window.
		_ = conn.SetDeadline(time.Now().Add(slowlorisDripBudget + time.Second))

		// Write a single header byte every 200ms until the server
		// closes or we hit the drip budget. A correct server closes
		// within its own read-header-timeout (celeris default: 10s);
		// the budget here is 12s, giving 2s slack for clock skew +
		// network latency before declaring a hang.
		//
		// After each write, attempt a non-blocking 1-byte read.
		// celeris-side close (FIN/RST) is observable via Read way
		// before TCP send-buffer-back-pressure shows up via Write.
		// Without this Read probe, walker can keep dripping into a
		// closed conn's local kernel send buffer for hundreds of ms
		// past the FIN, eating into the 2s slack budget and producing
		// false-positive hangs on slow refapps. Validated by the
		// v1.4.11 celeris-side fixes that bring iouring close to
		// kernel-precision: residual hangs are walker-observation
		// lag, not engine close failure.
		slowDripDeadline := time.After(slowlorisDripBudget)
		t := time.NewTicker(200 * time.Millisecond)
		defer t.Stop()
		readBuf := make([]byte, 1)
	slowloop:
		for {
			select {
			case <-ctx.Done():
				return
			case <-slowDripDeadline:
				// Conn still open after the full drip budget = server
				// didn't enforce its ReadHeaderTimeout = bug.
				tally.hang.Add(1)
				return
			case <-t.C:
				if _, err := conn.Write([]byte("X")); err != nil {
					tally.wellReject.Add(1)
					break slowloop
				}
				// Non-blocking read probe: if the server already FIN/RST'd,
				// Read returns immediately with EOF / ECONNRESET / 0 bytes.
				// SetReadDeadline(now) means "fail fast" on any pending
				// read state without blocking. Restore the longer drip
				// deadline afterwards so subsequent writes/reads still
				// have a sensible budget.
				_ = conn.SetReadDeadline(time.Now())
				if _, err := conn.Read(readBuf); err == nil || isCloseObservation(err) {
					tally.wellReject.Add(1)
					break slowloop
				}
				_ = conn.SetReadDeadline(time.Now().Add(slowlorisDripBudget + time.Second))
			}
		}
		return
	}

	// Read up to the first ~256 bytes of response, then classify by
	// status line. We don't need the full body for adversarial — the
	// status line tells us 2xx (accepted, bad) vs 4xx (rejected,
	// good) vs nothing (server closed without status, also good).
	buf := make([]byte, 256)
	n, err := conn.Read(buf)
	if err != nil || n == 0 {
		// EOF / RST / read deadline — server closed without
		// responding. That's a valid rejection of a malformed
		// request.
		tally.wellReject.Add(1)
		return
	}
	statusLine := string(buf[:n])
	// Look for "HTTP/1.x 2xx" — the bug shape.
	if strings.HasPrefix(statusLine, "HTTP/1.0 2") || strings.HasPrefix(statusLine, "HTTP/1.1 2") {
		tally.accepted.Add(1)
		return
	}
	// Anything else (4xx / 5xx / non-HTTP babble before close) =
	// well rejected. 5xx is technically a server error but still
	// "didn't accept", which is what the predicate cares about.
	tally.wellReject.Add(1)
}

// adversarialPayload returns the raw HTTP bytes for one mode. Each
// is deliberately malformed; sanity-checking via Go's net/http parser
// would reject every one of these BEFORE the wire write, which is
// why fireAdversarial uses a raw socket instead.
func adversarialPayload(mode adversarialMode) []byte {
	switch mode {
	case ModeBadChunks:
		// Transfer-Encoding: chunked with a negative length (bug
		// shape: server interprets "-1" as a huge unsigned value).
		return []byte(
			"POST /api/users HTTP/1.1\r\n" +
				"Host: probatorium\r\n" +
				"Transfer-Encoding: chunked\r\n" +
				"\r\n" +
				"-1\r\n" + // negative chunk size
				"GARBAGE\r\n" +
				"0\r\n\r\n",
		)
	case ModeOversizedHeader:
		// 64KiB single header value. Catches naive fixed-buffer
		// strncpy parsers that overflow before bounds-check.
		var b strings.Builder
		b.WriteString("GET / HTTP/1.1\r\n")
		b.WriteString("Host: probatorium\r\n")
		b.WriteString("X-Massive: ")
		for i := 0; i < 65536; i++ {
			b.WriteByte('A')
		}
		b.WriteString("\r\n\r\n")
		return []byte(b.String())
	case ModeNULInHeader:
		// Embedded NUL — RFC 9110 §5.5 forbids in field values.
		return []byte(
			"GET / HTTP/1.1\r\n" +
				"Host: probatorium\r\n" +
				"X-Sneaky: foo\x00bar\r\n" +
				"\r\n",
		)
	case ModeCRLFInjection:
		// Smuggle an extra header via CRLF in the value.
		return []byte(
			"GET / HTTP/1.1\r\n" +
				"Host: probatorium\r\n" +
				"X-Inject: foo\r\nX-Smuggled: yes\r\n" +
				"\r\n",
		)
	case ModeSlowloris:
		// Open with a partial request, then drip via fireAdversarial.
		return []byte(
			"GET / HTTP/1.1\r\n" +
				"Host: probatorium\r\n" +
				"X-Drip: ",
		)
	case ModeDoubleCL:
		// Two Content-Length headers with different values. RFC 9110
		// §6.4 — must reject (the request-smuggling bug class).
		return []byte(
			"POST /api/upload HTTP/1.1\r\n" +
				"Host: probatorium\r\n" +
				"Content-Length: 5\r\n" +
				"Content-Length: 7\r\n" +
				"\r\n" +
				"hello",
		)
	}
	return nil
}

// _ keeps the io import live; future slices fold a response-body
// reader through io.LimitReader for the streaming-attack workloads.
var _ io.Reader = (*adversarialReadStub)(nil)

type adversarialReadStub struct{}

func (*adversarialReadStub) Read([]byte) (int, error) { return 0, io.EOF }

// adversarialModeFor exposes the seed→mode mapping for tests.
func adversarialModeFor(seed uint64, step int) adversarialMode {
	rng := rand.New(rand.NewPCG(seed, ^seed^0xdead_beef_cafe_babe))
	var m adversarialMode
	for i := 0; i <= step; i++ {
		//nolint:gosec
		m = adversarialMode(rng.IntN(int(adversarialModeCount)))
	}
	return m
}

// summariseAdversarial formats a snapshot for the run summary log
// line. Used by driveTier1's epilogue.
func summariseAdversarial(s adversarialSnapshot) string {
	return fmt.Sprintf("adv_sent=%d adv_rejected=%d adv_accepted=%d adv_hang=%d",
		s.Sent, s.WellRejected, s.WrongAccepted, s.HangUntilTimeout)
}
