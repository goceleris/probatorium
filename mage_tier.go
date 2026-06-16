//go:build mage

package main

import (
	"fmt"
	"os"
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
// runs the saturation pass unconditionally. If the scenario is in
// the rated subset (currently get-json / post-4k / auto-mix-111) and
// the runner is launched with BENCH_RATED=1, the same cell ALSO runs
// the rated sweep after the saturation pass. The cell's JSON carries
// both maps on the same row; the bench's published Document has a
// per-scenario SaturationModeRPS (every scenario) + a per-scenario
// LatencyAtSLO (only the rated 3).
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
//  4. One Bench + one Publish onto run-1. BENCH_RATED=1 is set so every
//     cell does its rated sweep; BENCH_SKIP_RATED=1 disables the rated
//     subpass for throughput-only runs.
//
// Env knobs (in addition to every BENCH_*/PUBLISH_*/DOCS_TOKEN knob):
//
//	BENCH_PROFILE=full         full | headline (default: full — every
//	                           server × every scenario, capability-gated.
//	                           headline is the explicit opt-in for the
//	                           ~3h smoke-test path; never the silent
//	                           default, because users asked repeatedly
//	                           for "no missing tests" and got the
//	                           curated subset instead.)
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
	// weekly headline — the full profile is ~58h on one arch and cannot fit
	// 24h. The CI job's timeout-minutes must exceed this budget (+ Deploy /
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

	// One bench: the runner does BOTH the saturation pass and the rated
	// sweep (per rated-scenario) inside each cell. The published per-cell
	// JSON carries both maps on the same row. The rated sweep is opt-in via
	// BENCH_SKIP_RATED=1 (set when the caller wants throughput-only — e.g.
	// for the weekly smoke test). Per-scenario rated data lands in
	// benchmarks[].latency_at_slo alongside the per-scenario saturation
	// data, so the dashboard's headline reads "for this server × this
	// scenario: RPS, p99-at-1s, SLO" all from one Document.
	skipRated := os.Getenv("BENCH_SKIP_RATED") == "1" || os.Getenv("BENCH_SKIP_RATED") == "true"
	if skipRated {
		_ = os.Unsetenv("BENCH_RATED")
	} else {
		_ = os.Setenv("BENCH_RATED", "1")
	}
	setBenchEnvFromProfile(p, false)
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
	// that needed a manual revert.
	skipPublish := os.Getenv("BENCH_PUBLISH") == "0" || os.Getenv("BENCH_PUBLISH") == "false"
	if skipPublish {
		fmt.Printf("\n=== BENCH_PUBLISH=0 — skipping docs push (smoke test mode) ===\n")
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

// setBenchEnvFromProfile pushes the resolved profile's per-cell tuning +
// cell glob into the BENCH_* env Bench() reads. The new design does
// NOT have a separate rated pass — every cell does BOTH the saturation
// pass and the rated sweep inside one execution, so BENCH_RATED is
// managed by the caller (BenchTier) and this function only sets the
// per-pass tuning + cell glob. The `rated` argument is preserved for
// the legacy "two-pass" callers (Bench, Publish when BENCH_RATED is
// not set) — when true, the cell glob scopes to the rated subset
// AND BENCH_RATED is toggled.
//
// Honour a pre-set BENCH_DURATION / BENCH_WARMUP. The bench (and the
// BenchTier entrypoint) read BENCH_DURATION before setBenchEnvFromProfile
// runs, so a caller can pin a short window for a smoke test without
// having to add a new profile. The function only sets the env if the
// caller hasn't set it — a `mage Smoke` style command uses this to
// override the profile's 90s/20s with 5s/2s for a 30-minute sweep.
func setBenchEnvFromProfile(p budget.Profile, rated bool) {
	if rated {
		_ = os.Setenv("BENCH_RATED", "1")
		_ = os.Setenv("BENCH_CELLS", budget.RatedGlob(p))
		if os.Getenv("BENCH_DURATION") == "" {
			_ = os.Setenv("BENCH_DURATION", durString(p.RatedDuration))
		}
		if os.Getenv("BENCH_WARMUP") == "" {
			_ = os.Setenv("BENCH_WARMUP", durString(p.RatedWarmup))
		}
		_ = os.Setenv("BENCH_RATED_DURATION", durString(p.RatedDuration))
	} else {
		// Saturation-pass tuning: full cell glob, saturation
		// duration. Do NOT touch BENCH_RATED — the caller in the
		// unified path has already set it to "1" so the runner does
		// the rated sweep inside every cell. The legacy Bench() +
		// Publish() callers (without BENCH_RATED set) end up with
		// BENCH_RATED unset, which is the correct behaviour for a
		// pure-saturation call.
		_ = os.Setenv("BENCH_CELLS", budget.CellsGlob(p))
		if os.Getenv("BENCH_DURATION") == "" {
			_ = os.Setenv("BENCH_DURATION", durString(p.Duration))
		}
		if os.Getenv("BENCH_WARMUP") == "" {
			_ = os.Setenv("BENCH_WARMUP", durString(p.Warmup))
		}
	}
}

// durString renders a time.Duration as the Go-parseable string Bench()
// re-parses (e.g. "60s"). Uses the stdlib String() which round-trips.
func durString(d time.Duration) string { return d.String() }
