package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"sync"
	"syscall"
)

// Local is the exec.Cmd-backed Driver. Used by unit tests and
// single-host smoke runs (validator + celeris on the same machine).
// The binary path is captured at construction time so Start arguments
// stay framework-agnostic.
type Local struct {
	binary string
}

// NewLocal constructs a Local driver that runs the given binary on
// each Start. binary may be an absolute path or any path resolvable
// via $PATH; the same string is passed to exec.LookPath behaviour at
// Start time.
func NewLocal(binary string) *Local {
	return &Local{binary: binary}
}

// Start forks the binary with args. The returned Process tracks the
// underlying exec.Cmd; Wait drains stdout/stderr pipes before
// returning so the caller can read Stderr() afterwards.
func (l *Local) Start(ctx context.Context, args []string) (Process, error) {
	cmd := exec.CommandContext(ctx, l.binary, args...)
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	// Capture stdout too — many candidate binaries dump panic traces
	// to stdout, not stderr. Goroutines fan both pipes into a shared
	// pipe so a Scanner over the result sees interleaved output as
	// it arrives. io.MultiReader is the wrong shape here: it serialises
	// the streams (waits for the first to EOF before reading the
	// second), which deadlocks on a long-running candidate.
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start %s: %w", l.binary, err)
	}
	mergedR, mergedW := io.Pipe()
	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go func() { defer copyWG.Done(); _, _ = io.Copy(mergedW, stderr) }()
	go func() { defer copyWG.Done(); _, _ = io.Copy(mergedW, stdout) }()
	go func() { copyWG.Wait(); _ = mergedW.Close() }()
	p := &localProcess{
		cmd:       cmd,
		errReader: mergedR,
		done:      make(chan struct{}),
	}
	return p, nil
}

// Close is a no-op for the local driver — there's no persistent
// connection to tear down. Returns nil unconditionally.
func (l *Local) Close() error { return nil }

// localProcess is the exec.Cmd-backed Process.
type localProcess struct {
	cmd       *exec.Cmd
	errReader io.Reader

	// done is closed when Wait first observes the process exit. All
	// subsequent Wait calls return the cached result without
	// blocking.
	done chan struct{}

	mu     sync.Mutex
	result WaitResult
	waited bool
}

// PID returns the spawned process id, or 0 before Start has finished.
func (p *localProcess) PID() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Signal forwards sig to the underlying process. Returns nil if the
// process is already gone (matches orchestrator's "fire and forget"
// idiom for shutdown).
func (p *localProcess) Signal(sig int) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	err := p.cmd.Process.Signal(syscall.Signal(sig))
	// os.ErrProcessDone landed in Go 1.16 and means the process
	// already exited. The wrapped error from Signal on a finished
	// process is "os: process already finished" — match either.
	if err == nil || errors.Is(err, errProcessDone) {
		return nil
	}
	if err.Error() == "os: process already finished" {
		return nil
	}
	return err
}

// Wait blocks until the process exits and caches the result.
// Idempotent: subsequent calls return the cached WaitResult without
// re-blocking.
func (p *localProcess) Wait(ctx context.Context) (WaitResult, error) {
	p.mu.Lock()
	if p.waited {
		r := p.result
		p.mu.Unlock()
		return r, nil
	}
	p.mu.Unlock()

	waitErr := make(chan error, 1)
	go func() { waitErr <- p.cmd.Wait() }()
	var err error
	select {
	case err = <-waitErr:
	case <-ctx.Done():
		// Caller cancelled — return their context error but DO NOT
		// kill the process here. The orchestrator owns the lifetime;
		// it'll Signal SIGKILL explicitly if it wants to abandon the
		// child.
		return WaitResult{}, ctx.Err()
	}

	res := WaitResult{}
	if err == nil {
		res.ExitCode = 0
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		state := exitErr.ProcessState
		ws, isWaitStatus := state.Sys().(syscall.WaitStatus)
		if isWaitStatus && ws.Signaled() {
			res.Signaled = true
			res.Signal = int(ws.Signal())
			res.ExitCode = -res.Signal
		} else {
			res.ExitCode = state.ExitCode()
		}
	} else {
		// Generic exec error (couldn't start, pipe broken, ...).
		// Surface to caller via the error return; don't fake a code.
		return WaitResult{}, fmt.Errorf("wait: %w", err)
	}

	p.mu.Lock()
	p.result = res
	p.waited = true
	close(p.done)
	p.mu.Unlock()
	return res, nil
}

// Stderr returns a reader streaming stderr+stdout until process exit.
func (p *localProcess) Stderr() io.Reader { return p.errReader }

// errProcessDone is os.ErrProcessDone, threaded through a package-
// level var so callers don't need to import os. Defined here so the
// import graph is explicit at the file head.
var errProcessDone = newErrProcessDone()

func newErrProcessDone() error {
	// os.ErrProcessDone is a stable sentinel since Go 1.16. We
	// indirect through this constructor so a future stdlib rename
	// only touches one place.
	return errors.New("os: process already finished")
}
