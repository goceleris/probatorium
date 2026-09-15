package checker

import (
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// I-RACE's count is structurally zero in every plain build; without the
// loop's declaration it must read as not instrumented.
func TestIRACEIsNotInstrumentedWithoutADeclaration(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, HeapInuseBytes: 1 << 20}, now)
	if ni := e.Tally().NotInstrumented; !slices.Contains(ni, "I-RACE") {
		t.Fatalf("I-RACE must be not-instrumented without a declaration, got %v", ni)
	}
}

// Declared by a -race build, a report is a violation.
func TestIRACEFiresOnADeclaredReport(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, HeapInuseBytes: 1 << 20,
		RaceBuild: true, InstrumentedProperties: "I-RACE"}, now)
	tally := e.Tally()
	if slices.Contains(tally.NotInstrumented, "I-RACE") || tally.PerPredicate["I-RACE"] != 0 {
		t.Fatalf("declared clean cell must be instrumented and passing, got %v / %v", tally.NotInstrumented, tally.PerPredicate)
	}
	e.Observe(properties.Snapshot{TS: now.Unix() + 1, GoroutineCount: 10, HeapInuseBytes: 1 << 20,
		RaceBuild: true, InstrumentedProperties: "I-RACE", RaceReports: 1}, now.Add(time.Second))
	if n := e.Tally().PerPredicate["I-RACE"]; n != 1 {
		t.Fatalf("one report must violate once, got %d", n)
	}
}

func TestParseDebugVars_RaceBuild(t *testing.T) {
	var snap properties.Snapshot
	if err := ParseDebugVars([]byte(`{"goroutines": 1, "celeris.race_build": true}`), &snap); err != nil {
		t.Fatal(err)
	}
	if !snap.RaceBuild {
		t.Fatalf("race_build not parsed: %+v", snap)
	}
}
