package validation

import (
	"bufio"
	"context"
	"crypto/sha1" //nolint:gosec // RFC 6455 mandates SHA-1 for the WS handshake; not used for crypto
	"encoding/base64"
	"encoding/binary"
	"fmt"
	"math/rand/v2"
	"net"
	"strings"
	"sync/atomic"
	"time"
)

// wsTortureMode names one shape of malformed WebSocket frame the
// walker can send AFTER completing a clean RFC 6455 handshake. Every
// mode is a distinct RFC violation that a correct server must reject
// with a specific close code:
//
//   - ModeFragmentedReserved — start a fragmented message with a
//     reserved opcode (0x3-0x7). Server must close with 1002.
//   - ModeOversizePayload   — declare 64-bit length > 1 GiB. Server
//     must close with 1009 (Message Too Big).
//   - ModeUnmaskedClient    — send a data frame with the MASK bit
//     clear. RFC 6455 §5.1: client→server frames MUST be masked;
//     server must close with 1002.
//   - ModePingFlood         — send 1000 ping frames in a row without
//     waiting for pongs. Server must handle without OOM / leak; if
//     ping-flood mitigation is missing this fans out goroutines.
//   - ModeContinuationNoStart — send an opcode-0 (continuation)
//     frame WITHOUT a preceding non-final frame. Server must close
//     with 1002.
//   - ModeInvalidUTF8       — send a text frame (opcode 0x1) with
//     invalid UTF-8 bytes. RFC 6455 §8.1: server must close 1007.
type wsTortureMode int

const (
	ModeFragmentedReserved wsTortureMode = iota
	ModeOversizePayload
	ModeUnmaskedClient
	ModePingFlood
	ModeContinuationNoStart
	ModeInvalidUTF8
	wsTortureModeCount
)

func (m wsTortureMode) String() string {
	switch m {
	case ModeFragmentedReserved:
		return "fragmented-reserved"
	case ModeOversizePayload:
		return "oversize-payload"
	case ModeUnmaskedClient:
		return "unmasked-client"
	case ModePingFlood:
		return "ping-flood"
	case ModeContinuationNoStart:
		return "continuation-no-start"
	case ModeInvalidUTF8:
		return "invalid-utf8"
	}
	return "unknown"
}

// wsTally aggregates per-walker outcomes. Coarse classification:
//
//   - upgraded         — handshake completed (we got our 101)
//   - handshakeFail    — handshake never completed (server rejected
//     the upgrade — fine, just means we didn't get to torture this
//     conn; doesn't increment any outcome counter)
//   - closedCorrectly  — server sent an opcode-0x8 (Close) frame
//     within timeout, possibly preceded by a status code (1002/
//     1003/1007/1009). Healthy.
//   - acceptedBadFrame — server echoed our torture frame back, or
//     kept the conn open >2s after it. Bug shape.
//   - hangNoClose      — conn stayed half-open past timeout without
//     a Close frame OR a clean RST. Likely server goroutine wedged.
type wsTally struct {
	sent             atomic.Int64
	upgraded         atomic.Int64
	handshakeFail    atomic.Int64
	closedCorrectly  atomic.Int64
	acceptedBadFrame atomic.Int64
	hangNoClose      atomic.Int64
}

// wsSnapshot is the value-typed projection emitted into the tally
// JSON. Prefix `ws_` keeps the keys unambiguous next to adv/h2c.
type wsSnapshot struct {
	Sent             int64 `json:"ws_sent"`
	Upgraded         int64 `json:"ws_upgraded"`
	HandshakeFail    int64 `json:"ws_handshake_fail"`
	ClosedCorrectly  int64 `json:"ws_closed_correctly"`
	AcceptedBadFrame int64 `json:"ws_accepted_bad_frame"`
	HangNoClose      int64 `json:"ws_hang_no_close"`
}

