package main

import (
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/scenarios"
)

// fullGridPlan returns the columns of the real '*/*' grid (the same cells a
// bench-full runs, capability-gated) with the durations of profile p: a cell
// drives load for warmup + duration + the per-cell overhead, and a rated cell
// adds the rated sweep. Columns are in registry order, cells in scenario
// order, as run_bench_cell.yml walks them.
func fullGridPlan(t *testing.T, p budget.Profile) []interleave.PlanColumn {
	t.Helper()
	cfg, err := ParseArgs(ratedArgs(p), io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]*interleave.PlanColumn{}
	var order []string
	for _, id := range realizedGrid(t) {
		scn, srv, _ := strings.Cut(id, "/")
		c := cols[srv]
		if c == nil {
			c = &interleave.PlanColumn{Name: srv}
			cols[srv] = c
			order = append(order, srv)
		}
		d := p.Warmup + p.Duration + budget.PerCellOverhead
		if cfg.ratesCell(id) {
			d += time.Duration(p.RatedPasses) * (p.Warmup + p.RatedDuration)
		}
		c.Cells = append(c.Cells, interleave.PlanCell{Scenario: scn, Heavy: scenarios.ClientLoadOf(scn) == scenarios.LoadHeavy, Dur: d})
	}
	out := make([]interleave.PlanColumn, 0, len(order))
	for _, s := range order {
		out = append(out, *cols[s])
	}
	return out
}

func toWindows(slots []interleave.Slot) []report.LoadWindow {
	t0 := time.Date(2026, 10, 20, 0, 0, 0, 0, time.UTC)
	out := make([]report.LoadWindow, 0, len(slots))
	for _, s := range slots {
		cl := string(scenarios.ClientLoadOf(s.Scenario))
		out = append(out, report.LoadWindow{Competitor: s.Column, Scenario: s.Scenario, Kind: report.LoadKindSaturation, Class: cl, Start: t0.Add(s.Start), End: t0.Add(s.End)})
	}
	return out
}

type planCase struct {
	name   string
	policy interleave.LockPolicy
	b      func(cols []interleave.PlanColumn) interleave.ArchPlan
}

func planCases() []planCase {
	fwd := func(c []interleave.PlanColumn) interleave.ArchPlan { return interleave.ArchPlan{Columns: c} }
	return []planCase{
		{"today: no lock, both forward", interleave.LockOff, fwd},
		{"heavy lock, both forward", interleave.LockHeavy, fwd},
		{"heavy lock, arm64 cells reversed", interleave.LockHeavy, func(c []interleave.PlanColumn) interleave.ArchPlan {
			return interleave.ArchPlan{Columns: c, ReverseCells: true}
		}},
		{"heavy lock, arm64 columns reversed", interleave.LockHeavy, func(c []interleave.PlanColumn) interleave.ArchPlan {
			return interleave.ArchPlan{Columns: c, ReverseColumns: true}
		}},
		{"heavy lock, arm64 cells and columns reversed", interleave.LockHeavy, func(c []interleave.PlanColumn) interleave.ArchPlan {
			return interleave.ArchPlan{Columns: c, ReverseCells: true, ReverseColumns: true}
		}},
		{"all lock, both forward (strict)", interleave.LockAll, fwd},
	}
}

func runPlanCase(cols []interleave.PlanColumn, pc planCase) (interleave.PlanResult, report.OverlapReport) {
	r := interleave.SimulateArchPlan(interleave.ArchPlan{Columns: cols}, pc.b(cols),
		interleave.PlanParams{Policy: pc.policy, Cooldown: 5 * time.Second})
	pol := report.ExclusionHeavy
	if pc.policy == interleave.LockAll {
		pol = report.ExclusionAll
	}
	return r, report.CheckArchOverlap(toWindows(r.Slots[0]), toWindows(r.Slots[1]), pol)
}

// TestFullGridArchPlan is probatorium#442 on the real bench-full grid. Today
// (no lock, both arches walking the same order) hundreds of heavy cells of one
// arch run against the heavy cell of the other. With the lock none does, in
// any order; the order decides what the lock costs in wall-clock. The numbers
// it logs are the runtime estimate of the PR (go test -run FullGridArchPlan -v).
func TestFullGridArchPlan(t *testing.T) {
	p := budget.Full()
	cols := fullGridPlan(t, p)
	cells := 0
	for _, c := range cols {
		cells += len(c.Cells)
	}
	if cells != budget.FullRealizedCells {
		t.Fatalf("plan has %d cells, budget pins %d", cells, budget.FullRealizedCells)
	}
	t.Logf("%d columns, %d cells per arch", len(cols), cells)
	var single time.Duration
	for _, c := range cols {
		for k, cell := range c.Cells {
			single += cell.Dur
			if k+1 < len(c.Cells) {
				single += 5 * time.Second
			}
		}
	}
	t.Logf("one arch alone: %.1f h", single.Hours())
	res := map[string]interleave.PlanResult{}
	for _, pc := range planCases() {
		r, rep := runPlanCase(cols, pc)
		res[pc.name] = r
		hh, hl, ll := r.Overlaps()
		t.Logf("%-46s makespan %6.1f h (%+5.1f%% vs one arch)  lock wait x86 %5.1f h arm64 %5.1f h  overlaps heavy+heavy %3d heavy+light %4d light+light %4d  forbidden %d",
			pc.name, r.Makespan.Hours(), 100*(r.Makespan.Hours()/single.Hours()-1), r.LockWait[0].Hours(), r.LockWait[1].Hours(), hh, hl, ll, rep.Total)
		if pc.policy == interleave.LockOff {
			if hh == 0 {
				t.Errorf("%s: no heavy+heavy overlap, but today's schedule overlaps heavily (probatorium#442)", pc.name)
			}
			continue
		}
		if rep.Total != 0 || hh != 0 && pc.policy != interleave.LockOff {
			t.Errorf("%s: %d forbidden overlaps, heavy+heavy %d, want 0", pc.name, rep.Total, hh)
		}
	}
}

