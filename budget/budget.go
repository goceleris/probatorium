// Package budget models the wall-clock cost of a cluster bench so CI can
// assert the chosen weekly config fits its window before spending it.
//
// The benchmark tier (probatorium#172) runs a curated matrix on the
// 3-node cluster within a hard 24h budget (#166). This package is the
// single source of truth for that cost model: it lives outside the `mage`
// build tag so plain `go test ./...` reaches the budget invariant on
// every PR (mirroring how the SLO gate moved into report/ for the same
// reason), and the mage tier orchestrator imports it to assert the run
// fits before spending a 24h cluster window.
//
// Two named profiles are curated (#166):
//
//   - headline: the weekly cadence — the FULL grid (every server x every
//     scenario, capability-gated) at a shorter per-cell window so it fits
//     24h single-arch (amd64-only today; ArchParallel is blocked on #168).
//   - full: the same full grid at a longer window, the occasional
//     exhaustive sweep run as a manual dispatch with a raised budget.
//
// Rated/SLO sweeps (#156) are an ADDITIVE second pass scoped to a curated
// subset, so the expensive rated cost is bounded rather than multiplying
// the whole grid.
package budget

import (
	"fmt"
	"strings"
	"time"
)

// Budget is the hard ceiling the calculator asserts against: one cluster
// window is 24h. The benchmark-tier workflow's matrix job carries its own
// timeout-minutes near this; FitWithin guarantees the chosen config fits
// before the run starts so a 24h window is never half-spent on a matrix
// that was always going to overflow.
const Budget = 24 * time.Hour

// PerCellOverhead is the fixed wall-clock a single (scenario, server)
// cell costs OUTSIDE warmup+duration+cooldown: adapter spawn, the
// TCP readiness probe, the SIGTERM drain on teardown, and the mpstat /
// observer teardown. Measured empirically on the cluster; treated as a
// constant so the budget model stays a pure function of the knobs.
const PerCellOverhead = 12 * time.Second

// defaultCooldown is the inter-cell TIME_WAIT drain the runner inserts
// between cells (its built-in default). Carried per-profile so the model
// is explicit, but every curated profile uses this value today.
const defaultCooldown = 5 * time.Second

// DefaultRatedPasses is the number of offered-load steps the runner's
// rated sweep takes when no -rated-fractions override is given
// (cmd/runner defaultRatedFractions = 0.25,0.5,0.75,0.9). The bench
// playbook never overrides the fractions, so this is the multiplier
// ColumnWallClock budgets per rated cell. A cmd/runner test pins
// len(defaultRatedFractions) to this constant so the two cannot drift.
const DefaultRatedPasses = 4

// Profile is the cost model for one benchmark-tier run. Every field is a
// knob the workflow sets; WallClock projects them into a single duration
// the CI budget test and the mage orchestrator both assert against.
type Profile struct {
	Name     string        // "headline" | "full"
	Cells    int           // realized (capability-gated) cell count
	Duration time.Duration // per-cell measurement window (BENCH_DURATION)
	Warmup   time.Duration // per-cell warmup (BENCH_WARMUP)
	Cooldown time.Duration // inter-cell TIME_WAIT drain (runner default 5s)
	Runs     int           // per-cell repetitions; ALWAYS 1 (single-pass bench)
	Arches   int           // 1 (single target) or 2 (both, serial today)

	// ArchParallel, when true, means the two arches' bench passes overlap
	// on the wire (independent loadgen instances, #168) so the arch
	// dimension stops multiplying wall-clock. Blocked on the loadgen repo
	// shipping linux/arm64; until then every profile keeps this false.
	ArchParallel bool

	// Rated sweep (#156), an additive second pass scoped to a curated
	// subset of cells so the expensive SLO sweep is bounded.
	RatedCells    int           // realized rated subset cell count (0 = no rated pass)
	RatedPasses   int           // number of offered-load steps per cell (len(RatedFractions))
	RatedDuration time.Duration // per rated pass measurement window
	RatedWarmup   time.Duration // per rated pass warmup

	// Globs are the BENCH_CELLS glob set the workflow forwards to the
	// runner's -cells filter over "<scenario>/<server>". RatedGlobs is the
	// curated subset for the rated pass. These are data, not derived — the
	// realized Cells/RatedCells counts above are the capability-gated
	// result of expanding these (computed once, pinned as constants the
	// budget test asserts against).
	Globs      []string
	RatedGlobs []string
}

