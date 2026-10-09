// Package exitguard ends a harness server process on SIGTERM / SIGINT, with
// a bound, whatever the engine underneath does.
//
// Every refapp (validation/refapp/*) and the celeris bench SUT
// (servers/celeris) used to end the same way: a goroutine caught the signal,
// called srv.Shutdown(ctx) and threw the result away, while main stayed
// blocked in srv.Start / StartWithListener. That only works if Shutdown ends
// Listen. On a celeris build where it does not (celeris#595: the io_uring and
// epoll Engine.Shutdown was a no-op and Listen waited on a Background
// context) the process outlives its signal for ever. One such -race refapp,
// left behind by a smoke test on 2026-09-11, ran on msr1 for 28 days and
// accounted for 98.5 % of the host's IO pressure (probatorium#473). SIGTERM
// and SIGINT both reached the same stale handler, so only SIGKILL ended it.
//
// Install replaces that goroutine and Guard.Serve replaces the bare Start
// call:
//
//	guard := exitguard.Install(exitguard.Config{Name: "kitchen_sink"}, srv.Shutdown)
//	...
//	if err := guard.Serve(func() error { return srv.StartWithListener(ln) }); err != nil {
//		log.Fatalf("kitchen_sink: start: %v", err)
//	}
//
// On the first signal the guard logs "<name>: signal received, shutting
// down" (the line the refapps always printed), runs shutdown under
// Config.Timeout and then:
//
//   - shutdown returned an error: logs it and exits [ExitShutdownFailed]
//     at once; a half-drained server is not left to idle.
//   - shutdown returned nil: Start is expected to return within
//     Config.Grace (and never later than Timeout+Grace after the signal),
//     main then ends exactly as before (exit 0, no further log line). If
//     it does not, the guard logs that and exits [ExitStartStuck]. This is the celeris#595 shape: Shutdown reports
//     success and Listen never ends.
//   - shutdown has not returned by Timeout+Grace (it ignores its context):
//     the guard logs that and exits [ExitShutdownTimeout].
//
// A second signal at any point exits at once with 128+signal, the shell
// convention for "died of that signal".
package exitguard

import (
	"context"
	"log"
	"os"
	"os/signal"
	"sync"
	"syscall"
	"time"
)

// Exit codes the guard forces. They are all non-zero on purpose: a process
// that needed forcing did not shut down cleanly, and the harness should be
// able to tell. A clean shutdown is never forced and exits 0 through main.
const (
	// ExitShutdownFailed: the shutdown func returned an error.
	ExitShutdownFailed = 1
	// ExitShutdownTimeout: the shutdown func did not return by Timeout+Grace.
	ExitShutdownTimeout = 2
	// ExitStartStuck: shutdown returned nil, but Start was still running
	// Grace later.
	ExitStartStuck = 3
)

const (
	// DefaultTimeout is the deadline given to the shutdown func: the 10 s
	// every refapp already used.
	DefaultTimeout = 10 * time.Second
	// DefaultGrace is how long Start has to return after a shutdown that
	// reported success, and how long a shutdown that ignores its context
	// gets past its deadline, before the guard forces the exit.
	DefaultGrace = 5 * time.Second
)

// Config configures [Install]. The zero value of every field but Name
// selects the production behaviour; the rest exist for tests.
type Config struct {
	// Name prefixes every log line ("kitchen_sink: signal received, ...").
	Name string
	// Timeout is the shutdown func's deadline. 0 = [DefaultTimeout].
	Timeout time.Duration
	// Grace: see [DefaultGrace]. 0 = DefaultGrace.
	Grace time.Duration

	// Logf is the logger. nil = log.Printf.
	Logf func(format string, args ...any)
	// Exit ends the process. nil = os.Exit. A test substitutes a recorder;
	// the guard never relies on it not returning.
	Exit func(code int)
	// Signals delivers the signals. nil = a channel registered with
	// signal.Notify for SIGTERM and SIGINT.
	Signals <-chan os.Signal
}

// Guard is an installed signal watcher. See [Install].
type Guard struct {
	cfg      Config
	shutdown func(context.Context) error

	startReturned chan struct{} // closed by Serve once start has returned
	signaled      chan struct{} // closed when the first signal arrives
	settled       chan struct{} // closed when the watcher has decided the outcome
	once          sync.Once
}

