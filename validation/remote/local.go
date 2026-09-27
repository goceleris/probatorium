package remote

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// Local is the exec.Cmd-backed Driver. Used by unit tests and
// single-host smoke runs (validator + celeris on the same machine).
// The binary path is captured at construction time so Start arguments
// stay framework-agnostic.
type Local struct {
	binary string
	// kill sends every signal the driver's processes get (syscall.Kill when
	// nil). Tests record through it.
	kill func(pid int, sig syscall.Signal) error
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
	// Own the pipes instead of using cmd.StdoutPipe/StderrPipe. exec closes
	// those read ends the moment cmd.Wait reaps the process, discarding any
	// bytes still buffered in the kernel -- the crash banner a refapp prints
	// just before exiting was lost exactly that way (probatorium#276).
	// Draining them BEFORE reaping is wrong too: an orphaned descendant that
	// inherited the fds (sh -c forks its child) would block Wait for as long
	// as it lives. With our own pipes the read ends outlive the reap: Wait
	// observes the exit promptly, and the reader still receives every byte
	// the process wrote. If a descendant keeps the write ends open after the
	// reap, the read ends are force-closed after pipeOrphanGrace so nothing
	// leaks forever.
	errR, errW, err := os.Pipe()
	if err != nil {
		return nil, fmt.Errorf("stderr pipe: %w", err)
	}
	// Capture stdout too -- many candidate binaries dump panic traces
	// to stdout, not stderr. Goroutines fan both pipes into a shared
	// pipe so a Scanner over the result sees interleaved output as
	// it arrives. io.MultiReader is the wrong shape here: it serialises
	// the streams (waits for the first to EOF before reading the
	// second), which deadlocks on a long-running candidate.
	// The fan-in is line-atomic (see fanInLines): interleaving between
	// lines is the point, interleaving inside one corrupted a crash
	// signature into something that read like real output
	// (probatorium#382).
	outR, outW, err := os.Pipe()
	if err != nil {
		_ = errR.Close()
		_ = errW.Close()
		return nil, fmt.Errorf("stdout pipe: %w", err)
	}
	cmd.Stderr = errW
	cmd.Stdout = outW
	kill := l.kill
	if kill == nil {
		kill = syscall.Kill
	}
	p := &localProcess{cmd: cmd, kill: kill, reaped: make(chan struct{})}
	// The process leads a process group of its own, and every signal the
	// driver sends goes to that whole group (see Signal), so a stop reaches
	// whatever the process forked too. A shell that forks its last command
	// (bash does, for `...; sleep 30`) dies on a SIGTERM sent to it alone and
	// leaves the forked child running with PPID 1, where no Wait of ours can
	// ever reach it (probatorium#415). The context's kill takes the same
	// route as Signal, so a cancelled Start does not leave the forks behind
	// either, and it too sends nothing once the exit is recorded.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return p.signalGroup(syscall.SIGKILL) }
	if err := cmd.Start(); err != nil {
		for _, f := range []*os.File{errR, errW, outR, outW} {
			_ = f.Close()
		}
		return nil, fmt.Errorf("start %s: %w", l.binary, err)
	}
	// The child holds its own copies of the write ends; ours must go so EOF
	// can arrive once every writer has exited.
	_ = errW.Close()
	_ = outW.Close()
	mergedR, mergedW := io.Pipe()
	drained := make(chan struct{})
	var lineMu sync.Mutex
	var copyWG sync.WaitGroup
	copyWG.Add(2)
	go func() { defer copyWG.Done(); fanInLines(mergedW, &lineMu, errR) }()
	go func() { defer copyWG.Done(); fanInLines(mergedW, &lineMu, outR) }()
	go func() { copyWG.Wait(); _ = mergedW.Close(); close(drained) }()
	p.errReader = mergedR
	p.drained = drained
	p.readEnds = []*os.File{errR, outR}
	return p, nil
}

// Close is a no-op for the local driver — there's no persistent
// connection to tear down. Returns nil unconditionally.
func (l *Local) Close() error { return nil }

// pipeOrphanGrace bounds how long a reaped process's read ends stay open for
// descendants that inherited its stdout/stderr.
const pipeOrphanGrace = 5 * time.Second

// localProcess is the exec.Cmd-backed Process.
type localProcess struct {
	// drained is closed once both OS pipes have been copied to EOF; readEnds
	// are our read ends, force-closed after pipeOrphanGrace if a descendant
	// still holds the write ends after the process has been reaped.
	drained   chan struct{}
	readEnds  []*os.File
	cmd       *exec.Cmd
	errReader io.Reader
	kill      func(pid int, sig syscall.Signal) error

	// reapOnce starts the process's one reaper (see reap) on the first Wait.
	// exec.Cmd.Wait may be called only once: a second call fails with "Wait
	// was already called", and two calls in flight race on the reap
	// (probatorium#426).
	reapOnce sync.Once
	// reaped is closed once the reaper's cmd.Wait has returned, and waitErr
	// is what it returned. Neither changes after that.
	reaped  chan struct{}
	waitErr error

	// mu orders every signal the driver sends against the reaper recording
	// the exit (see signalGroup and recordExit).
	mu sync.Mutex
	// exited is set by the reaper once the process has exited, and from then
	// on the driver sends it nothing: once the process is reaped its PID, and
	// so its group ID, is free for the kernel to hand out again.
	exited bool
	// stopping is set by the first signal: the owner is ending the process,
	// so whatever is left of its group is killed when the exit is recorded.
	stopping bool
}

