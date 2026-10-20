package interleave

import "time"

// Arch plan (probatorium#442): a deterministic model of the two arch passes of
// a BENCH_TARGET=both run, which drive the x86 SUT and the arm64 SUT from one
// loadgen host at the same time. Each arch walks its columns (one SUT process
// per column) and, inside a column, its cells one after the other. The model
// replays that walk with the cross-arch client lock the runner takes around
// heavy cells (cmd/runner/clientlock.go) and reports when every cell ran, so a
// test can count overlaps and a profile can state the wall-clock of the fix.

// LockPolicy says which cells take the cross-arch client lock.
type LockPolicy string

const (
	// LockOff takes no lock: both arches run freely (every both-arch run
	// before the fix).
	LockOff LockPolicy = "off"
	// LockHeavy locks heavy cells only; light cells run under anything.
	LockHeavy LockPolicy = "heavy"
	// LockAll locks every cell: the arches never run at the same time.
	LockAll LockPolicy = "all"
	// LockHeavyRated is not a runner policy; it prices an option the
	// maintainer decision names: heavy cells hold the lock whole, and every
	// other cell holds it for its rated sweep only (its saturation pass runs
	// free), so no rated pass of either class overlaps a peer's heavy cell or
	// a peer's rated sweep.
	LockHeavyRated LockPolicy = "heavy+rated"
)

// PlanCell is one cell of a column: the scenario, whether it is heavy
// (scenarios.ClientLoadOf) and how long it drives load, saturation pass and
// rated sweep together, the cooldown excluded.
type PlanCell struct {
	Scenario string
	Heavy    bool
	Dur      time.Duration
	// Rated is the part of Dur that is the rated sweep, the tail of the cell
	// (0 = no rated sweep).
	Rated time.Duration
}

// PlanColumn is one column (one SUT) with its cells in registry order.
type PlanColumn struct {
	Name  string
	Cells []PlanCell
}

// ArchPlan is one arch's walk. ReverseCells makes it run each column's cells
// last to first; ReverseColumns walks the columns last to first.
type ArchPlan struct {
	Columns        []PlanColumn
	ReverseCells   bool
	ReverseColumns bool
	// StartOffset is when this arch starts, after time zero (the two passes of
	// a real run never start together to the second).
	StartOffset time.Duration
	// ColumnSetup overrides PlanParams.ColumnSetup for this arch when non-zero
	// (the two archs bring a column up in different times, which is how a run
	// drifts out of lockstep).
	ColumnSetup time.Duration
}

// PlanParams are the constants of the walk.
type PlanParams struct {
	Policy LockPolicy
	// Cooldown is the idle gap after every cell but a column's last.
	Cooldown time.Duration
	// ColumnSetup is the time to bring a column's SUT up before its first cell.
	ColumnSetup time.Duration
}

// Slot is one cell as run: its load interval [Start, End) and how long it
// waited for the lock first.
type Slot struct {
	Column, Scenario string
	Heavy            bool
	Start, End       time.Duration
	Waited           time.Duration
	// RatedStart is when the rated sweep began (zero when the cell has none);
	// the sweep runs [RatedStart, End).
	RatedStart time.Duration
}

// PlanResult is the outcome of SimulateArchPlan.
type PlanResult struct {
	Slots    [2][]Slot
	ArchEnd  [2]time.Duration
	LockWait [2]time.Duration
	Makespan time.Duration
}

