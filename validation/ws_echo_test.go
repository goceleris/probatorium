package validation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"math/rand/v2"
	"net"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// --- frame encoder: the three RFC 6455 length forms -------------------------

func TestWSAppendFrame_LengthForms(t *testing.T) {
	mask := [4]byte{0xAB, 0xCD, 0xEF, 0x12}
	cases := []struct {
		n       int
		hdrLen  int
		lenByte byte
		ext     []byte
	}{
		{0, 2, 0, nil},
		{125, 2, 125, nil},
		{126, 4, 126, []byte{0x00, 0x7E}},
		{300, 4, 126, []byte{0x01, 0x2C}},
		{0xffff, 4, 126, []byte{0xFF, 0xFF}},
		{0x10000, 10, 127, []byte{0, 0, 0, 0, 0, 1, 0, 0}},
		{wsEchoFrameBytes, 10, 127, []byte{0, 0, 0, 0, 0, 1, 0, 0}},
	}
	for _, tc := range cases {
		t.Run(strconv.Itoa(tc.n), func(t *testing.T) {
			payload := make([]byte, tc.n)
			for i := range payload {
				payload[i] = byte(i * 7)
			}
			f := wsAppendFrame(nil, true, 0x2, payload, &mask)
			if len(f) != tc.hdrLen+4+tc.n {
				t.Fatalf("frame length: got %d, want %d (hdr %d + mask 4 + payload %d)", len(f), tc.hdrLen+4+tc.n, tc.hdrLen, tc.n)
			}
			if f[0] != 0x82 {
				t.Errorf("byte 0: got 0x%02x, want 0x82 (FIN|binary)", f[0])
			}
			if f[1] != 0x80|tc.lenByte {
				t.Errorf("byte 1: got 0x%02x, want 0x%02x (MASK|%d)", f[1], 0x80|tc.lenByte, tc.lenByte)
			}
			if !bytes.Equal(f[2:tc.hdrLen], tc.ext) {
				t.Errorf("extended length: got %x, want %x", f[2:tc.hdrLen], tc.ext)
			}
			if !bytes.Equal(f[tc.hdrLen:tc.hdrLen+4], mask[:]) {
				t.Errorf("mask key: got %x, want %x", f[tc.hdrLen:tc.hdrLen+4], mask)
			}
			for i, b := range payload {
				if got := f[tc.hdrLen+4+i]; got != b^mask[i&3] {
					t.Fatalf("payload[%d]: got 0x%02x, want 0x%02x (masked)", i, got, b^mask[i&3])
				}
			}
			// Unmasked (server) form: no MASK bit, no key, verbatim payload.
			u := wsAppendFrame(nil, true, 0x2, payload, nil)
			if len(u) != tc.hdrLen+tc.n || u[1] != tc.lenByte || !bytes.Equal(u[tc.hdrLen:], payload) {
				t.Errorf("unmasked frame malformed: len=%d byte1=0x%02x", len(u), u[1])
			}
		})
	}
	// Agrees with the torture walker's encoder on the <=125 form it does
	// support, so the two never put different bytes on the wire for the
	// same frame.
	small := []byte("hi")
	if got, want := wsAppendFrame(nil, true, 0x1, small, &mask), wsFrame(true, 0x1, small, true); !bytes.Equal(got, want) {
		t.Errorf("<=125 form disagrees with wsFrame: got %x want %x", got, want)
	}
}

