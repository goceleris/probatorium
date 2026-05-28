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

func TestH2CChurnMode_String(t *testing.T) {
	cases := map[h2cChurnMode]string{
		ChurnRSTBeforeRead:  "rst-before-read",
		ChurnRSTAfter101:    "rst-after-101",
		ChurnPartialPreface: "partial-preface",
		h2cChurnMode(99):    "unknown",
	}
	for m, want := range cases {
		if got := m.String(); got != want {
			t.Errorf("(%d).String(): got %q, want %q", m, got, want)
		}
	}
}

func TestH2CUpgradePreamble_HasRequiredHeaders(t *testing.T) {
	p := h2cUpgradePreamble("example.com:8080")
	required := []string{
		"GET / HTTP/1.1\r\n",
		"Host: example.com:8080\r\n",
		"Connection: Upgrade, HTTP2-Settings\r\n",
		"Upgrade: h2c\r\n",
		"HTTP2-Settings: AAQAAP__AAMAAABk\r\n",
	}
	for _, want := range required {
		if !bytes.Contains(p, []byte(want)) {
			t.Errorf("preamble missing %q: got %q", want, p)
		}
	}
	if !bytes.HasSuffix(p, []byte("\r\n\r\n")) {
		t.Errorf("preamble must end with empty line, got %q", p)
	}
}

func TestH2CChurnModeFor_Deterministic(t *testing.T) {
	for step := 0; step < 8; step++ {
		a := h2cChurnModeFor(0xfeed, step)
		b := h2cChurnModeFor(0xfeed, step)
		if a != b {
			t.Errorf("non-deterministic at step %d: %v vs %v", step, a, b)
		}
	}
}

func TestH2CChurnModeFor_DifferentSeedsDiverge(t *testing.T) {
	differ := false
	for step := 0; step < 16; step++ {
		if h2cChurnModeFor(0xa1, step) != h2cChurnModeFor(0xb2, step) {
			differ = true
			break
		}
	}
	if !differ {
		t.Error("two distinct seeds produced identical first 16 modes")
	}
}

func TestSummariseH2CChurn_FormatsAllCounters(t *testing.T) {
	s := h2cSnapshot{Sent: 50, Upgraded: 12, Declined: 30, Crashed: 1, Hang: 7}
	got := summariseH2CChurn(s)
	for _, want := range []string{"h2c_sent=50", "h2c_upgraded=12", "h2c_declined=30", "h2c_crashed=1", "h2c_hang=7"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: got %q", want, got)
		}
	}
}

// fakeH2CServer is a tiny TCP accept loop where each conn is handled
// per the configured policy. Same shape as fakeAdversarialServer but
// kept separate so the test policies can compose without overloading
// the adversarial fake.
type fakeH2CServer struct {
	listener net.Listener
	policy   func(net.Conn)
	wg       sync.WaitGroup
}

func newFakeH2CServer(t *testing.T, policy func(net.Conn)) *fakeH2CServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeH2CServer{listener: ln, policy: policy}
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

func (s *fakeH2CServer) HostPort() string { return s.listener.Addr().String() }

func TestFireH2CChurn_UpgradingServerCountsUpgraded(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		// Reply with the canonical 101 Switching Protocols line.
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: h2c\r\n\r\n"))
		// Then a tiny SETTINGS frame placeholder (9-byte frame header
		// with type=4, flags=0, stream_id=0, len=0). Real H2 client
		// would parse this; we don't care.
		_, _ = c.Write([]byte{0, 0, 0, 4, 0, 0, 0, 0, 0})
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.Upgraded != 1 {
		t.Errorf("Upgraded: got %d, want 1", s.Upgraded)
	}
	if s.Declined != 0 || s.Crashed != 0 {
		t.Errorf("expected only Upgraded, got %+v", s)
	}
}

func TestFireH2CChurn_DecliningServerCountsDeclined(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		// Server doesn't speak h2c — replies with plain 200 to the GET.
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 2\r\n\r\nok"))
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTBeforeRead, &tally)
	// ChurnRSTBeforeRead closes WITHOUT reading the response, so
	// neither Upgraded nor Declined increments — only Sent. Verify the
	// counter for a mode that DOES read instead.
	if tally.snapshot().Sent != 1 {
		t.Errorf("Sent: got %d, want 1", tally.snapshot().Sent)
	}

	// Re-fire with a mode that reads the response.
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.Declined != 1 {
		t.Errorf("Declined: got %d, want 1 (server returned 200 to upgrade)", s.Declined)
	}
}

