package report

import (
	"strings"
	"testing"
)

// probatorium#297: 9 of 14 requested predicates were not_instrumented in
// EVERY cell of both the passing nightly and the failing soak, and the run
// reported PASS. A predicate that judged nothing anywhere is a hole in the
// matrix, so the gate fails on it unless the hole is on the waiver record.
func TestGate_UninstrumentedEverywhereIsAViolation(t *testing.T) {
	a := cleanCell("auth_session_ratelimit", "iouring", "arm64")
	b := cleanCell("kitchen_sink", "std", "amd64")
	a.PropertiesNotInstrumented = []string{"I-MW-SESSION", "I-RACE"}
	b.PropertiesNotInstrumented = []string{"I-MW-SESSION", "I-RACE"}
	opts := GateOptions{RequireTier3: true, RequireProperties: true, RequireInstrumented: true}

	v := Gate([]ValidationCellResult{a, b}, nil, opts)
	if len(v) != 1 {
		t.Fatalf("want exactly one violation (I-RACE is waived), got %v", v)
	}
	if v[0].Field != "properties_not_instrumented.I-MW-SESSION" {
		t.Fatalf("violation must name the predicate, got %q", v[0].Field)
	}
	if !strings.Contains(v[0].Why, "every cell") {
		t.Fatalf("Why must say the predicate judged nothing in every cell, got %q", v[0].Why)
	}

	// Instrumented in ONE cell is enough: the predicate has a data source
	// somewhere in the matrix, and the cells whose refapp does not install
	// that middleware are legitimately silent.
	a.PropertiesNotInstrumented = []string{"I-RACE"}
	if v := Gate([]ValidationCellResult{a, b}, nil, opts); len(v) != 0 {
		t.Fatalf("one instrumented cell must clear the predicate, got %v", v)
	}

	// Off by default so a pre-5.6 run (no property fields at all) and an
	// explicit opt-out behave exactly as before.
	a.PropertiesNotInstrumented = []string{"I-MW-SESSION", "I-RACE"}
	if v := Gate([]ValidationCellResult{a, b}, nil, GateOptions{RequireTier3: true}); len(v) != 0 {
		t.Fatalf("the check must be opt-in, got %v", v)
	}
}

// Every waived ID must carry a reason: the point of the waiver list is that
// a hole is DECLARED, not silent.
func TestWaivedUninstrumented_ReasonsAreOnRecord(t *testing.T) {
	for _, id := range []string{"I-RACE", "I-CHECKPTR"} {
		if strings.TrimSpace(WaivedUninstrumented[id]) == "" {
			t.Errorf("%s must carry a waiver reason", id)
		}
	}
	for id, why := range WaivedUninstrumented {
		if strings.TrimSpace(why) == "" {
			t.Errorf("%s is waived with an empty reason", id)
		}
	}
	// I-DRV is instrumented in the driver cells now; a waiver would hide a
	// regression of exactly that coverage.
	if _, ok := WaivedUninstrumented["I-DRV"]; ok {
		t.Error("I-DRV must not be waived: the driver refapps declare it")
	}
	// The three middleware predicates are instrumented now; waiving them
	// would re-hide exactly the gap probatorium#297 closed.
	for _, id := range []string{"I-MW-SESSION", "I-MW-JWT", "I-MW-RATELIMIT"} {
		if _, ok := WaivedUninstrumented[id]; ok {
			t.Errorf("%s must not be waived: it has a refapp-side data source", id)
		}
	}
}

// A cell whose property loop never ran cannot vote on instrumentation: it
// reports nothing, and counting it as "not instrumented here" would fail
// every predicate on an ssh-driven run.
func TestGate_InstrumentationIgnoresCellsWithoutALoop(t *testing.T) {
	a := cleanCell("auth_session_ratelimit", "iouring", "arm64")
	a.PropertiesNotInstrumented = []string{"I-MW-SESSION"}
	b := cleanCell("kitchen_sink", "std", "amd64")
	b.Tier1.PropertyEvaluations = 0
	b.Tier1.PropertyLoopSkipped = "ssh driver: remote /debug/vars is loopback-only"

	opts := GateOptions{RequireTier3: true, RequireProperties: true, RequireInstrumented: true}
	v := Gate([]ValidationCellResult{a, b}, nil, opts)
	if len(v) != 1 || v[0].Field != "properties_not_instrumented.I-MW-SESSION" {
		t.Fatalf("want the one violation from the cell that DID run a loop, got %v", v)
	}
	// No cell ran a loop at all: nothing to conclude, no violation.
	if v := Gate([]ValidationCellResult{b}, nil, opts); len(v) != 0 {
		t.Fatalf("no property loop anywhere means no instrumentation verdict, got %v", v)
	}
}

// A tier built to cover a predicate must not be able to fall back on the
// waiver: with ExpectInstrumented the cell that never declared the
// predicate fails, by name, even though the ID is on the waiver record.
func TestGate_ExpectInstrumentedOverridesTheWaiver(t *testing.T) {
	a := cleanCell("auth_session_ratelimit", "iouring", "arm64")
	b := cleanCell("kitchen_sink", "std", "amd64")
	a.PropertiesNotInstrumented = []string{"I-MEM-2"}
	b.PropertiesNotInstrumented = []string{"I-MEM-2"}
	if _, waived := WaivedUninstrumented["I-MEM-2"]; !waived {
		t.Fatal("this test needs I-MEM-2 on the waiver record")
	}
	base := GateOptions{RequireTier3: true, RequireProperties: true, RequireInstrumented: true}

	// Waived, not expected: silent.
	if v := Gate([]ValidationCellResult{a, b}, nil, base); len(v) != 0 {
		t.Fatalf("a waived predicate must not fail by itself, got %v", v)
	}

	// Expected: one violation per cell that never declared it.
	opts := base
	opts.ExpectInstrumented = []string{"I-MEM-2"}
	v := Gate([]ValidationCellResult{a, b}, nil, opts)
	if len(v) != 2 {
		t.Fatalf("want one violation per undeclaring cell, got %v", v)
	}
	for _, x := range v {
		if x.Field != "properties_not_instrumented.I-MEM-2" || x.Refapp == "*" {
			t.Fatalf("violation must name the predicate and the cell, got %+v", x)
		}
		if !strings.Contains(x.Why, "VALIDATE_GATE_EXPECT_INSTRUMENTED") {
			t.Fatalf("Why must name the knob, got %q", x.Why)
		}
	}

	// Declared in one cell, missing in the other: only the other fails.
	a.PropertiesNotInstrumented = nil
	v = Gate([]ValidationCellResult{a, b}, nil, opts)
	if len(v) != 1 || v[0].Refapp != "kitchen_sink" {
		t.Fatalf("only the undeclaring cell may fail, got %v", v)
	}

	// A cell whose property loop never ran (ssh driver) has nothing to say.
	b.Tier1.PropertyLoopSkipped = "ssh driver"
	if v := Gate([]ValidationCellResult{a, b}, nil, opts); len(v) != 0 {
		t.Fatalf("a cell that ran no property loop cannot be expected to declare, got %v", v)
	}
}
