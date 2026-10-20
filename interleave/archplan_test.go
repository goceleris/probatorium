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