// SimulateArchPlan replays arch 0 (a) and arch 1 (b) from time zero under p.
//
// It advances whichever arch is ready first, so the arch that asks for the
// lock first gets it, as with the runner's flock. The lock is released when the
// cell's load ends, before the cooldown, and the waiting arch takes it at that
// instant (the runner polls once a second, so real waits are up to 1 s longer).
func SimulateArchPlan(a, b ArchPlan, p PlanParams) PlanResult {
	type walker struct {
		cells []walkCell
		next  int
		ready time.Duration // when the arch can start its next cell
	}
	var w [2]walker
	var setup [2]time.Duration
	for i, ap := range []ArchPlan{a, b} {
		w[i].ready = ap.StartOffset
		setup[i] = p.ColumnSetup
		if ap.ColumnSetup > 0 {
			setup[i] = ap.ColumnSetup
		}
		for _, c := range orderedColumns(ap) {
			cells := orderedCells(c, ap.ReverseCells)
			for k, cell := range cells {
				wc := walkCell{col: c.Name, cell: cell, first: k == 0, last: k+1 == len(cells)}
				w[i].cells = append(w[i].cells, wc)
			}
		}
	}
	var r PlanResult
	var lockFree time.Duration
	for {
		// pick the ready-first arch that still has cells; ties go to arch 0.
		pick := -1
		for i := range w {
			if w[i].next >= len(w[i].cells) {
				continue
			}
			if pick < 0 || w[i].ready < w[pick].ready {
				pick = i
			}
		}
		if pick < 0 {
			break
		}
		x := &w[pick]
		wc := x.cells[x.next]
		x.next++
		t := x.ready
		if wc.first {
			t += setup[pick]
		}
		start := t
		needsLock := p.Policy == LockAll || ((p.Policy == LockHeavy || p.Policy == LockHeavyRated) && wc.cell.Heavy)
		if needsLock && lockFree > start {
			start = lockFree
		}
		end := start + wc.cell.Dur
		var ratedStart time.Duration
		if wc.cell.Rated > 0 {
			ratedStart = end - wc.cell.Rated
		}
		waited := start - t
		if needsLock {
			lockFree = end
		} else if p.Policy == LockHeavyRated && wc.cell.Rated > 0 {
			// a light cell: the saturation pass runs free, the rated sweep
			// waits for the lock.
			if lockFree > ratedStart {
				waited += lockFree - ratedStart
				ratedStart = lockFree
				end = ratedStart + wc.cell.Rated
			}
			lockFree = end
		}
		r.Slots[pick] = append(r.Slots[pick], Slot{
			Column: wc.col, Scenario: wc.cell.Scenario, Heavy: wc.cell.Heavy,
			Start: start, End: end, Waited: waited, RatedStart: ratedStart,
		})
		r.LockWait[pick] += waited
		x.ready = end
		if !wc.last {
			x.ready += p.Cooldown
		}
		r.ArchEnd[pick] = end
		r.Makespan = max(r.Makespan, end)
	}
	return r
}

type walkCell struct {
	col         string
	cell        PlanCell
	first, last bool
}

func orderedColumns(ap ArchPlan) []PlanColumn {
	out := append([]PlanColumn(nil), ap.Columns...)
	if ap.ReverseColumns {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

func orderedCells(c PlanColumn, reverse bool) []PlanCell {
	out := append([]PlanCell(nil), c.Cells...)
	if reverse {
		for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 {
			out[i], out[j] = out[j], out[i]
		}
	}
	return out
}

// Overlaps counts the (arch 0 slot, arch 1 slot) pairs that share time, split
// by class: both heavy, exactly one heavy, both light.
func (r PlanResult) Overlaps() (heavyHeavy, heavyLight, lightLight int) {
	for _, x := range r.Slots[0] {
		for _, y := range r.Slots[1] {
			if x.Start >= y.End || y.Start >= x.End {
				continue
			}
			switch {
			case x.Heavy && y.Heavy:
				heavyHeavy++
			case x.Heavy || y.Heavy:
				heavyLight++
			default:
				lightLight++
			}
		}
	}
	return
}

// RatedUnderHeavy counts the rated sweeps of light cells (the cells the heavy
// policy lets run unlocked) that ran while a heavy cell of the other arch was
// running: the cells, the (sweep, heavy cell) pairs, and the total overlapped
// time. A light row's rated passes are ranked by latency at load, so this is
// what the default policy leaves exposed to the peer's heavy load.
func (r PlanResult) RatedUnderHeavy() (cells, pairs int, overlapped time.Duration) {
	for side := 0; side < 2; side++ {
		for _, x := range r.Slots[side] {
			if x.Heavy || x.RatedStart == 0 {
				continue
			}
			hit := false
			for _, y := range r.Slots[1-side] {
				if !y.Heavy || y.Start >= x.End || x.RatedStart >= y.End {
					continue
				}
				lo, hi := max(x.RatedStart, y.Start), min(x.End, y.End)
				if hi <= lo {
					continue
				}
				hit = true
				pairs++
				overlapped += hi - lo
			}
			if hit {
				cells++
			}
		}
	}
	return cells, pairs, overlapped
}

// RatedPassSharing classifies the rated passes of the light cells that kept
// RatedStart: passes of passDur each, back to back from RatedStart, n per cell.
// A pass is "alone" when no slot of the other arch touches it, "heavy" when a
// heavy cell of the other arch does, "light" when only light cells do.
func (r PlanResult) RatedPassSharing(n int, passDur time.Duration) (alone, heavy, light int) {
	for side := 0; side < 2; side++ {
		for _, x := range r.Slots[side] {
			if x.Heavy || x.RatedStart == 0 {
				continue
			}
			for k := 0; k < n; k++ {
				ps := x.RatedStart + time.Duration(k)*passDur
				pe := ps + passDur
				var sawHeavy, sawLight bool
				for _, y := range r.Slots[1-side] {
					if y.Start >= pe || ps >= y.End {
						continue
					}
					if y.Heavy {
						sawHeavy = true
					} else {
						sawLight = true
					}
				}
				switch {
				case sawHeavy:
					heavy++
				case sawLight:
					light++
				default:
					alone++
				}
			}
		}
	}
	return alone, heavy, light
}
