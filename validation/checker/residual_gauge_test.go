package checker

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// celeris#687's five residual gauges are the connections a draining engine
// still holds, by the reason the hand-off refused them, and celeris asks for
// them to be read at every switch verdict. The tally keeps each one's PEAK:
// the only reduction under which a zero clears every switch the cell made.
//
// A reflective "does the key reach the tally" guard cannot see the combining
// rule -- a summed or last-kept gauge is present and nonzero all the same --
// and TestEachEngineCounterIsReducedByItsDeclaredKind holds the reducer to
// whatever kind is DECLARED, so a wrong declaration passes it. These tests
// write the expected numbers down independently of the declaration.

// residualSample publishes one /debug/vars document with the five gauges at
// the given readings, through the real parse.
func residualSample(t *testing.T, busy, detached, h2, pinned, unstarted int64) properties.Snapshot {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"celeris.engine": "adaptive",
		"celeris.engine_transplant_residual_busy":      busy,
		"celeris.engine_transplant_residual_detached":  detached,
		"celeris.engine_transplant_residual_h2":        h2,
		"celeris.engine_transplant_residual_pinned":    pinned,
		"celeris.engine_transplant_residual_unstarted": unstarted,
	})
	if err != nil {
		t.Fatal(err)
	}
	var snap properties.Snapshot
	if err := ParseDebugVars(body, &snap); err != nil {
		t.Fatalf("ParseDebugVars: %v", err)
	}
	return snap
}

// TestTheResidualGaugesKeepTheirPeakNotTheirSumOrLast drives the five gauges
// through three samples of one switch -- the drain starts, peaks, and settles
// with the permanent residue decaying as its streams close -- at readings
// where the peak, the sum, the first and the last reading are four different
// numbers for every gauge, so a reducer applying any other rule fails.
func TestTheResidualGaugesKeepTheirPeakNotTheirSumOrLast(t *testing.T) {
	type gauge struct {
		key      string
		readings [3]int64
	}
	gauges := []gauge{
		{"engine_transplant_residual_busy", [3]int64{3, 7, 0}},
		{"engine_transplant_residual_detached", [3]int64{4, 6, 1}},
		{"engine_transplant_residual_h2", [3]int64{1, 5, 2}},
		{"engine_transplant_residual_pinned", [3]int64{2, 8, 3}},
		{"engine_transplant_residual_unstarted", [3]int64{5, 9, 4}},
	}
	e := NewEvaluator(nil)
	start := time.Unix(1_700_000_000, 0)
	for s := range 3 {
		e.Observe(residualSample(t,
			gauges[0].readings[s], gauges[1].readings[s], gauges[2].readings[s],
			gauges[3].readings[s], gauges[4].readings[s]), start.Add(time.Duration(s)*time.Second))
	}
	tally := e.Tally().EngineCounters
	var checked int
	for _, g := range gauges {
		r := g.readings
		peak, sum, first, last := max(r[0], r[1], r[2]), r[0]+r[1]+r[2], r[0], r[2]
		if peak == sum || peak == first || peak == last || sum == first || sum == last || first == last {
			t.Fatalf("%s readings %v cannot tell peak, sum, first and last apart: the fixture is blind", g.key, r)
		}
		got, ok := tally[g.key]
		switch {
		case !ok:
			t.Errorf("%s is absent from the tally after 3 engine samples", g.key)
			continue
		case got == sum:
			t.Errorf("%s = %d, the SUM of readings %v: a level held over several samples is not their total; want the peak, %d", g.key, got, r, peak)
			continue
		case got == last:
			t.Errorf("%s = %d, the LAST of readings %v: that clears only the final instant; want the peak, %d", g.key, got, r, peak)
			continue
		case got != peak:
			t.Errorf("%s = %d over readings %v; want the peak, %d", g.key, got, r, peak)
			continue
		}
		checked++
	}
	t.Logf("checked %d residual gauge(s) over 3 samples: each kept its peak, not its sum, first or last reading", checked)
	if checked != len(gauges) {
		t.Fatalf("checked %d of %d residual gauges", checked, len(gauges))
	}
}

// TestAResidualAnEarlierSwitchLeftStandingSurvivesALaterCleanSwitch is the
// reason for the peak rather than the last reading, in the shape celeris#687
// names as the placement bug: the first switch settles with Busy residue
// standing, and a later switch drains clean. Last would report the cell as
// clean; the peak keeps the first switch's verdict.
func TestAResidualAnEarlierSwitchLeftStandingSurvivesALaterCleanSwitch(t *testing.T) {
	e := NewEvaluator(nil)
	start := time.Unix(1_700_000_000, 0)
	// busy per sample: settled after switch 1 with 4 standing, still 4, then
	// switch 2 moves everything and it reads 0 to the end of the cell.
	for s, busy := range []int64{0, 4, 4, 0, 0} {
		e.Observe(residualSample(t, busy, 0, 0, 0, 0), start.Add(time.Duration(s)*time.Second))
	}
	if got := e.Tally().EngineCounters["engine_transplant_residual_busy"]; got != 4 {
		t.Fatalf("engine_transplant_residual_busy = %d after a switch left 4 standing and a later one drained clean; want 4 -- a tally that keeps 0 reports the placement bug as a clean cell", got)
	}
}
