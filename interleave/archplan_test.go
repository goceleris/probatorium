package interleave

import (
	"testing"
	"time"
)

// twoColumns is a toy grid: every column is two light cells then two heavy
// cells, each 100 s, so the arithmetic below is checkable by hand.
func twoColumns() []PlanColumn {
	cell := func(n string, heavy bool) PlanCell {
		return PlanCell{Scenario: n, Heavy: heavy, Dur: 100 * time.Second}
	}
	col := func(n string) PlanColumn {
		return PlanColumn{Name: n, Cells: []PlanCell{cell("l1", false), cell("l2", false), cell("h1", true), cell("h2", true)}}
	}
	return []PlanColumn{col("a"), col("b")}
}

var toyParams = PlanParams{Cooldown: 5 * time.Second}

// TestArchPlanLockstepOverlapsToday is the state of every both-arch run before
// the fix: both arches walk the same order from the same start, so every heavy
// cell of one arch runs against the same heavy cell of the other.
func TestArchPlanLockstepOverlapsToday(t *testing.T) {
	p := toyParams
	p.Policy = LockOff
	r := SimulateArchPlan(ArchPlan{Columns: twoColumns()}, ArchPlan{Columns: twoColumns()}, p)
	hh, _, _ := r.Overlaps()
	if hh != 4 { // 2 columns x 2 heavy cells, each against its twin
		t.Fatalf("heavy+heavy overlaps with no lock = %d, want 4", hh)
	}
}

// TestArchPlanHeavyLockSerialisesHeavyCells: with the lock no heavy cell of one
// arch shares time with a heavy cell of the other, in the worst order (lockstep).
func TestArchPlanHeavyLockSerialisesHeavyCells(t *testing.T) {
	p := toyParams
	p.Policy = LockHeavy
	r := SimulateArchPlan(ArchPlan{Columns: twoColumns()}, ArchPlan{Columns: twoColumns()}, p)
	if hh, _, _ := r.Overlaps(); hh != 0 {
		t.Fatalf("heavy+heavy overlaps under LockHeavy = %d, want 0", hh)
	}
	if r.LockWait[0]+r.LockWait[1] == 0 {
		t.Fatal("lockstep arches must have waited for the lock")
	}
}

// TestArchPlanReversedArchFitsAroundTheLock: when the second arch walks each
// column backwards the heavy phases of the two arches fall in different halves
// of the column, nobody waits, and the run takes as long as one arch alone.
func TestArchPlanReversedArchFitsAroundTheLock(t *testing.T) {
	p := toyParams
	p.Policy = LockHeavy
	alone := SimulateArchPlan(ArchPlan{Columns: twoColumns()}, ArchPlan{}, p).Makespan
	r := SimulateArchPlan(ArchPlan{Columns: twoColumns()}, ArchPlan{Columns: twoColumns(), ReverseCells: true}, p)
	if hh, _, _ := r.Overlaps(); hh != 0 {
		t.Fatalf("heavy+heavy overlaps = %d, want 0", hh)
	}
	if w := r.LockWait[0] + r.LockWait[1]; w != 0 {
		t.Fatalf("anti-phase arches waited %v for the lock, want 0", w)
	}
	if r.Makespan != alone {
		t.Fatalf("makespan %v, want one arch's %v", r.Makespan, alone)
	}
}

// TestArchPlanAllLockNeverOverlaps: LockAll is the strict option.
func TestArchPlanAllLockNeverOverlaps(t *testing.T) {
	p := toyParams
	p.Policy = LockAll
	r := SimulateArchPlan(ArchPlan{Columns: twoColumns()}, ArchPlan{Columns: twoColumns(), ReverseCells: true}, p)
	hh, hl, ll := r.Overlaps()
	if hh+hl+ll != 0 {
		t.Fatalf("overlaps under LockAll = %d+%d+%d, want 0", hh, hl, ll)
	}
	// 16 cells of 100 s with one lock: nothing can finish sooner than the sum,
	// and the other arch's cells fill each cooldown, so it is no more than that.
	if want := 1600 * time.Second; r.Makespan != want {
		t.Fatalf("LockAll makespan %v, want the sum of every cell, %v", r.Makespan, want)
	}
}