func TestWSReadFrame_RoundTripsEveryLengthForm(t *testing.T) {
	mask := [4]byte{1, 2, 3, 4}
	for _, n := range []int{0, 1, 125, 126, 0xffff, 0x10000, wsEchoFrameBytes} {
		payload := wsEchoPayload(7)[:n]
		for _, m := range []*[4]byte{nil, &mask} {
			f := wsAppendFrame(nil, true, 0x2, payload, m)
			fin, op, got, err := wsReadFrame(bufio.NewReader(bytes.NewReader(f)), wsEchoFrameBytes)
			if err != nil {
				t.Fatalf("n=%d masked=%v: %v", n, m != nil, err)
			}
			if !fin || op != 0x2 || !bytes.Equal(got, payload) {
				t.Fatalf("n=%d masked=%v: fin=%v op=%d len=%d", n, m != nil, fin, op, len(got))
			}
		}
	}
	// A declared length beyond the cap is a framing error, not an allocation.
	over := wsAppendFrame(nil, true, 0x2, make([]byte, 0x10000), nil)[:10] // 127-form header only
	binary.BigEndian.PutUint64(over[2:10], 1<<40)
	if _, _, _, err := wsReadFrame(bufio.NewReader(bytes.NewReader(over)), wsEchoFrameBytes); err == nil {
		t.Fatal("oversize declared length must error")
	}
	// A truncated frame is io.ErrUnexpectedEOF, which the fire books as missing.
	short := wsAppendFrame(nil, true, 0x2, make([]byte, 300), nil)[:100]
	if _, _, _, err := wsReadFrame(bufio.NewReader(bytes.NewReader(short)), wsEchoFrameBytes); err == nil {
		t.Fatal("truncated frame must error")
	}
}

// --- verifier: the four classes, without a network ---------------------------

func TestWSEchoVerifier_Classes(t *testing.T) {
	const sent = 6
	v := newWSEchoVerifier()
	if got := v.score(true, 0x2, wsEchoPayload(1), sent); got != wsEchoOutcomeOK {
		t.Fatalf("in-order intact frame: %v", got)
	}
	// 3 before 2: reorder; then 2 lands and is ok, and 3 is consumed.
	if got := v.score(true, 0x2, wsEchoPayload(3), sent); got != wsEchoOutcomeReorder {
		t.Fatalf("out-of-order intact frame: %v", got)
	}
	if got := v.score(true, 0x2, wsEchoPayload(2), sent); got != wsEchoOutcomeOK {
		t.Fatalf("late in-order frame: %v", got)
	}
	if v.next != 4 {
		t.Fatalf("next after 1,3,2: got %d want 4", v.next)
	}
	// A duplicate of an already-echoed sequence is corruption, not order --
	// and since every byte of it is an expected echo at the wrong place it
	// is egress-shaped (a repeated send), so it lands in the interleave split.
	// It takes no place in the order: 4 is still next.
	if got := v.score(true, 0x2, wsEchoPayload(3), sent); got != wsEchoOutcomeCorruptInterleave {
		t.Fatalf("duplicate echo: %v", got)
	}
	if v.next != 4 {
		t.Fatalf("a duplicate must not consume the order: next=%d want 4", v.next)
	}
	// A flipped byte in 4: corrupt, NOT attributed to an interleave, and it
	// consumes 4's place so 5 is judged in order rather than as a reorder.
	flipped := wsEchoPayload(4)
	flipped[1000] ^= 0xFF
	if got := v.score(true, 0x2, flipped, sent); got != wsEchoOutcomeCorruptOther {
		t.Fatalf("flipped byte: %v", got)
	}
	if got := v.score(true, 0x2, wsEchoPayload(5), sent); got != wsEchoOutcomeOK {
		t.Fatalf("intact 5 after damaged 4 must be in order, got %v (next=%d)", got, v.next)
	}
	// Framing-level damage cannot say which sequence it stood for; it takes
	// the next place (6). A fresh verifier per case keeps the arithmetic
	// readable.
	fresh := func() *wsEchoVerifier { w := newWSEchoVerifier(); w.next = 6; return w }
	for name, tc := range map[string]struct {
		fin     bool
		opcode  byte
		payload []byte
	}{
		"text opcode": {true, 0x1, wsEchoPayload(6)},
		"non-FIN":     {false, 0x2, wsEchoPayload(6)},
		"short frame": {true, 0x2, wsEchoPayload(6)[:1000]},
	} {
		w := fresh()
		if got := w.score(tc.fin, tc.opcode, tc.payload, sent); got != wsEchoOutcomeCorruptOther {
			t.Errorf("%s: got %v, want other", name, got)
		}
		if w.next != 7 {
			t.Errorf("%s: must consume the next place, next=%d want 7", name, w.next)
		}
	}
	// A sequence never sent (9 > sent=6): corrupt other, consumes next.
	w := fresh()
	if got := w.score(true, 0x2, wsEchoPayload(9), sent); got != wsEchoOutcomeCorruptOther || w.next != 7 {
		t.Errorf("unsent sequence: got %v next=%d, want other/7", got, w.next)
	}
}

