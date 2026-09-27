package remote

import (
	"context"
	"errors"
	"slices"
	"sync"
	"syscall"
	"testing"
	"time"
)

// killLog records every signal a Local driver sends, then sends it.
type killLog struct {
	// note, when set, runs before each signal and its result is recorded
	// with it. Set it before the first Signal or Wait.
	note func(pid int, sig syscall.Signal) string

	mu    sync.Mutex
	calls []killCall
}

type killCall struct {
	pid  int
	sig  syscall.Signal
	note string
}

func (k *killLog) kill(pid int, sig syscall.Signal) error {
	c := killCall{pid: pid, sig: sig}
	if k.note != nil {
		c.note = k.note(pid, sig)
	}
	k.mu.Lock()
	k.calls = append(k.calls, c)
	k.mu.Unlock()
	return syscall.Kill(pid, sig)
}

func (k *killLog) snapshot() []killCall {
	k.mu.Lock()
	defer k.mu.Unlock()
	return slices.Clone(k.calls)
}

// recordingLocal is a Local driver for /bin/sh whose signals go through a
// killLog.
func recordingLocal() (*Local, *killLog) {
	k := &killLog{}
	l := NewLocal("/bin/sh")
	l.kill = k.kill
	return l, k
}

// awaitReaped blocks until the process's reaper has returned. The reaper has
// recorded the exit by then.
func awaitReaped(t *testing.T, proc Process) {
	t.Helper()
	lp, ok := proc.(*localProcess)
	if !ok {
		t.Fatalf("process is a %T, not a *localProcess", proc)
	}
	select {
	case <-lp.reaped:
	case <-time.After(10 * time.Second):
		t.Fatal("the reaper did not reap the process within 10s")
	}
}

// TestLocal_NoSignalOnceTheExitIsRecorded: once a process is reaped its PID,
// and so its group ID, is free for the kernel to hand out again, so the
// driver must send it nothing more. The reaper records the exit even when
// the Wait that started it gave up first. Before, only a Wait that received
// the result recorded it, so after a Wait whose context ended, Signal sent
// kill(-pgid) to the freed ID. Tier 1 does this once per cell:
// watchProcessExit's Wait returns at the end of the run, exec's context kill
// stops the refapp, and driveTier1's deferred SIGTERM comes after the reap.
func TestLocal_NoSignalOnceTheExitIsRecorded(t *testing.T) {
	t.Run("a Wait timed out then the process exited on its own", func(t *testing.T) {
		l, k := recordingLocal()
		proc, err := l.Start(context.Background(), []string{"-c", "exec sleep 0.3"})
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		_, err = proc.Wait(ctx)
		cancel()
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("first Wait: got %v, want the context's deadline", err)
		}
		awaitReaped(t, proc)
		before := len(k.snapshot())
		for _, sig := range []syscall.Signal{syscall.SIGTERM, syscall.SIGKILL} {
			if err := proc.Signal(int(sig)); err != nil {
				t.Errorf("Signal(%v) after the reap: %v", sig, err)
			}
		}
		if sent := k.snapshot()[before:]; len(sent) > 0 {
			t.Errorf("Signal sent %+v to pid %d's group after the reaper had reaped it", sent, proc.PID())
		}
		res, err := proc.Wait(context.Background())
		if err != nil || res != (WaitResult{}) {
			t.Errorf("Wait after the cancelled one: got %+v, %v; want exit 0", res, err)
		}
	})

	t.Run("the run context ended as at the end of a Tier 1 cell", func(t *testing.T) {
		l, k := recordingLocal()
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		proc, err := l.Start(ctx, []string{"-c", "exec sleep 30"})
		if err != nil {
			t.Fatal(err)
		}
		pid := proc.PID()
		waitErr := make(chan error, 1)
		go func() { _, err := proc.Wait(ctx); waitErr <- err }()
		cancel()
		if err := <-waitErr; !errors.Is(err, context.Canceled) {
			t.Fatalf("watchProcessExit's Wait: got %v, want the context's cancel", err)
		}
		awaitReaped(t, proc)
		calls := k.snapshot()
		if !slices.Contains(calls, killCall{pid: -pid, sig: syscall.SIGKILL}) {
			t.Fatalf("the context's kill was not recorded (calls %+v), so the log would miss a later signal too", calls)
		}
		if err := proc.Signal(int(syscall.SIGTERM)); err != nil {
			t.Errorf("driveTier1's deferred SIGTERM: %v", err)
		}
		if sent := k.snapshot()[len(calls):]; len(sent) > 0 {
			t.Errorf("the deferred SIGTERM sent %+v to pid %d's group after the reaper had reaped it", sent, pid)
		}
		res, err := proc.Wait(context.Background())
		if err != nil || !res.Signaled || res.Signal != int(syscall.SIGKILL) {
			t.Errorf("Wait after the reap: got %+v, %v; want killed by SIGKILL", res, err)
		}
	})
}

