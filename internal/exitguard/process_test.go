package exitguard

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strconv"
	"syscall"
	"testing"
	"time"
)

// The in-process cases inject the signal and the exit. These run a real child
// process, hand it a real SIGTERM and watch it die (or not): the property
// that matters is that no refapp outlives its signal.
//
// The child is this test binary re-executed into TestHelperProcess, which
// plays a refapp main whose server cannot end Listen (celeris#595): Shutdown
// returns nil, Start blocks for ever.

const (
	childEnv   = "EXITGUARD_TEST_CHILD" // mode; unset = this is the parent
	childTO    = "EXITGUARD_TEST_TIMEOUT_MS"
	childGrace = "EXITGUARD_TEST_GRACE_MS"

	modeGuardedStuck = "guarded-stuck" // Install/Serve, Listen never ends
	modeGuardedClean = "guarded-clean" // Install/Serve, Shutdown ends Listen
	modeGuardedHang  = "guarded-hang"  // Install/Serve, Shutdown never returns
	modeLegacyStuck  = "legacy-stuck"  // the pre-exitguard pattern, Listen never ends
)

// TestHelperProcess is not a test: it is the child's main.
func TestHelperProcess(t *testing.T) {
	mode := os.Getenv(childEnv)
	if mode == "" {
		t.Skip("child entry point, run by the process tests")
	}
	ms := func(env string) time.Duration {
		n, _ := strconv.Atoi(os.Getenv(env))
		return time.Duration(n) * time.Millisecond
	}
	log.SetFlags(0)
	f := newFake()
	f.endListen = mode == modeGuardedClean
	if mode == modeGuardedHang {
		f.hang = make(chan struct{})
	}

	var serve func() error
	switch mode {
	case modeLegacyStuck:
		// Verbatim shape of what every refapp had: handler goroutine
		// ignoring Shutdown's result, main blocked in Start.
		go func() {
			sig := make(chan os.Signal, 1)
			signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
			<-sig
			log.Printf("child: signal received, shutting down")
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			_ = f.Shutdown(ctx)
		}()
		serve = f.Start
	default:
		g := Install(Config{Name: "child", Timeout: ms(childTO), Grace: ms(childGrace)}, f.Shutdown)
		serve = func() error { return g.Serve(f.Start) }
	}
	fmt.Println("ready")
	if err := serve(); err != nil {
		log.Fatalf("child: start: %v", err)
	}
	os.Exit(0)
}

type child struct {
	cmd    *exec.Cmd
	stderr *bytes.Buffer
	exited chan error
}

func startChild(t *testing.T, mode string, timeout, grace time.Duration) *child {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=^TestHelperProcess$")
	cmd.Env = append(os.Environ(),
		childEnv+"="+mode,
		childTO+"="+strconv.Itoa(int(timeout/time.Millisecond)),
		childGrace+"="+strconv.Itoa(int(grace/time.Millisecond)),
	)
	c := &child{cmd: cmd, stderr: &bytes.Buffer{}, exited: make(chan error, 1)}
	cmd.Stderr = c.stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	// The test can never leak a process itself, whatever the verdict.
	t.Cleanup(func() { _ = cmd.Process.Kill() })
	sc := bufio.NewScanner(out)
	if !sc.Scan() || sc.Text() != "ready" {
		t.Fatalf("child never became ready: %q (stderr %q)", sc.Text(), c.stderr.String())
	}
	go func() { _, _ = io.Copy(io.Discard, out); c.exited <- cmd.Wait() }()
	return c
}

// outcome waits up to d for the child to end. ended=false means it is still
// alive: it outlived its signal.
func (c *child) outcome(d time.Duration) (ended bool, code int) {
	select {
	case err := <-c.exited:
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return true, ee.ExitCode()
		}
		return true, 0
	case <-time.After(d):
		return false, 0
	}
}

func (c *child) sig(t *testing.T, s syscall.Signal) {
	t.Helper()
	if err := c.cmd.Process.Signal(s); err != nil {
		t.Fatal(err)
	}
}