func TestWSEchoVerifier_InterleaveAttribution(t *testing.T) {
	const sent = 5
	// (a) another frame's server header inside this payload.
	p := wsEchoPayload(2)
	hdr := wsAppendFrame(nil, true, 0x2, wsEchoPayload(3), nil)
	copy(p[4096:], hdr[:64])
	v := newWSEchoVerifier()
	v.next = 2
	if got := v.score(true, 0x2, p, sent); got != wsEchoOutcomeCorruptInterleave {
		t.Errorf("frame header inside payload: got %v, want interleave", got)
	}
	// (b) another sequence's payload start inside this payload.
	p = wsEchoPayload(2)
	copy(p[4096:], wsEchoPayload(5)[:64])
	v = newWSEchoVerifier()
	v.next = 2
	if got := v.score(true, 0x2, p, sent); got != wsEchoOutcomeCorruptInterleave {
		t.Errorf("other sequence's prefix inside payload: got %v, want interleave", got)
	}
	// (c) the pattern continuing from a different offset (a chunk of an
	// expected echo landed in the wrong place).
	p = wsEchoPayload(2)
	copy(p[4096:4096+64], wsEchoPayload(4)[9000:9064])
	v = newWSEchoVerifier()
	v.next = 2
	if got := v.score(true, 0x2, p, sent); got != wsEchoOutcomeCorruptInterleave {
		t.Errorf("shifted pattern chunk: got %v, want interleave", got)
	}
	// Negative control for the attribution: damage that is not another
	// echo -- a zeroed page, a flipped byte -- must stay "other".
	p = wsEchoPayload(2)
	for i := 4096; i < 4096+512; i++ {
		p[i] = 0
	}
	v = newWSEchoVerifier()
	v.next = 2
	if got := v.score(true, 0x2, p, sent); got != wsEchoOutcomeCorruptOther {
		t.Errorf("zeroed page: got %v, want other", got)
	}
	p = wsEchoPayload(2)
	p[4096] ^= 0x55
	v = newWSEchoVerifier()
	v.next = 2
	if got := v.score(true, 0x2, p, sent); got != wsEchoOutcomeCorruptOther {
		t.Errorf("flipped byte: got %v, want other", got)
	}
}

// --- fires against a minimal RFC 6455 echo server ----------------------------

// shortEchoFire shrinks the per-fire durations so a whole fire, including
// its drain and close handshake, takes a fraction of a second.
func shortEchoFire(t *testing.T, hold time.Duration) {
	t.Helper()
	savedStream, savedHold := wsEchoStreamDuration, wsEchoMaxHold
	wsEchoStreamDuration = 120 * time.Millisecond
	wsEchoMaxHold = hold
	t.Cleanup(func() { wsEchoStreamDuration, wsEchoMaxHold = savedStream, savedHold })
}

// echoServer is the minimal RFC 6455 echo: read one client frame (masked,
// any length form), hand it to mutate, write the result back unmasked. A
// client Close is answered with a Close. mutate returning nil drops the
// echo; returning stop=true closes the connection after that frame.
func echoServer(t *testing.T, mutate func(n int, fin bool, opcode byte, payload []byte) (out []byte, stop bool)) *fakeWSServer {
	t.Helper()
	return newFakeWSServer(t, false, func(c net.Conn) {
		br := bufio.NewReaderSize(c, wsEchoFrameBytes)
		n := 0
		for {
			fin, op, payload, err := wsReadFrame(br, wsEchoFrameBytes)
			if err != nil {
				return
			}
			if op == 0x8 {
				_, _ = c.Write(wsAppendFrame(nil, true, 0x8, payload, nil))
				return
			}
			n++
			out, stop := mutate(n, fin, op, payload)
			if out != nil {
				if _, err := c.Write(wsAppendFrame(nil, fin, op, out, nil)); err != nil {
					return
				}
			}
			if stop {
				return
			}
		}
	})
}

