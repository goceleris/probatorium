package validation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestWSTortureMode_String(t *testing.T) {
	cases := map[wsTortureMode]string{
		ModeFragmentedReserved:  "fragmented-reserved",
		ModeOversizePayload:     "oversize-payload",
		ModeUnmaskedClient:      "unmasked-client",
		ModePingFlood:           "ping-flood",
		ModeContinuationNoStart: "continuation-no-start",
		ModeInvalidUTF8:         "invalid-utf8",
		wsTortureMode(99):       "unknown",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("(%d).String(): got %q, want %q", m, got, want)
		}
	}
}

func TestWSUpgradeRequest_HasRequiredHeaders(t *testing.T) {
	p := wsUpgradeRequest("example.com:8080", "/ws", "dGhlIHNhbXBsZSBub25jZQ==")
	required := []string{
		"GET /ws HTTP/1.1\r\n",
		"Host: example.com:8080\r\n",
		"Upgrade: websocket\r\n",
		"Connection: Upgrade\r\n",
		"Sec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n",
		"Sec-WebSocket-Version: 13\r\n",
	}
	for _, want := range required {
		if !bytes.Contains(p, []byte(want)) {
			t.Errorf("upgrade request missing %q", want)
		}
	}
}

func TestWSFrame_FINAndMaskBits(t *testing.T) {
	// FIN=true, opcode=0x1 (text), payload="hi", masked=true.
	f := wsFrame(true, 0x1, []byte("hi"), true)
	if len(f) != 2+4+2 {
		t.Errorf("masked text frame length: got %d, want 8", len(f))
	}
	if f[0] != 0x81 {
		t.Errorf("byte 0: got 0x%02x, want 0x81 (FIN|text)", f[0])
	}
	if f[1]&0x80 == 0 {
		t.Errorf("byte 1: MASK bit clear, want set")
	}
	// Payload bytes should be XOR'd against {0xAB,0xCD,0xEF,0x12}.
	wantH := byte('h') ^ 0xAB
	wantI := byte('i') ^ 0xCD
	if f[6] != wantH || f[7] != wantI {
		t.Errorf("masked payload: got [%02x %02x], want [%02x %02x]",
			f[6], f[7], wantH, wantI)
	}
}

func TestWSFrame_UnmaskedClient(t *testing.T) {
	// MASK=0 (the protocol violation we WANT to send).
	f := wsFrame(true, 0x1, []byte("hi"), false)
	if f[1]&0x80 != 0 {
		t.Errorf("byte 1: MASK bit set when masked=false")
	}
	// Payload is verbatim.
	if string(f[2:]) != "hi" {
		t.Errorf("unmasked payload: got %q, want %q", f[2:], "hi")
	}
}

func TestWSOversizeHeader_DeclaresLargeLength(t *testing.T) {
	h := wsOversizeHeader(0x80000000) // 2 GiB
	if len(h) != 14 {
		t.Errorf("oversize header length: got %d, want 14", len(h))
	}
	if h[0] != 0x81 {
		t.Errorf("byte 0: got 0x%02x, want 0x81", h[0])
	}
	if h[1] != 0x80|127 {
		t.Errorf("byte 1: got 0x%02x, want 0x%02x", h[1], 0x80|127)
	}
	declared := binary.BigEndian.Uint64(h[2:10])
	if declared != 0x80000000 {
		t.Errorf("declared length: got %d, want %d", declared, 0x80000000)
	}
}

func TestWSTortureModeFor_Deterministic(t *testing.T) {
	for step := 0; step < 8; step++ {
		a := wsTortureModeFor(0xabcd, step)
		b := wsTortureModeFor(0xabcd, step)
		if a != b {
			t.Errorf("non-deterministic at step %d: %v vs %v", step, a, b)
		}
	}
}

func TestWSTortureModeFor_DifferentSeedsDiverge(t *testing.T) {
	differ := false
	for step := 0; step < 16; step++ {
		if wsTortureModeFor(0x11, step) != wsTortureModeFor(0x22, step) {
			differ = true
			break
		}
	}
	if !differ {
		t.Error("two distinct seeds produced identical first 16 modes")
	}
}

