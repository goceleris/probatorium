package validation

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Wall-clock-free drivers for the tier-1 end-to-end tests.
//
// A whole class of tests in this package used to be written as "give the
// cell a fixed deadline of a few hundred milliseconds, then assert that N
// units of work happened". That makes the assertion a function of how
// much work fits inside a fixed budget -- and everything that happens
// before the work comes out of the same budget: fork/exec of /bin/sh,
// waitForReady tailing its output, the pre-flight route probe, goroutine
// scheduling under -race. On a loaded runner the prelude eats the budget,
// the work never happens, and the test goes red with nothing wrong in the
// code it guards. That is probatorium#357: runs 34844660650 (window
// sequence [0 1 0]), 34727613829 and 34352461790.
//
// Enlarging the budget does not fix it, it moves the cliff: the
// idle-window test was "fixed" that way once and the same failure came
// back one window later. The fix is to stop timing the work.
//
// Each driver here turns the fixed CEILING into a condition the test
// waits for. The run ends the moment the thing the test asserts is true,
// and the context deadline becomes a safety net that only a genuinely
// wedged tier trips. Where a test needs the cell to keep running for a
// while (the "this slice stays dormant" tests), the duration becomes a
// FLOOR measured from the first request -- a floor cannot be squeezed out
// by a slow box, only a ceiling can. Slow boxes now make these tests
// slower, never red.
const (
	// tier1TestBudget is the safety net, not a budget the work has to fit
	// in: reaching it means the tier wedged. Deliberately under a minute
	// -- driveTier1 runs a 30 s /healthz warm-up when the cell has a
	// minute or more left, and none of these are warm-up tests.
	tier1TestBudget = 30 * time.Second
	// tier1TestReadyTimeout covers fork/exec plus one line of output.
	// Generous because process spawn latency is not what any of these
	// tests measure, and because it is charged to the safety net above
	// rather than to the work.
	tier1TestReadyTimeout = 15 * time.Second
	// tier1TestTick is how often the live tally is offered to the
	// predicate. Short so the run ends promptly once the condition holds.
	tier1TestTick = 10 * time.Millisecond
)

// readyThenIdle is the stock refapp script for these tests: announce the
// address the walkers should hit, then stay alive. The sleep is long
// enough that a slow box cannot let the test outlive the refapp -- a
// refapp that exits is scored as a crash, which would be a second way for
// load alone to turn a test red. driveTier1 SIGTERMs it on the way out,
// so the length costs nothing.
func readyThenIdle(url string) []string {
	return []string{"-c", `echo "ready addr=` + url + `"; sleep 120`}
}

// runTier1Until drives one Tier 1 cell and ends it as soon as done holds
// on the live tally, rather than at a fixed wall-clock deadline. It fills
// in the timeouts every caller wants and chains any TallyCallback the
// caller set.
//
// done is called from driveTier1's snapshot ticker, on one goroutine; the
// final call below is ordered after that goroutine has finished (the tier
// joins its snapshot writer before returning), so a predicate may keep
// unsynchronised state of its own.
func runTier1Until(t *testing.T, cfg tier1Config, done func(tier1TallySnapshot) bool) (tier1TallySnapshot, error) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), tier1TestBudget)
	defer cancel()
	if cfg.ReadyTimeout < tier1TestReadyTimeout {
		cfg.ReadyTimeout = tier1TestReadyTimeout
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = time.Second
	}
	if cfg.TallyCallbackInterval <= 0 {
		cfg.TallyCallbackInterval = tier1TestTick
	}
	inner := cfg.TallyCallback
	var met atomic.Bool
	cfg.TallyCallback = func(snap tier1TallySnapshot) {
		if inner != nil {
			inner(snap)
		}
		if done != nil && done(snap) {
			met.Store(true)
			cancel()
		}
	}
	started := time.Now()
	s, err := driveTier1(ctx, cfg)
	if err == nil && done != nil && !met.Load() && !done(s) {
		t.Fatalf("the cell ran %s and the condition this test waits for never held (safety budget %s); tally: %s",
			time.Since(started).Round(time.Millisecond), tier1TestBudget, s)
	}
	return s, err
}

// sentAtLeast is the commonest condition: the cell got n requests out.
func sentAtLeast(n int64) func(tier1TallySnapshot) bool {
	return func(s tier1TallySnapshot) bool { return s.RequestsSent >= n }
}

// workedFor turns a duration from a ceiling into a floor: it holds once
// the cell has been SERVING for d, measured from the first tick that saw
// a request rather than from when the test started. The tests that assert
// a slice stays dormant need the cell to run for a while -- a walker that
// wrongly fires on a 100 ms tick needs a few hundred ms of cell to be
// caught -- but they must not also be paying for fork/exec and readiness
// out of the same window, which is what made them both flaky AND liable
// to pass vacuously on a slow box (nothing ran, so nothing fired).
func workedFor(d time.Duration) func(tier1TallySnapshot) bool {
	var first time.Time
	return func(s tier1TallySnapshot) bool {
		if s.RequestsSent < 1 {
			return false
		}
		if first.IsZero() {
			first = time.Now()
		}
		return time.Since(first) >= d
	}
}

// cancelWhen returns a context that ends as soon as reached() is true,
// for the tests that drive a single walker instead of a whole cell. Same
// trade as runTier1Until: the walker runs until it has done the work the
// test asserts, and the deadline is only a wedge detector. The returned
// cancel joins the poller, so nothing of the test outlives it.
func cancelWhen(t *testing.T, reached func() bool) (context.Context, context.CancelFunc) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), tier1TestBudget)
	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			if reached() {
				cancel()
				return
			}
			select {
			case <-stop:
				return
			case <-ctx.Done():
				return
			case <-time.After(tier1TestTick):
			}
		}
	}()
	var once sync.Once
	return ctx, func() {
		once.Do(func() { close(stop) })
		wg.Wait()
		cancel()
	}
}