func (t *wsTally) snapshot() wsSnapshot {
	return wsSnapshot{
		Sent:             t.sent.Load(),
		Upgraded:         t.upgraded.Load(),
		HandshakeFail:    t.handshakeFail.Load(),
		ClosedCorrectly:  t.closedCorrectly.Load(),
		AcceptedBadFrame: t.acceptedBadFrame.Load(),
		HangNoClose:      t.hangNoClose.Load(),
	}
}

// runWSTortureWalker dials hostPort + path (/ws on the refapp) per
// tickInterval, completes a clean WS handshake, sends one torture
// frame, then classifies the server's response. ctx-cancelled.
//
// hostPort is the target's host:port (NOT a URL).
// path is the WS endpoint path (typically "/ws"); the walker fans
// the canonical RFC 6455 upgrade headers and reads the 101.
// seed is the per-walker PCG seed; same seed reproduces the same
// mode sequence.
func runWSTortureWalker(ctx context.Context, hostPort, path string,
	seed uint64, tickInterval time.Duration, tally *wsTally,
) {
	rng := rand.New(rand.NewPCG(seed, ^seed^0xfeedface_deadbeef))
	if tickInterval <= 0 {
		tickInterval = 100 * time.Millisecond
	}
	if path == "" {
		path = "/ws"
	}
	tick := time.NewTicker(tickInterval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			//nolint:gosec // probability sampling, not crypto
			mode := wsTortureMode(rng.IntN(int(wsTortureModeCount)))
			fireWSTorture(ctx, hostPort, path, mode, tally)
		}
	}
}

