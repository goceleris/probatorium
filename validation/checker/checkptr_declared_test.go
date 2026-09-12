package checker

import (
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// TestCheckptrIsNotInstrumentedInANormalBuild: the checker is compiled out
// of a normal refapp, so CheckptrReports is structurally zero there. A pass
// on that zero would be the exact vacuity the declared-only path prevents.
func TestCheckptrIsNotInstrumentedInANormalBuild(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, HeapInuseBytes: 1 << 20}, now)
	if !slices.Contains(e.Tally().NotInstrumented, "I-CHECKPTR") {
		t.Fatalf("I-CHECKPTR reported as instrumented with no checkptr declaration; not_instrumented = %v",
			e.Tally().NotInstrumented)
	}
}

// TestCheckptrFiresWhenDeclaredAndReported closes the loop the property loop
// runs at cell end: a declared cell whose liveness scan counted a checkptr
// throw must produce a violation, not merely count as covered.
func TestCheckptrFiresWhenDeclaredAndReported(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	now := time.Unix(1_700_000_000, 0)
	// A normal tick first, as the real loop would have produced.
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, HeapInuseBytes: 1 << 20,
		InstrumentedProperties: "I-CHECKPTR"}, now)
	// Then the final Observe the loop performs on shutdown: the last good
	// sample, with the count filled in from the liveness scan.
	var ids []string
	for _, v := range e.Observe(properties.Snapshot{TS: now.Unix() + 1, GoroutineCount: 10, HeapInuseBytes: 1 << 20,
		InstrumentedProperties: "I-CHECKPTR", CheckptrReports: 1}, now.Add(time.Second)) {
		ids = append(ids, v.ID)
	}
	if !slices.Contains(ids, "I-CHECKPTR") {
		t.Fatalf("declared cell with checkptr_reports=1 did not raise I-CHECKPTR; violations = %v", ids)
	}
	if slices.Contains(e.Tally().NotInstrumented, "I-CHECKPTR") {
		t.Fatal("I-CHECKPTR still listed as not instrumented after the refapp declared it")
	}
}
