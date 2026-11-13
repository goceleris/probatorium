package main

// The registry-carried rated plan (LF2F; THROWAWAY ab/lf2f-diag-20261113, not for merge).
//
// A rated sweep normally runs the same saturation-relative fractions for every cell (-rated-fractions, default 0.25/0.5/0.75/0.9, which the ansible playbook, mage and the
// workflow never override). LF2F needs, for ONE column that runs ONE scenario, fractions AND absolute offered rates (the same req/s on every arm), and the two scenarios of
// the experiment have different absolute rates, so one dispatch-wide list cannot hold them. The plan therefore lives in the registry column (servers.Adapter.RatedPlans, keyed
// by scenario) and is applied here, in run(), BEFORE any ratesCell use and before the manifest is written: cfg.RatedFractions / cfg.RatedAbsoluteRPS / cfg.RatedPlanFrom then
// say what the column really ran. No flag, ansible variable, mage input or workflow input is added; a column without a plan behaves exactly as before.
//
// An absolute pass is recorded with fraction = target / the cell's saturation rate (so the validators' target_rps = fraction x saturation_mode_rps invariant holds); a reader
// must key the passes by ORDER and by target_rps, never by the fraction value.

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

// ratedPass is one pass of the sweep: a fraction of the saturation rate, or (AbsoluteRPS > 0) an absolute offered rate.
type ratedPass struct {
	Fraction    float64
	AbsoluteRPS float64
}

// ratedPasses is the sweep in execution order: the fractions, then the absolute rates. With no absolute rates it is cfg.RatedFractions, as ever.
func (c Config) ratedPasses() []ratedPass {
	out := make([]ratedPass, 0, len(c.RatedFractions)+len(c.RatedAbsoluteRPS))
	for _, f := range c.RatedFractions {
		out = append(out, ratedPass{Fraction: f})
	}
	for _, a := range c.RatedAbsoluteRPS {
		out = append(out, ratedPass{AbsoluteRPS: a})
	}
	return out
}

// validateRatedPlan checks one (column, scenario) plan.
func validateRatedPlan(p servers.RatedPlan) error {
	n := len(p.Fractions) + len(p.AbsoluteRPS)
	if p.SaturationOnly {
		if n != 0 || p.Placeholder {
			return fmt.Errorf("a saturation-only plan has no rated pass (%d given) and is no placeholder", n)
		}
		return nil
	}
	if n == 0 {
		return fmt.Errorf("the plan has no pass")
	}
	if n > budget.DefaultRatedPasses {
		return fmt.Errorf("the plan has %d passes, more than the %d the hang guard budgets (budget.DefaultRatedPasses)", n, budget.DefaultRatedPasses)
	}
	for _, f := range p.Fractions {
		if !(f > 0 && f <= 1) || math.IsNaN(f) {
			return fmt.Errorf("fraction %v is not in (0,1]", f)
		}
	}
	for _, a := range p.AbsoluteRPS {
		if p.Placeholder && a == 0 {
			continue
		}
		if !(a > 0) || math.IsInf(a, 0) || math.IsNaN(a) {
			return fmt.Errorf("absolute rate %v is not a positive finite number", a)
		}
	}
	return nil
}

// resolveRatedPlan applies the registry plan of the column this runner process runs, when it has one. It returns an error (and the run does not start) when a planned
// column cannot be run exactly as planned:
//   - a planned column has a cell outside -rated-cells, or its rated cells span more than one scenario, or it shares the run with another column;
//   - the rated scenario has no plan entry;
//   - the plan is invalid, or its absolute rates are FILL placeholders (a real run only: -dry-run lists everything and says so loudly);
//   - a lf2f- column of the schedule has NO plan entry at all (RatedPlans nil): every lf2f column is either planned or EXPLICITLY saturation-only (PREREG-LF2F statement 10);
//   - a saturation-only column (RatedPlan.SaturationOnly: the guard columns g0 / g2 / g4) has a cell inside -rated-cells (it would run the default 0.25..0.9 sweep).
//     A saturation-only column leaves cfg untouched: its manifest carries the runner's default sweep list, no absolute rates and no plan source, and no cell of it is rated.
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
		} else if strings.HasPrefix(name, "lf2f-") && (cfg.Target != "" || !cfg.DryRun) {
			// a lf2f column with no plan entry (not even the explicit saturation-only one) would silently run the default sweep: refuse, in every mode but the local cross-product dry run
			return fmt.Errorf("rated plan: lf2f column %s has NO rated plan entry (every lf2f column is planned or explicitly saturation-only, servers.RatedPlan.SaturationOnly): refusing to run it with the default sweep", name)
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
	real := servers.Registry[slug]
	if satOnly, err := saturationOnlyColumn(slug, real, schedule, *cfg); err != nil {
		return err
	} else if satOnly {
		fmt.Fprintf(logw, "probatorium-runner: saturation-only column %s (registry plan entry SaturationOnly): zero rated passes, no cell of it is in -rated-cells; the sweep config is left as is and is never run\n", slug)
		return nil
	}
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
	if plan.Placeholder {
		if !cfg.DryRun {
			return fmt.Errorf("rated plan of %s / %s: the absolute rates are FILL PLACEHOLDERS (servers/p3a_arms.go was generated without --freeze): refusing to run", slug, scenario)
		}
		fmt.Fprintf(logw, "probatorium-runner: WARNING PLACEHOLDER: the absolute rates of %s / %s are FILL placeholders; a real run REFUSES to start\n", slug, scenario)
	}
	cfg.RatedFractions = append([]float64{}, plan.Fractions...)
	cfg.RatedAbsoluteRPS = append([]float64{}, plan.AbsoluteRPS...)
	cfg.RatedPlanFrom = "registry:" + slug + ":" + scenario
	fmt.Fprintf(logw, "probatorium-runner: rated plan from the registry column %s, scenario %s: fractions=%v absolute_rps=%v passes=%d (replaces -rated-fractions)\n",
		slug, scenario, cfg.RatedFractions, cfg.RatedAbsoluteRPS, len(cfg.ratedPasses()))
	return nil
}

// saturationOnlyColumn reports whether the planned column slug is an EXPLICIT saturation-only column (every plan entry has SaturationOnly: the LF2F guard columns). A mix of
// saturation-only and rated entries is an error; so is a saturation-only column with a scheduled cell of its own (or of another column of the run) inside -rated-cells, or
// with a scheduled scenario that has no plan entry.
func saturationOnlyColumn(slug string, real servers.Adapter, schedule []interleave.Cell, cfg Config) (bool, error) {
	n := 0
	for _, p := range real.RatedPlans {
		if p.SaturationOnly {
			n++
		}
	}
	if n == 0 {
		return false, nil
	}
	if n != len(real.RatedPlans) {
		return false, fmt.Errorf("rated plan: column %s mixes saturation-only and rated plan entries", slug)
	}
	for _, cell := range schedule {
		id := cell.Scenario.Name() + "/" + cell.Server.Name()
		if cfg.ratesCell(id) {
			return false, fmt.Errorf("rated plan: saturation-only column %s has the rated cell %s in the run (-rated-cells %q): it would run the default sweep (fix rated_cells: the guard glob belongs to cells only)", slug, id, cfg.RatedCells)
		}
		if cell.Server.Name() == slug {
			if _, ok := real.RatedPlans[cell.Scenario.Name()]; !ok {
				return false, fmt.Errorf("rated plan: saturation-only column %s runs scenario %s, which has no plan entry", slug, cell.Scenario.Name())
			}
		}
	}
	return true, nil
}
