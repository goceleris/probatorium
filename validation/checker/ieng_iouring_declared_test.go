package checker

import (
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// I-ENG-IOURING's counter is the zero of a stub in every plain build and
// in every non-io_uring cell. Without the loop's declaration it must read
// as not instrumented.
func TestIENGIOURingIsNotInstrumentedWithoutADeclaration(t *testing.T) {
	e := NewEvaluator(SelectPredicates("engine"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, EngineName: "iouring"}, now)
	if ni := e.Tally().NotInstrumented; !slices.Contains(ni, "I-ENG-IOURING") {
		t.Fatalf("I-ENG-IOURING must be not-instrumented without a declaration, got %v", ni)
	}
}

// Declared, a corruption count is a violation.
func TestIENGIOURingFiresOnADeclaredCorruption(t *testing.T) {
	e := NewEvaluator(SelectPredicates("engine"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, EngineName: "iouring",
		ValidationBuild: true, InstrumentedProperties: "I-ENG-IOURING"}, now)
	tally := e.Tally()
	if slices.Contains(tally.NotInstrumented, "I-ENG-IOURING") || tally.PerPredicate["I-ENG-IOURING"] != 0 {
		t.Fatalf("declared clean cell must be instrumented and passing, got %v / %v", tally.NotInstrumented, tally.PerPredicate)
	}
	e.Observe(properties.Snapshot{TS: now.Unix() + 1, GoroutineCount: 10, EngineName: "iouring",
		ValidationBuild: true, InstrumentedProperties: "I-ENG-IOURING", IouringSQECorruptions: 1}, now.Add(time.Second))
	if n := e.Tally().PerPredicate["I-ENG-IOURING"]; n != 1 {
		t.Fatalf("one corruption must violate once, got %d", n)
	}
}

func TestParseDebugVars_ValidationBuildAndSQECorruptions(t *testing.T) {
	var snap properties.Snapshot
	err := ParseDebugVars([]byte(`{"goroutines": 1, "celeris.engine": "iouring", "celeris.validation_build": true,
		"celeris.iouring_sqe_corruptions": 3}`), &snap)
	if err != nil {
		t.Fatal(err)
	}
	if !snap.ValidationBuild || snap.IouringSQECorruptions != 3 || snap.EngineName != "iouring" {
		t.Fatalf("not parsed: %+v", snap)
	}
}