// echoServerEndingAfter echoes faithfully and then ENDS the stream after
// the nth data frame, with the rest of the fire's frames unanswered.
//
// It ends it by half-closing -- FIN on the server->client direction only,
// then draining whatever the client is still sending until the client
// closes. echoServer's stop=true instead closes outright, and closing a
// socket that still has unread inbound data makes the kernel send an RST;
// an RST discards whatever the peer had received but not yet read, so
// "the client saw all n echoes" became a race between its 3 ms read pace
// (wsEchoReadPace) and the reset. CI lost two of three echoes that way
// (run 34790609266: ok=1, want 3) with nothing wrong in the scorer.
//
// A half-close cannot lose them: the echoes and the FIN are bytes in one
// stream, delivered in order, so the client reads all n and then EOF, on
// any machine at any load. The property under test -- a peer that ENDS
// the connection with echoes outstanding is one missing fire, not a
// timeout -- is the same either way; which of FIN and RST ends it is not
// something this test claims to pin.
func echoServerEndingAfter(t *testing.T, n int) *fakeWSServer {
	t.Helper()
	return newFakeWSServer(t, false, func(c net.Conn) {
		br := bufio.NewReaderSize(c, wsEchoFrameBytes)
		seen := 0
		for {
			fin, op, payload, err := wsReadFrame(br, wsEchoFrameBytes)
			if err != nil {
				return
			}
			if op == 0x8 {
				_, _ = c.Write(wsAppendFrame(nil, true, 0x8, payload, nil))
				return
			}
			seen++
			if _, err := c.Write(wsAppendFrame(nil, fin, op, payload, nil)); err != nil {
				return
			}
			if seen < n {
				continue
			}
			tcp, ok := c.(*net.TCPConn)
			if !ok {
				t.Errorf("fake WS server wants a TCP conn to half-close, got %T", c)
				return
			}
			if err := tcp.CloseWrite(); err != nil {
				t.Errorf("half-close: %v", err)
				return
			}
			// Drain the rest of the client's stream so nothing is left
			// unread when the conn is finally closed. The deadline is a
			// wedge detector, not a budget: the client closes as soon as
			// it sees the EOF just queued above.
			_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
			_, _ = io.Copy(io.Discard, br)
			return
		}
	})
}

func faithful(_ int, _ bool, _ byte, p []byte) ([]byte, bool) { return p, false }

func fireOnce(t *testing.T, srv *fakeWSServer) wsEchoSnapshot {
	t.Helper()
	var tally wsEchoTally
	rng := rand.New(rand.NewPCG(1, 2))
	fireWSLargeEcho(context.Background(), srv.HostPort(), "/ws", rng, &tally)
	s := tally.snapshot()
	t.Logf("fire: %s (interleave=%d other=%d frame_err=%d close_ok=%d)", summariseWSEcho(s), s.EgressInterleave, s.OtherCorrupt, s.FrameErr, s.CloseOK)
	return s
}