// TestArchLockOverheadMatchesPlanner pins the budget model's lock-overhead
// constants to the planner on the real grid: the constants must not be lower
// than what the planner measures (so FitWithin never under-counts), and not
// absurdly higher either.
func TestArchLockOverheadMatchesPlanner(t *testing.T) {
	for _, p := range []budget.Profile{budget.Full(), budget.HeadlineWeekly()} {
		cols := fullGridPlan(t, p)
		one := interleave.SimulateArchPlan(interleave.ArchPlan{Columns: cols}, interleave.ArchPlan{},
			interleave.PlanParams{Policy: interleave.LockOff, Cooldown: 5 * time.Second}).Makespan
		for _, c := range []struct {
			policy interleave.LockPolicy
			b      interleave.ArchPlan
			want   float64
		}{
			{interleave.LockHeavy, interleave.ArchPlan{Columns: cols, ReverseCells: true}, budget.ArchLockOverheadHeavy},
			{interleave.LockAll, interleave.ArchPlan{Columns: cols}, budget.ArchLockOverheadAll},
		} {
			r := interleave.SimulateArchPlan(interleave.ArchPlan{Columns: cols}, c.b,
				interleave.PlanParams{Policy: c.policy, Cooldown: 5 * time.Second})
			got := float64(r.Makespan)/float64(one) - 1
			if got > c.want || got < c.want-0.08 {
				t.Errorf("%s policy %s: planner overhead %.3f, budget constant %.3f (want constant in [planner, planner+0.08])",
					p.Name, c.policy, got, c.want)
			}
		}
	}
}

// TestArchPlanDriftSweep: the model above starts both arches together with
// identical column set-up. Real runs drift (36441790519 started the same cell
// +61 s apart; 38431303162 a median -247 s). Sweep the second arch's start
// offset and both arches' column set-up time, for the default schedule (arm64
// cells reversed) and for no reversal, and log the range. At every point no
// heavy cell may overlap a heavy cell of the other arch. The reversal pays when
// the arches stay within minutes of each other, as both full runs did; with an
// offset of hours the arches sit in different columns and it is no better.
func TestArchPlanDriftSweep(t *testing.T) {
	cols := fullGridPlan(t, budget.Full())
	params := interleave.PlanParams{Policy: interleave.LockHeavy, Cooldown: 5 * time.Second}
	setups := [][2]time.Duration{{0, 0}, {60 * time.Second, 60 * time.Second}, {30 * time.Second, 120 * time.Second}, {120 * time.Second, 30 * time.Second}}
	type band struct {
		lo, hi, sum float64
		n           int
	}
	bands := map[string]*band{}
	add := func(k string, v float64) {
		b := bands[k]
		if b == nil {
			b = &band{lo: 1e9}
			bands[k] = b
		}
		b.lo, b.hi, b.sum, b.n = min(b.lo, v), max(b.hi, v), b.sum+v, b.n+1
	}
	for _, off := range []time.Duration{0, time.Minute, 10 * time.Minute, time.Hour, 3 * time.Hour} {
		for _, su := range setups {
			a := interleave.ArchPlan{Columns: cols, ColumnSetup: su[0]}
			one := interleave.SimulateArchPlan(a, interleave.ArchPlan{}, interleave.PlanParams{Policy: interleave.LockOff, Cooldown: params.Cooldown}).Makespan
			var ratio [2]float64
			for i, reversed := range []bool{false, true} {
				b := interleave.ArchPlan{Columns: cols, ColumnSetup: su[1], StartOffset: off, ReverseCells: reversed}
				r := interleave.SimulateArchPlan(a, b, params)
				if hh, _, _ := r.Overlaps(); hh != 0 {
					t.Fatalf("offset %v setup %v reversed=%v: %d heavy+heavy overlaps", off, su, reversed, hh)
				}
				ratio[i] = float64(r.Makespan) / float64(one)
				kind := "offsets up to 10 min"
				if off > 10*time.Minute {
					kind = "offsets of 1-3 h"
				}
				add(fmt.Sprintf("reversed=%v, %s", reversed, kind), ratio[i])
			}
			if off <= 10*time.Minute && ratio[1] > ratio[0]+0.03 {
				t.Errorf("offset %v setup %v: reversed %.3f is worse than forward %.3f where the arches are within minutes", off, su, ratio[1], ratio[0])
			}
		}
	}
	for k, b := range bands {
		t.Logf("%-40s makespan / one arch alone: min %.3f mean %.3f max %.3f over %d drift points", k, b.lo, b.sum/float64(b.n), b.hi, b.n)
		if b.hi > 1.30 {
			t.Errorf("%s: worst case %.3f exceeds the +30%% the workflow budgets cover", k, b.hi)
		}
	}
}
