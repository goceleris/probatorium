package main

import (
	"os"
	"regexp"
	"testing"
	"time"
)

// The race tier's adaptive cell keeps its sizing and paces its Markov walkers
// (probatorium#481, celeris#871). Promotion depends on the NUMBER of open
// connections (conns per worker), so the walker and worker counts stay; what
// drops is the rate each walker offers.
func TestRaceAdaptiveCellKeepsItsCountsAndPacesItsWalkers(t *testing.T) {
	// Literals, not the constants: a change to the sizing must be a decision
	// made here, in review, not a side effect of editing the constant.
	if got := cellRefappWorkers("adaptive", 0); got != 2 {
		t.Errorf("race adaptive workers = %d, want 2 (unchanged)", got)
	}
	if got := cellConcurrencyFloor("adaptive"); got != 110 {
		t.Errorf("race adaptive walker floor = %d, want 110 (unchanged)", got)
	}
	if got := cellMarkovMinPeriod("adaptive", true); got != 6500*time.Microsecond {
		t.Errorf("race adaptive Markov min period = %v, want 6.5ms", got)
	}
	if raceAdaptiveMarkovMinPeriod != 6500*time.Microsecond {
		t.Errorf("raceAdaptiveMarkovMinPeriod = %v, want 6.5ms", raceAdaptiveMarkovMinPeriod)
	}
}

// Every configuration but the race tier's adaptive cell is byte-for-byte what
// it was: no pause, the same worker cap, the same walker floor. A table of
// today's values as literals, over every engine, with and without -race-tier
// and with an explicit worker cap.
func TestEveryOtherCellConfigurationIsUnchanged(t *testing.T) {
	type row struct {
		engine   string
		raceTier bool
		capFlag  int
		workers  int
		floor    int
		period   time.Duration
	}
	rows := []row{
		// Not the race tier: nightly, weekend, checkptr, PR, bench, soak.
		{"adaptive", false, 0, 2, 110, 0},
		{"adaptive", false, 4, 4, 110, 0},
		{"iouring", false, 0, 0, 0, 0},
		{"iouring", false, 8, 8, 0, 0},
		{"epoll", false, 0, 0, 0, 0},
		{"epoll", false, 8, 8, 0, 0},
		{"std", false, 0, 0, 0, 0},
		{"", false, 0, 0, 0, 0},
		// The race tier's other engines: they pass today, so they are left alone.
		{"iouring", true, 0, 0, 0, 0},
		{"iouring", true, 8, 8, 0, 0},
		{"epoll", true, 0, 0, 0, 0},
		{"std", true, 0, 0, 0, 0},
		{"", true, 0, 0, 0, 0},
		// The race tier's adaptive cell: the counts stay, the pause is new.
		{"adaptive", true, 0, 2, 110, 6500 * time.Microsecond},
		{"adaptive", true, 4, 4, 110, 6500 * time.Microsecond},
	}
	for _, r := range rows {
		if got := cellRefappWorkers(r.engine, r.capFlag); got != r.workers {
			t.Errorf("engine=%q raceTier=%t cap=%d: workers %d, want %d", r.engine, r.raceTier, r.capFlag, got, r.workers)
		}
		if got := cellConcurrencyFloor(r.engine); got != r.floor {
			t.Errorf("engine=%q raceTier=%t: walker floor %d, want %d", r.engine, r.raceTier, got, r.floor)
		}
		if got := cellMarkovMinPeriod(r.engine, r.raceTier); got != r.period {
			t.Errorf("engine=%q raceTier=%t: Markov min period %v, want %v", r.engine, r.raceTier, got, r.period)
		}
	}
}

// matrix.go hands the pause to every matrix cell, and the single-cell path in
// main.go never does: shaping is a matrix-only, adaptive-only, race-tier-only
// decision, like cellConcurrencyFloor.
func TestMarkovMinPeriodReachesTheMatrixCellConfigOnly(t *testing.T) {
	m, err := os.ReadFile("matrix.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`MarkovMinPeriod:\s+cellMarkovMinPeriod\(mc\.Engine,\s*cfg\.RaceTier\)`).Match(m) {
		t.Error("matrix.go does not build the cell's validation.Config with MarkovMinPeriod: cellMarkovMinPeriod(mc.Engine, cfg.RaceTier)")
	}
	s, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`MarkovMinPeriod`).Match(s) {
		t.Error("main.go (the single-cell path) must not set MarkovMinPeriod")
	}
}

// The period is derived, not picked (PREREG.md, run 39560945814): the fleet's
// ceiling, 67 Markov walkers / period, must stay at least 15% above the
// promotion floor (8.35k requests a second: below it a healthy 64 KiB echo
// pushes a tick's bytes per request to the controller's 8192 refusal) and clearly
// below the 16.56k a second the same walkers sent closed-loop when the cell
// saturated. A change of the period that leaves this window is a change of the
// derivation, made here and in PREREG.md.
func TestRaceAdaptivePeriodSitsBetweenThePromotionFloorAndTheSaturatingRate(t *testing.T) {
	const (
		markovWalkers = 67 // 110 walkers less 22 adversarial, 11 h2c, 5 ws, 5 sse
		floor         = 8350.0
		saturating    = 16560.0
	)
	ceiling := markovWalkers / raceAdaptiveMarkovMinPeriod.Seconds()
	if ceiling < floor*1.15 {
		t.Errorf("fleet ceiling %.0f/s is under 15%% above the promotion floor %.0f/s", ceiling, floor)
	}
	if ceiling > saturating*0.7 {
		t.Errorf("fleet ceiling %.0f/s is not clearly below the %.0f/s that saturated the cell", ceiling, saturating)
	}
}
