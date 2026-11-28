package validation

import (
	"context"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

// A walker whose own context is already cancelled and whose connection then
// ends (probatorium#595). The runner ends a cell on a hard incident by
// cancelling the run and then killing the refapp, so the h2c and websocket
// walkers that are blocked in a read see EOF or a reset: the harness's own
// stop, not a fault of the server. These tests hold the read open, cancel the
// walker's context, and only then end the connection, so the order is fixed by
// the channels and not by timing.

// holdingServer accepts one connection, reads the walker's request, replies
// with prefix (possibly nothing), tells the test it is holding the read open,
// and then, when released, ends the connection: a close (EOF on the walker's
// side) or, with reset set, a close with linger 0 (a reset).
type holdingServer struct {
	addr    string
	holding chan struct{}
	release chan struct{}
	done    sync.WaitGroup
	ln      net.Listener
}

func newHoldingServer(t *testing.T, prefix string, reset bool) *holdingServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	s := &holdingServer{
		addr:    ln.Addr().String(),
		holding: make(chan struct{}, 16),
		release: make(chan struct{}),
		ln:      ln,
	}
	s.done.Add(1)
	go func() {
		defer s.done.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			s.done.Add(1)
			go func() {
				defer s.done.Done()
				buf := make([]byte, 1024)
				_, _ = c.Read(buf)
				if prefix != "" {
					_, _ = c.Write([]byte(prefix))
				}
				s.holding <- struct{}{}
				<-s.release
				if tc, ok := c.(*net.TCPConn); ok && reset {
					_ = tc.SetLinger(0)
				}
				_ = c.Close()
			}()
		}
	}()
	t.Cleanup(func() {
		select {
		case <-s.release:
		default:
			close(s.release)
		}
		_ = ln.Close()
		s.done.Wait()
	})
	return s
}

// endConnections releases every held connection.
func (s *holdingServer) endConnections() { close(s.release) }

func waitForHolding(t *testing.T, s *holdingServer) {
	t.Helper()
	select {
	case <-s.holding:
	case <-time.After(10 * time.Second):
		t.Fatal("the server never received the walker's request")
	}
}

// wsHeadersWithoutEnd is a 101 whose header block never ends, so the walker is
// blocked in the header-drain read and not in the status-line read.
const wsHeadersWithoutEnd = "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\n"

func TestH2CWalkerDoesNotCountAReadErrorAfterItsOwnCancel(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name := "eof"
		if reset {
			name = "reset"
		}
		t.Run(name, func(t *testing.T) {
			srv := newHoldingServer(t, "", reset)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var tally h2cTally
			fired := make(chan struct{})
			go func() {
				defer close(fired)
				fireH2CChurn(ctx, srv.addr, ChurnRSTAfter101, &tally)
			}()
			waitForHolding(t, srv)
			cancel()
			srv.endConnections()
			<-fired
			s := tally.snapshot()
			if s.Hang != 0 || s.HangEOF != 0 || s.HangReset != 0 || s.HangTimeout != 0 || s.HangOther != 0 {
				t.Fatalf("a read error after the walker's own cancel was counted as a hang: %+v", s)
			}
			if len(s.SlowReads) != 0 || s.HangSpan != nil {
				t.Fatalf("a read error after the walker's own cancel was filed as an event: %+v", s)
			}
		})
	}
}

func TestH2CWalkerStillCountsAReadErrorWhenItIsNotCancelled(t *testing.T) {
	for _, reset := range []bool{false, true} {
		name, cause := "eof", "eof"
		if reset {
			name, cause = "reset", "reset"
		}
		t.Run(name, func(t *testing.T) {
			srv := newHoldingServer(t, "", reset)
			var tally h2cTally
			fired := make(chan struct{})
			go func() {
				defer close(fired)
				fireH2CChurn(context.Background(), srv.addr, ChurnRSTAfter101, &tally)
			}()
			waitForHolding(t, srv)
			srv.endConnections()
			<-fired
			s := tally.snapshot()
			if s.Hang != 1 {
				t.Fatalf("an uncancelled read error must still count as one hang: %+v", s)
			}
			if cause == "eof" && s.HangEOF != 1 || cause == "reset" && s.HangReset != 1 {
				t.Fatalf("the hang's cause was lost: %+v", s)
			}
			if s.HangSpan == nil {
				t.Fatalf("the hang's instant was not recorded: %+v", s)
			}
		})
	}
}

