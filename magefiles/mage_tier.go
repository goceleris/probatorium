//go:build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goceleris/probatorium/budget"
)

// Benchmark-tier orchestration (probatorium#172/#166/#167). BenchTier
// resolves a curated profile into a budget-asserted matrix and runs it
// exactly ONCE, publishing the single pass as run-1. It is a thin shell
// over the pure helpers in budget/ and report/ so the cost model is
// unit-tested without the mage build tag.
//
// History: BenchTier used to run the matrix back-to-back N times
// (PUBLISH_RUN_ID=run-1..run-N) and bump that N when a new release was
// detected. That multi-pass machinery is gone — the bench ALWAYS does
// one pass. If more passes are wanted, more benchmarks are scheduled.

// BenchTier runs the budget-asserted curated benchmark matrix exactly
// once, publishing it as run-1 under the bench's version/date/arch. The
// runner executes BOTH the saturation pass (open-loop blast) AND the
// rated sweep (closed-loop coordinated-omission-corrected at fractions of
// measured saturation) inside a single cell, so the published per-cell
// JSON carries both panels on the same scenario.
//
// Per-cell execution: a cell visits ONE (server, scenario) pair and
// runs the saturation pass unconditionally. If the profile is rated and
// the cell matches the profile's rated glob (budget.RatedGlob: every
// budget.RatedScenarios scenario on every capable server, 401 cells), the
// same cell ALSO runs the rated sweep once its saturation pass is clean.
// The cell's JSON carries both maps on the same row; the published
// Document has a per-scenario SaturationModeRPS (every scenario) and a
// per-scenario LatencyAtSLO / RatedModeP99AtTargetRPS (rated scenarios
// only). Before probatorium#418 the rated glob never reached the runner,
// which then rated every clean cell (~770 of 813).
//
// Flow:
//
//  1. Resolve BENCH_PROFILE (headline|full) into a budget.Profile with the
//     curated cell globs + per-cell tuning.
//  2. Assert the single-pass config fits the 24h budget (or BENCH_BUDGET
//     override) via FitWithin — NEVER silently truncates: if it overflows,
//     fail loudly.
//  3. Set BENCH_START_DATE to the bench start timestamp (UTC yyyymmdd) and
//     reuse it for Publish so the whole run lands under a single date even
//     when it crosses midnight.
//  4. One Bench + one Publish onto run-1. For a rated profile BENCH_RATED=1,
//     BENCH_RATED_CELLS (the rated glob) and BENCH_RATED_DURATION (the
//     profile's rated pass window) are set, so the rated cells do their
//     rated sweep; BENCH_SKIP_RATED=1 disables it for throughput-only runs.
//
// Env knobs (in addition to every BENCH_*/PUBLISH_*/DOCS_TOKEN knob):
//
//	BENCH_PROFILE=full         full | headline. Both cover the SAME full
//	                           grid (every server × every scenario,
//	                           capability-gated); they differ ONLY by the
//	                           per-cell window. headline (the weekly
//	                           cadence) uses 40s/12s so the whole grid fits
//	                           24h single-arch (~16.7h); full uses 90s/20s
//	                           for the exhaustive sweep (~30h on one arch,
//	                           needs a raised BENCH_BUDGET). Default: full.
//	BENCH_TARGET=both          msa2-server | msr1 | both (both = 2 arches)
//	BENCH_SKIP_RATED=          "1" runs saturation passes only. Every
//	                           cell still runs the saturation pass; the
//	                           rated sweep is skipped per-cell.
//	BENCH_PUBLISH=0            "0" / "false" skips the docs push so a
//	                           short-window smoke test (e.g.
//	                           BENCH_DURATION=5s BENCH_WARMUP=2s
//	                           BENCH_SKIP_RATED=1 BENCH_PUBLISH=0) can
//	                           verify every (server, scenario) pair
//	                           produces data WITHOUT shipping partial
//	                           results to the public docs site. Real
//	                           publishes leave BENCH_PUBLISH unset
//	                           (default = push).
func BenchTier() error {
	p := budget.ForProfile(os.Getenv("BENCH_PROFILE"))
	// The bench ALWAYS runs exactly one pass; profiles ship Runs=1.
	p.Runs = 1
	// Arch count drives the cost model: BENCH_TARGET=both is two arches
	// (serial today — #168 ArchParallel is blocked on loadgen arm64).
	if envOrDefault("BENCH_TARGET", defaultClusterTarget) == "both" {
		p.Arches = 2
	} else {
		p.Arches = 1
	}

	// The fit budget defaults to the 24h weekly cluster invariant
	// (budget.Budget) but can be raised per-invocation via BENCH_BUDGET for
	// a manual full-matrix dispatch that intentionally runs longer than the
	// weekly headline — the full profile is ~30h on one arch (the same full
	// grid as headline, at the longer 90s/20s window) and cannot fit 24h.
	// The CI job's timeout-minutes must exceed this budget (+ Deploy /
	// Cleanup overhead).
	fitBudget := budget.Budget
	if v := os.Getenv("BENCH_BUDGET"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("BENCH_BUDGET %q: %w", v, err)
		}
		fitBudget = d
	}

	log, ok := p.FitWithin(fitBudget)
	fmt.Println(log)
	if !ok {
		return fmt.Errorf("the single-pass benchmark config does not fit the %s budget for profile %q; aborting rather than truncating the matrix:\n%s",
			fitBudget, p.Name, log)
	}

	fmt.Printf("\n=== BenchTier: profile=%s runs/cell=%d cells=%d rated-cells=%d ===\n",
		p.Name, p.Runs, p.Cells, p.RatedCells)

	// Bench start date is set ONCE before the run and inherited by Publish
	// so a 10h bench that crosses midnight lands all cells under the same
	// date.
	benchStartDate := time.Now().UTC().Format("20060102")
	_ = os.Setenv("BENCH_START_DATE", benchStartDate)

	// One bench: the runner does BOTH the saturation pass and, on the rated
	// cells, the rated sweep inside each cell. The published per-cell
	// JSON carries both maps on the same row. The rated sweep is opt-in via
	// BENCH_SKIP_RATED=1 (set when the caller wants throughput-only — e.g.
	// for the weekly smoke test). Per-scenario rated data lands in
	// benchmarks[].latency_at_slo alongside the per-scenario saturation
	// data, so the dashboard's headline reads "for this server × this
	// scenario: RPS, p99-at-1s, SLO" all from one Document.
	// Rated OFF when the caller asks (BENCH_SKIP_RATED) OR the profile ships
	// no rated sweep (RatedPasses==0, e.g. the "fast" routine/weekly profile).
	// Rated is the dominant per-cell cost (4 closed-loop passes), so the
	// default fast profile leaves it off and runs the full grid saturation-only.
	benchTierEnv(p)

	// Publish preflight: if this run WILL publish at the end, prove NOW (before
	// the multi-hour bench) that the docs token + version resolve. A missing
	// DOCS_DISPATCH_TOKEN secret otherwise wastes the entire run — the v1.5.5
	// bench finished all 813 cells and then failed the final push because the
	// token was unresolvable on the runner. Skipped for data-only runs.
	skipPublish := os.Getenv("BENCH_PUBLISH") == "0" || os.Getenv("BENCH_PUBLISH") == "false"
	if !skipPublish {
		if err := publishPreflight(); err != nil {
			return fmt.Errorf("publish preflight failed — aborting before the bench to avoid wasting cluster time (set BENCH_PUBLISH=0 for a data-only run): %w", err)
		}
	}

	if err := Bench(); err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	// BENCH_PUBLISH=0 / false skips the docs push so a smoke test
	// (`mage BenchTier BENCH_DURATION=5s BENCH_WARMUP=2s BENCH_SKIP_RATED=1
	// BENCH_PUBLISH=0`) can verify every (server, scenario) pair produces a
	// non-zero RPS result WITHOUT shipping partial / short-window data to
	// the public docs site. v3.8's 5s smoke test accidentally published
	// 110 OK + 2 DNF + 21 not_applicable cells because there was no env
	// knob to suppress the auto-publish — root cause of the docs pollution
	// that needed a manual revert. (skipPublish is computed above, before the
	// bench, so the publish preflight can gate the run.)
	if skipPublish {
		fmt.Printf("\n=== BENCH_PUBLISH=0 — skipping docs push (smoke test mode) ===\n")
	} else if clusterTarget() == "both" {
		// A both-arch bench wrote results-amd64.json AND results-arm64.json
		// instead of one blended results.json, so publish each in turn and
		// pin Publish to the exact file — auto-discovery cannot choose.
		// Each Document carries its own HostArchPair, so archTagFromHostArchPair
		// lands them on separate publish paths.
		dir, err := latestBenchDir("")
		if err != nil {
			return fmt.Errorf("locate per-arch bench results: %w", err)
		}
		for _, arch := range []string{"amd64", "arm64"} {
			f := filepath.Join(dir, "results-"+arch+".json")
			if _, statErr := os.Stat(f); statErr != nil {
				return fmt.Errorf("expected per-arch results %s for a BENCH_TARGET=both run: %w", f, statErr)
			}
			fmt.Printf("\n=== Publish arch=%s (%s) ===\n", arch, f)
			_ = os.Setenv("PUBLISH_RESULTS", f)
			if err := Publish(); err != nil {
				_ = os.Unsetenv("PUBLISH_RESULTS")
				return fmt.Errorf("publish %s: %w", arch, err)
			}
		}
		_ = os.Unsetenv("PUBLISH_RESULTS")
	} else {
		if err := Publish(); err != nil {
			return fmt.Errorf("publish: %w", err)
		}
	}
	_ = os.Unsetenv("BENCH_RATED")
	_ = os.Unsetenv("BENCH_START_DATE")
	fmt.Printf("\n=== BenchTier complete (single pass) ===\n")
	return nil
}

