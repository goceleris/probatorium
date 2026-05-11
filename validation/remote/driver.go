// Package remote owns the per-process lifecycle of celeris (or any
// candidate binary) that the validator orchestrator drives. It
// abstracts the "start a binary, watch it, signal it, kill it" loop
// across local execution (dev hosts, single-machine smoke tests) and
// remote SSH execution (the production cluster where the validator
// runs on msa2-client and celeris runs on msa2-server / msr1).
//
// The Orchestrator (validation/runner.go) holds a single Driver and
// asks it to spawn a fresh process per Tier 3 replay seed or once
// per Tier 1 run. Tier-specific code never speaks to exec.Cmd or
// golang.org/x/crypto/ssh directly — that keeps the orchestrator
// portable and lets the validator-replay subprocess use the same
// surface in unit tests against a mock Driver.
//
// Concrete implementations:
//
//   - Local — exec.Cmd-backed. Used by tests and by single-host smoke
//     runs (validator + celeris on the same machine).
//   - SSH   — golang.org/x/crypto/ssh-backed. Holds a reusable control
//     connection so per-seed restarts don't pay the TCP handshake +
//     key exchange cost each time. (Lands in a follow-up PR.)
package remote

import (
	"context"
	"io"
)

// Driver is the surface the validator orchestrator uses to manage one
// long-running candidate binary (typically the validation-build of
// celeris). One Driver instance owns one host's lifecycle; the
// orchestrator constructs a Driver per bench_target it drives.
//
// Implementations MUST be safe for concurrent Start / Stop / Kill /
// Wait calls — Tier 3 may Start a seed while Tier 1 is in the middle
// of Stop for the previous run. The Driver is expected to serialize
// internally; callers do NOT hold a mutex.
type Driver interface {
	// Start launches a fresh process with the given argv. Returns a
	// Process handle whose Wait / Signal methods cover the rest of
	// the lifecycle. The args slice does NOT include the binary
	// path — that's set on the Driver at construction time so the
	// orchestrator can stay binary-agnostic.
	//
	// Start blocks until the OS reports the process exists (fork +
	// initial exec succeeded). It does NOT wait for the process to
	// open any listening sockets — bind-readiness is the caller's
	// concern (existing wait-for-bind ansible task is the model).
	Start(ctx context.Context, args []string) (Process, error)

	// Close releases driver-owned resources (SSH control connection,
	// any background goroutines). Safe to call multiple times; only
	// the first call closes.
	Close() error
}

// Process is a handle to one running candidate. Methods are safe for
// concurrent calls from different goroutines (typically Wait runs in
// one goroutine while Signal is called from another).
type Process interface {
	// PID returns the OS process id. For the SSH driver this is the
	// REMOTE pid (the value of `$$` in the spawned shell), not any
	// local id the SSH session uses.
	PID() int

	// Signal sends a signal to the process. Use syscall.SIGTERM for
	// graceful shutdown, syscall.SIGKILL for the hard-stop. Returns
	// nil even if the process is already gone — that matches the
	// orchestrator's "kill, don't care if it's already dead" idiom.
	Signal(sig int) error

	// Wait blocks until the process exits (clean or signalled) and
	// returns its exit code + any captured output. Idempotent: every
	// subsequent call returns the same result without blocking.
	//
	// The returned WaitResult is set BEFORE Wait returns, so a caller
	// that fans Wait out to multiple goroutines sees consistent
	// state.
	Wait(ctx context.Context) (WaitResult, error)

	// Stderr returns a reader for the process's stderr. The reader
	// streams until the process exits AND Wait has been called at
	// least once; after that it returns io.EOF. Used by the
	// validator-checker tail (race / checkptr markers).
	Stderr() io.Reader
}

// WaitResult captures the post-exit state of one process.
type WaitResult struct {
	// ExitCode is the process exit status. 0 for clean exit.
	// Negative for "killed by signal N" (matches Go's exec semantics
	// — encoded as -signal_number).
	ExitCode int

	// Signaled is true when the process exited due to an unhandled
	// signal (typical: SIGSEGV from a panic that bypassed Go's
	// recover, SIGKILL from our hard-stop, SIGTERM from a clean
	// shutdown request, ...).
	Signaled bool

	// Signal is the signal number when Signaled is true; zero
	// otherwise. The orchestrator uses this to classify exits:
	// SIGTERM = clean shutdown we requested; SIGSEGV = bug.
	Signal int
}
