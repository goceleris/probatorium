package validation

import (
	"context"
	"net"
	"testing"
	"time"
)

// sseTestServer accepts one conn, answers the SSE handshake, streams events
// until release is closed, then closes the conn.
func sseTestServer(t *testing.T, release <-chan struct{}) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer func() { _ = c.Close() }()
		buf := make([]byte, 4096)
		_, _ = c.Read(buf) // consume the GET
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\n\r\n"))
		tk := time.NewTicker(20 * time.Millisecond)
		defer tk.Stop()
		for {
			select {
			case <-release:
				return
			case <-tk.C:
				if _, err := c.Write([]byte("data: tick\n\n")); err != nil {
					return
				}
			}
		}
	}()
	return ln.Addr().String()
}

// A stream that ends because the walker's context ended (tier budget, cell
// teardown) is a deadline cut, not a server defect. Before the fix exactly one
// in-flight stream per cell was booked as sse_server_closed_early.
func TestFireSSEKill_DeadlineCutIsNotServerClosedEarly(t *testing.T) {
	release := make(chan struct{})
	addr := sseTestServer(t, release)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var tally sseTally
	done := make(chan struct{})
	go func() { fireSSEKill(ctx, addr, "/events", 10*time.Second, &tally); close(done) }()
	time.Sleep(200 * time.Millisecond) // established, a few events read
	cancel()                           // tier budget exhausted ...
	close(release)                     // ... then the cell is torn down
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fireSSEKill did not return")
	}
	s := tally.snapshot()
	if s.Established != 1 || s.CutAtDeadline != 1 || s.ServerClosedEarly != 0 {
		t.Fatalf("deadline cut must be tallied as sse_cut_at_deadline, not a server defect: %+v", s)
	}
}

// A server that closes while the walker's context is still live is a
// genuine early close and must still be counted as a defect.
func TestFireSSEKill_GenuineEarlyCloseIsStillCounted(t *testing.T) {
	release := make(chan struct{})
	addr := sseTestServer(t, release)
	var tally sseTally
	done := make(chan struct{})
	go func() { fireSSEKill(context.Background(), addr, "/events", 10*time.Second, &tally); close(done) }()
	time.Sleep(200 * time.Millisecond)
	close(release)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("fireSSEKill did not return")
	}
	s := tally.snapshot()
	if s.Established != 1 || s.ServerClosedEarly != 1 || s.CutAtDeadline != 0 {
		t.Fatalf("a genuine early close must still be a defect: %+v", s)
	}
}
