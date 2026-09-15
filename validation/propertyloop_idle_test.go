package validation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/checker"
	"github.com/goceleris/probatorium/validation/properties"
)

// End to end through the real loop, evaluator and predicate: the loop
// stamps every sample with the orchestrator's window, declares I-MEM-2
// when window 2 begins, and the second window is judged against the
// first one's settled count. Run twice: a refapp whose idle count comes
// back (clean) and one that kept 22 goroutines from its load (a leak).
func TestRunPropertyLoop_IMEM2JudgesTheSecondIdleWindow(t *testing.T) {
	saved := properties.IdleSettle
	properties.IdleSettle = time.Second
	t.Cleanup(func() { properties.IdleSettle = saved })

	var window atomic.Int32
	var goroutines atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/vars" {
			w.WriteHeader(404)
			return
		}
		_, _ = fmt.Fprintf(w, `{"goroutines": %d, "celeris.accepted_conn_total": 10, "celeris.closed_conn_total": 9,
			"celeris.active_conns": 1, "celeris.panic_count": 0, "memstats": {"HeapInuse": 4194304, "HeapAlloc": 3000000}}`,
			goroutines.Load())
	}))
	t.Cleanup(srv.Close)

	run := func(secondIdle int64) checker.Tally {
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan checker.Tally, 1)
		go func() {
			done <- runPropertyLoop(ctx, propertyLoopConfig{
				MetricsURL: srv.URL + "/debug/vars",
				Interval:   20 * time.Millisecond,
				Specs:      []properties.Spec{properties.IMEM2},
				IdleWindow: func() int { return int(window.Load()) },
			})
		}()
		phase := func(w int32, g int64, d time.Duration) {
			goroutines.Store(g)
			window.Store(w)
			time.Sleep(d)
		}
		phase(0, 40, 100*time.Millisecond) // readiness + burst
		phase(1, 48, 200*time.Millisecond) // first idle window settles at 48
		phase(0, 70, 200*time.Millisecond) // sustained load
		// Window 2: the settle clock ticks in whole seconds off the sample
		// TS, so 1.6 s guarantees judged samples past the 1 s settle.
		phase(2, secondIdle, 1600*time.Millisecond)
		cancel()
		return <-done
	}

	clean := run(48)
	if clean.IdleWindows != 2 || clean.IdleBaselineGoroutines != 48 {
		t.Fatalf("clean run: windows=%d baseline=%d, want 2/48", clean.IdleWindows, clean.IdleBaselineGoroutines)
	}
	if slices.Contains(clean.NotInstrumented, "I-MEM-2") || slices.Contains(clean.NotJudged, "I-MEM-2") {
		t.Fatalf("clean run: I-MEM-2 must be declared and judged, got not_instrumented=%v not_judged=%v", clean.NotInstrumented, clean.NotJudged)
	}
	if clean.PerPredicate["I-MEM-2"] != 0 {
		t.Fatalf("clean run: no violation expected, got %d: %v", clean.PerPredicate["I-MEM-2"], clean.FailureSummaries)
	}

	window.Store(0)
	leak := run(70)
	if leak.PerPredicate["I-MEM-2"] < 1 {
		t.Fatalf("leak run: 70 idle against 48+8 must violate, got %d (samples=%d skips=%d evals=%d)",
			leak.PerPredicate["I-MEM-2"], leak.Samples, leak.Skips, leak.Evaluations)
	}
	t.Logf("leak run: %s", leak.FailureSummaries["I-MEM-2"])
}
