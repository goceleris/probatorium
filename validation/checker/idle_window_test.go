package checker

import (
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// idleFeeder streams 1 Hz samples through an Evaluator, declaring I-MEM-2
// from window 2 on exactly as the property loop does.
type idleFeeder struct {
	ev   *Evaluator
	base int64
	i    int
}

func (f *idleFeeder) feed(window int, goroutines int64, n int) {
	for k := 0; k < n; k++ {
		snap := properties.Snapshot{TS: f.base + int64(f.i), GoroutineCount: goroutines, IdleWindow: window}
		if window >= 2 {
			snap.InstrumentedProperties = "I-MEM-2"
		}
		f.ev.Observe(snap, time.Unix(f.base+int64(f.i), 0))
		f.i++
	}
}

func TestEvaluator_LeavingTheFirstIdleWindowFixesTheBaseline(t *testing.T) {
	ev := NewEvaluator([]properties.Spec{properties.IMEM2})
	f := &idleFeeder{ev: ev, base: 1_700_000_000}

	f.feed(0, 40, 3) // readiness, then the burst
	f.feed(1, 52, 2) // first idle window, still draining
	f.feed(1, 44, 1) // ...settled
	if c := ev.Context(); c.IdleBaselineGoroutines != 0 || !c.LoadStartedAt.IsZero() {
		t.Fatalf("inside window 1 nothing is fixed yet, got baseline=%d load=%v", c.IdleBaselineGoroutines, c.LoadStartedAt)
	}

	f.feed(0, 70, 3) // sustained load
	c := ev.Context()
	if c.IdleBaselineGoroutines != 44 {
		t.Fatalf("baseline must be window 1's LAST sample (44), got %d", c.IdleBaselineGoroutines)
	}
	if want := time.Unix(f.base+6, 0); !c.LoadStartedAt.Equal(want) {
		t.Fatalf("LoadStartedAt must be the first load sample after window 1 (%v), got %v", want, c.LoadStartedAt)
	}
	if c.IdleWindow != 0 || c.IdleMode {
		t.Fatalf("under load IdleWindow/IdleMode must be clear, got %d/%v", c.IdleWindow, c.IdleMode)
	}

	// Second window at the same level: judged, clean.
	f.feed(2, 44, 35)
	tally := ev.Tally()
	if tally.PerPredicate["I-MEM-2"] != 0 {
		t.Fatalf("clean second window must not violate, got %d", tally.PerPredicate["I-MEM-2"])
	}
	if slices.Contains(tally.NotInstrumented, "I-MEM-2") || slices.Contains(tally.NotJudged, "I-MEM-2") {
		t.Fatalf("I-MEM-2 must count as instrumented and judged, got not_instrumented=%v not_judged=%v", tally.NotInstrumented, tally.NotJudged)
	}
	if tally.IdleWindows != 2 || tally.IdleBaselineGoroutines != 44 {
		t.Fatalf("tally must record windows=2 baseline=44, got %d/%d", tally.IdleWindows, tally.IdleBaselineGoroutines)
	}

	// Sixteen goroutines that never left: over 44+8 for Persist samples.
	f.feed(2, 60, 3)
	tally = ev.Tally()
	if tally.PerPredicate["I-MEM-2"] != 1 {
		t.Fatalf("a settled second window over budget must violate once after Persist, got %d", tally.PerPredicate["I-MEM-2"])
	}
}

// A cell that never idled reports I-MEM-2 as not instrumented, never as a
// pass on a predicate that only skipped.
func TestEvaluator_NeverIdledIsNotInstrumented(t *testing.T) {
	ev := NewEvaluator([]properties.Spec{properties.IMEM2})
	f := &idleFeeder{ev: ev, base: 1_700_000_000}
	f.feed(0, 40, 40)
	tally := ev.Tally()
	if !slices.Contains(tally.NotInstrumented, "I-MEM-2") {
		t.Fatalf("I-MEM-2 must be not-instrumented without idle windows, got %v (passed=%d)", tally.NotInstrumented, tally.Passed())
	}
	if tally.Passed() != 0 {
		t.Fatalf("nothing can have passed, got %d", tally.Passed())
	}
}
