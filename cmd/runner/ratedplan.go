package main

// The registry-carried rated plan (GOVAB; THROWAWAY ab/gov-ab-20261122, not for merge; the fractions-only form of the L755A mechanism).
//
// A rated sweep normally runs the same saturation-relative fractions for every cell (-rated-fractions, default 0.25/0.5/0.75/0.9, which the ansible playbook, mage and the
// workflow never override). The GOVAB A/B needs 0.25 + 0.9 for ws-echo and 0.5 + 0.9 for ws-large-echo in one dispatch, so one dispatch-wide list cannot hold them. The plan
// therefore lives in the registry column (servers.Adapter.RatedPlans, keyed by scenario) and is applied here, in run(), BEFORE any ratesCell use and before the manifest is
// written: cfg.RatedFractions / cfg.RatedPlanFrom then say what the column really ran. No flag, ansible variable, mage input or workflow input is added; a column without a
// plan behaves exactly as before.

import (
	"fmt"
	"io"
	"math"
	"sort"
	"strings"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/servers"
)

// govabColumnPrefixes are the registry prefixes of the GOVAB columns (servers/gov_ab_arms.go): every one is planned, never run with the default sweep.
var govabColumnPrefixes = []string{"gabw-", "gabl-"}

func isGovabColumn(name string) bool {
	for _, p := range govabColumnPrefixes {
		if strings.HasPrefix(name, p) {
			return true
		}
	}
	return false
}

// validateRatedPlan checks one (column, scenario) plan.
func validateRatedPlan(p servers.RatedPlan) error {
	if len(p.Fractions) == 0 {
		return fmt.Errorf("the plan has no pass")
	}
	if len(p.Fractions) > budget.DefaultRatedPasses {
		return fmt.Errorf("the plan has %d passes, more than the %d the hang guard budgets (budget.DefaultRatedPasses)", len(p.Fractions), budget.DefaultRatedPasses)
	}
	for _, f := range p.Fractions {
		if !(f > 0 && f <= 1) || math.IsNaN(f) {
			return fmt.Errorf("fraction %v is not in (0,1]", f)
		}
	}
	return nil
}

// resolveRatedPlan applies the registry plan of the column this runner process runs, when it has one. It returns an error (and the run does not start) when a planned column
// cannot be run exactly as planned:
//   - a planned column has a cell outside -rated-cells, or its rated cells span more than one scenario, or it shares the run with another column's rated cell, or the run holds
//     more than one planned column;
//   - the rated scenario has no plan entry, or the plan is invalid;
//   - a gabw- / gabl- column of the schedule has NO plan at all (it would silently run the default sweep).
//
// It does nothing when rated mode is off or no scheduled cell belongs to a planned column.
func resolveRatedPlan(cfg *Config, schedule []interleave.Cell, logw io.Writer) error {
	if !cfg.RatedMode {
		return nil
	}
	planned := map[string]bool{}
	for _, cell := range schedule {
		name := cell.Server.Name()
		r, ok := servers.Registry[name]
		if ok && r.RatedPlans != nil {
			planned[name] = true
		} else if isGovabColumn(name) && (cfg.Target != "" || !cfg.DryRun) {
			// a GOVAB column with no plan would silently run the default sweep: refuse, in every mode but the local cross-product dry run
			return fmt.Errorf("rated plan: GOVAB column %s has NO rated plan: refusing to run it with the default sweep", name)
		}
	}
	if len(planned) == 0 {
		return nil
	}
	if cfg.Target == "" && cfg.DryRun {
		fmt.Fprintf(logw, "probatorium-runner: local dry run: %d planned column(s) in the schedule; the registry rated plan is applied only in remote-target mode (-target + -server-name), one column per process\n", len(planned))
		return nil
	}
	names := make([]string, 0, len(planned))
	for n := range planned {
		names = append(names, n)
	}
	sort.Strings(names)
	if len(names) != 1 {
		return fmt.Errorf("rated plan: %d planned columns in one run (%s): a plan is applied per column process (-target + -server-name)", len(names), strings.Join(names, ", "))
	}
	slug := names[0]
	scs := map[string]bool{}
	for _, cell := range schedule {
		id := cell.Scenario.Name() + "/" + cell.Server.Name()
		if cell.Server.Name() != slug {
			if cfg.ratesCell(id) {
				return fmt.Errorf("rated plan: planned column %s shares the run with the rated cell %s", slug, id)
			}
			continue
		}
		if !cfg.ratesCell(id) {
			return fmt.Errorf("rated plan: cell %s of the planned column %s is not in -rated-cells %q: the plan would not run (fix rated_cells)", id, slug, cfg.RatedCells)
		}
		scs[cell.Scenario.Name()] = true
	}
	if len(scs) != 1 {
		var got []string
		for s := range scs {
			got = append(got, s)
		}
		sort.Strings(got)
		return fmt.Errorf("rated plan: planned column %s matches %d rated scenarios %v, want exactly one (a column runs one scenario)", slug, len(got), got)
	}
	var scenario string
	for s := range scs {
		scenario = s
	}
	real := servers.Registry[slug]
	plan, ok := real.RatedPlans[scenario]
	if !ok {
		var have []string
		for s := range real.RatedPlans {
			have = append(have, s)
		}
		sort.Strings(have)
		return fmt.Errorf("rated plan: planned column %s runs scenario %s, which has no plan entry (plans: %v)", slug, scenario, have)
	}
	if err := validateRatedPlan(plan); err != nil {
		return fmt.Errorf("rated plan of %s / %s: %w", slug, scenario, err)
	}
	cfg.RatedFractions = append([]float64{}, plan.Fractions...)
	cfg.RatedPlanFrom = "registry:" + slug + ":" + scenario
	fmt.Fprintf(logw, "probatorium-runner: rated plan from the registry column %s, scenario %s: fractions=%v passes=%d (replaces -rated-fractions)\n",
		slug, scenario, cfg.RatedFractions, len(cfg.RatedFractions))
	return nil
}