// TestLocal_WaitAfterACancelledWaitSendsNothing: a Wait whose context ended,
// then a stop, then another Wait, as in a bounded Wait, SIGKILL, Wait-again
// teardown. The later Waits must return the process's exit, and the stop must
// sweep the group once, when the exit is recorded. Before, the second Wait
// called exec.Cmd.Wait again, which fails at once with "Wait was already
// called" (probatorium#426), and still ran the stop's group kill, arbitrarily
// long after the reap.
func TestLocal_WaitAfterACancelledWaitSendsNothing(t *testing.T) {
	l, k := recordingLocal()
	proc, err := l.Start(context.Background(), []string{"-c", "exec sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	pid := proc.PID()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	_, err = proc.Wait(ctx)
	cancel()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("first Wait: got %v, want the context's deadline", err)
	}
	if err := proc.Signal(int(syscall.SIGTERM)); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	awaitReaped(t, proc)
	calls := k.snapshot()
	if want := []killCall{{pid: -pid, sig: syscall.SIGTERM}, {pid: -pid, sig: syscall.SIGKILL}}; !slices.Equal(calls, want) {
		t.Fatalf("signals up to the reap: got %+v, want the stop and then one sweep, %+v", calls, want)
	}
	for i := range 2 {
		res, err := proc.Wait(context.Background())
		if err != nil || !res.Signaled || res.Signal != int(syscall.SIGTERM) {
			t.Errorf("Wait %d after the cancelled one: got %+v, %v; want killed by SIGTERM", i+2, res, err)
		}
	}
	if err := proc.Signal(int(syscall.SIGKILL)); err != nil {
		t.Errorf("SIGKILL after the reap: %v", err)
	}
	if sent := k.snapshot()[len(calls):]; len(sent) > 0 {
		t.Errorf("after the reap the driver sent %+v to pid %d's group", sent, pid)
	}
}

// TestLocal_OverlappingWaitsGetOneResult: the Process contract lets Wait run
// from several goroutines at once. Each must get the process's exit. Two
// exec.Cmd.Wait calls in flight race on the reap: one got ECHILD, or blocked
// forever on exec's context channel, which sends one value
// (probatorium#426). The Start context is cancellable, as the harness's
// always is.
func TestLocal_OverlappingWaitsGetOneResult(t *testing.T) {
	for rep := range 10 {
		startCtx, cancelStart := context.WithCancel(context.Background())
		proc, err := NewLocal("/bin/sh").Start(startCtx, []string{"-c", "exec sleep 0.2"})
		if err != nil {
			cancelStart()
			t.Fatal(err)
		}
		type result struct {
			res WaitResult
			err error
		}
		results := make(chan result, 2)
		for range 2 {
			go func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				res, err := proc.Wait(ctx)
				results <- result{res, err}
			}()
		}
		for range 2 {
			if r := <-results; r.err != nil || r.res != (WaitResult{}) {
				t.Errorf("rep %d: an overlapping Wait got %+v, %v; want exit 0", rep, r.res, r.err)
			}
		}
		cancelStart()
	}
}
