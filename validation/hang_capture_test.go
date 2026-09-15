package validation

import (
	"context"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// The per-event capture the v1.5.11 soak did not have (celeris#588): every
// h2c_hang and every ws_handshake_fail must leave the instant, the per-leg
// elapsed, the verbatim error and the walker's addresses in the cell
// document. These tests fire the real walkers at fake servers shaped like
// the failure classes and read the ring back; each has its negative
// control, a fast server whose fire must NOT be in the ring, because a
// capture that files everything proves nothing about the threshold.

// TestFireH2CChurn_SlowCloseLandsInRingWithError: a server that accepts,
// holds the request for over a second and then closes without answering.
// The walker classifies it hang-eof; the ring must carry the "EOF" string
// and the elapsed, and the histogram must file the read in the 1-2 s
// bucket -- the bucket a #493-residual stall would fill.
func TestFireH2CChurn_SlowCloseLandsInRingWithError(t *testing.T) {
	const hold = 1200 * time.Millisecond
	srv := newFakeH2CServer(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		time.Sleep(hold)
		_ = c.Close()
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.Hang != 1 || s.HangEOF != 1 {
		t.Fatalf("expected one hang-eof, got %+v", s)
	}
	if s.SlowReadsTotal != 1 || len(s.SlowReads) != 1 {
		t.Fatalf("the slow fire must be in the ring: total=%d ring=%v", s.SlowReadsTotal, s.SlowReads)
	}
	f := s.SlowReads[0]
	if f.Outcome != "hang-eof" {
		t.Errorf("outcome = %q, want hang-eof", f.Outcome)
	}
	if f.Err != "EOF" {
		t.Errorf("err = %q, want the verbatim \"EOF\" (the string six reviews could not recover from the v1.5.11 artifact)", f.Err)
	}
	if f.ReadMs < hold.Milliseconds() {
		t.Errorf("read_ms = %d, want >= %d", f.ReadMs, hold.Milliseconds())
	}
	if !strings.HasPrefix(f.LocalAddr, "127.0.0.1:") || f.RemoteAddr != srv.HostPort() {
		t.Errorf("addresses lost: local=%q remote=%q (server %s)", f.LocalAddr, f.RemoteAddr, srv.HostPort())
	}
	if f.NRead != 0 {
		t.Errorf("n_read = %d, want 0 for a never-answered request", f.NRead)
	}
	if s.Latency.Read.Lt2s != 1 || s.Latency.Read.Total() != 1 {
		t.Errorf("read histogram must file the 1.2 s read in lt_2s: %+v", s.Latency.Read)
	}
	if s.Latency.Dial.Total() != 1 || s.Latency.Write.Total() != 1 {
		t.Errorf("dial/write legs not filed: %+v", s.Latency)
	}
	if s.HangMaxElapsedMs < hold.Milliseconds() {
		t.Errorf("the pre-existing max_elapsed must keep its meaning: %d", s.HangMaxElapsedMs)
	}
}

// TestFireH2CChurn_SlowButAnsweredLandsInRingAsDeclined: the case the hang
// counters are blind to by construction -- a stall shorter than the 20 s
// budget that then answers. It is not a hang (h2c_hang stays 0), it IS a
// slow read, and the ring keeps it with outcome=declined and no error.
func TestFireH2CChurn_SlowButAnsweredLandsInRingAsDeclined(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		time.Sleep(1100 * time.Millisecond)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.Hang != 0 || s.Declined != 1 {
		t.Fatalf("a slow 200 is declined, not a hang: %+v", s)
	}
	if len(s.SlowReads) != 1 || s.SlowReads[0].Outcome != "declined" || s.SlowReads[0].Err != "" {
		t.Fatalf("slow answered read must be in the ring as declined with no error: %v", s.SlowReads)
	}
	if s.SlowReads[0].NRead == 0 {
		t.Fatalf("n_read must carry the bytes the answer had: %+v", s.SlowReads[0])
	}
}

// TestFireH2CChurn_FastFireDoesNotLandInRing is the negative control: the
// same walker against a server that answers at once files the histogram
// and leaves the ring empty. Without this, the previous tests would pass
// against a capture that keeps every fire.
func TestFireH2CChurn_FastFireDoesNotLandInRing(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	})
	var tally h2cTally
	for i := 0; i < 5; i++ {
		fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	}
	s := tally.snapshot()
	if s.Declined != 5 {
		t.Fatalf("declined = %d, want 5", s.Declined)
	}
	if s.SlowReadsTotal != 0 || s.SlowReads != nil {
		t.Fatalf("fast fires must NOT be in the ring: total=%d ring=%v", s.SlowReadsTotal, s.SlowReads)
	}
	if s.Latency.Read.Total() != 5 || s.Latency.Read.Lt1s+s.Latency.Read.Lt100ms != 5 {
		t.Fatalf("histogram must still file every fast read under 1 s: %+v", s.Latency.Read)
	}
	if s.Latency.Read.Timeout != 0 {
		t.Fatalf("no read timed out: %+v", s.Latency.Read)
	}
	// The rst-before-read mode has no read leg: dial and write only.
	var rst h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTBeforeRead, &rst)
	if l := rst.snapshot().Latency; l.Read.Total() != 0 || l.Dial.Total() != 1 || l.Write.Total() != 1 {
		t.Fatalf("rst-before-read must file dial+write and no read: %+v", l)
	}
}