// Install registers the signal handler and starts watching. It must run
// before the listener is bound (where the old handler goroutine started),
// so a signal that lands during startup is not lost to the default action
// without a record. shutdown is the graceful stop, normally srv.Shutdown;
// anything that must run before it (closing hubs, cancelling a lifetime
// context) belongs inside it, so that work is bounded too.
func Install(cfg Config, shutdown func(context.Context) error) *Guard {
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if cfg.Grace <= 0 {
		cfg.Grace = DefaultGrace
	}
	if cfg.Logf == nil {
		cfg.Logf = log.Printf
	}
	if cfg.Exit == nil {
		cfg.Exit = os.Exit
	}
	if cfg.Signals == nil {
		// Buffered for two: the second signal must never be dropped while
		// the first is being handled.
		ch := make(chan os.Signal, 2)
		signal.Notify(ch, syscall.SIGTERM, syscall.SIGINT)
		cfg.Signals = ch
	}
	g := &Guard{
		cfg:           cfg,
		shutdown:      shutdown,
		startReturned: make(chan struct{}),
		signaled:      make(chan struct{}),
		settled:       make(chan struct{}),
	}
	go g.watch()
	return g
}

// Serve runs start (srv.Start, srv.StartWithListener, ...) and returns its
// result. If a signal arrived, Serve first waits for the guard to settle the
// outcome, so a failed shutdown exits non-zero even when start returned
// first; that wait is bounded by Timeout+Grace.
func (g *Guard) Serve(start func() error) error {
	err := start()
	g.once.Do(func() { close(g.startReturned) })
	select {
	case <-g.signaled:
		<-g.settled
	default:
	}
	return err
}

func (g *Guard) logf(format string, args ...any) {
	g.cfg.Logf(g.cfg.Name+": "+format, args...)
}

// watch is the one goroutine: first signal -> bounded shutdown -> verdict.
func (g *Guard) watch() {
	<-g.cfg.Signals
	close(g.signaled)
	g.logf("signal received, shutting down")
	// From here on every further signal is the operator saying "now".
	go func() {
		for s := range g.cfg.Signals {
			g.logf("second signal (%v) while shutting down; exiting immediately", s)
			g.cfg.Exit(exitCode(s))
		}
	}()

	ctx, cancel := context.WithTimeout(context.Background(), g.cfg.Timeout)
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- g.shutdown(ctx) }()

	hard := time.NewTimer(g.cfg.Timeout + g.cfg.Grace)
	defer hard.Stop()
	select {
	case err := <-done:
		if err != nil {
			g.logf("shutdown failed: %v; exiting %d", err, ExitShutdownFailed)
			g.settle(ExitShutdownFailed)
			return
		}
	case <-hard.C:
		g.logf("shutdown did not return within %v (+%v grace); exiting %d", g.cfg.Timeout, g.cfg.Grace, ExitShutdownTimeout)
		g.settle(ExitShutdownTimeout)
		return
	}

	// Shutdown reported success. A clean server's Start has returned (or
	// returns now) and main ends with exit 0: stay silent.
	grace := time.NewTimer(g.cfg.Grace)
	defer grace.Stop()
	select {
	case <-g.startReturned:
		close(g.settled)
	case <-grace.C:
		g.stuck()
	case <-hard.C: // the overall bound: the signal's Timeout+Grace, never later
		g.stuck()
	}
}

func (g *Guard) stuck() {
	g.logf("shutdown returned nil but Start has not returned (the engine cannot end Listen); exiting %d", ExitStartStuck)
	g.settle(ExitStartStuck)
}

// settle forces the exit and releases Serve. close comes after Exit so a
// test's recorder has seen the code by the time Serve returns.
func (g *Guard) settle(code int) {
	g.cfg.Exit(code)
	close(g.settled)
}

// exitCode is 128+signo for a syscall.Signal, the shell's convention.
func exitCode(s os.Signal) int {
	if n, ok := s.(syscall.Signal); ok {
		return 128 + int(n)
	}
	return 130
}