// The positive control: a faithful echo scores nothing but ok and closes
// cleanly. Every failure-class test below is only meaningful against this.
func TestFireWSLargeEcho_FaithfulEchoIsClean(t *testing.T) {
	shortEchoFire(t, 2*time.Second)
	s := fireOnce(t, echoServer(t, faithful))
	if s.Upgraded != 1 || s.Fires != 1 {
		t.Fatalf("fires=%d upgraded=%d, want 1/1", s.Fires, s.Upgraded)
	}
	if s.OK < 2 {
		t.Errorf("ok=%d, want >= 2 echoed frames", s.OK)
	}
	if s.OK != s.Sent {
		t.Errorf("ok=%d sent=%d: every frame sent must have been echoed and scored ok", s.OK, s.Sent)
	}
	if s.Corrupt+s.Reorder+s.Missing+s.Timeout != 0 {
		t.Errorf("faithful echo scored a defect: corrupt=%d reorder=%d missing=%d timeout=%d", s.Corrupt, s.Reorder, s.Missing, s.Timeout)
	}
	if s.CloseOK != 1 {
		t.Errorf("close_ok=%d, want 1 (server answered the Close)", s.CloseOK)
	}
	if s.CutAtDeadline != 0 || s.FrameErr != 0 {
		t.Errorf("cut=%d frame_err=%d, want 0/0", s.CutAtDeadline, s.FrameErr)
	}
}

// Negative control for the corruption detector: an echo that flips ONE
// byte of every frame must score every frame corrupt, none ok, and the
// damage must be attributed as other (not an interleave).
func TestFireWSLargeEcho_ByteFlipIsCorrupt(t *testing.T) {
	shortEchoFire(t, 2*time.Second)
	s := fireOnce(t, echoServer(t, func(_ int, _ bool, _ byte, p []byte) ([]byte, bool) {
		p[12345] ^= 0x01
		return p, false
	}))
	if s.Corrupt < 1 {
		t.Fatalf("corrupt=%d: the detector did not fail an echo with a flipped byte", s.Corrupt)
	}
	if s.Corrupt != s.Sent || s.OK != 0 {
		t.Errorf("corrupt=%d ok=%d sent=%d: every flipped echo must score corrupt and none ok", s.Corrupt, s.OK, s.Sent)
	}
	if s.OtherCorrupt != s.Corrupt || s.EgressInterleave != 0 {
		t.Errorf("attribution: other=%d interleave=%d corrupt=%d; a flipped byte is not an interleave", s.OtherCorrupt, s.EgressInterleave, s.Corrupt)
	}
	if s.Reorder+s.Missing+s.Timeout != 0 {
		t.Errorf("flipped bytes leaked into other classes: reorder=%d missing=%d timeout=%d", s.Reorder, s.Missing, s.Timeout)
	}
}

// An echo that carries another frame's header inside its payload -- the
// shape a raw inline write landing inside a ring send leaves -- is
// corrupt AND attributed to the interleave class.
func TestFireWSLargeEcho_InterleaveIsAttributed(t *testing.T) {
	shortEchoFire(t, 2*time.Second)
	s := fireOnce(t, echoServer(t, func(n int, _ bool, _ byte, p []byte) ([]byte, bool) {
		if n%2 == 0 {
			prev := wsAppendFrame(nil, true, 0x2, wsEchoPayload(uint64(n-1)), nil)
			copy(p[4096:], prev[:4096])
		}
		return p, false
	}))
	if s.EgressInterleave < 1 {
		t.Fatalf("egress_interleave=%d (corrupt=%d other=%d): a foreign frame header inside a payload was not attributed", s.EgressInterleave, s.Corrupt, s.OtherCorrupt)
	}
	if s.OtherCorrupt != 0 {
		t.Errorf("other_corrupt=%d, want 0: every damaged frame here is interleave-shaped", s.OtherCorrupt)
	}
	if s.EgressInterleave+s.OtherCorrupt != s.Corrupt {
		t.Errorf("split does not sum: interleave=%d other=%d corrupt=%d", s.EgressInterleave, s.OtherCorrupt, s.Corrupt)
	}
	if s.OK < 1 {
		t.Errorf("ok=%d: the untouched odd frames must still score ok", s.OK)
	}
	if s.Reorder != 0 {
		t.Errorf("reorder=%d, want 0: a corrupted frame consumes its place, the frames after it are in order", s.Reorder)
	}
	if s.OK+s.Corrupt != s.Sent {
		t.Errorf("ok=%d + corrupt=%d != sent=%d", s.OK, s.Corrupt, s.Sent)
	}
}

