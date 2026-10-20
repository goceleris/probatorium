package main

import (
	"context"
	"errors"
	"io"
	"os"
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

// TestClientLockUncontendedAcquireReportsNoWait: a lock that was free at the
// first attempt is not a wait. executeCell re-stamps started_at and records
// client_lock_wait_s for any positive wait, so a lock that reports the few
// microseconds it took to flock made every heavy cell look queued, and the
// validation's "the lock engaged" criterion could not fail (probatorium#442).
func TestClientLockUncontendedAcquireReportsNoWait(t *testing.T) {
	l := testLock(t, filepath.Join(t.TempDir(), "client.lock"), "")
	for i := 0; i < 100; i++ {
		release, waited, err := l.acquire(context.Background(), "post-4k")
		if err != nil {
			t.Fatal(err)
		}
		if waited != 0 {
			t.Fatalf("acquire %d of a free lock reported a wait of %v, want exactly 0", i, waited)
		}
		release()
	}
}

// TestClientLockWaitIsBounded: a holder that never lets go (a wedged runner of
// the other arch, or a stray process) must not stall this arch for longer than
// maxWait. The cell fails as a client-lock error, which is not an interruption.
func TestClientLockWaitIsBounded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	holder, mine := testLock(t, path, ""), testLock(t, path, "")
	releaseHolder, _, err := holder.acquire(context.Background(), "post-4k")
	if err != nil {
		t.Fatal(err)
	}
	defer releaseHolder()
	mine.maxWait = 150 * time.Millisecond
	start := time.Now()
	_, waited, err := mine.acquire(context.Background(), "post-4k") // a background context: only maxWait can end it
	if !errors.Is(err, errClientLockWait) {
		t.Fatalf("err = %v, want errClientLockWait", err)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a wait limit must not look like a cancelled run: %v", err)
	}
	if el := time.Since(start); el < 150*time.Millisecond || el > 2*time.Second {
		t.Fatalf("gave up after %v, want about 150ms", el)
	}
	if waited < 150*time.Millisecond {
		t.Fatalf("reported wait %v, want >= 150ms", waited)
	}
}

// TestClientLockHolderIsReleasedByItsWatchdog: a holder whose cell never
// returns (loadgen's workers wedged in blocked reads) frees the lock after
// maxHold, so the peer is not stuck behind it until the holder's hang guard.
// The holder's own late release must then not touch the peer's lock.
func TestClientLockHolderIsReleasedByItsWatchdog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	holder, peer, third := testLock(t, path, ""), testLock(t, path, ""), testLock(t, path, "")
	holder.maxHold = 120 * time.Millisecond
	releaseHolder, _, err := holder.acquire(context.Background(), "post-4k")
	if err != nil {
		t.Fatal(err)
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	releasePeer, waited, err := peer.acquire(ctx, "post-4k")
	if err != nil {
		t.Fatalf("the peer never got the lock from a holder past its hold limit: %v", err)
	}
	if el := time.Since(start); el < 100*time.Millisecond || el > 2*time.Second {
		t.Fatalf("peer started after %v, want about the 120ms hold limit", el)
	}
	if waited <= 0 {
		t.Fatalf("the peer's wait was not recorded: %v", waited)
	}
	// the wedged holder's cell finally returns: its deferred release, twice,
	// must leave the peer's lock alone.
	releaseHolder()
	releaseHolder()
	ctx2, cancel2 := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel2()
	if _, _, err := third.acquire(ctx2, "post-4k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("a late release by the old holder freed the lock the peer holds (third acquirer err = %v)", err)
	}
	releasePeer()
}

func TestClientLockReleaseIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	a, b := testLock(t, path, ""), testLock(t, path, "")
	release, _, err := a.acquire(context.Background(), "post-4k")
	if err != nil {
		t.Fatal(err)
	}
	release()
	release()
	r2, w, err := b.acquire(context.Background(), "post-4k")
	if err != nil || w != 0 {
		t.Fatalf("lock after a double release: waited %v err %v", w, err)
	}
	r2()
}

// TestClientLockFollowsAReplacedLockFile: a tmp cleaner unlinks the lock file
// while a holder has it. A waiter that wakes on the dead inode must not hold a
// lock nobody else can see: it re-opens the path, so a third runner is still
// excluded.
func TestClientLockFollowsAReplacedLockFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	holder, waiter, third := testLock(t, path, ""), testLock(t, path, ""), testLock(t, path, "")
	releaseHolder, _, err := holder.acquire(context.Background(), "post-4k")
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan func(), 1)
	go func() {
		release, _, err := waiter.acquire(context.Background(), "post-4k")
		if err != nil {
			t.Error(err)
			got <- func() {}
			return
		}
		got <- release
	}()
	time.Sleep(50 * time.Millisecond) // the waiter is polling on the old inode
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	releaseHolder()
	var releaseWaiter func()
	select {
	case releaseWaiter = <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the waiter never got the lock")
	}
	defer releaseWaiter()
	ctx, cancel := context.WithTimeout(context.Background(), 80*time.Millisecond)
	defer cancel()
	if _, _, err := third.acquire(ctx, "post-4k"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("the waiter holds a lock on an unlinked file: a third runner got in (err = %v)", err)
	}
}

func TestClientLockRefusesASymlinkedLockFile(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "elsewhere")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "client.lock")
	if err := os.Symlink(target, path); err != nil {
		t.Fatal(err)
	}
	if _, _, err := testLock(t, path, "").acquire(context.Background(), "post-4k"); err == nil {
		t.Fatal("a symlink at the lock path must be refused")
	}
}

// TestClientLockKeepsTheFileFresh: the lock file is never written, so on a host
// that age-cleans /tmp its times would date from the first run; taking the lock
// refreshes them.
func TestClientLockKeepsTheFileFresh(t *testing.T) {
	path := filepath.Join(t.TempDir(), "client.lock")
	if err := os.WriteFile(path, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	old := time.Now().Add(-30 * 24 * time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	release, _, err := testLock(t, path, "").acquire(context.Background(), "post-4k")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if age := time.Since(fi.ModTime()); age > time.Minute {
		t.Fatalf("lock file mtime is %v old after an acquire", age)
	}
}

func TestClientLockLimitsCoverTheCell(t *testing.T) {
	cfg, err := ParseArgs([]string{"-rated", "-rated-cells", "*", "-warmup", "20s", "-duration", "90s",
		"-rated-duration", "30s", "-rated-fractions", "0.5,0.7,0.9,0.95"}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	hold, wait := cfg.clientLockLimits()
	// saturation 5*20+2*90+60 = 340 s; each of 4 rated passes 5*20+2*30+60 = 220 s; +60 s.
	if want := (340 + 4*220 + 60) * time.Second; hold != want {
		t.Fatalf("hold limit %v, want %v", hold, want)
	}
	if wait != hold+2*time.Minute {
		t.Fatalf("wait limit %v, want hold + 2m (%v)", wait, hold+2*time.Minute)
	}
	cfg.RatedMode = false
	if h, _ := cfg.clientLockLimits(); h != (340+60)*time.Second {
		t.Fatalf("hold limit without rated: %v", h)
	}
}
