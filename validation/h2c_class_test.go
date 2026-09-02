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

// TestRecordHangClassifies pins the discrimination that celeris#470
// needs: `hang` alone cannot tell a real >10s stall from an immediate
// close, because its predicate (`err != nil || n == 0`) scores both as 1.
//
// Kept as a pure unit test over recordHang rather than standing up a
// silent server per case: the timeout path would otherwise cost the full
// 10s read budget per run and needs a parked connection, which leaks a
// goroutine into the package-wide leak detector (TestWaitForReady_NoGoroutineLeak).
func TestRecordHangClassifies(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		elapsed time.Duration
		want    func(h2cSnapshot) int64
	}{
		{"deadline_is_timeout", os.ErrDeadlineExceeded, 10 * time.Second,
			func(s h2cSnapshot) int64 { return s.HangTimeout }},
		{"wrapped_deadline_is_timeout", &net.OpError{Op: "read", Err: os.ErrDeadlineExceeded}, 10 * time.Second,
			func(s h2cSnapshot) int64 { return s.HangTimeout }},
		{"eof_is_eof", io.EOF, 0,
			func(s h2cSnapshot) int64 { return s.HangEOF }},
		{"unexpected_eof_is_eof", io.ErrUnexpectedEOF, 0,
			func(s h2cSnapshot) int64 { return s.HangEOF }},
		{"reset_is_reset", syscall.ECONNRESET, 0,
			func(s h2cSnapshot) int64 { return s.HangReset }},
		{"wrapped_reset_is_reset", &net.OpError{Op: "read", Err: syscall.ECONNRESET}, 0,
			func(s h2cSnapshot) int64 { return s.HangReset }},
		{"nil_is_other", nil, 0,
			func(s h2cSnapshot) int64 { return s.HangOther }},
		{"unknown_is_other", errors.New("something else"), 0,
			func(s h2cSnapshot) int64 { return s.HangOther }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tally h2cTally
			tally.recordHang(tc.err, tc.elapsed)
			s := tally.snapshot()
			if s.Hang != 1 {
				t.Fatalf("hang total: want 1, got %d", s.Hang)
			}
			if got := tc.want(s); got != 1 {
				t.Fatalf("expected bucket to hold 1, got %d (snapshot %+v)", got, s)
			}
			// Exactly one bucket may claim the event.
			if sum := s.HangEOF + s.HangTimeout + s.HangReset + s.HangOther; sum != 1 {
				t.Fatalf("buckets must partition the total, sum=%d (%+v)", sum, s)
			}
		})
	}
}

// TestRecordHangTracksMaxElapsed proves the elapsed watermark is a max,
// not last-write-wins — it is what distinguishes a stall from a fast
// close when several hangs land in the same cell.
func TestRecordHangTracksMaxElapsed(t *testing.T) {
	var tally h2cTally
	for _, d := range []time.Duration{5 * time.Millisecond, 10 * time.Second, 2 * time.Millisecond} {
		tally.recordHang(io.EOF, d)
	}
	if got := tally.snapshot().HangMaxElapsedMs; got != 10000 {
		t.Fatalf("want max 10000ms, got %d", got)
	}
}

// TestFireH2CChurnCloseWithoutReplyIsEOF is the end-to-end half: it
// proves the classification is actually wired into the read site, using
// the fast (EOF) path so the test stays sub-second and leak-free.
func TestFireH2CChurnCloseWithoutReplyIsEOF(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			c, err := ln.Accept()
			if err != nil {
				return // listener closed
			}
			b := make([]byte, 512)
			_, _ = c.Read(b)
			_ = c.Close() // reply with nothing at all
		}
	}()
	t.Cleanup(func() {
		_ = ln.Close()
		wg.Wait()
	})

	var tally h2cTally
	fireH2CChurn(context.Background(), ln.Addr().String(), ChurnRSTAfter101, &tally)

	s := tally.snapshot()
	if s.Hang != 1 || s.HangEOF != 1 {
		t.Fatalf("close-without-reply should score exactly one EOF hang, got %+v", s)
	}
	if s.HangTimeout != 0 {
		t.Fatalf("close-without-reply must not score a timeout, got %+v", s)
	}
	if s.HangMaxElapsedMs > 2000 {
		t.Fatalf("close-without-reply should be immediate, took %dms", s.HangMaxElapsedMs)
	}
}
