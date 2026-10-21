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
	if got := cellMarkovThinkTime("adaptive", true); got != 4*time.Millisecond {
		t.Errorf("race adaptive Markov think time = %v, want 4ms", got)
	}
	if raceAdaptiveMarkovThink != 4*time.Millisecond {
		t.Errorf("raceAdaptiveMarkovThink = %v, want 4ms", raceAdaptiveMarkovThink)
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
		think    time.Duration
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
		{"adaptive", true, 0, 2, 110, 4 * time.Millisecond},
		{"adaptive", true, 4, 4, 110, 4 * time.Millisecond},
	}
	for _, r := range rows {
		if got := cellRefappWorkers(r.engine, r.capFlag); got != r.workers {
			t.Errorf("engine=%q raceTier=%t cap=%d: workers %d, want %d", r.engine, r.raceTier, r.capFlag, got, r.workers)
		}
		if got := cellConcurrencyFloor(r.engine); got != r.floor {
			t.Errorf("engine=%q raceTier=%t: walker floor %d, want %d", r.engine, r.raceTier, got, r.floor)
		}
		if got := cellMarkovThinkTime(r.engine, r.raceTier); got != r.think {
			t.Errorf("engine=%q raceTier=%t: Markov think time %v, want %v", r.engine, r.raceTier, got, r.think)
		}
	}
}

// matrix.go hands the pause to every matrix cell, and the single-cell path in
// main.go never does: shaping is a matrix-only, adaptive-only, race-tier-only
// decision, like cellConcurrencyFloor.
func TestMarkovThinkTimeReachesTheMatrixCellConfigOnly(t *testing.T) {
	m, err := os.ReadFile("matrix.go")
	if err != nil {
		t.Fatal(err)
	}
	if !regexp.MustCompile(`MarkovThinkTime:\s+cellMarkovThinkTime\(mc\.Engine,\s*cfg\.RaceTier\)`).Match(m) {
		t.Error("matrix.go does not build the cell's validation.Config with MarkovThinkTime: cellMarkovThinkTime(mc.Engine, cfg.RaceTier)")
	}
	s, err := os.ReadFile("main.go")
	if err != nil {
		t.Fatal(err)
	}
	if regexp.MustCompile(`MarkovThinkTime`).Match(s) {
		t.Error("main.go (the single-cell path) must not set MarkovThinkTime")
	}
}