func TestExpectedWSAccept_KnownVector(t *testing.T) {
	// RFC 6455 §1.3 worked example.
	got := expectedWSAccept("dGhlIHNhbXBsZSBub25jZQ==")
	want := "s3pPLMBiTxaQ9kYGzzhZRbK+xOo="
	if got != want {
		t.Errorf("expectedWSAccept: got %q, want %q", got, want)
	}
}

func TestSummariseWSTorture_FormatsAllCounters(t *testing.T) {
	s := wsSnapshot{
		Sent:             100,
		Upgraded:         95,
		HandshakeFail:    5,
		ClosedCorrectly:  90,
		AcceptedBadFrame: 4,
		HangNoClose:      1,
	}
	got := summariseWSTorture(s)
	for _, want := range []string{
		"ws_sent=100", "ws_upgraded=95", "ws_handshake_fail=5",
		"ws_closed=90", "ws_accepted_bad=4", "ws_hang=1",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: got %q", want, got)
		}
	}
}

// fakeWSServer is a tiny TCP accept loop where each conn:
//  1. reads the WS upgrade request,
//  2. replies with HTTP/1.1 101 + canonical headers (or 400 if
//     rejectHandshake is true),
//  3. delegates the post-upgrade torture-response to the policy.
type fakeWSServer struct {
	listener        net.Listener
	rejectHandshake bool
	policy          func(net.Conn)
	wg              sync.WaitGroup
}

func newFakeWSServer(t *testing.T, rejectHandshake bool, policy func(net.Conn)) *fakeWSServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeWSServer{listener: ln, rejectHandshake: rejectHandshake, policy: policy}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer func() { _ = c.Close() }()
				br := bufio.NewReader(c)
				// Read the HTTP request until empty line.
				var clientKey string
				for {
					line, err := br.ReadString('\n')
					if err != nil {
						return
					}
					if strings.HasPrefix(line, "Sec-WebSocket-Key:") {
						clientKey = strings.TrimSpace(strings.TrimPrefix(line, "Sec-WebSocket-Key:"))
					}
					if line == "\r\n" || line == "\n" {
						break
					}
				}
				if s.rejectHandshake {
					_, _ = c.Write([]byte("HTTP/1.1 400 Bad Request\r\nContent-Length: 0\r\n\r\n"))
					return
				}
				// Reply with a canonical 101.
				_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\n"))
				_, _ = c.Write([]byte("Upgrade: websocket\r\n"))
				_, _ = c.Write([]byte("Connection: Upgrade\r\n"))
				_, _ = c.Write([]byte("Sec-WebSocket-Accept: " + expectedWSAccept(clientKey) + "\r\n"))
				_, _ = c.Write([]byte("\r\n"))
				// Hand off post-upgrade to the policy.
				if s.policy != nil {
					s.policy(c)
				}
			}(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeWSServer) HostPort() string { return s.listener.Addr().String() }

func TestFireWSTorture_RejectedHandshakeCountsHandshakeFail(t *testing.T) {
	srv := newFakeWSServer(t, true, nil)
	var tally wsTally
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModeFragmentedReserved, &tally)
	s := tally.snapshot()
	if s.HandshakeFail != 1 {
		t.Errorf("HandshakeFail: got %d, want 1", s.HandshakeFail)
	}
	if s.Upgraded != 0 {
		t.Errorf("Upgraded: got %d, want 0 (handshake rejected)", s.Upgraded)
	}
}

func TestFireWSTorture_ServerClosesCleanlyCountsClosed(t *testing.T) {
	// Policy: after upgrade, immediately send a Close frame and quit.
	closeFrame := []byte{0x88, 0x02, 0x03, 0xEA} // FIN|close, len=2, code=1002
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096)) // drain the torture frame
		_, _ = c.Write(closeFrame)
	})
	var tally wsTally
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModeUnmaskedClient, &tally)
	s := tally.snapshot()
	if s.Upgraded != 1 {
		t.Errorf("Upgraded: got %d, want 1", s.Upgraded)
	}
	if s.ClosedCorrectly != 1 {
		t.Errorf("ClosedCorrectly: got %d, want 1", s.ClosedCorrectly)
	}
	if s.AcceptedBadFrame != 0 {
		t.Errorf("AcceptedBadFrame: got %d, want 0", s.AcceptedBadFrame)
	}
}