func TestFireH2CChurn_GarbageResponseCountsCrashed(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		// Reply with non-HTTP babble.
		_, _ = c.Write([]byte("oh no the server crashed\r\n"))
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.Crashed != 1 {
		t.Errorf("Crashed: got %d, want 1 (server returned non-HTTP babble)", s.Crashed)
	}
}

func TestFireH2CChurn_SilentServerCountsHang(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		// Accept, drain, then sit forever. The 2s timeout in
		// fireH2CChurn should kick in.
		_, _ = c.Read(make([]byte, 4096))
		// Sleep longer than fireH2CChurn's 2s timeout.
		time.Sleep(5 * time.Second)
		_ = c.Close()
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnPartialPreface, &tally)
	s := tally.snapshot()
	if s.Hang != 1 {
		t.Errorf("Hang: got %d, want 1 (server stayed silent past timeout)", s.Hang)
	}
}

// TestFireH2CChurn_SlowButCorrectServerNotHang pins the v1.4.10
// follow-up fix: a refapp that responds slowly (e.g. observability
// /api/error under load on arm64, taking ~5s) must NOT be flagged as
// h2c_hang. Pre-fix the walker only waited 2s, so soak 26333090001
// recorded 18K+ false-positive hang events on observability/
// static_swagger_proxy arm64 cells. Post-fix the read budget is 10s.
//
// Server takes 4s to respond — well within the 10s budget. Walker
// should classify as declined (200 OK to the upgrade request), NOT
// hang.
func TestFireH2CChurn_SlowButCorrectServerNotHang(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		// 4s delay: longer than the OLD 2s budget, well inside the new 10s.
		time.Sleep(4 * time.Second)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.Hang != 0 {
		t.Errorf("slow-but-correct server must NOT count as hang, got %d", s.Hang)
	}
	if s.Declined != 1 {
		t.Errorf("slow 200 response should count as declined, got %d", s.Declined)
	}
}

// TestFireH2CChurn_RSTBeforeReadCountsIntentional verifies that
// ChurnRSTBeforeRead increments the intentionalRST counter (workload
// intent) and NOT the hang counter (server-wedge signal). Pre-split,
// these were conflated in `h2c_hang` — the 3-day soak's 317K h2c_hang
// count on amd64 was almost entirely walker-intentional RSTs rather
// than real server hangs.
func TestFireH2CChurn_RSTBeforeReadCountsIntentional(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		// Drain the preamble + 101 reply (irrelevant — walker closes
		// before reading anything).
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTBeforeRead, &tally)
	s := tally.snapshot()
	if s.IntentionalRST != 1 {
		t.Errorf("IntentionalRST: got %d, want 1", s.IntentionalRST)
	}
	if s.Hang != 0 {
		t.Errorf("Hang: got %d, want 0 (this should NOT count as a server hang)", s.Hang)
	}
}

func TestFireH2CChurn_DialFailureDoesNotIncrementOutcomes(t *testing.T) {
	// No server listening — every dial fails. Sent IS incremented (we
	// tried) but Upgraded/Declined/Crashed/Hang must all stay zero.
	var tally h2cTally
	fireH2CChurn(context.Background(), "127.0.0.1:1", ChurnRSTBeforeRead, &tally)
	s := tally.snapshot()
	if s.Sent != 1 {
		t.Errorf("Sent: got %d, want 1", s.Sent)
	}
	if s.Upgraded > 0 || s.Declined > 0 || s.Crashed > 0 || s.Hang > 0 {
		t.Errorf("dial failure leaked into outcome counters: %+v", s)
	}
}

func TestRunH2CChurnWalker_FiresMultipleRequests(t *testing.T) {
	// Server replies 101 then closes — every request counts as
	// upgraded.
	srv := newFakeH2CServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: h2c\r\n\r\n"))
	})
	var tally h2cTally
	ctx, cancel := context.WithTimeout(context.Background(), 350*time.Millisecond)
	defer cancel()
	// 50ms tick → ~6 churns in 350ms (allowing for the per-fire 2s
	// timeout cap to fire well inside that on this fake server).
	runH2CChurnWalker(ctx, srv.HostPort(), 0xa1b2, 50*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.Sent < 2 {
		t.Errorf("Sent: got %d, want >= 2", s.Sent)
	}
}