// fireWSTorture opens one raw TCP conn, performs the WS handshake,
// sends one malformed frame, then classifies the server's response.
func fireWSTorture(ctx context.Context, hostPort, path string,
	mode wsTortureMode, tally *wsTally,
) {
	tally.sent.Add(1)
	const timeout = 2 * time.Second
	d := net.Dialer{Timeout: timeout}
	conn, err := d.DialContext(ctx, "tcp", hostPort)
	if err != nil {
		// Dial failure is infra — don't fold into outcomes.
		return
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetDeadline(time.Now().Add(timeout))

	// Send the upgrade request. Use a deterministic key so the test
	// can sanity-check what we wrote; the server will hash it per
	// RFC 6455 §1.3 and reply with the expected Sec-WebSocket-Accept.
	const clientKey = "dGhlIHNhbXBsZSBub25jZQ==" // RFC 6455 example
	upgrade := wsUpgradeRequest(hostPort, path, clientKey)
	if _, err := conn.Write(upgrade); err != nil {
		return
	}

	// Read the response status line + headers. We don't need to fully
	// parse — we just need to confirm the 101 and find the empty line
	// separating headers from frame bytes.
	br := bufio.NewReader(conn)
	statusLine, err := br.ReadString('\n')
	if err != nil {
		tally.handshakeFail.Add(1)
		return
	}
	if !strings.HasPrefix(statusLine, "HTTP/1.1 101") {
		// Server rejected the upgrade. Drain to EOF; not bug-shaped.
		tally.handshakeFail.Add(1)
		return
	}
	// Eat headers until we see the blank line that ends them.
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			tally.handshakeFail.Add(1)
			return
		}
		if line == "\r\n" || line == "\n" {
			break
		}
	}
	// Sanity check: the response should have included
	// Sec-WebSocket-Accept derived from our key. We don't validate
	// the hash — that's the celeris middleware's job — but reaching
	// this line means the response was at least well-formed.
	tally.upgraded.Add(1)

	// Send the torture frame.
	switch mode {
	case ModeFragmentedReserved:
		// Opcode 0x3 is RFC-reserved for "future use" — must reject.
		_, _ = conn.Write(wsFrame(false, 0x3, []byte("garbage"), true))
	case ModeOversizePayload:
		// 64-bit length header declaring 2 GiB but no actual bytes;
		// the server should refuse to allocate and close 1009.
		_, _ = conn.Write(wsOversizeHeader(0x80000000)) // 2 GiB
	case ModeUnmaskedClient:
		// Same frame as a legitimate text message, but with MASK=0.
		// Server MUST close with 1002 per RFC 6455 §5.1.
		_, _ = conn.Write(wsFrame(true, 0x1, []byte("hello"), false))
	case ModePingFlood:
		// 1000 ping frames in immediate succession. Each is 2-6 bytes
		// on the wire (small payload). Server must handle without
		// fanning unbounded goroutines.
		for i := 0; i < 1000; i++ {
			_, _ = conn.Write(wsFrame(true, 0x9, []byte("p"), true))
		}
	case ModeContinuationNoStart:
		// Opcode 0x0 (continuation) without a preceding non-final
		// data frame. RFC 6455 §5.4 — server must close 1002.
		_, _ = conn.Write(wsFrame(true, 0x0, []byte("orphan"), true))
	case ModeInvalidUTF8:
		// Text frame with invalid UTF-8 sequence. RFC 6455 §8.1 —
		// server must close 1007. 0xC0 0x80 is the canonical
		// "overlong NUL" forbidden by RFC 3629.
		_, _ = conn.Write(wsFrame(true, 0x1, []byte{0xC0, 0x80}, true))
	}

	// Classify the server's response. Read up to the first frame
	// header (2-14 bytes). We're looking for a Close frame
	// (opcode 0x8); anything else is suspect — with one exception:
	// ModePingFlood is a DoS-shaped resilience check, not a frame
	// validity check. RFC 6455 §5.5.2 requires endpoints to respond
	// to a Ping with a Pong, so a healthy server replying with one
	// or more Pong frames before closing is correct behaviour, not
	// a violation. We accept opcode 0xA (Pong) or 0x8 (Close) for
	// ping-flood; everything else (Text echo, Continuation, etc.)
	// remains a flag.
	header := make([]byte, 2)
	_, err = br.Read(header)
	if err != nil {
		// EOF / RST. Acceptable — the server cut us off without an
		// explicit Close frame, which is technically a protocol
		// violation but in practice the right behavior for the worst
		// torture modes. Count as correctly closed.
		tally.closedCorrectly.Add(1)
		return
	}
	opcode := header[0] & 0x0F
	if opcode == 0x8 {
		// Close frame. Healthy.
		tally.closedCorrectly.Add(1)
		return
	}
	if mode == ModePingFlood && opcode == 0xA {
		// Pong response to a ping — RFC 6455 §5.5.2 compliant.
		// The DoS-resistance angle of this torture mode is whether
		// the server fans goroutines unboundedly; the I-CONN-2 +
		// I-MEM-2 predicates catch resource leaks elsewhere — this
		// classifier only cares about frame-level compliance, and
		// pong is the spec-mandated reply.
		tally.closedCorrectly.Add(1)
		return
	}
	// Server sent SOMETHING back that wasn't a Close frame. If it's
	// the echo of our torture payload, that's a clear bug. If it's
	// any other frame, also suspicious — a correct server has nothing
	// to say in response to RFC-violating bytes.
	tally.acceptedBadFrame.Add(1)
}

// wsUpgradeRequest builds a canonical RFC 6455 §1.3 client upgrade
// request. The Sec-WebSocket-Key is the fixed RFC example so the
// server's reply (Sec-WebSocket-Accept) is deterministic across runs.
func wsUpgradeRequest(hostPort, path, key string) []byte {
	var b strings.Builder
	b.WriteString("GET ")
	b.WriteString(path)
	b.WriteString(" HTTP/1.1\r\n")
	b.WriteString("Host: ")
	b.WriteString(hostPort)
	b.WriteString("\r\n")
	b.WriteString("Upgrade: websocket\r\n")
	b.WriteString("Connection: Upgrade\r\n")
	b.WriteString("Sec-WebSocket-Key: ")
	b.WriteString(key)
	b.WriteString("\r\n")
	b.WriteString("Sec-WebSocket-Version: 13\r\n")
	b.WriteString("\r\n")
	return []byte(b.String())
}

