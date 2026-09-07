package remote

import (
	"context"
	"io"
	"strings"
	"testing"
	"time"
)

// TestLocal_OutputWrittenAroundExitIsNotLost: bytes a process (or a child
// still holding its stderr) writes around exit must reach the Stderr()
// reader even when Wait runs concurrently. exec's StdoutPipe/StderrPipe are
// closed by cmd.Wait on reap, discarding whatever is still buffered -- the
// crash banner a refapp prints just before dying was lost to that
// (probatorium#276). The driver owns its pipes so the reap cannot touch them.
func TestLocal_OutputWrittenAroundExitIsNotLost(t *testing.T) {
	l := NewLocal("/bin/sh")
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	p, err := l.Start(ctx, []string{"-c", `( sleep 0.2; echo "fatal error: late banner" >&2 ) & exit 2`})
	if err != nil {
		t.Fatal(err)
	}
	got := make(chan string, 1)
	go func() { b, _ := io.ReadAll(p.Stderr()); got <- string(b) }()
	res, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if res.ExitCode != 2 {
		t.Fatalf("exit code: %+v", res)
	}
	select {
	case out := <-got:
		if !strings.Contains(out, "late banner") {
			t.Fatalf("bytes written around exit were lost to the reap: %q", out)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("stderr reader never reached EOF")
	}
}

// TestLocal_WaitDoesNotBlockOnOrphanHoldingPipes: a descendant that inherited
// the pipes must not delay Wait (sh -c forks its child on Linux, so a
// SIGTERM'd "sh -c 'sleep 30'" leaves sleep holding the fds). Wait reaps
// promptly; the read ends are force-closed after pipeOrphanGrace so the
// reader still terminates.
func TestLocal_WaitDoesNotBlockOnOrphanHoldingPipes(t *testing.T) {
	l := NewLocal("/bin/sh")
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	p, err := l.Start(ctx, []string{"-c", `( sleep 8 ) & exit 0`})
	if err != nil {
		t.Fatal(err)
	}
	eof := make(chan struct{})
	go func() { _, _ = io.Copy(io.Discard, p.Stderr()); close(eof) }()
	start := time.Now()
	res, err := p.Wait(ctx)
	if err != nil {
		t.Fatalf("wait: %v", err)
	}
	if took := time.Since(start); took > time.Second {
		t.Fatalf("Wait blocked on the orphan for %s (want prompt reap)", took)
	}
	if res.ExitCode != 0 {
		t.Fatalf("exit code: %+v", res)
	}
	select {
	case <-eof:
	case <-time.After(pipeOrphanGrace + 2*time.Second):
		t.Fatal("reader never terminated: read ends not force-closed after the orphan grace")
	}
}
