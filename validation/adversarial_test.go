package validation

import (
	"bytes"
	"context"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdversarialMode_String(t *testing.T) {
	cases := map[adversarialMode]string{
		ModeBadChunks:       "bad-chunks",
		ModeOversizedHeader: "oversized-header",
		ModeNULInHeader:     "nul-in-header",
		ModeCRLFInjection:   "crlf-injection",
		ModeSlowloris:       "slowloris",
		ModeDoubleCL:        "double-content-length",
		adversarialMode(99): "unknown",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("(%d).String(): got %q, want %q", m, got, want)
		}
	}
}

func TestAdversarialPayload_BadChunksContainsNegativeLen(t *testing.T) {
	p := adversarialPayload(ModeBadChunks)
	if !bytes.Contains(p, []byte("-1\r\n")) {
		t.Errorf("expected negative chunk size, got %q", p)
	}
	if !bytes.Contains(p, []byte("Transfer-Encoding: chunked")) {
		t.Errorf("expected chunked header, got %q", p)
	}
}

func TestAdversarialPayload_OversizedHeaderIsLarge(t *testing.T) {
	p := adversarialPayload(ModeOversizedHeader)
	if len(p) < 65536 {
		t.Errorf("oversized payload only %d bytes, want >= 64KiB", len(p))
	}
}

func TestAdversarialPayload_NULInHeader(t *testing.T) {
	p := adversarialPayload(ModeNULInHeader)
	if !bytes.Contains(p, []byte{0x00}) {
		t.Errorf("expected NUL byte in payload")
	}
}

func TestAdversarialPayload_CRLFInjection(t *testing.T) {
	p := adversarialPayload(ModeCRLFInjection)
	if !bytes.Contains(p, []byte("X-Smuggled: yes")) {
		t.Errorf("expected smuggled header line, got %q", p)
	}
}

func TestAdversarialPayload_DoubleCL(t *testing.T) {
	p := adversarialPayload(ModeDoubleCL)
	if bytes.Count(p, []byte("Content-Length:")) != 2 {
		t.Errorf("expected exactly 2 Content-Length headers, got %q", p)
	}
}

func TestAdversarialModeFor_Deterministic(t *testing.T) {
	// Same seed + step → same mode every time.
	for step := 0; step < 5; step++ {
		a := adversarialModeFor(0xc0ffee, step)
		b := adversarialModeFor(0xc0ffee, step)
		if a != b {
			t.Errorf("non-deterministic at step %d: %v vs %v", step, a, b)
		}
	}
}

func TestAdversarialModeFor_DifferentSeedsDiverge(t *testing.T) {
	// Two different seeds should produce different sequences at SOME
	// step within the first 16. Otherwise the seed material isn't
	// actually mixing in.
	differ := false
	for step := 0; step < 16; step++ {
		if adversarialModeFor(0xc0ffee, step) != adversarialModeFor(0xbeef, step) {
			differ = true
			break
		}
	}
	if !differ {
		t.Error("two distinct seeds produced identical first 16 modes")
	}
}

func TestSummariseAdversarial_FormatsAllCounters(t *testing.T) {
	s := adversarialSnapshot{
		Sent:             100,
		WellRejected:     95,
		WrongAccepted:    3,
		HangUntilTimeout: 2,
	}
	got := summariseAdversarial(s)
	for _, want := range []string{"adv_sent=100", "adv_rejected=95", "adv_accepted=3", "adv_hang=2"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: got %q", want, got)
		}
	}
}

// fakeAdversarialServer is a tiny TCP accept loop that handles each
// incoming connection per the configured policy: "well-reject" closes
// immediately, "accept" returns HTTP/1.1 200, "hang" reads forever.
// Used by the integration test to verify the walker counts each
// outcome correctly.
type fakeAdversarialServer struct {
	listener net.Listener
	policy   func(net.Conn)
	wg       sync.WaitGroup
}

func newFakeAdversarialServer(t *testing.T, policy func(net.Conn)) *fakeAdversarialServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeAdversarialServer{listener: ln, policy: policy}
	s.wg.Add(1)
	go func() {
		defer s.wg.Done()
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go policy(conn)
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		s.wg.Wait()
	})
	return s
}

func (s *fakeAdversarialServer) HostPort() string {
	return s.listener.Addr().String()
}

func TestFireAdversarial_RejectingServerCountsWellRejected(t *testing.T) {
	srv := newFakeAdversarialServer(t, func(c net.Conn) {
		// Read one byte (so any client write is consumed) then close.
		buf := make([]byte, 1)
		_, _ = c.Read(buf)
		_ = c.Close()
	})
	var tally adversarialTally
	// Use a non-slowloris mode so we hit the standard read path.
	fireAdversarial(context.Background(), srv.HostPort(), ModeBadChunks, &tally)
	s := tally.snapshot()
	if s.WellRejected != 1 {
		t.Errorf("WellRejected: got %d, want 1", s.WellRejected)
	}
	if s.WrongAccepted != 0 {
		t.Errorf("WrongAccepted: got %d, want 0", s.WrongAccepted)
	}
}

func TestFireAdversarial_AcceptingServerCountsAsBug(t *testing.T) {
	srv := newFakeAdversarialServer(t, func(c net.Conn) {
		// Pretend we accepted the malformed request — return 200.
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	})
	var tally adversarialTally
	fireAdversarial(context.Background(), srv.HostPort(), ModeCRLFInjection, &tally)
	s := tally.snapshot()
	if s.WrongAccepted != 1 {
		t.Errorf("WrongAccepted: got %d, want 1 (server returned 200 to malformed)", s.WrongAccepted)
	}
}

func TestRunAdversarialWalker_FiresMultipleRequests(t *testing.T) {
	// Server immediately closes — every cell is well-rejected.
	srv := newFakeAdversarialServer(t, func(c net.Conn) {
		_ = c.Close()
	})
	var tally adversarialTally
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	// 50ms tick → ~6 requests in 300ms.
	runAdversarialWalker(ctx, srv.HostPort(), 0x1, 50*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.Sent < 2 {
		t.Errorf("Sent: got %d, want >= 2", s.Sent)
	}
}

func TestFireAdversarial_DialFailureDoesNotIncrement(t *testing.T) {
	// No server listening on the chosen port. The walker shouldn't
	// fold dial-failure into wellRejected/accepted — that would
	// silently inflate the rejection rate.
	var tally adversarialTally
	tally.sent.Store(0) // explicit reset
	fireAdversarial(context.Background(), "127.0.0.1:1", ModeBadChunks, &tally)
	s := tally.snapshot()
	if s.WellRejected > 0 {
		t.Errorf("dial failure shouldn't count as rejection, got %d", s.WellRejected)
	}
	if s.WrongAccepted > 0 {
		t.Errorf("dial failure shouldn't count as acceptance, got %d", s.WrongAccepted)
	}
	// Sent IS incremented (we tried).
	if s.Sent != 1 {
		t.Errorf("Sent: got %d, want 1", s.Sent)
	}
}