// PerCell is the wall-clock one (scenario, server) cell costs for one
// run: warmup + duration + cooldown + the fixed per-cell overhead.
func (p Profile) PerCell() time.Duration {
	return p.Warmup + p.Duration + p.Cooldown + PerCellOverhead
}

// perRatedCell is the wall-clock one rated cell costs for one run:
// warmup + ratedPasses x ratedDuration + the fixed per-cell overhead.
func (p Profile) perRatedCell() time.Duration {
	return p.RatedWarmup + time.Duration(p.RatedPasses)*p.RatedDuration + PerCellOverhead
}

// Saturation is the non-rated wall-clock: cells x runs x per-cell,
// multiplied by the arch count (the arches run serially today).
func (p Profile) Saturation() time.Duration {
	return time.Duration(p.Cells) * time.Duration(p.Runs) * p.PerCell() * time.Duration(p.arches())
}

// Rated is the additive rated-sweep wall-clock: ratedCells x runs x
// per-rated-cell, multiplied by the arch count. Zero when no rated subset
// is configured.
func (p Profile) Rated() time.Duration {
	if p.RatedCells == 0 || p.RatedPasses == 0 {
		return 0
	}
	return time.Duration(p.RatedCells) * time.Duration(p.Runs) * p.perRatedCell() * time.Duration(p.arches())
}

// WallClock is the total run cost. When ArchParallel is true AND both
// arches run, the arch dimension overlaps on the wire (the #168 win) so
// the doubled Saturation/Rated cost is halved back to a single arch's
// wall-clock.
func (p Profile) WallClock() time.Duration {
	total := p.Saturation() + p.Rated()
	if p.ArchParallel && p.Arches == 2 {
		// Saturation/Rated already counted x2 via arches(); the parallel
		// overlap removes one arch's wall-clock.
		total /= 2
	}
	return total
}

// ColumnWallClock projects the healthy wall-clock of ONE distributed-bench
// column pass: a single runner invocation expanding `scenarios`
// capability-gated scenarios back-to-back against one competitor. Per
// scenario the runner spends warmup+duration on the saturation pass,
// then — in rated mode — ratedPasses extra closed-loop passes of
// warmup+ratedDuration each (every rated pass re-runs the FULL cfg.Warmup:
// runRatedSweep clones the saturation loadgen.Config, there is no separate
// rated warmup on the wire), plus the inter-cell cooldown and the fixed
// per-cell overhead. ratedPasses == 0 models rated mode off.
//
// mage Bench forwards this projection to ansible as
// bench_cell_budget_seconds; run_bench_cell.yml derives both the mpstat
// sampler window and the runner hang-guard timeout from it (budget +
// slack). This function is the single source of truth for that sizing —
// the v3.8 guard was sized from scenarios x (warmup+duration) alone, so
// rated mode (which roughly triples the per-scenario cost: ~5.3min real
// vs ~110s budgeted at 90s/20s/4x30s) blew the 2h22m cap 28 cells into
// every healthy 33-cell celeris column.
func ColumnWallClock(scenarios, ratedPasses int, warmup, duration, ratedDuration time.Duration) time.Duration {
	if scenarios < 1 {
		// Floor: an empty schedule still gets a sane (non-zero) guard —
		// `timeout 0` would DISABLE the hang guard entirely.
		scenarios = 1
	}
	per := warmup + duration + defaultCooldown + PerCellOverhead
	if ratedPasses > 0 {
		per += time.Duration(ratedPasses) * (warmup + ratedDuration)
	}
	return time.Duration(scenarios) * per
}

