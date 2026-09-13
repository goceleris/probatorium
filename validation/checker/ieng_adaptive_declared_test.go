package checker

import (
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// celeris.adaptive_switches only ever moves under the adaptive engine; on
// iouring, epoll and std it is a structural zero, and before celeris#580
// I-ENG-ADAPTIVE passed vacuously in every cell of every run. Without the
// loop's declaration it must read as not instrumented.
func TestIENGAdaptiveIsNotInstrumentedWithoutADeclaration(t *testing.T) {
	e := NewEvaluator(SelectPredicates("engine"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, EngineName: "epoll"}, now)
	if ni := e.Tally().NotInstrumented; !slices.Contains(ni, "I-ENG-ADAPTIVE") {
		t.Fatalf("I-ENG-ADAPTIVE must be not-instrumented without a declaration, got %v", ni)
	}
}

// Declared, the predicate judges the counter: a negative value violates.
func TestIENGAdaptiveJudgesWhenDeclared(t *testing.T) {
	e := NewEvaluator(SelectPredicates("engine"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10, EngineName: "adaptive",
		InstrumentedProperties: "I-ENG-ADAPTIVE", AdaptiveSwitches: 1}, now)
	tally := e.Tally()
	if slices.Contains(tally.NotInstrumented, "I-ENG-ADAPTIVE") || tally.PerPredicate["I-ENG-ADAPTIVE"] != 0 {
		t.Fatalf("declared clean cell must be instrumented and passing, got %v / %v", tally.NotInstrumented, tally.PerPredicate)
	}
	e.Observe(properties.Snapshot{TS: now.Unix() + 1, GoroutineCount: 10, EngineName: "adaptive",
		InstrumentedProperties: "I-ENG-ADAPTIVE", AdaptiveSwitches: -1}, now.Add(time.Second))
	if n := e.Tally().PerPredicate["I-ENG-ADAPTIVE"]; n != 1 {
		t.Fatalf("a negative switch counter must violate once, got %d", n)
	}
}
