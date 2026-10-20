package main

import (
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

func testLock(t *testing.T, path, policy string) *clientLock {
	t.Helper()
	l, err := newClientLock(path, policy, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	l.poll = 5 * time.Millisecond
	return l
}

// TestClientLockExcludesAHeavyCellFromAHeavyCell: while one holder (the other
// arch's runner, here a second lock object on the same file, which flock
// treats as another process) is in a heavy cell, a heavy cell of this arch
// must not start, and starts when the holder releases (probatorium#442).
func TestClientLockExcludesAHeavyCellFromAHeavyCell(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	peer, mine := testLock(t, path, ""), testLock(t, path, "")

	releasePeer, _, err := peer.acquire(context.Background(), "ws-large-echo")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan time.Duration, 1)
	go func() {
		release, waited, err := mine.acquire(context.Background(), "post-4k")
		if err != nil {
			t.Error(err)
		}
		release()
		got <- waited
	}()
	select {
	case w := <-got:
		t.Fatalf("a heavy cell started while the peer held the lock (waited %v)", w)
	case <-time.After(100 * time.Millisecond):
	}
	releasePeer()
	select {
	case w := <-got:
		if w < 80*time.Millisecond {
			t.Fatalf("waited %v, want about the 100 ms the peer held the lock", w)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the heavy cell never started after the peer released")
	}
}

// TestClientLockLetsLightCellsRun: a light cell takes no lock, under policy
// heavy, whatever the peer holds.
func TestClientLockLetsLightCellsRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	peer, mine := testLock(t, path, ""), testLock(t, path, "")
	releasePeer, _, err := peer.acquire(context.Background(), "post-4k")
	if err != nil {
		t.Fatal(err)
	}
	release, waited, err := mine.acquire(context.Background(), "driver-pg-read")
	if err != nil || waited != 0 {
		t.Fatalf("light cell: waited %v err %v, want immediate", waited, err)
	}
	release()
	// and a light cell does not make the peer's heavy cell wait either
	releaseLight, _, _ := mine.acquire(context.Background(), "driver-pg-read")
	defer releaseLight()
	releasePeer()
	r2, w2, err := peer.acquire(context.Background(), "get-json")
	if err != nil || w2 > 50*time.Millisecond {
		t.Fatalf("heavy cell behind a light cell: waited %v err %v, want immediate", w2, err)
	}
	r2()
}

// TestClientLockPolicyAllLocksLightCells is the strict option.
func TestClientLockPolicyAllLocksLightCells(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	peer, mine := testLock(t, path, report.ExclusionAll), testLock(t, path, report.ExclusionAll)
	releasePeer, _, _ := peer.acquire(context.Background(), "driver-pg-read")
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	if _, _, err := mine.acquire(ctx, "driver-pg-read"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("under policy all a light cell must wait for the peer; err = %v", err)
	}
	releasePeer()
}

// TestClientLockWaitEndsWhenTheRunIsCancelled: a SIGTERM during the wait must
// not leave the runner stuck on the peer's cell.
func TestClientLockWaitEndsWhenTheRunIsCancelled(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	peer, mine := testLock(t, path, ""), testLock(t, path, "")
	releasePeer, _, _ := peer.acquire(context.Background(), "post-4k")
	defer releasePeer()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { _, _, err := mine.acquire(ctx, "post-4k"); done <- err }()
	time.Sleep(30 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v, want context.Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the wait ignored the cancelled context")
	}
}

func TestClientLockOffAndNoPathTakeNothing(t *testing.T) {
	for _, l := range []*clientLock{
		testLock(t, "", ""),
		testLock(t, filepath.Join(t.TempDir(), "x.lock"), report.ExclusionOff),
	} {
		if l.needs("post-4k") {
			t.Fatalf("lock %+v takes a lock for post-4k", l)
		}
	}
	if _, err := newClientLock("", "all", io.Discard); err == nil {
		t.Fatal("a policy without a lock path must be refused")
	}
	if _, err := newClientLock("/tmp/x", "sometimes", io.Discard); err == nil {
		t.Fatal("an unknown policy must be refused")
	}
}