// TestFireH2CChurn_DialFailureIsCountedAndBucketed: a dial that fails used
// to return with no trace at all. It must now count as h2c_dial_fail, file
// the dial leg, and leave the outcome counters and the ring alone.
func TestFireH2CChurn_DialFailureIsCountedAndBucketed(t *testing.T) {
	var tally h2cTally
	fireH2CChurn(context.Background(), "127.0.0.1:1", ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.DialFail != 1 {
		t.Fatalf("dial_fail = %d, want 1", s.DialFail)
	}
	if s.Latency.Dial.Total() != 1 || s.Latency.Write.Total() != 0 || s.Latency.Read.Total() != 0 {
		t.Fatalf("only the dial leg is filed on a dial failure: %+v", s.Latency)
	}
	if s.Hang != 0 || s.SlowReads != nil {
		t.Fatalf("a dial failure is not a hang and not a slow read: %+v", s)
	}
}

// slowWSHandler answers the WS upgrade GET after hold with status, over
// net/http (the task's httptest shape). net/http's server never answers
// 101 by itself, so this covers the non-101 classes; the 101 and timeout
// classes use the raw fake below.
func slowWSHandler(hold time.Duration, status int) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(hold)
		w.WriteHeader(status)
	})
}

// TestFireWSTorture_SlowRejectLandsInRingWithStatus: an httptest server
// that sleeps past the threshold and then rejects the upgrade with 400.
// handshake-fail-status, with the status line kept.
func TestFireWSTorture_SlowRejectLandsInRingWithStatus(t *testing.T) {
	const hold = 1200 * time.Millisecond
	srv := httptest.NewServer(slowWSHandler(hold, http.StatusBadRequest))
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")
	var tally wsTally
	fireWSTorture(context.Background(), hostPort, "/ws", ModeUnmaskedClient, &tally)
	s := tally.snapshot()
	if s.HandshakeFail != 1 || s.HandshakeFailStatus != 1 {
		t.Fatalf("expected one handshake-fail-status, got %+v", s)
	}
	if len(s.SlowReads) != 1 {
		t.Fatalf("the slow handshake must be in the ring: %v", s.SlowReads)
	}
	f := s.SlowReads[0]
	if f.Outcome != "handshake-fail-status" || !strings.HasPrefix(f.Status, "HTTP/1.1 400") {
		t.Errorf("ring entry lost the outcome or the status line: %+v", f)
	}
	if f.ReadMs < hold.Milliseconds() || f.Err != "" {
		t.Errorf("read_ms=%d err=%q, want >= %d and no error", f.ReadMs, f.Err, hold.Milliseconds())
	}
	if !strings.HasPrefix(f.LocalAddr, "127.0.0.1:") || f.RemoteAddr != hostPort {
		t.Errorf("addresses lost: %+v", f)
	}
	if s.Latency.Read.Lt2s != 1 {
		t.Errorf("read histogram: %+v", s.Latency.Read)
	}
}

// TestFireWSTorture_SilentServerLandsInRingAsTimeout: a server that
// accepts and never answers. The 2 s handshake budget expires:
// handshake-fail-timeout, err names the i/o timeout, read leg in the
// timeout bucket, and the record says the READ leg expired (dial and write
// were instant) -- the "which leg" the design asked for.
func TestFireWSTorture_SilentServerLandsInRingAsTimeout(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		time.Sleep(wsMaxHold + 2*time.Second)
		_ = c.Close()
	})
	var tally wsTally
	start := time.Now()
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModePingFlood, &tally)
	if el := time.Since(start); el > wsMaxHold+time.Second {
		t.Fatalf("fire took %v, the 2 s hold bound is not being honoured", el)
	}
	s := tally.snapshot()
	if s.HandshakeFail != 1 || s.HandshakeFailTimeout != 1 {
		t.Fatalf("expected one handshake-fail-timeout, got %+v", s)
	}
	if len(s.SlowReads) != 1 {
		t.Fatalf("ring: %v", s.SlowReads)
	}
	f := s.SlowReads[0]
	if f.Outcome != "handshake-fail-timeout" || !strings.Contains(f.Err, "i/o timeout") {
		t.Errorf("ring entry: %+v", f)
	}
	if f.DialMs > 500 || f.WriteMs > 500 || f.ReadMs < wsMaxHold.Milliseconds()-100 {
		t.Errorf("the record must say the read leg expired, not dial or write: %+v", f)
	}
	if s.Latency.Read.Timeout != 1 || s.Latency.Dial.Timeout != 0 {
		t.Errorf("histogram legs: %+v", s.Latency)
	}
}