// The failing-first test: a server whose Listen never returns, Shutdown
// returning nil, SIGTERM. It must be gone within Timeout+Grace (plus slack).
func TestProcessWithStuckListenEndsWithinBound(t *testing.T) {
	const to, grace = 200 * time.Millisecond, 300 * time.Millisecond
	c := startChild(t, modeGuardedStuck, to, grace)
	start := time.Now()
	c.sig(t, syscall.SIGTERM)
	ended, code := c.outcome(10 * time.Second)
	if !ended {
		t.Fatal("process is still running 10s after SIGTERM: it outlived its signal")
	}
	if code != ExitStartStuck {
		t.Fatalf("exit code %d, want %d (stderr %q)", code, ExitStartStuck, c.stderr.String())
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("took %v for a %v+%v bound", d, to, grace)
	}
}

// SIGINT goes through the same path as SIGTERM.
func TestProcessSIGINTEndsWithinBound(t *testing.T) {
	c := startChild(t, modeGuardedStuck, 100*time.Millisecond, 100*time.Millisecond)
	c.sig(t, syscall.SIGINT)
	if ended, code := c.outcome(10 * time.Second); !ended || code != ExitStartStuck {
		t.Fatalf("ended=%v code=%d, want ended with %d (stderr %q)", ended, code, ExitStartStuck, c.stderr.String())
	}
}

// Clean path through a real process: exit 0, only the signal line on stderr.
func TestProcessCleanShutdownExitsZeroSilently(t *testing.T) {
	c := startChild(t, modeGuardedClean, 200*time.Millisecond, 200*time.Millisecond)
	c.sig(t, syscall.SIGTERM)
	ended, code := c.outcome(10 * time.Second)
	if !ended || code != 0 {
		t.Fatalf("ended=%v code=%d, want a clean exit 0 (stderr %q)", ended, code, c.stderr.String())
	}
	// go test's own child stderr may carry nothing else; the refapp line must be exact.
	if got, want := c.stderr.String(), "child: signal received, shutting down\n"; got != want {
		t.Fatalf("stderr = %q, want %q", got, want)
	}
}

// Second real signal while Shutdown hangs: immediate, 128+SIGTERM.
func TestProcessSecondSignalExitsAtOnce(t *testing.T) {
	c := startChild(t, modeGuardedHang, time.Hour, time.Hour)
	c.sig(t, syscall.SIGTERM)
	time.Sleep(200 * time.Millisecond)
	if ended, _ := c.outcome(0); ended {
		t.Fatal("exited on the first signal while Shutdown was still running")
	}
	c.sig(t, syscall.SIGTERM)
	ended, code := c.outcome(5 * time.Second)
	if !ended || code != 128+int(syscall.SIGTERM) {
		t.Fatalf("ended=%v code=%d, want %d", ended, code, 128+int(syscall.SIGTERM))
	}
}

// NEGATIVE CONTROL. The pre-exitguard pattern (ignore Shutdown's result, main
// blocked in Start) against the same stuck server must NOT end: this is the
// process that ran on msr1 for 28 days. If it ever ended here, the tests
// above would be passing for a reason other than the guard.
func TestLegacyPatternOutlivesItsSignal(t *testing.T) {
	c := startChild(t, modeLegacyStuck, 0, 0)
	c.sig(t, syscall.SIGTERM)
	if ended, code := c.outcome(3 * time.Second); ended {
		t.Fatalf("the ignore-and-block pattern ended (code %d): the stuck-Listen fake no longer reproduces the leak, so TestProcessWithStuckListenEndsWithinBound proves nothing", code)
	}
	c.sig(t, syscall.SIGINT) // a second, different signal: same stale handler, still alive
	if ended, _ := c.outcome(time.Second); ended {
		t.Fatal("SIGINT ended the legacy-pattern process; it should hit the same stale handler")
	}
	// Cleanup SIGKILLs it, the only signal that ever ended the real one.
}
