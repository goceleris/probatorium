package checker

import (
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/properties"
)

// documentedMustStayZero is, written down by hand, every published engine
// counter celeris documents as must-stay-zero that probatorium records but
// does not gate yet, with where celeris says so. report.EngineCounters marks
// each with MustStayZero; this list is the independent copy that catches a
// counter silently losing the mark, or gaining one celeris never claimed.
//
// None is in report.ZeroWitnessMeaning, because that registry IS the gate
// (report.Gate fails a cell on any nonzero entry). A diagnostic counter is not
// gated until a run has said what normal looks like; the move is a gate
// change of its own.
var documentedMustStayZero = map[string]string{
	"engine_transplant_stranded":          "celeris#647, engine.EngineMetrics.TransplantStranded: the two flags it sits between are mutually exclusive by construction",
	"engine_stale_recv_data_transplanted": "celeris#681: W1 (StaleRecvData{Transplanted,Unattributed}) == 0 is the gate the fix supports; celeris#687 lists it among the counters the validator should gate",
	"engine_stale_recv_data_unattributed": "celeris#681: the other half of W1",
	"engine_transplant_handoff_in_flight": "celeris#681: W2 == 0; celeris#676: 'PR 2 must take it to 0'",
	"engine_transplant_hold_rescued":      "engine.EngineMetrics.TransplantHoldRescued: 'must stay 0'",
	"engine_transplant_double_claim":      "engine.EngineMetrics.TransplantDoubleClaim: 'Must stay 0: a release gate can require it'",
	"engine_transplant_reap_failed":       "engine.EngineMetrics.TransplantReapFailed: 'Must stay 0'",
}

// TestEveryDocumentedMustStayZeroCounterIsRecordedButNotGated holds the
// must-stay-zero declarations to the documented set, and to the shape the
// declaration promises: a cumulative counter (a must-stay-zero event count),
// with a series column (a nonzero reading's first question is when), recorded
// in engine_counters at its own value and nowhere in engine_zero_witness.
func TestEveryDocumentedMustStayZeroCounterIsRecordedButNotGated(t *testing.T) {
	var declared, documented []string
	for name, c := range report.EngineCounters {
		if c.MustStayZero != "" {
			declared = append(declared, name)
		}
	}
	for name, where := range documentedMustStayZero {
		documented = append(documented, name)
		if where == "" {
			t.Errorf("documentedMustStayZero lists %s without saying where celeris documents it", name)
		}
	}
	sort.Strings(declared)
	sort.Strings(documented)
	if !slices.Equal(declared, documented) {
		t.Errorf("report.EngineCounters marks %d counter(s) MustStayZero and celeris documents %d; the sets differ:\n  marked:     %v\n  documented: %v",
			len(declared), len(documented), declared, documented)
	}

	e := NewEvaluator(nil)
	snap := properties.Snapshot{EngineName: "adaptive"}
	// One reading per counter, each its own value, set through the fields
	// recordEngineCounters reads; the by-value guards in engine_keys_test.go
	// already hold those fields to the published keys.
	fields := map[string]*int64{
		"engine_transplant_stranded":          &snap.EngineTransplantStranded,
		"engine_stale_recv_data_transplanted": &snap.EngineStaleRecvDataTransplanted,
		"engine_stale_recv_data_unattributed": &snap.EngineStaleRecvDataUnattributed,
		"engine_transplant_handoff_in_flight": &snap.EngineTransplantHandoffInFlight,
		"engine_transplant_hold_rescued":      &snap.EngineTransplantHoldRescued,
		"engine_transplant_double_claim":      &snap.EngineTransplantDoubleClaim,
		"engine_transplant_reap_failed":       &snap.EngineTransplantReapFailed,
	}
	want := map[string]int64{}
	for i, name := range documented {
		p, ok := fields[name]
		if !ok {
			t.Fatalf("%s is documented must-stay-zero and this test has no Snapshot field for it", name)
		}
		want[name] = int64(i + 1)
		*p = want[name]
	}
	e.Observe(snap, time.Unix(1_700_000_000, 0))
	tally := e.Tally()

	var checked int
	for _, name := range documented {
		c, ok := report.EngineCounters[name]
		switch {
		case !ok:
			t.Errorf("%s is documented must-stay-zero and report.EngineCounters does not declare it", name)
			continue
		case c.MustStayZero == "":
			t.Errorf("%s is documented must-stay-zero and its report.EngineCounters entry does not say what a nonzero reading witnesses", name)
		case c.Kind != report.CounterCumulative:
			t.Errorf("%s is declared %q; a must-stay-zero event count is cumulative", name, c.Kind)
		case !c.Series:
			t.Errorf("%s has no series column: a nonzero reading's first question is when, against adaptive_switches", name)
		}
		if _, gated := report.ZeroWitnessMeaning[name]; gated {
			t.Errorf("%s is in report.ZeroWitnessMeaning, which the gate fails a cell on: that is a gate change, not a record", name)
		}
		if _, inWitness := tally.EngineZeroWitness[name]; inWitness {
			t.Errorf("%s reached tally.EngineZeroWitness, the map the gate judges", name)
		}
		if got := tally.EngineCounters[name]; got != want[name] {
			t.Errorf("tally.EngineCounters[%s] = %d, want its own reading %d", name, got, want[name])
			continue
		}
		checked++
	}
	t.Logf("checked %d must-stay-zero counter(s): declared with their meaning, recorded in engine_counters, absent from the gated witness map", checked)
	if checked == 0 {
		t.Fatal("no must-stay-zero counter was checked -- this test is vacuous")
	}
}