// TestFireWSTorture_FastHandshakeDoesNotLandInRing is the WS negative
// control: a server that answers 101 at once files the histogram and
// leaves the ring empty.
func TestFireWSTorture_FastHandshakeDoesNotLandInRing(t *testing.T) {
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte{0x88, 0x02, 0x03, 0xEA})
	})
	var tally wsTally
	for i := 0; i < 5; i++ {
		fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModeUnmaskedClient, &tally)
	}
	s := tally.snapshot()
	if s.Upgraded != 5 || s.HandshakeFail != 0 {
		t.Fatalf("expected 5 clean upgrades: %+v", s)
	}
	if s.SlowReadsTotal != 0 || s.SlowReads != nil {
		t.Fatalf("fast handshakes must NOT be in the ring: total=%d ring=%v", s.SlowReadsTotal, s.SlowReads)
	}
	if s.Latency.Read.Total() != 5 || s.Latency.Read.Lt100ms+s.Latency.Read.Lt1s != 5 {
		t.Fatalf("histogram must file every handshake under 1 s: %+v", s.Latency.Read)
	}
	// A fast rejection is filed too, and stays out of the ring.
	rej := newFakeWSServer(t, true, nil)
	var rt wsTally
	fireWSTorture(context.Background(), rej.HostPort(), "/ws", ModeUnmaskedClient, &rt)
	if rs := rt.snapshot(); rs.HandshakeFailStatus != 1 || rs.SlowReads != nil || rs.Latency.Read.Total() != 1 {
		t.Fatalf("fast rejection: %+v", rs)
	}
}

// TestFireWSTorture_DialFailureIsCountedAndBucketed mirrors the h2c case.
func TestFireWSTorture_DialFailureIsCountedAndBucketed(t *testing.T) {
	var tally wsTally
	fireWSTorture(context.Background(), "127.0.0.1:1", "/ws", ModeUnmaskedClient, &tally)
	s := tally.snapshot()
	if s.DialFail != 1 || s.Latency.Dial.Total() != 1 || s.Latency.Read.Total() != 0 {
		t.Fatalf("dial failure: %+v", s)
	}
	if s.HandshakeFail != 0 || s.SlowReads != nil {
		t.Fatalf("a dial failure is not a handshake failure and not a slow read: %+v", s)
	}
}

// TestTier1SummaryCarriesTheCapture: the walker fields reach the report
// shape the matrix writes. The completeness test guards the int64 maps;
// this guards the typed fields it exempts.
func TestTier1SummaryCarriesTheCapture(t *testing.T) {
	var snap tier1TallySnapshot
	snap.H2CChurn.Sent = 1
	snap.H2CChurn.DialFail = 2
	snap.H2CChurn.SlowReadsTotal = 3
	snap.H2CChurn.Latency.Read.Lt5s = 4
	snap.WSTorture.Sent = 1
	snap.WSTorture.WriteFail = 5
	snap.ReadyAt = "2026-09-13T04:10:00Z"
	snap.RefappStderrTail = []string{"WARN accepted fd exceeds conn table cap; dropping"}
	sum := snap.Tier1Summary()
	if sum.H2CChurn["h2c_dial_fail"] != 2 || sum.H2CChurn["h2c_slow_reads_total"] != 3 || sum.WSTorture["ws_write_fail"] != 5 {
		t.Fatalf("informational counters not projected: %v / %v", sum.H2CChurn, sum.WSTorture)
	}
	if sum.H2CLatency == nil || sum.H2CLatency.Read.Lt5s != 4 {
		t.Fatalf("h2c latency not projected: %+v", sum.H2CLatency)
	}
	if sum.WSLatency == nil {
		t.Fatal("ws latency not projected for a slice that sent")
	}
	if sum.ReadyAt != snap.ReadyAt || len(sum.RefappStderrTail) != 1 {
		t.Fatalf("ready_at / stderr tail not projected: %q %v", sum.ReadyAt, sum.RefappStderrTail)
	}
	// A slice that never ran omits its histogram rather than shipping zeros.
	var quiet tier1TallySnapshot
	if q := quiet.Tier1Summary(); q.H2CLatency != nil || q.WSLatency != nil || q.H2CSlowReads != nil {
		t.Fatalf("a dormant slice must omit its capture: %+v", q)
	}
}
