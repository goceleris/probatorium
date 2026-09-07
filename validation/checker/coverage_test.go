package checker

import (
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// slopeOracles are the predicates that need a window before they can
// say anything at all.
var slopeOracles = []string{"I-MEM-1", "I-MEM-3", "I-MEM-4"}

// A 150 s nightly cell cannot reach the slope oracles' 15 min
// (warm-up + span). They are not judged, and the tally must say the
// cell was structurally incapable of judging them -- otherwise the
// absolute gate cannot tell this silence from an oracle that had the
// time and stayed quiet (probatorium#299).
func TestEvaluator_ShortCellSilenceIsByDesign(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	start := time.Unix(1_700_000_000, 0)
	for i := 0; i <= 150; i++ {
		now := start.Add(time.Duration(i) * time.Second)
		e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 100, HeapInuseBytes: 50 << 20, RSSBytes: 200 << 20}, now)
	}
	tl := e.Tally()
	if tl.Observed != 150*time.Second {
		t.Fatalf("observed=%s want 150s", tl.Observed)
	}
	for _, id := range slopeOracles {
		if !slices.Contains(tl.NotJudged, id) {
			t.Fatalf("%s must be not judged in a 150 s cell: %v", id, tl.NotJudged)
		}
		if !slices.Contains(tl.NotJudgedByDesign, id) {
			t.Fatalf("%s could not have been judged in a 150 s cell: by_design=%v", id, tl.NotJudgedByDesign)
		}
	}
	// Only the windowed oracles are excused; a predicate that judges
	// from a single snapshot has no excuse to be in the list.
	for _, id := range tl.NotJudgedByDesign {
		if !slices.Contains(slopeOracles, id) {
			t.Fatalf("%s declares no MinObservation and must not be excused: %v", id, tl.NotJudgedByDesign)
		}
	}
}

// The same silence in a cell that HAD the time is a coverage gap, not a
// design consequence. Here the loop polls once every 30 s (a refapp
// whose /debug/vars answers only every other tick), so I-MEM-3's 10 min
// window never holds the 60 samples it needs -- for 40 minutes.
func TestEvaluator_SparseHistoryInALongCellIsNotByDesign(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	start := time.Unix(1_700_000_000, 0)
	for i := 0; i <= 40*60; i++ {
		now := start.Add(time.Duration(i) * time.Second)
		if i%30 != 0 {
			e.RecordPollError(now)
			continue
		}
		e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 100, HeapInuseBytes: 50 << 20, RSSBytes: 200 << 20}, now)
	}
	tl := e.Tally()
	if tl.Observed != 40*time.Minute {
		t.Fatalf("observed=%s want 40m", tl.Observed)
	}
	if !slices.Contains(tl.NotJudged, "I-MEM-3") {
		t.Fatalf("a 30 s cadence starves I-MEM-3's 60-sample window: not_judged=%v", tl.NotJudged)
	}
	if slices.Contains(tl.NotJudgedByDesign, "I-MEM-3") {
		t.Fatalf("the cell ran 40 min: I-MEM-3's silence is a gap, not a design consequence: %v", tl.NotJudgedByDesign)
	}
}

// A refapp whose /debug/vars dies two minutes into a 30 minute cell
// must not have its silent oracles excused: the cell was long enough,
// the samples were not there. The observation window therefore counts
// poll ATTEMPTS, not successes.
func TestEvaluator_DeadEndpointDoesNotShrinkTheWindow(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	start := time.Unix(1_700_000_000, 0)
	for i := 0; i <= 30*60; i++ {
		now := start.Add(time.Duration(i) * time.Second)
		if i > 120 {
			e.RecordPollError(now)
			continue
		}
		e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 100, HeapInuseBytes: 50 << 20, RSSBytes: 200 << 20}, now)
	}
	tl := e.Tally()
	if tl.Observed != 30*time.Minute {
		t.Fatalf("observed=%s want 30m (poll errors widen the window)", tl.Observed)
	}
	for _, id := range slopeOracles {
		if slices.Contains(tl.NotJudgedByDesign, id) {
			t.Fatalf("%s: a dead endpoint is not a short cell: %v", id, tl.NotJudgedByDesign)
		}
	}
}

// The soak shape: an hour of dense samples judges every slope oracle,
// so nothing is not-judged and nothing is excused.
func TestEvaluator_LongCellJudgesTheSlopeOracles(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	start := time.Unix(1_700_000_000, 0)
	for i := 0; i <= 20*60; i++ {
		now := start.Add(time.Duration(i) * time.Second)
		e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 100, HeapInuseBytes: 50 << 20, RSSBytes: 200 << 20}, now)
	}
	tl := e.Tally()
	for _, id := range slopeOracles {
		if slices.Contains(tl.NotJudged, id) || slices.Contains(tl.NotJudgedByDesign, id) {
			t.Fatalf("%s judged from t=15min: not_judged=%v by_design=%v", id, tl.NotJudged, tl.NotJudgedByDesign)
		}
	}
}