// ratedPair is arch 0's light cell with a 40 s rated sweep at its end, and
// arch 1's heavy cell starting at offset.
func ratedPair(offset time.Duration) (ArchPlan, ArchPlan) {
	a := ArchPlan{Columns: []PlanColumn{{Name: "c", Cells: []PlanCell{{Scenario: "l", Dur: 100 * time.Second, Rated: 40 * time.Second}}}}}
	b := ArchPlan{StartOffset: offset, Columns: []PlanColumn{{Name: "c", Cells: []PlanCell{{Scenario: "h", Heavy: true, Dur: 100 * time.Second}}}}}
	return a, b
}

// TestArchPlanLightRatedSweepIsExposedToAPeerHeavyCell: the heavy policy locks
// heavy cells, so a light cell's rated sweep (the tail of the cell) runs while
// the other arch's heavy cell does. RatedUnderHeavy counts that, and
// RatedPassSharing splits its passes into alone and shared.
func TestArchPlanLightRatedSweepIsExposedToAPeerHeavyCell(t *testing.T) {
	p := toyParams
	p.Policy = LockHeavy
	a, b := ratedPair(70 * time.Second) // the sweep is [60 s, 100 s); the heavy cell [70 s, 170 s)
	r := SimulateArchPlan(a, b, p)
	cells, pairs, ov := r.RatedUnderHeavy()
	if cells != 1 || pairs != 1 || ov != 30*time.Second {
		t.Fatalf("RatedUnderHeavy = %d cells %d pairs %v, want 1, 1, 30s", cells, pairs, ov)
	}
	alone, heavy, light := r.RatedPassSharing(4, 10*time.Second) // passes [60,70) [70,80) [80,90) [90,100)
	if alone != 1 || heavy != 3 || light != 0 {
		t.Fatalf("RatedPassSharing = alone %d heavy %d light %d, want 1, 3, 0", alone, heavy, light)
	}
	a, b = ratedPair(100 * time.Second) // the heavy cell starts as the sweep ends
	if cells, _, _ := SimulateArchPlan(a, b, p).RatedUnderHeavy(); cells != 0 {
		t.Fatalf("a heavy cell that starts when the sweep ends overlaps it: %d", cells)
	}
}

// TestArchPlanHeavyRatedLockProtectsLightRatedSweeps prices the option that
// locks every rated sweep: the heavy cell now waits for the sweep.
func TestArchPlanHeavyRatedLockProtectsLightRatedSweeps(t *testing.T) {
	p := toyParams
	p.Policy = LockHeavyRated
	a, b := ratedPair(70 * time.Second)
	r := SimulateArchPlan(a, b, p)
	if cells, _, _ := r.RatedUnderHeavy(); cells != 0 {
		t.Fatalf("under heavy+rated the sweep still shares time with a heavy cell: %d", cells)
	}
	h := r.Slots[1][0]
	if h.Start != 100*time.Second || h.Waited != 30*time.Second {
		t.Fatalf("heavy cell started at %v after waiting %v, want 100s after 30s", h.Start, h.Waited)
	}
	// and a light cell whose rated sweep must wait for a heavy cell's lock
	a2 := ArchPlan{Columns: []PlanColumn{{Name: "c", Cells: []PlanCell{{Scenario: "h", Heavy: true, Dur: 100 * time.Second}}}}}
	b2 := ArchPlan{Columns: []PlanColumn{{Name: "c", Cells: []PlanCell{{Scenario: "l", Dur: 100 * time.Second, Rated: 40 * time.Second}}}}}
	r = SimulateArchPlan(a2, b2, p)
	l := r.Slots[1][0]
	if l.RatedStart != 100*time.Second || l.End != 140*time.Second {
		t.Fatalf("light cell: rated sweep [%v, %v), want [100s, 140s) after the heavy cell", l.RatedStart, l.End)
	}
}
