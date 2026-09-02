package validation

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestRecordHangClassifies proves the cause split actually discriminates:
// a server that closes without replying must score EOF (fast), and one
// that accepts but never writes must score timeout (slow). Before the
// split both were indistinguishable h2c_hang=1.
func TestRecordHangClassifies(t *testing.T) {
	t.Run("close_without_reply_is_eof", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				b := make([]byte, 512)
				_, _ = c.Read(b)
				_ = c.Close() // reply with nothing
			}
		}()
		var tally h2cTally
		fireH2CChurn(context.Background(), ln.Addr().String(), ChurnRSTAfter101, &tally)
		s := tally.snapshot()
		if s.Hang != 1 || s.HangEOF != 1 {
			t.Fatalf("want hang=1 eof=1, got %+v", s)
		}
		if s.HangMaxElapsedMs > 2000 {
			t.Fatalf("close-without-reply should be fast, got %dms", s.HangMaxElapsedMs)
		}
		t.Logf("EOF case: hang=%d eof=%d elapsed=%dms", s.Hang, s.HangEOF, s.HangMaxElapsedMs)
	})

	t.Run("silent_server_is_timeout", func(t *testing.T) {
		if testing.Short() {
			t.Skip("needs the full 10s read budget")
		}
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		defer func() { _ = ln.Close() }()
		held := make(chan net.Conn, 4)
		go func() {
			for {
				c, err := ln.Accept()
				if err != nil {
					return
				}
				held <- c // accept, read nothing, never reply
			}
		}()
		var tally h2cTally
		start := time.Now()
		fireH2CChurn(context.Background(), ln.Addr().String(), ChurnRSTAfter101, &tally)
		el := time.Since(start)
		s := tally.snapshot()
		if s.Hang != 1 || s.HangTimeout != 1 {
			t.Fatalf("want hang=1 timeout=1, got %+v", s)
		}
		if el < 9*time.Second {
			t.Fatalf("timeout case should consume the read budget, took %v", el)
		}
		t.Logf("timeout case: hang=%d timeout=%d elapsed=%dms", s.Hang, s.HangTimeout, s.HangMaxElapsedMs)
		close(held)
		for c := range held {
			_ = c.Close()
		}
	})
}
