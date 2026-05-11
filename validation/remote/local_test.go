package remote

import (
	"context"
	"io"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestLocal_StartAndCleanExit(t *testing.T) {
	d := NewLocal("/usr/bin/true")
	proc, err := d.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if proc.PID() <= 0 {
		t.Errorf("PID should be > 0, got %d", proc.PID())
	}
	res, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.ExitCode != 0 {
		t.Errorf("ExitCode: got %d, want 0", res.ExitCode)
	}
	if res.Signaled {
		t.Errorf("Signaled: got true, want false")
	}
}

func TestLocal_NonZeroExitCode(t *testing.T) {
	d := NewLocal("/usr/bin/false")
	proc, err := d.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	res, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.ExitCode == 0 {
		t.Errorf("ExitCode: got 0, want non-zero")
	}
}

func TestLocal_SignalAfterExitIsNoop(t *testing.T) {
	d := NewLocal("/usr/bin/true")
	proc, err := d.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	if _, err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	// Process is already gone — signalling should NOT return an
	// error, matching the orchestrator's fire-and-forget shutdown.
	if err := proc.Signal(int(syscall.SIGTERM)); err != nil {
		t.Errorf("signal after exit: got %v, want nil", err)
	}
}

func TestLocal_WaitIsIdempotent(t *testing.T) {
	d := NewLocal("/usr/bin/true")
	proc, err := d.Start(context.Background(), nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	r1, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait 1: %v", err)
	}
	r2, err := proc.Wait(context.Background())
	if err != nil {
		t.Fatalf("wait 2: %v", err)
	}
	if r1 != r2 {
		t.Errorf("Wait not idempotent: %+v vs %+v", r1, r2)
	}
}

func TestLocal_SignalKillsLongRunner(t *testing.T) {
	d := NewLocal("/bin/sh")
	proc, err := d.Start(context.Background(), []string{"-c", "sleep 30"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	pid := proc.PID()
	if pid <= 0 {
		t.Fatalf("PID 0 after start")
	}
	// Allow the child to actually reach exec.
	time.Sleep(50 * time.Millisecond)
	if err := proc.Signal(int(syscall.SIGTERM)); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	// Wait for the process to actually exit. Without a timeout this
	// would hang the test on regression.
	doneCh := make(chan WaitResult, 1)
	go func() {
		r, _ := proc.Wait(context.Background())
		doneCh <- r
	}()
	select {
	case r := <-doneCh:
		if !r.Signaled || r.Signal != int(syscall.SIGTERM) {
			t.Errorf("unexpected exit: %+v (want Signaled=true Signal=SIGTERM)", r)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("process did not exit within 3s of SIGTERM")
	}
}

func TestLocal_StderrCapturesStderrAndStdout(t *testing.T) {
	d := NewLocal("/bin/sh")
	proc, err := d.Start(context.Background(), []string{"-c", "echo from-stdout; echo from-stderr >&2"})
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	buf, err := io.ReadAll(proc.Stderr())
	if err != nil {
		t.Fatalf("read stderr: %v", err)
	}
	if _, err := proc.Wait(context.Background()); err != nil {
		t.Fatalf("wait: %v", err)
	}
	body := string(buf)
	if !strings.Contains(body, "from-stdout") || !strings.Contains(body, "from-stderr") {
		t.Errorf("expected both streams in Stderr, got %q", body)
	}
}

func TestLocal_BinaryNotFoundFailsStart(t *testing.T) {
	d := NewLocal("/this/path/does/not/exist/at/all/anywhere")
	_, err := d.Start(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for missing binary, got nil")
	}
}

func TestLocal_CloseIsNoop(t *testing.T) {
	d := NewLocal("/usr/bin/true")
	if err := d.Close(); err != nil {
		t.Errorf("Close 1: %v", err)
	}
	if err := d.Close(); err != nil {
		t.Errorf("Close 2: %v", err)
	}
}