// arches clamps Arches to at least 1 so a zero-valued Profile never
// collapses wall-clock to zero (which would silently "fit" any budget).
func (p Profile) arches() int {
	if p.Arches < 1 {
		return 1
	}
	return p.Arches
}

// FitWithin asserts the single-pass config fits the budget. The bench
// ALWAYS runs exactly one pass (Runs=1) — the back-to-back / multi-run
// machinery was removed — so there is no Runs dimension left to trim.
// FitWithin therefore projects the one config's wall-clock and returns
// ok=false iff it overflows, in which case the caller MUST fail loudly
// rather than silently shrink the matrix (the no-silent-truncation rule,
// #166). The log records the projected wall-clock so a CI reader sees
// exactly why the config fit (or why it did not).
func (p Profile) FitWithin(budget time.Duration) (log string, ok bool) {
	var sb strings.Builder
	fmt.Fprintf(&sb, "budget fit for profile %q (%d cells, %d rated cells, %d arch%s, parallel=%v):\n",
		p.Name, p.Cells, p.RatedCells, p.arches(), plural(p.arches()), p.ArchParallel)
	fmt.Fprintf(&sb, "  per-cell=%s rated-per-cell=%s budget=%s\n",
		p.PerCell().Round(time.Second), p.perRatedCell().Round(time.Second), budget)

	wc := p.WallClock()
	fit := wc <= budget
	fmt.Fprintf(&sb, "  saturation=%s rated=%s total=%s (%s)\n",
		p.Saturation().Round(time.Minute),
		p.Rated().Round(time.Minute),
		wc.Round(time.Minute),
		fitLabel(fit))
	if fit {
		fmt.Fprintf(&sb, "  fits: total=%s headroom=%s\n",
			wc.Round(time.Minute), (budget - wc).Round(time.Minute))
		return sb.String(), true
	}
	fmt.Fprintf(&sb, "  NO FIT: the single-pass config overflows the %s budget; caller must fail loudly\n",
		budget)
	return sb.String(), false
}

func fitLabel(fit bool) string {
	if fit {
		return "fits"
	}
	return "OVER BUDGET"
}

func plural(n int) string {
	if n == 1 {
		return ""
	}
	return "es"
}

// ForProfile resolves a BENCH_PROFILE env value into a configured
// Profile. Both "headline" and "full" now cover the SAME grid — every
// registered server × every scenario, capability-gated (Globs "*/*") —
// so NEITHER silently drops servers or scenarios. They differ only by the
// per-cell window: "headline" (the weekly cadence) uses a shorter 60s/15s
// window so the whole grid fits the 24h budget single-arch; "full" uses a
// longer 90s/20s window for the occasional exhaustive sweep (over 24h,
// run as a manual dispatch with a raised BENCH_BUDGET). The default (empty
// / unknown) is "full". The caller may still mutate Runs / Arches /
// ArchParallel before asserting FitWithin.
//
// History: "headline" used to be a curated ~14-server × 12-scenario
// subset that silently dropped ~85% of the registry (driver-*, chain-*,
// tls-*, ws-hub-*, h2/h2c variants, and the long-tail servers) from the
// weekly publish. That curation is gone — the weekly grid is now the full
// grid, fit under 24h by the window rather than by dropping coverage.
func ForProfile(name string) Profile {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case "fast", "":
		return Fast()
	case "full":
		return Full()
	case "headline":
		return HeadlineWeekly()
	default:
		return Fast()
	}
}

// RatedGlob joins a profile's rated glob subset into the comma-separated
// BENCH_CELLS value the rated pass forwards to the runner. Empty when the
// profile carries no rated subset.
func RatedGlob(p Profile) string {
	return strings.Join(p.RatedGlobs, ",")
}

// CellsGlob joins a profile's saturation glob set into the comma-separated
// BENCH_CELLS value the saturation pass forwards to the runner.
func CellsGlob(p Profile) string {
	return strings.Join(p.Globs, ",")
}
