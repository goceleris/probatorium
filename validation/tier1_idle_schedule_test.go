package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/remote"
)

// With IdleWindows the tier runs burst, idle, load, idle: the published
// window goes 0 -> 1 -> 0 -> 2, and no walker request lands inside a
// window (the responsiveness probe's /healthz is the only traffic).
//
// Nothing here is timed. The sequence comes from OnIdleWindowChange, so
// it is what the schedule DID rather than what a sampler managed to
// catch, and the cell ends when the last window opens rather than at a
// deadline. Two earlier shapes of this test were wall-clock races:
//
//   - sampling the pull accessor every 2 ms missed any window the
//     schedule held for less than a tick;
//   - the whole schedule hung off a 6 s deadline, and the load fleet's
//     wind-down (an in-flight request, goroutine scheduling under -race)
//     could outlast the 1 s tail. When it did, holdIdle found the run
//     context already done and published NO second window -- [0 1 0]
//     instead of [0 1 0 2], run 34844660650. That shape had already been
//     "fixed" once by enlarging the deadline, which only moved the
//     failure one window later; the deadline is not the bug.
func TestDriveTier1_IdleWindowsBurstIdleLoadIdle(t *testing.T) {
	saved := [3]time.Duration{idleBurstDuration, idleWindowDuration, idleTailDuration}
	// Burst and window are FLOORS -- how long the schedule spends in a
	// phase -- so shrinking them only makes the test quick. The tail is
	// the opposite: the schedule derives the END of the load phase from
	// the cell deadline minus the tail, so a tail that is nearly the
	// whole budget is what guarantees the load fleet is asked to stop
	// with the rest of the budget still ahead of it, however long its
	// wind-down takes. The budget is never reached: the test ends the
	// cell itself the moment the last window opens.
	idleBurstDuration, idleWindowDuration, idleTailDuration =
		500*time.Millisecond, 300*time.Millisecond, tier1TestBudget-2*time.Second
	t.Cleanup(func() { idleBurstDuration, idleWindowDuration, idleTailDuration = saved[0], saved[1], saved[2] })

	type stamp struct {
		at     time.Time
		window int
	}
	var window atomic.Pointer[func() int]
	current := func() int {
		if f := window.Load(); f != nil {
			return (*f)()
		}
		return 0
	}
	var mu sync.Mutex
	var requests []stamp
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			mu.Lock()
			requests = append(requests, stamp{time.Now(), current()})
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), tier1TestBudget)
	defer cancel()

	// Every transition the schedule publishes, in order. The hook runs
	// inline on driveTier1's own goroutine -- which, since driveTier1 is
	// called synchronously below, is this test's goroutine.
	var seq []int
	cfg := tier1Config{
		Driver:         remote.NewLocal("/bin/sh"),
		RefappArgs:     readyThenIdle(srv.URL),
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    2,
		ReadyTimeout:   tier1TestReadyTimeout,
		RequestTimeout: time.Second,
		IdleWindows:    true,
		OnIdleWindow:   func(f func() int) { window.Store(&f) },
		OnIdleWindowChange: func(w int) {
			seq = append(seq, w)
			// The pull accessor the property loop reads must agree with
			// the transition being announced -- the two must never be
			// able to disagree about which window a sample belongs to.
			if got := current(); got != w {
				t.Errorf("transition announced window %d, accessor reads %d", w, got)
			}
			if w == 2 {
				// The tail window is the last phase and holdIdle holds it
				// until the cell ends, so the cell has nothing left to do:
				// end it here instead of waiting out the safety budget.
				cancel()
			}
		},
	}
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.RequestsSent < 1 {
		t.Fatalf("no requests sent: %+v", s)
	}

	want := []int{0, 1, 0, 2}
	if len(seq) != len(want) {
		t.Fatalf("window sequence must be %v, got %v", want, seq)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("window sequence must be %v, got %v", want, seq)
		}
	}

	mu.Lock()
	defer mu.Unlock()
	perWindow := map[int]int{}
	for _, r := range requests {
		perWindow[r.window]++
	}
	// A window opens only after the fleet that preceded it has joined, so
	// the only walker traffic that can still reach the server inside one
	// is a request its walker abandoned mid-flight -- at most one per
	// walker goroutine. A fleet that kept running would stamp requests
	// through the whole window, hundreds of them against a local
	// httptest server, so this bound is exactly what the design forbids,
	// expressed without a stopwatch. (The previous form allowed "up to
	// 250 ms into the window", which IS a stopwatch: on a loaded box an
	// abandoned request's handler can run later than that with nothing
	// wrong.)
	for w, n := range perWindow {
		if w > 0 && n > cfg.Concurrency {
			t.Errorf("%d walker requests inside idle window %d; at most %d abandoned in-flight requests are possible, more means a fleet kept running",
				n, w, cfg.Concurrency)
		}
	}
	if perWindow[0] == 0 {
		t.Errorf("no walker requests under load: %v", perWindow)
	}
	t.Logf("requests per window: %v; transitions: %v", perWindow, seq)
}

// Without IdleWindows nothing changes: the window stays 0 for the whole
// run and the walkers run to the deadline.
func TestDriveTier1_NoIdleWindowsByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	var window atomic.Pointer[func() int]
	var seq []int
	cfg := tier1Config{
		Driver:             remote.NewLocal("/bin/sh"),
		RefappArgs:         readyThenIdle(srv.URL),
		BaseURL:            srv.URL,
		Matrix:             minimalMatrix(t),
		Seed:               42,
		Concurrency:        2,
		OnIdleWindow:       func(f func() int) { window.Store(&f) },
		OnIdleWindowChange: func(w int) { seq = append(seq, w) },
	}
	if _, err := runTier1Until(t, cfg, sentAtLeast(1)); err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	f := window.Load()
	if f == nil {
		t.Fatal("OnIdleWindow must publish the accessor even when the tier never idles")
	}
	if (*f)() != 0 {
		t.Fatalf("window must stay 0 without IdleWindows, got %d", (*f)())
	}
	if len(seq) != 1 || seq[0] != 0 {
		t.Fatalf("a cell that never idles must announce the one window it is in, got %v", seq)
	}
}