// Echoes delivered intact but in the wrong order are reorders, not
// corruption, and nothing goes missing.
func TestFireWSLargeEcho_SwappedEchoesAreReorder(t *testing.T) {
	shortEchoFire(t, 2*time.Second)
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		br := bufio.NewReaderSize(c, wsEchoFrameBytes)
		var held []byte
		flush := func() {
			if held != nil {
				_, _ = c.Write(wsAppendFrame(nil, true, 0x2, held, nil))
				held = nil
			}
		}
		for {
			// Wait a bounded time for a partner to swap with; a lone frame is
			// echoed on its own so the client can always drain.
			_ = c.SetReadDeadline(time.Now().Add(20 * time.Millisecond))
			_, op, payload, err := wsReadFrame(br, wsEchoFrameBytes)
			if err != nil {
				if errors.Is(err, os.ErrDeadlineExceeded) {
					flush()
					continue
				}
				return
			}
			if op == 0x8 {
				flush()
				_, _ = c.Write(wsAppendFrame(nil, true, 0x8, payload, nil))
				return
			}
			if held == nil {
				held = payload
				continue
			}
			_, _ = c.Write(wsAppendFrame(nil, true, 0x2, payload, nil))
			flush()
		}
	})
	s := fireOnce(t, srv)
	if s.Reorder < 1 {
		t.Fatalf("reorder=%d: swapped echoes were not detected", s.Reorder)
	}
	if s.Corrupt != 0 {
		t.Errorf("corrupt=%d: swapped intact frames must not score corrupt", s.Corrupt)
	}
	if s.Missing+s.Timeout != 0 {
		t.Errorf("missing=%d timeout=%d, want 0/0: every frame was eventually echoed", s.Missing, s.Timeout)
	}
	if s.OK+s.Reorder != s.Sent {
		t.Errorf("ok=%d + reorder=%d != sent=%d", s.OK, s.Reorder, s.Sent)
	}
}

// A server that ends the connection with echoes outstanding is one
// missing fire, not a timeout. See echoServerEndingAfter for why the
// server half-closes rather than slamming the socket shut.
func TestFireWSLargeEcho_DroppedEchoesAreMissing(t *testing.T) {
	shortEchoFire(t, 2*time.Second)
	started := time.Now()
	s := fireOnce(t, echoServerEndingAfter(t, 3))
	if s.Missing != 1 {
		t.Fatalf("missing=%d, want 1 (one fire, ended with frames unanswered)", s.Missing)
	}
	if s.Timeout != 0 {
		t.Errorf("timeout=%d, want 0: the peer closed, the hold did not expire", s.Timeout)
	}
	if s.OK != 3 {
		t.Errorf("ok=%d, want 3 (the frames echoed before the close)", s.OK)
	}
	if s.Corrupt+s.Reorder != 0 {
		t.Errorf("corrupt=%d reorder=%d, want 0/0", s.Corrupt, s.Reorder)
	}
	if el := time.Since(started); el >= wsEchoMaxHold {
		t.Errorf("fire took %v, must return on the peer's close rather than wait out the hold (%v)", el, wsEchoMaxHold)
	}
}

// A server that stops delivering but keeps the connection open is one
// timeout fire.
func TestFireWSLargeEcho_StalledEchoesAreTimeout(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	s := fireOnce(t, echoServer(t, func(n int, _ bool, _ byte, p []byte) ([]byte, bool) {
		if n > 2 {
			return nil, false // swallow everything after the second frame
		}
		return p, false
	}))
	if s.Timeout != 1 {
		t.Fatalf("timeout=%d, want 1", s.Timeout)
	}
	if s.Missing != 0 {
		t.Errorf("missing=%d, want 0: the connection never ended, it stalled", s.Missing)
	}
	if s.OK != 2 {
		t.Errorf("ok=%d, want 2", s.OK)
	}
}

