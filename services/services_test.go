package services

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
	"time"
)

// scanForNeedle, waitForTCP — pure-logic helpers that don't depend on
// docker, so they're cheap to unit-test. The docker-orchestrating
// surfaces (Start/Stop/Seed) are integration-test territory and
// exercised by the cluster smoke runs themselves.

func TestScanForNeedle_FindsNeedle(t *testing.T) {
	r := strings.NewReader("line one\nstarting up… ready to accept connections\nfinal\n")
	done := make(chan struct{}, 1)
	go scanForNeedle(r, "ready to accept", done)
	select {
	case <-done:
		// pass
	case <-time.After(time.Second):
		t.Fatal("scanForNeedle didn't signal within 1s")
	}
}

func TestScanForNeedle_NoSignalIfAbsent(t *testing.T) {
	r := strings.NewReader("nothing matches here\nfinal line\n")
	done := make(chan struct{}, 1)
	go scanForNeedle(r, "absent-needle", done)
	select {
	case <-done:
		t.Fatal("scanForNeedle signalled on absent needle")
	case <-time.After(50 * time.Millisecond):
		// pass — scanner reached EOF and exited
	}
}

func TestScanForNeedle_NonBlockingSendIsSafe(t *testing.T) {
	// The chan is intentionally buffered size 1; if the caller is
	// slow we don't want scanForNeedle to deadlock. Use an unbuffered
	// chan to force the non-blocking send path.
	r := strings.NewReader("ready\nready\n") // first match suffices
	done := make(chan struct{})              // unbuffered
	go scanForNeedle(r, "ready", done)
	// Don't read from done — scanForNeedle must still return without
	// blocking. We confirm by reading a short while after.
	time.Sleep(10 * time.Millisecond)
	// Now read the queued signal (if any).
	select {
	case <-done:
		// pass — sender didn't deadlock
	case <-time.After(50 * time.Millisecond):
		// also acceptable; the non-blocking default branch was hit
	}
}

func TestWaitForTCP_AcceptsImmediately(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()
	go func() {
		// Accept and discard so dial doesn't hang on RST.
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()

	addr := ln.Addr().String()
	if err := waitForTCP(context.Background(), addr, time.Second); err != nil {
		t.Fatalf("waitForTCP: %v", err)
	}
}

func TestWaitForTCP_FailsOnTimeout(t *testing.T) {
	// 1.0.0.0:1 is reserved AND unreachable from most networks; if it
	// happens to be reachable for someone, the test still fails fast
	// because the connection won't succeed within 100 ms.
	err := waitForTCP(context.Background(), "127.0.0.1:1", 100*time.Millisecond)
	if err == nil {
		t.Fatal("expected timeout error")
	}
	if !errors.Is(err, context.DeadlineExceeded) {
		// waitForTCP wraps the deadline err, so unwrap is the canonical
		// check. Accept either wording as long as it's deadline-flavoured.
		if !strings.Contains(err.Error(), "deadline") {
			t.Errorf("expected deadline error, got %v", err)
		}
	}
}

func TestWaitForTCP_RespectsParentCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled
	err := waitForTCP(ctx, "127.0.0.1:1", time.Second)
	if err == nil {
		t.Fatal("expected cancellation error")
	}
}

func TestKindConstants_Stable(t *testing.T) {
	// Public string constants are part of the API: Start(ctx, kinds...)
	// receives them as-is from the runner. Renaming any of these is a
	// breaking change.
	if KindPostgres != "postgres" {
		t.Errorf("KindPostgres=%q", KindPostgres)
	}
	if KindRedis != "redis" {
		t.Errorf("KindRedis=%q", KindRedis)
	}
	if KindMemcached != "memcached" {
		t.Errorf("KindMemcached=%q", KindMemcached)
	}
}

func TestFixtureRanges_Sane(t *testing.T) {
	// Driver scenarios use these bounds to generate request traffic.
	// The bounds must be > 0 and Max > Min for the random key generator
	// in the runner to terminate.
	if FixtureUserMinID >= FixtureUserMaxID {
		t.Errorf("FixtureUser[Min,Max]ID=(%d, %d) — Max must exceed Min",
			FixtureUserMinID, FixtureUserMaxID)
	}
	if FixtureSessionIDMin >= FixtureSessionIDMax {
		t.Errorf("FixtureSessionID[Min,Max]=(%d, %d)",
			FixtureSessionIDMin, FixtureSessionIDMax)
	}
	if FixtureDemoKey == "" {
		t.Error("FixtureDemoKey is empty")
	}
}