// benchTierEnv resolves the BENCH_* env the one Bench of a BenchTier run of
// profile p reads.
func benchTierEnv(p budget.Profile) {
	skipRated := os.Getenv("BENCH_SKIP_RATED") == "1" || os.Getenv("BENCH_SKIP_RATED") == "true" || p.RatedPasses == 0
	if skipRated {
		_ = os.Unsetenv("BENCH_RATED")
	} else {
		_ = os.Setenv("BENCH_RATED", "1")
	}
	setBenchEnvFromProfile(p, !skipRated)
}

// setBenchEnvFromProfile pushes the resolved profile's per-cell tuning +
// cell glob into the BENCH_* env Bench() reads. There is no separate
// rated pass: a rated cell does BOTH the saturation pass and the rated
// sweep inside one execution, so the saturation glob and window are set
// either way. BENCH_RATED itself is the caller's (benchTierEnv). With
// rated, it also scopes the sweep and sizes its passes from the profile:
// BENCH_RATED_CELLS = budget.RatedGlob(p) (the runner's -rated-cells)
// and BENCH_RATED_DURATION = p.RatedDuration. Without those two the
// runner rated every clean cell at mage Bench's 30 s default
// (probatorium#418), whatever the budget model planned.
//
// Honour a pre-set BENCH_DURATION / BENCH_WARMUP. The bench (and the
// BenchTier entrypoint) read BENCH_DURATION before setBenchEnvFromProfile
// runs, so a caller can pin a short window for a smoke test without
// having to add a new profile. The function only sets the env if the
// caller hasn't set it — a `mage Smoke` style command uses this to
// override the profile's 90s/20s with 5s/2s for a 30-minute sweep.
//
// BENCH_CELLS is honoured the same way (celeris#585): a non-blank preset
// (benchmark-tier.yml's `cells` input) scopes the run to those globs
// instead of the profile's, so a 3-scenario A/B does not pay for the
// column's whole catalogue. The budget projection (FitWithin, run before
// this) still counts the profile's full grid, which a scoped run passes
// trivially — logged, not hidden.
func setBenchEnvFromProfile(p budget.Profile, rated bool) {
	setCells := func(profileGlob string) {
		glob, overridden := resolveBenchCells(os.Getenv("BENCH_CELLS"), profileGlob)
		if overridden {
			fmt.Printf("  BENCH_CELLS preset %q overrides the %s profile glob %q (the budget projection counted the profile's %d cells; this scoped run is strictly smaller)\n",
				glob, p.Name, profileGlob, p.Cells)
		}
		_ = os.Setenv("BENCH_CELLS", glob)
	}
	setCells(budget.CellsGlob(p))
	if os.Getenv("BENCH_DURATION") == "" {
		_ = os.Setenv("BENCH_DURATION", durString(p.Duration))
	}
	if os.Getenv("BENCH_WARMUP") == "" {
		_ = os.Setenv("BENCH_WARMUP", durString(p.Warmup))
	}
	if !rated {
		return
	}
	// A preset wins here too, like BENCH_CELLS above: a caller may widen or
	// narrow the rated scope, or shorten the passes, for a scoped run.
	if strings.TrimSpace(os.Getenv("BENCH_RATED_CELLS")) == "" {
		_ = os.Setenv("BENCH_RATED_CELLS", budget.RatedGlob(p))
	}
	if os.Getenv("BENCH_RATED_DURATION") == "" {
		_ = os.Setenv("BENCH_RATED_DURATION", durString(p.RatedDuration))
	}
}

// resolveBenchCells picks the cell glob a bench pass runs: a non-blank
// caller preset (BENCH_CELLS from the workflow's `cells` input) wins over
// the profile's glob; blank or whitespace falls through to the profile.
// overridden reports which branch was taken so the caller can log it.
func resolveBenchCells(preset, profileGlob string) (glob string, overridden bool) {
	if p := strings.TrimSpace(preset); p != "" {
		return p, true
	}
	return profileGlob, false
}

// durString renders a time.Duration as the Go-parseable string Bench()
// re-parses (e.g. "60s"). Uses the stdlib String() which round-trips.
func durString(d time.Duration) string { return d.String() }