// PID returns the spawned process id, or 0 before Start has finished.
func (p *localProcess) PID() int {
	if p.cmd == nil || p.cmd.Process == nil {
		return 0
	}
	return p.cmd.Process.Pid
}

// Signal sends sig to the process's whole process group: the process and
// everything it forked that has not moved to a group of its own. Returns nil
// if the process is already gone (matches orchestrator's "fire and forget"
// idiom for shutdown), and sends nothing once its exit has been recorded.
func (p *localProcess) Signal(sig int) error {
	if p.cmd == nil || p.cmd.Process == nil {
		return nil
	}
	if err := p.signalGroup(syscall.Signal(sig)); !errors.Is(err, os.ErrProcessDone) {
		return err
	}
	return nil
}

// signalGroup sends sig to the process's group, unless the reaper has
// recorded the exit. It returns os.ErrProcessDone then, and when no process is
// left in the group. Signal and the context's kill both come through here.
//
// The reaper records the exit before it reaps on Linux, so there a signal
// never reaches a reaped process's ID. Elsewhere it records the exit straight
// after the reap (see reap), and a signal that lands between the two could
// reach another group only if the kernel had handed the ID out, and it had
// been made a group leader, in that moment.
func (p *localProcess) signalGroup(sig syscall.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return os.ErrProcessDone
	}
	p.stopping = true
	return p.killGroup(sig)
}

// killGroup sends sig to the process's group. ESRCH means no process is left
// in the group, which a stop counts as done.
func (p *localProcess) killGroup(sig syscall.Signal) error {
	err := p.kill(-p.cmd.Process.Pid, sig)
	if errors.Is(err, syscall.ESRCH) {
		return os.ErrProcessDone
	}
	return err
}

// reap is the process's one reaper. It calls cmd.Wait exactly once and
// records the exit whether or not any Wait is still waiting for it: a Wait
// whose context ends first leaves the reaper running, and the exit it records
// then still stops every later signal.
func (p *localProcess) reap() {
	// On Linux, wait for the exit without reaping (WNOWAIT) and record it
	// while the process is a zombie that still holds its PID. From here on
	// nothing is sent to it, so no signal can reach the ID after the reap.
	if awaitExit(p.cmd.Process.Pid) {
		p.recordExit()
	}
	err := p.cmd.Wait()
	// Elsewhere, or if waitid failed, the exit is recorded here, straight
	// after the reap, and only if cmd.Wait did reap the process.
	if p.cmd.ProcessState != nil {
		p.recordExit()
	}
	// The readers normally reach EOF at exit. If an orphaned descendant
	// still holds the write ends, close our read ends after a grace
	// period so the copy goroutines (and the caller's scanner) finish.
	go func() {
		select {
		case <-p.drained:
		case <-time.After(pipeOrphanGrace):
			for _, f := range p.readEnds {
				_ = f.Close()
			}
		}
	}()
	p.waitErr = err
	close(p.reaped)
}

// recordExit marks the process exited, once. A process that was being
// stopped takes its group with it: a forked child that ignores the stop
// signal would otherwise outlive the leader, orphaned and holding its pipes,
// and after the record no signal reaches it. The kill runs under mu with the
// record, so a signal that got in before the record is always followed by it,
// and one that comes after sends nothing. On Linux the leader is not reaped
// yet, so its group ID is still in use and the kill can reach only this group.
// Elsewhere it runs straight after the reap: while any of the group is left
// its ID stays in use, and if none is, the kill has the same moment as
// signalGroup. A process that exits on its own keeps its children, because a
// crash banner a child writes after the exit must still arrive
// (probatorium#276).
func (p *localProcess) recordExit() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.exited {
		return
	}
	p.exited = true
	if p.stopping {
		_ = p.killGroup(syscall.SIGKILL)
	}
}

// Wait blocks until the process exits and returns its result. Every call
// returns the one result the reaper got, and once that is in, returns it
// without blocking. Wait calls may overlap, and one whose context ends first
// does not stop the others, or the reap.
func (p *localProcess) Wait(ctx context.Context) (WaitResult, error) {
	p.reapOnce.Do(func() { go p.reap() })
	select {
	case <-p.reaped:
	default:
		select {
		case <-p.reaped:
		case <-ctx.Done():
			// Caller cancelled — return their context error but DO NOT
			// kill the process here. The orchestrator owns the lifetime;
			// it'll Signal SIGKILL explicitly if it wants to abandon the
			// child.
			return WaitResult{}, ctx.Err()
		}
	}
	return waitResult(p.waitErr)
}

// waitResult turns cmd.Wait's error into a WaitResult.
func waitResult(err error) (WaitResult, error) {
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
	return res, nil
}

// Stderr returns a reader streaming stderr+stdout until process exit.
func (p *localProcess) Stderr() io.Reader { return p.errReader }