// A sender that drains everything must not be booked as a timeout when the
// reader was parked with nothing outstanding (the poke path).
func TestFireWSLargeEcho_DrainedFireNeverTimesOut(t *testing.T) {
	shortEchoFire(t, 2*time.Second)
	for i := 0; i < 5; i++ {
		s := fireOnce(t, echoServer(t, faithful))
		if s.Timeout+s.Missing != 0 || s.CloseOK != 1 {
			t.Fatalf("run %d: timeout=%d missing=%d close_ok=%d", i, s.Timeout, s.Missing, s.CloseOK)
		}
	}
}

func TestFireWSLargeEcho_HandshakeOutcomes(t *testing.T) {
	shortEchoFire(t, time.Second)
	rejected := fireOnce(t, newFakeWSServer(t, true, nil))
	if rejected.HandshakeFail != 1 || rejected.Upgraded != 0 {
		t.Errorf("400 upgrade: handshake_fail=%d upgraded=%d, want 1/0", rejected.HandshakeFail, rejected.Upgraded)
	}
	absent := httptest404(t)
	var tally wsEchoTally
	fireWSLargeEcho(context.Background(), absent, "/ws", rand.New(rand.NewPCG(1, 2)), &tally)
	if s := tally.snapshot(); s.EndpointAbsent != 1 || s.HandshakeFail != 0 {
		t.Errorf("404 upgrade: endpoint_absent=%d handshake_fail=%d, want 1/0", s.EndpointAbsent, s.HandshakeFail)
	}
	var dial wsEchoTally
	fireWSLargeEcho(context.Background(), "127.0.0.1:1", "/ws", rand.New(rand.NewPCG(1, 2)), &dial)
	if s := dial.snapshot(); s.Fires != 1 || s.Upgraded+s.HandshakeFail+s.Missing+s.Timeout != 0 {
		t.Errorf("dial failure leaked into outcomes: %+v", s)
	}
}

// httptest404 serves a plain 404 for everything and returns its host:port.
func httptest404(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: http.NotFoundHandler(), ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return ln.Addr().String()
}

func TestRunWSLargeEchoWalker_FiresRepeatedly(t *testing.T) {
	shortEchoFire(t, 2*time.Second)
	srv := echoServer(t, faithful)
	var tally wsEchoTally
	// Run until the walker HAS fired repeatedly, rather than for 900 ms
	// and hoping two ~150 ms fires fit (tier1_until_test.go).
	const want = 4
	ctx, cancel := cancelWhen(t, func() bool { return tally.snapshot().Fires >= want })
	defer cancel()
	runWSLargeEchoWalker(ctx, srv.HostPort(), "/ws", 0xfeed, 20*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.Fires < want {
		t.Errorf("fires=%d, want >= %d", s.Fires, want)
	}
	if s.Corrupt+s.Reorder+s.Missing+s.Timeout != 0 {
		t.Errorf("faithful echo under the walker scored a defect: %s", summariseWSEcho(s))
	}
	// A fire cut by the run context is booked as such, never as missing or
	// timeout: at 900 ms with ~150 ms fires, the last one is in flight.
	t.Logf("walker: %s cut=%d", summariseWSEcho(s), s.CutAtDeadline)
}

// --- tier wiring --------------------------------------------------------------

// The slice is scheduled exactly where the torture slice is: at streaming
// concurrency, only where /ws is not known to be absent, and never below
// the threshold.
func TestDriveTier1_WSEchoSliceGating(t *testing.T) {
	shortEchoFire(t, time.Second)
	present := driveAgainst(t, streamingRouteServer(t, http.StatusBadRequest), 10,
		func(s tier1TallySnapshot) bool { return s.WSEcho.Fires >= 1 })
	if present.WSEcho.Fires < 1 {
		t.Errorf("present /ws at the matrix default concurrency: fires=%d, want >= 1", present.WSEcho.Fires)
	}
	if !present.WSEcho.RouteProbed || !present.WSEcho.RoutePresent {
		t.Errorf("route verdict not recorded on the echo tally: probed=%v present=%v", present.WSEcho.RouteProbed, present.WSEcho.RoutePresent)
	}
	// "Never fires" needs a floor of serving time, not a ceiling on the
	// whole cell: the echo walker paces at 250 ms, so 700 ms of a running
	// cell is what gives a wrongly-scheduled slice room to show itself.
	absent := driveAgainst(t, streamingRouteServer(t, http.StatusNotFound), 10, workedFor(700*time.Millisecond))
	if absent.WSEcho.Fires != 0 {
		t.Errorf("404 /ws: fires=%d, want 0 (absent=%d)", absent.WSEcho.Fires, absent.WSEcho.EndpointAbsent)
	}
	if !absent.WSEcho.RouteProbed || absent.WSEcho.RoutePresent {
		t.Errorf("absent route verdict: probed=%v present=%v", absent.WSEcho.RouteProbed, absent.WSEcho.RoutePresent)
	}
	dormant := driveAgainst(t, streamingRouteServer(t, http.StatusBadRequest), 1, workedFor(400*time.Millisecond))
	if dormant.WSEcho.Fires != 0 || dormant.WSEcho.RouteProbed {
		t.Errorf("below the streaming threshold: fires=%d probed=%v, want 0/false", dormant.WSEcho.Fires, dormant.WSEcho.RouteProbed)
	}
	sum := present.Tier1Summary()
	if sum.WSEcho["ws_echo_fires"] != present.WSEcho.Fires || sum.WSEcho["ws_echo_route_present"] != 1 {
		t.Errorf("Tier1Summary ws_echo map: %v", sum.WSEcho)
	}
}

func TestSummariseWSEcho_FormatsGatedCounters(t *testing.T) {
	got := summariseWSEcho(wsEchoSnapshot{Fires: 3, Sent: 900, OK: 897, Corrupt: 1, Reorder: 2, Missing: 0, Timeout: 0})
	for _, want := range []string{"ws_echo_fires=3", "ws_echo_sent=900", "ws_echo_ok=897", "ws_echo_corrupt=1", "ws_echo_reorder=2", "ws_echo_missing=0", "ws_echo_timeout=0"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: %q", want, got)
		}
	}
}

