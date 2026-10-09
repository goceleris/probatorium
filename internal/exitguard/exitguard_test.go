package exitguard

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// Small bounds so the timing cases run in well under a second each. bound is
// the most a case may take before it is a failure: generous against CI
// scheduling noise, still far below "never".
const (
	testTimeout = 150 * time.Millisecond
	testGrace   = 150 * time.Millisecond
	bound       = 5 * time.Second
)

// fakeServer is the part of a celeris.Server the guard sees: a Start that
// blocks until Listen ends, and a Shutdown with a scriptable fault.
type fakeServer struct {
	listenDone chan struct{} // Start returns when this closes
	endListen  bool          // Shutdown ends Listen (a healthy engine)
	err        error         // Shutdown's result
	hang       chan struct{} // non-nil: Shutdown ignores ctx and blocks on it
}

func newFake() *fakeServer { return &fakeServer{listenDone: make(chan struct{})} }

func (f *fakeServer) Start() error {
	<-f.listenDone
	return nil
}

func (f *fakeServer) Shutdown(ctx context.Context) error {
	if f.hang != nil {
		<-f.hang
	}
	if f.endListen {
		close(f.listenDone)
	}
	return f.err
}

// rig wires a Guard to injected signals, exit and log.
type rig struct {
	sigs   chan os.Signal
	mu     sync.Mutex
	exits  []int
	logs   []string
	exited chan int
}

func newRig() *rig { return &rig{sigs: make(chan os.Signal, 2), exited: make(chan int, 8)} }

func (r *rig) cfg() Config {
	return Config{
		Name:    "fake",
		Timeout: testTimeout,
		Grace:   testGrace,
		Signals: r.sigs,
		Exit: func(code int) {
			r.mu.Lock()
			r.exits = append(r.exits, code)
			r.mu.Unlock()
			r.exited <- code
		},
		Logf: func(format string, args ...any) {
			r.mu.Lock()
			defer r.mu.Unlock()
			r.logs = append(r.logs, fmt.Sprintf(format, args...))
		},
	}
}

func (r *rig) snapshot() (exits []int, logs []string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]int(nil), r.exits...), append([]string(nil), r.logs...)
}

// serve runs Serve in a goroutine and returns the channel its result lands on.
func serve(g *Guard, f *fakeServer) <-chan error {
	out := make(chan error, 1)
	go func() { out <- g.Serve(f.Start) }()
	return out
}

func waitExit(t *testing.T, r *rig, within time.Duration) int {
	t.Helper()
	select {
	case c := <-r.exited:
		return c
	case <-time.After(within):
		t.Fatalf("no forced exit within %v: the process would have outlived its signal", within)
		return -1
	}
}

// A healthy engine: Shutdown ends Listen. Nothing is forced, Serve returns
// Start's nil (main then exits 0), and the only log line is the one the
// refapps always printed.
func TestCleanShutdownIsUntouched(t *testing.T) {
	r := newRig()
	f := newFake()
	f.endListen = true
	g := Install(r.cfg(), f.Shutdown)
	res := serve(g, f)
	r.sigs <- syscall.SIGTERM
	select {
	case err := <-res:
		if err != nil {
			t.Fatalf("Serve = %v, want nil", err)
		}
	case <-time.After(bound):
		t.Fatal("Serve did not return after a clean shutdown")
	}
	// Outlast Timeout+Grace: a forced exit scheduled behind a clean shutdown
	// would land here.
	time.Sleep(testTimeout + testGrace + 100*time.Millisecond)
	exits, logs := r.snapshot()
	if len(exits) != 0 {
		t.Fatalf("clean shutdown forced exit %v", exits)
	}
	if len(logs) != 1 || logs[0] != "fake: signal received, shutting down" {
		t.Fatalf("clean-shutdown log = %q, want exactly the signal line", logs)
	}
}

// The celeris#595 shape: Shutdown reports success, Listen never ends.
func TestShutdownNilButListenNeverReturns(t *testing.T) {
	r := newRig()
	f := newFake() // endListen=false
	t.Cleanup(func() { close(f.listenDone) })
	g := Install(r.cfg(), f.Shutdown)
	serve(g, f)
	start := time.Now()
	r.sigs <- syscall.SIGTERM
	if code := waitExit(t, r, bound); code != ExitStartStuck {
		t.Fatalf("exit code %d, want %d", code, ExitStartStuck)
	}
	if d := time.Since(start); d < testGrace {
		t.Fatalf("forced exit after %v, before the %v grace Start was owed", d, testGrace)
	}
	_, logs := r.snapshot()
	if !strings.Contains(strings.Join(logs, "\n"), "Start has not returned") {
		t.Fatalf("the forced exit was not logged: %q", logs)
	}
}

