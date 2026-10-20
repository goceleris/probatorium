package main

import (
	"fmt"
	"io"
	"slices"
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
		var rated time.Duration
		if cfg.ratesCell(id) {
			rated = time.Duration(p.RatedPasses) * (p.Warmup + p.RatedDuration)
			d += rated
		}
		c.Cells = append(c.Cells, interleave.PlanCell{Scenario: scn, Heavy: scenarios.ClientLoadOf(scn) == scenarios.LoadHeavy, Dur: d, Rated: rated})
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
		{"all lock, arm64 cells reversed (strict)", interleave.LockAll, func(c []interleave.PlanColumn) interleave.ArchPlan {
			return interleave.ArchPlan{Columns: c, ReverseCells: true}
		}},
		// priced option, not a runner policy: heavy cells whole, every other
		// cell's rated sweep under the lock too.
		{"heavy+rated lock, arm64 cells reversed (option)", interleave.LockHeavyRated, func(c []interleave.PlanColumn) interleave.ArchPlan {
			return interleave.ArchPlan{Columns: c, ReverseCells: true}
		}},
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
		rc, rp, ro := r.RatedUnderHeavy()
		t.Logf("%-46s makespan %6.1f h (%+5.1f%% vs one arch)  lock wait x86 %5.1f h arm64 %5.1f h  overlaps heavy+heavy %3d heavy+light %4d light+light %4d  forbidden %d  | light rated sweeps under a peer heavy cell: %d cells, %d pairs, %.1f h",
			pc.name, r.Makespan.Hours(), 100*(r.Makespan.Hours()/single.Hours()-1), r.LockWait[0].Hours(), r.LockWait[1].Hours(), hh, hl, ll, rep.Total, rc, rp, ro.Hours())
		switch pc.policy {
		case interleave.LockHeavy:
			// the exposure the maintainer decision names: the default does not
			// protect the rated sweeps of the light rows.
			if rc == 0 && pc.name != "heavy lock, both forward" {
				t.Errorf("%s: no light rated sweep under a peer heavy cell; the decision text says there are many", pc.name)
			}
		case interleave.LockAll, interleave.LockHeavyRated:
			if rc != 0 {
				t.Errorf("%s: %d light rated sweeps under a peer heavy cell, want 0", pc.name, rc)
			}
		}
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

// validationScope is the dispatch of docs/ARCH-EXCLUSION-VALIDATION.md.
var (
	validationColumns = []string{"axum", "celeris-adaptive-auto+upg-async", "celeris-iouring-auto+upg-async"}
	validationRows    = []string{"churn-close", "get-simple-256c", "post-4k", "ws-hub-broadcast-128", "ws-large-echo"}
)

// validationPlan returns the three columns of the validation dispatch at the
// `full` window with the rated sweep on ratedRows, in the order the runner
// walks them.
func validationPlan(t *testing.T, ratedRows ...string) []interleave.PlanColumn {
	t.Helper()
	p := budget.Full()
	var glob []string
	for _, r := range ratedRows {
		glob = append(glob, r+"/*")
	}
	cfg, err := ParseArgs([]string{"-rated", "-rated-duration", p.RatedDuration.String(), "-rated-cells", strings.Join(glob, ",")}, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	cols := map[string]*interleave.PlanColumn{}
	for _, id := range realizedGrid(t) {
		scn, srv, _ := strings.Cut(id, "/")
		if !slices.Contains(validationColumns, srv) || !slices.Contains(validationRows, scn) {
			continue
		}
		c := cols[srv]
		if c == nil {
			c = &interleave.PlanColumn{Name: srv}
			cols[srv] = c
		}
		d := p.Warmup + p.Duration + budget.PerCellOverhead
		var rated time.Duration
		if cfg.ratesCell(id) {
			rated = time.Duration(p.RatedPasses) * (p.Warmup + p.RatedDuration)
			d += rated
		}
		c.Cells = append(c.Cells, interleave.PlanCell{Scenario: scn, Heavy: scenarios.ClientLoadOf(scn) == scenarios.LoadHeavy, Dur: d, Rated: rated})
	}
	var out []interleave.PlanColumn
	for _, n := range validationColumns {
		if c := cols[n]; c != nil {
			out = append(out, *c)
		}
	}
	return out
}

// TestValidationScopeGroupSizes predicts, for the validation dispatch, how many
// rated passes of its light rows land alone, under a peer's light cell or under
// a peer's heavy cell. The pre-registered light-row criterion
// (docs/ARCH-EXCLUSION-VALIDATION.md) compares those groups, so they must be
// large enough: with the arm64 pass walking each column in reverse, the light
// cells of one arch fall under the heavy cells of the other, and the `alone`
// group is the one at risk of being empty. The log is the table in the doc.
func TestValidationScopeGroupSizes(t *testing.T) {
	cols := validationPlan(t, "ws-large-echo", "post-4k", "ws-hub-broadcast-128", "churn-close")
	if len(cols) != 3 {
		t.Fatalf("the validation scope found %d of its 3 columns", len(cols))
	}
	var order []string
	for _, c := range cols[0].Cells {
		order = append(order, c.Scenario)
	}
	t.Logf("cells per column in walk order: %v", order)
	const passes, passDur = 4, 50 * time.Second
	one := interleave.SimulateArchPlan(interleave.ArchPlan{Columns: cols}, interleave.ArchPlan{}, interleave.PlanParams{Policy: interleave.LockOff, Cooldown: 5 * time.Second})
	off := interleave.SimulateArchPlan(interleave.ArchPlan{Columns: cols}, interleave.ArchPlan{Columns: cols, ReverseCells: true}, interleave.PlanParams{Policy: interleave.LockOff, Cooldown: 5 * time.Second})
	t.Logf("arm C (target=amd64, one arch alone): %.2f h; arm D (arch_exclusive=off): %.2f h", one.Makespan.Hours(), off.Makespan.Hours())
	for _, pc := range []struct {
		name   string
		policy interleave.LockPolicy
		b      func([]interleave.PlanColumn) interleave.ArchPlan
	}{
		{"heavy lock, arm64 cells reversed (default)", interleave.LockHeavy, func(c []interleave.PlanColumn) interleave.ArchPlan {
			return interleave.ArchPlan{Columns: c, ReverseCells: true}
		}},
		{"heavy lock, both forward", interleave.LockHeavy, func(c []interleave.PlanColumn) interleave.ArchPlan { return interleave.ArchPlan{Columns: c} }},
		{"all lock (arch_exclusive=all)", interleave.LockAll, func(c []interleave.PlanColumn) interleave.ArchPlan { return interleave.ArchPlan{Columns: c} }},
	} {
		r := interleave.SimulateArchPlan(interleave.ArchPlan{Columns: cols}, pc.b(cols),
			interleave.PlanParams{Policy: pc.policy, Cooldown: 5 * time.Second})
		alone, heavy, light := r.RatedPassSharing(passes, passDur)
		hh, hl, ll := r.Overlaps()
		t.Logf("%-44s makespan %5.2f h; light-row rated passes alone %2d, under a peer light cell %2d, under a peer heavy cell %2d; overlaps heavy+heavy %d heavy+light %d light+light %d",
			pc.name, r.Makespan.Hours(), alone, light, heavy, hh, hl, ll)
		if hh != 0 {
			t.Errorf("%s: %d heavy+heavy overlaps", pc.name, hh)
		}
		// each heavy row: how many of its cells, per arch, have at least a quarter
		// of their saturation window under a peer light cell (P6's "shared"). P6
		// needs both arches covered, and the probe row that covers one arch may
		// not cover the other, so the log says which.
		for _, row := range []string{"get-simple-256c", "ws-large-echo", "post-4k"} {
			var shared, total [2]int
			for side := 0; side < 2; side++ {
				for _, x := range r.Slots[side] {
					if x.Scenario != row {
						continue
					}
					total[side]++
					satEnd := x.End
					if x.RatedStart != 0 {
						satEnd = x.RatedStart
					}
					var ov time.Duration
					for _, y := range r.Slots[1-side] {
						if y.Heavy {
							continue
						}
						lo, hi := max(x.Start, y.Start), min(satEnd, y.End)
						if hi > lo {
							ov += hi - lo
						}
					}
					if ov*4 >= satEnd-x.Start {
						shared[side]++
					}
				}
			}
			t.Logf("%-44s %-16s cells with >= 1/4 of the saturation window under a peer light cell: x86 %d of %d, arm64 %d of %d", pc.name, row, shared[0], total[0], shared[1], total[1])
		}
		if pc.policy == interleave.LockAll && (heavy != 0 || light != 0 || alone != 48) {
			t.Errorf("%s: alone %d heavy %d light %d, want every one of the 48 light-row passes alone", pc.name, alone, heavy, light)
		}
	}
}