func TestFireWSTorture_EchoServerCountsAcceptedBadFrame(t *testing.T) {
	// Policy: after upgrade, echo whatever the client wrote (bug shape).
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		buf := make([]byte, 4096)
		n, _ := c.Read(buf)
		// Send a text frame back regardless — that's the bug we want
		// to catch.
		_, _ = c.Write([]byte{0x81, 0x05, 'h', 'e', 'l', 'l', 'o'})
		_ = n
	})
	var tally wsTally
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModeFragmentedReserved, &tally)
	s := tally.snapshot()
	if s.AcceptedBadFrame != 1 {
		t.Errorf("AcceptedBadFrame: got %d, want 1 (server echoed bad frame)", s.AcceptedBadFrame)
	}
}

// TestFireWSTorture_PongOnPingFloodCountsClosed pins the
// pong-is-not-a-violation rule for ModePingFlood. Pre-fix nightly
// 25993346060 hard-failed because auth_session_ratelimit-std's WS
// middleware replied to the flood with a Pong (RFC 6455 §5.5.2 —
// "the receiver of the Ping frame MUST send a Pong frame in
// response") before closing the connection. The walker was
// classifying any non-Close opcode as AcceptedBadFrame, mis-flagging
// spec-compliant Pong responses.
func TestFireWSTorture_PongOnPingFloodCountsClosed(t *testing.T) {
	// Policy: after upgrade, drain the client's ping flood, send a
	// single Pong back, then close. Pong is RFC-mandated.
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte{0x8A, 0x01, 'p'}) // FIN|pong, len=1, payload "p"
		_ = c.Close()
	})
	var tally wsTally
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModePingFlood, &tally)
	s := tally.snapshot()
	if s.ClosedCorrectly != 1 {
		t.Errorf("ClosedCorrectly: got %d, want 1 (pong-to-ping is spec-compliant)", s.ClosedCorrectly)
	}
	if s.AcceptedBadFrame != 0 {
		t.Errorf("AcceptedBadFrame: got %d, want 0 (pong is not a bad frame)", s.AcceptedBadFrame)
	}
}

// TestFireWSTorture_PongOnNonPingModeStillCountsBad confirms the
// carve-out is narrow: pong-as-response only excuses ModePingFlood.
// If the server emits a Pong frame in reply to, say, an unmasked
// client frame, that IS still suspicious — the spec response is a
// Close 1002, not a Pong.
func TestFireWSTorture_PongOnNonPingModeStillCountsBad(t *testing.T) {
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte{0x8A, 0x01, 'p'})
		_ = c.Close()
	})
	var tally wsTally
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModeUnmaskedClient, &tally)
	s := tally.snapshot()
	if s.AcceptedBadFrame != 1 {
		t.Errorf("AcceptedBadFrame: got %d, want 1 (pong is not a valid reply to unmasked-client)", s.AcceptedBadFrame)
	}
}

func TestFireWSTorture_AbruptCloseCountsClosed(t *testing.T) {
	// Policy: after upgrade, close the conn without sending anything.
	// Walker counts this as ClosedCorrectly (acceptable behaviour for
	// the worst torture modes — RST is just as good as Close).
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		_ = c.Close()
	})
	var tally wsTally
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModePingFlood, &tally)
	s := tally.snapshot()
	if s.ClosedCorrectly != 1 {
		t.Errorf("ClosedCorrectly: got %d, want 1 (RST is acceptable)", s.ClosedCorrectly)
	}
}

func TestFireWSTorture_DialFailureDoesNotIncrementOutcomes(t *testing.T) {
	var tally wsTally
	fireWSTorture(context.Background(), "127.0.0.1:1", "/ws", ModeFragmentedReserved, &tally)
	s := tally.snapshot()
	if s.Sent != 1 {
		t.Errorf("Sent: got %d, want 1", s.Sent)
	}
	if s.Upgraded > 0 || s.HandshakeFail > 0 || s.ClosedCorrectly > 0 || s.AcceptedBadFrame > 0 {
		t.Errorf("dial failure leaked into outcome counters: %+v", s)
	}
}

func TestRunWSTortureWalker_FiresMultipleRequests(t *testing.T) {
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte{0x88, 0x02, 0x03, 0xEA}) // close 1002
	})
	var tally wsTally
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	runWSTortureWalker(ctx, srv.HostPort(), "/ws", 0xfeed, 50*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.Sent < 2 {
		t.Errorf("Sent: got %d, want >= 2", s.Sent)
	}
}