// Shutdown fails: exit non-zero at once, even though Start returns (nil) as
// soon as Listen ends, which would otherwise let main exit 0.
func TestShutdownErrorExitsNonZeroEvenWhenStartReturns(t *testing.T) {
	r := newRig()
	f := newFake()
	f.endListen = true
	f.err = errors.New("drain incomplete")
	g := Install(r.cfg(), f.Shutdown)
	res := serve(g, f)
	r.sigs <- syscall.SIGINT
	if code := waitExit(t, r, bound); code != ExitShutdownFailed {
		t.Fatalf("exit code %d, want %d", code, ExitShutdownFailed)
	}
	select {
	case <-res:
	case <-time.After(bound):
		t.Fatal("Serve never returned")
	}
	exits, logs := r.snapshot()
	if len(exits) != 1 {
		t.Fatalf("exits = %v, want exactly one", exits)
	}
	if !strings.Contains(strings.Join(logs, "\n"), "shutdown failed: drain incomplete") {
		t.Fatalf("the Shutdown error was not logged: %q", logs)
	}
}

// Shutdown ignores its context and blocks: the guard still ends the process
// at Timeout+Grace.
func TestShutdownThatIgnoresItsDeadline(t *testing.T) {
	r := newRig()
	f := newFake()
	f.hang = make(chan struct{})
	t.Cleanup(func() { close(f.hang); close(f.listenDone) })
	g := Install(r.cfg(), f.Shutdown)
	serve(g, f)
	start := time.Now()
	r.sigs <- syscall.SIGTERM
	if code := waitExit(t, r, bound); code != ExitShutdownTimeout {
		t.Fatalf("exit code %d, want %d", code, ExitShutdownTimeout)
	}
	if d := time.Since(start); d < testTimeout {
		t.Fatalf("gave up after %v, before Shutdown's %v deadline", d, testTimeout)
	}
}

// The operator's second signal does not wait for the bound.
func TestSecondSignalExitsImmediately(t *testing.T) {
	r := newRig()
	cfg := r.cfg()
	cfg.Timeout, cfg.Grace = time.Hour, time.Hour // the bound must not be what ends it
	f := newFake()
	f.hang = make(chan struct{})
	t.Cleanup(func() { close(f.hang); close(f.listenDone) })
	g := Install(cfg, f.Shutdown)
	serve(g, f)
	r.sigs <- syscall.SIGTERM
	// Let the first signal be taken before the second is sent.
	time.Sleep(50 * time.Millisecond)
	if exits, _ := r.snapshot(); len(exits) != 0 {
		t.Fatalf("exited %v on the first signal", exits)
	}
	r.sigs <- syscall.SIGINT
	if code := waitExit(t, r, 2*time.Second); code != 128+int(syscall.SIGINT) {
		t.Fatalf("exit code %d, want %d", code, 128+int(syscall.SIGINT))
	}
}

// A Start that fails on its own (no signal) is returned untouched and the
// guard forces nothing.
func TestStartErrorWithoutSignal(t *testing.T) {
	r := newRig()
	g := Install(r.cfg(), func(context.Context) error { return nil })
	want := errors.New("bind: address in use")
	if got := g.Serve(func() error { return want }); got != want {
		t.Fatalf("Serve = %v, want %v", got, want)
	}
	if exits, _ := r.snapshot(); len(exits) != 0 {
		t.Fatalf("forced exit %v without a signal", exits)
	}
}

func TestDefaults(t *testing.T) {
	g := Install(Config{Name: "d", Signals: make(chan os.Signal)}, func(context.Context) error { return nil })
	if g.cfg.Timeout != DefaultTimeout || g.cfg.Grace != DefaultGrace {
		t.Fatalf("defaults = %v/%v, want %v/%v", g.cfg.Timeout, g.cfg.Grace, DefaultTimeout, DefaultGrace)
	}
	if DefaultTimeout != 10*time.Second {
		t.Fatalf("DefaultTimeout = %v: the refapps' shutdown deadline was 10s", DefaultTimeout)
	}
}

// The overall bound is Timeout+Grace from the signal, not Timeout+2*Grace: a
// Shutdown that returns nil just before the hard deadline does not earn Start
// another full Grace.
func TestHardBoundIsNotExtendedByALateNilShutdown(t *testing.T) {
	const to, grace = 600 * time.Millisecond, 600 * time.Millisecond
	r := newRig()
	cfg := r.cfg()
	cfg.Timeout, cfg.Grace = to, grace
	f := newFake() // Listen never ends
	t.Cleanup(func() { close(f.listenDone) })
	g := Install(cfg, func(ctx context.Context) error {
		time.Sleep(1000 * time.Millisecond) // returns nil 200ms before to+grace
		return nil
	})
	serve(g, f)
	start := time.Now()
	r.sigs <- syscall.SIGTERM
	if code := waitExit(t, r, bound); code != ExitStartStuck {
		t.Fatalf("exit code %d, want %d", code, ExitStartStuck)
	}
	// to+grace = 1200ms; an extra full grace would land at 1600ms.
	if d := time.Since(start); d > 1450*time.Millisecond {
		t.Fatalf("forced exit after %v: the bound is %v, not %v", d, to+grace, to+2*grace)
	}
}