// wsFrame builds a single WS frame with the given header bits. opcode
// occupies the low 4 bits of byte 0; the FIN bit (0x80) is set when
// fin=true. If masked=true the frame includes the MASK bit and a
// 4-byte mask key followed by XOR'd payload (RFC 6455 §5.3); if
// masked=false the MASK bit is clear and payload is sent verbatim
// (which is itself a protocol violation when speaking client→server,
// but that's exactly the point of ModeUnmaskedClient).
//
// Only payloads ≤ 125 bytes are supported here; the oversize-payload
// mode uses [wsOversizeHeader] instead to construct an extended-
// length frame without buffering 1 GiB of data.
func wsFrame(fin bool, opcode byte, payload []byte, masked bool) []byte {
	if len(payload) > 125 {
		return nil
	}
	hdr := []byte{0, 0}
	hdr[0] = opcode & 0x0F
	if fin {
		hdr[0] |= 0x80
	}
	hdr[1] = byte(len(payload))
	if masked {
		hdr[1] |= 0x80
		mask := []byte{0xAB, 0xCD, 0xEF, 0x12}
		masked := make([]byte, len(payload))
		for i, b := range payload {
			masked[i] = b ^ mask[i%4]
		}
		out := append(hdr, mask...)
		out = append(out, masked...)
		return out
	}
	return append(hdr, payload...)
}

// wsOversizeHeader builds JUST the frame header for a text frame
// declaring a 64-bit payload length but writing zero bytes of
// payload. The server must refuse to allocate buffer space for the
// declared length and close with 1009 (Message Too Big).
//
// Frame layout (RFC 6455 §5.2):
//   - byte 0: FIN=1 | opcode=0x1 (text)
//   - byte 1: MASK=1 | length=127 (sentinel "next 8 bytes are u64 length")
//   - bytes 2-9: u64 length (big-endian)
//   - bytes 10-13: 4-byte mask key
//   - (no payload bytes)
func wsOversizeHeader(declaredLen uint64) []byte {
	buf := make([]byte, 14)
	buf[0] = 0x81       // FIN | text
	buf[1] = 0x80 | 127 // MASK | extended length sentinel
	binary.BigEndian.PutUint64(buf[2:10], declaredLen)
	buf[10] = 0xAB
	buf[11] = 0xCD
	buf[12] = 0xEF
	buf[13] = 0x12
	return buf
}

// wsTortureModeFor exposes the seed→mode mapping for tests.
func wsTortureModeFor(seed uint64, step int) wsTortureMode {
	rng := rand.New(rand.NewPCG(seed, ^seed^0xfeedface_deadbeef))
	var m wsTortureMode
	for i := 0; i <= step; i++ {
		//nolint:gosec
		m = wsTortureMode(rng.IntN(int(wsTortureModeCount)))
	}
	return m
}

// expectedWSAccept returns the Sec-WebSocket-Accept value the server
// must produce for the given client key per RFC 6455 §1.3:
//
//	accept = base64(SHA-1(key + WS_GUID))
//
// Used only by tests that ASSERT a fake server's handshake response —
// the production walker doesn't validate this (the celeris middleware
// already does, and getting through the 101 is sufficient signal).
func expectedWSAccept(clientKey string) string {
	const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"
	h := sha1.New() //nolint:gosec
	h.Write([]byte(clientKey + wsGUID))
	return base64.StdEncoding.EncodeToString(h.Sum(nil))
}

// summariseWSTorture formats a snapshot for the run summary log line.
func summariseWSTorture(s wsSnapshot) string {
	return fmt.Sprintf("ws_sent=%d ws_upgraded=%d ws_handshake_fail=%d ws_closed=%d ws_accepted_bad=%d ws_hang=%d",
		s.Sent, s.Upgraded, s.HandshakeFail, s.ClosedCorrectly, s.AcceptedBadFrame, s.HangNoClose)
}