// --- live target (docker measurement) ------------------------------------------

// TestWSLargeEcho_LiveTarget drives the walker at a running refapp named by
// VALIDATE_WS_ECHO_TARGET (host:port) for VALIDATE_WS_ECHO_SECONDS (default
// 20) and asserts the four gated classes are zero. Skipped without the env
// var; it is the container measurement against the real engines, not a
// unit test.
func TestWSLargeEcho_LiveTarget(t *testing.T) {
	target := os.Getenv("VALIDATE_WS_ECHO_TARGET")
	if target == "" {
		t.Skip("VALIDATE_WS_ECHO_TARGET not set")
	}
	secs := 20
	if v, err := strconv.Atoi(os.Getenv("VALIDATE_WS_ECHO_SECONDS")); err == nil && v > 0 {
		secs = v
	}
	var tally wsEchoTally
	ctx, cancel := context.WithTimeout(context.Background(), time.Duration(secs)*time.Second)
	defer cancel()
	runWSLargeEchoWalker(ctx, target, "/ws", 0x587, wsEchoInterval, &tally)
	s := tally.snapshot()
	t.Logf("LIVE %s: %s interleave=%d other=%d frame_err=%d close_ok=%d cut=%d handshake_fail=%d upgraded=%d",
		target, summariseWSEcho(s), s.EgressInterleave, s.OtherCorrupt, s.FrameErr, s.CloseOK, s.CutAtDeadline, s.HandshakeFail, s.Upgraded)
	if s.Upgraded < 1 || s.OK < 1 {
		t.Fatalf("no echo traffic reached the target: upgraded=%d ok=%d", s.Upgraded, s.OK)
	}
	if s.Corrupt+s.Reorder+s.Missing+s.Timeout != 0 {
		t.Fatalf("ASSERT ws_echo defects: corrupt=%d reorder=%d missing=%d timeout=%d", s.Corrupt, s.Reorder, s.Missing, s.Timeout)
	}
	t.Logf("ASSERT ws_echo clean: ok=%d corrupt=0 reorder=0 missing=0 timeout=0", s.OK)
}
