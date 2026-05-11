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

func TestSSEGetRequest_HasRequiredHeaders(t *testing.T) {
	p := sseGetRequest("example.com:8080", "/events")
	required := []string{
		"GET /events HTTP/1.1\r\n",
		"Host: example.com:8080\r\n",
		"Accept: text/event-stream\r\n",
		"Cache-Control: no-cache\r\n",
		"Connection: keep-alive\r\n",
	}
	for _, want := range required {
		if !bytes.Contains(p, []byte(want)) {
			t.Errorf("SSE GET missing %q", want)
		}
	}
}

func TestSummariseSSEKill_FormatsAllCounters(t *testing.T) {
	s := sseSnapshot{
		Sent:              50,
		Established:       45,
		EventsRead:        300,
		KilledMidStream:   40,
		ServerClosedEarly: 5,
		HandshakeFail:     5,
	}
	got := summariseSSEKill(s)
	for _, want := range []string{
		"sse_sent=50", "sse_established=45", "sse_events=300",
		"sse_killed=40", "sse_early_close=5", "sse_handshake_fail=5",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("summary missing %q: got %q", want, got)
		}
	}
}

// fakeSSEServer is a TCP accept loop that handles each conn per the
// configured policy. Used to drive fireSSEKill through each
// classification path deterministically.
type fakeSSEServer struct {
	listener net.Listener
	policy   func(net.Conn)
	wg       sync.WaitGroup
}

func newFakeSSEServer(t *testing.T, policy func(net.Conn)) *fakeSSEServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSSEServer{listener: ln, policy: policy}
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

func (s *fakeSSEServer) HostPort() string { return s.listener.Addr().String() }

// drainRequest reads bytes from c until it sees the end of HTTP
// headers (CRLFCRLF), then returns. Tests need this so the fake
// server can respond AFTER the client has finished writing.
func drainRequest(c net.Conn) {
	buf := make([]byte, 1)
	hist := make([]byte, 0, 128)
	for {
		if _, err := c.Read(buf); err != nil {
			return
		}
		hist = append(hist, buf[0])
		if len(hist) >= 4 && string(hist[len(hist)-4:]) == "\r\n\r\n" {
			return
		}
	}
}

func TestFireSSEKill_NonSSEServerCountsHandshakeFail(t *testing.T) {
	// Server returns 200 with plain text — no event-stream
	// Content-Type, so the walker classifies as handshake-fail.
	srv := newFakeSSEServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		drainRequest(c)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2\r\n\r\nok"))
	})
	var tally sseTally
	fireSSEKill(context.Background(), srv.HostPort(), "/events", 100*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.HandshakeFail != 1 {
		t.Errorf("HandshakeFail: got %d, want 1 (non-SSE Content-Type)", s.HandshakeFail)
	}
	if s.Established != 0 {
		t.Errorf("Established: got %d, want 0", s.Established)
	}
}

func TestFireSSEKill_404CountsHandshakeFail(t *testing.T) {
	srv := newFakeSSEServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		drainRequest(c)
		_, _ = c.Write([]byte("HTTP/1.1 404 Not Found\r\nContent-Length: 0\r\n\r\n"))
	})
	var tally sseTally
	fireSSEKill(context.Background(), srv.HostPort(), "/events", 100*time.Millisecond, &tally)
	if tally.snapshot().HandshakeFail != 1 {
		t.Errorf("HandshakeFail: got %d, want 1 (404 response)", tally.snapshot().HandshakeFail)
	}
}

func TestFireSSEKill_RealSSECountsKilled(t *testing.T) {
	// Server replies with valid SSE then streams ticks every 50ms.
	// The walker holds for 250ms then RSTs — should classify as
	// killedMidStream.
	srv := newFakeSSEServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		drainRequest(c)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\n\r\n"))
		tick := time.NewTicker(50 * time.Millisecond)
		defer tick.Stop()
		n := 0
		for range tick.C {
			n++
			if _, err := c.Write([]byte("event: tick\ndata: " + strings.Repeat("x", 4) + "\n\n")); err != nil {
				return
			}
			if n > 30 {
				return
			}
		}
	})
	var tally sseTally
	fireSSEKill(context.Background(), srv.HostPort(), "/events", 250*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.Established != 1 {
		t.Errorf("Established: got %d, want 1", s.Established)
	}
	if s.KilledMidStream != 1 {
		t.Errorf("KilledMidStream: got %d, want 1", s.KilledMidStream)
	}
	if s.EventsRead < 1 {
		t.Errorf("EventsRead: got %d, want >= 1 (we held for 250ms, server ticks 50ms)", s.EventsRead)
	}
}

func TestFireSSEKill_ServerClosesEarlyCountsEarlyClose(t *testing.T) {
	// Server replies with valid SSE then immediately closes.
	srv := newFakeSSEServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		drainRequest(c)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"))
		// Don't send any events; close right away.
	})
	var tally sseTally
	// Hold for 500ms — much longer than the server takes to close, so
	// the walker should observe the early close.
	fireSSEKill(context.Background(), srv.HostPort(), "/events", 500*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.Established != 1 {
		t.Errorf("Established: got %d, want 1", s.Established)
	}
	if s.ServerClosedEarly != 1 {
		t.Errorf("ServerClosedEarly: got %d, want 1", s.ServerClosedEarly)
	}
	if s.KilledMidStream != 0 {
		t.Errorf("KilledMidStream: got %d, want 0 (server cut us off first)", s.KilledMidStream)
	}
}

func TestFireSSEKill_DialFailureDoesNotIncrementOutcomes(t *testing.T) {
	var tally sseTally
	fireSSEKill(context.Background(), "127.0.0.1:1", "/events", 100*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.Sent != 1 {
		t.Errorf("Sent: got %d, want 1", s.Sent)
	}
	if s.Established > 0 || s.KilledMidStream > 0 || s.ServerClosedEarly > 0 || s.HandshakeFail > 0 {
		t.Errorf("dial failure leaked into outcome counters: %+v", s)
	}
}

func TestRunSSEKillWalker_FiresMultipleStreams(t *testing.T) {
	srv := newFakeSSEServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		drainRequest(c)
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\n\r\n"))
		// Hold the conn until the client RSTs.
		_, _ = c.Read(make([]byte, 1))
	})
	var tally sseTally
	ctx, cancel := context.WithTimeout(context.Background(), 700*time.Millisecond)
	defer cancel()
	// 100ms tick → ~6 fires in 700ms (but per-fire hold is 50-1500ms
	// so effective rate is governed by hold + handshake budget).
	runSSEKillWalker(ctx, srv.HostPort(), "/events", 0x5e5e, 100*time.Millisecond, &tally)
	s := tally.snapshot()
	if s.Sent < 1 {
		t.Errorf("Sent: got %d, want >= 1", s.Sent)
	}
}