func TestWSWalkerDoesNotCountAReadErrorAfterItsOwnCancel(t *testing.T) {
	for _, prefix := range []string{"", wsHeadersWithoutEnd} {
		for _, reset := range []bool{false, true} {
			name := "status-line"
			if prefix != "" {
				name = "headers"
			}
			if reset {
				name += "-reset"
			} else {
				name += "-eof"
			}
			t.Run(name, func(t *testing.T) {
				srv := newHoldingServer(t, prefix, reset)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				var tally wsTally
				fired := make(chan struct{})
				go func() {
					defer close(fired)
					fireWSTorture(ctx, srv.addr, "/ws", ModeUnmaskedClient, &tally)
				}()
				waitForHolding(t, srv)
				cancel()
				srv.endConnections()
				<-fired
				s := tally.snapshot()
				if s.HandshakeFail != 0 || s.HandshakeFailEOF != 0 || s.HandshakeFailReset != 0 ||
					s.HandshakeFailTimeout != 0 || s.HandshakeFailOther != 0 {
					t.Fatalf("a read error after the walker's own cancel was counted as a handshake failure: %+v", s)
				}
				if len(s.SlowReads) != 0 || s.HandshakeFailSpan != nil {
					t.Fatalf("a read error after the walker's own cancel was filed as an event: %+v", s)
				}
			})
		}
	}
}

func TestWSWalkerStillCountsAReadErrorWhenItIsNotCancelled(t *testing.T) {
	for _, prefix := range []string{"", wsHeadersWithoutEnd} {
		for _, reset := range []bool{false, true} {
			name := "status-line"
			if prefix != "" {
				name = "headers"
			}
			if reset {
				name += "-reset"
			} else {
				name += "-eof"
			}
			t.Run(name, func(t *testing.T) {
				srv := newHoldingServer(t, prefix, reset)
				var tally wsTally
				fired := make(chan struct{})
				go func() {
					defer close(fired)
					fireWSTorture(context.Background(), srv.addr, "/ws", ModeUnmaskedClient, &tally)
				}()
				waitForHolding(t, srv)
				srv.endConnections()
				<-fired
				s := tally.snapshot()
				if s.HandshakeFail != 1 {
					t.Fatalf("an uncancelled read error must still count as one handshake failure: %+v", s)
				}
				if reset && s.HandshakeFailReset != 1 || !reset && s.HandshakeFailEOF != 1 {
					t.Fatalf("the failure's cause was lost: %+v", s)
				}
				if s.HandshakeFailSpan == nil {
					t.Fatalf("the failure's instant was not recorded: %+v", s)
				}
			})
		}
	}
}

// The same through the walker loops, the way driveTier1 runs them: the loop's
// context is the one the fleet is stopped with.
func TestWalkerLoopsDoNotCountTheEndOfTheirOwnConnectionAfterStop(t *testing.T) {
	t.Run("h2c", func(t *testing.T) {
		srv := newHoldingServer(t, "", false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var tally h2cTally
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			runH2CChurnWalker(ctx, srv.addr, 1, time.Millisecond, &tally)
		}()
		waitForHolding(t, srv)
		cancel()
		srv.endConnections()
		<-ended
		if s := tally.snapshot(); s.Hang != 0 {
			t.Fatalf("h2c_hang after the fleet was stopped: %+v", s)
		}
	})
	t.Run("ws", func(t *testing.T) {
		srv := newHoldingServer(t, "", false)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var tally wsTally
		ended := make(chan struct{})
		go func() {
			defer close(ended)
			runWSTortureWalker(ctx, srv.addr, "/ws", 1, time.Millisecond, &tally)
		}()
		waitForHolding(t, srv)
		cancel()
		srv.endConnections()
		<-ended
		if s := tally.snapshot(); s.HandshakeFail != 0 {
			t.Fatalf("ws_handshake_fail after the fleet was stopped: %+v", s)
		}
	})
}

// A read that ran out its deadline is a stall whatever the context did, so a
// cancelled context does not excuse it; every other error is the connection
// ending, which after a cancel is the harness's doing.
func TestReadErrorAfterCancelClassifies(t *testing.T) {
	live := context.Background()
	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		err  error
		want bool
	}{
		{"cancelled_eof", cancelled, io.EOF, true},
		{"cancelled_unexpected_eof", cancelled, io.ErrUnexpectedEOF, true},
		{"cancelled_reset", cancelled, syscall.ECONNRESET, true},
		{"cancelled_wrapped_reset", cancelled, &net.OpError{Op: "read", Err: os.NewSyscallError("read", syscall.ECONNRESET)}, true},
		{"cancelled_other", cancelled, errors.New("boom"), true},
		{"cancelled_deadline_still_counts", cancelled, os.ErrDeadlineExceeded, false},
		{"cancelled_wrapped_deadline_still_counts", cancelled, &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, false},
		{"live_eof", live, io.EOF, false},
		{"live_reset", live, syscall.ECONNRESET, false},
		{"live_deadline", live, os.ErrDeadlineExceeded, false},
		{"cancelled_no_error", cancelled, nil, false},
		{"live_no_error", live, nil, false},
	}
	for _, c := range cases {
		if got := readErrorAfterCancel(c.ctx, c.err); got != c.want {
			t.Errorf("%s: readErrorAfterCancel = %v, want %v", c.name, got, c.want)
		}
	}
}
