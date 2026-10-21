//go:build mage

package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/servers"
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
//	BENCH_ARCH_EXCLUSIVE=      both only: all (default) | heavy | off, see Bench
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
	// Arch count drives the cost model: BENCH_TARGET=both is two arches, run
	// concurrently from the one msa2-client (#168), under the cross-arch client
	// lock (BENCH_ARCH_EXCLUSIVE, probatorium#442), which costs wall-clock.
	target := envOrDefault("BENCH_TARGET", defaultClusterTarget)
	if target == "both" {
		p.Arches = 2
	} else {
		p.Arches = 1
	}
	archPolicy, err := resolveArchExclusion(target)
	if err != nil {
		return err
	}
	p.ArchExclusion = archPolicy

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

	// The opt-in baseline columns (BENCH_BASELINE=1, servers/baseline.go) are
	// extra cells the profile's pins do not count: project the grid that will
	// run, so the gate cannot pass a baseline dispatch on the baseline-less
	// numbers. The workflow raises BENCH_BUDGET for a baseline dispatch.
	// A preset scope (the workflow's `cells` and `rated_cells` inputs) changes
	// which cells run and how many are rated: project that, not the profile's
	// pins. rated_cells='*' on the full profile rates all 813 cells (about twice
	// the model's 401) and used to pass this gate on the pins' numbers.
	p, err = tierProjection(p)
	if err != nil {
		return err
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
		if err := publishPolicyGate(archPolicy); err != nil {
			return err
		}
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

// dryRunSummary is the runner's dry-run line for a rated run:
// "the rated sweep runs on <rated> of the <cells> cells".
var dryRunSummary = regexp.MustCompile(`the rated sweep runs on (\d+) of the (\d+) cells`)

// parseDryRunSummary reads the cell count and the rated cell count out of a
// rated `cmd/runner -dry-run`'s stderr. Pure, so it is unit-testable.
func parseDryRunSummary(stderr string) (cells, rated int, err error) {
	m := dryRunSummary.FindStringSubmatch(stderr)
	if m == nil {
		return 0, 0, fmt.Errorf("the runner's dry run printed no rated-sweep summary:\n%s", strings.TrimSpace(stderr))
	}
	rated, _ = strconv.Atoi(m[1])
	cells, _ = strconv.Atoi(m[2])
	return cells, rated, nil
}

// realizedCounts asks the runner itself which cells a scope schedules and how
// many get the rated sweep: the registry, the capability gating and
// BENCH_BASELINE all apply, as they will in the run. A variable so a test can
// stand in for the exec.
var realizedCounts = func(cells, ratedCells string) (int, int, error) {
	cmd := exec.Command("go", "run", "./cmd/runner", "-dry-run", "-runs", "1",
		"-cells", cells, "-rated", "-rated-cells", ratedCells)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if _, err := cmd.Output(); err != nil {
		return 0, 0, fmt.Errorf("resolve the cell counts of -cells %q -rated-cells %q: %w\n%s", cells, ratedCells, err, strings.TrimSpace(stderr.String()))
	}
	return parseDryRunSummary(stderr.String())
}

// scopedProjection returns the profile to gate. With neither BENCH_CELLS nor
// BENCH_RATED_CELLS preset it is p itself (the pins are the grid). Otherwise
// the cell and rated cell counts are the runner's realized counts for the
// scope that will run (baseline columns included, since the dry run reads
// BENCH_BASELINE), so a wider rated scope is priced and a narrower one is not
// over-charged. scoped reports that the counts already include the baseline
// columns. A profile without a rated sweep rates nothing whatever the glob.
func scopedProjection(p budget.Profile) (budget.Profile, bool, error) {
	cells := strings.TrimSpace(os.Getenv("BENCH_CELLS"))
	rated := strings.TrimSpace(os.Getenv("BENCH_RATED_CELLS"))
	comps := strings.TrimSpace(os.Getenv("BENCH_COMPETITORS"))
	if comps == "all" {
		comps = ""
	}
	if cells == "" && comps == "" && (rated == "" || p.RatedPasses == 0) {
		return p, false, nil
	}
	if cells == "" {
		cells = budget.CellsGlob(p)
	}
	if comps != "" {
		// the runner's dry run schedules every column the cells glob matches;
		// BENCH_COMPETITORS narrows the columns the bench runs, so narrow the
		// glob the same way.
		cells = narrowCellsToColumns(cells, splitTrimNonEmpty(comps))
	}
	if rated == "" {
		rated = budget.RatedGlob(p)
	}
	n, r, err := realizedCounts(cells, rated)
	if err != nil {
		return p, false, fmt.Errorf("cannot project the scoped run, refusing rather than gating the profile's pins: %w", err)
	}
	if p.RatedPasses == 0 {
		r = 0
	}
	fmt.Printf("scoped run: BENCH_CELLS=%q BENCH_RATED_CELLS=%q realize %d cells, %d rated (the profile's pins: %d, %d)\n", cells, rated, n, r, p.Cells, p.RatedCells)
	p.Cells, p.RatedCells = n, r
	return p, true, nil
}

// tierProjection is the profile BenchTier gates: the scoped counts when a scope
// is preset (they already include the baseline columns), else the pins plus the
// baseline columns when BENCH_BASELINE is on.
func tierProjection(p budget.Profile) (budget.Profile, error) {
	p, scoped, err := scopedProjection(p)
	if err != nil {
		return p, err
	}
	if !scoped {
		p = baselineProjection(p)
	}
	return p, nil
}

// publishPolicyGate refuses, before the multi-hour bench, a run that would
// publish documents the publish gate will refuse: policy heavy (the exemption of
// the light rows failed P6 on arm64) and policy off (the arches overlapped).
// Run data-only with BENCH_PUBLISH=0, or override with BENCH_PUBLISH_FORCE=1 as
// at the publish gate. A single-arch run has no policy ("") and is not affected.
// The workflow's setup job refuses the same dispatches.
func publishPolicyGate(policy string) error {
	if (policy == "heavy" || policy == "off") && os.Getenv("BENCH_PUBLISH_FORCE") != "1" {
		return fmt.Errorf("BENCH_ARCH_EXCLUSIVE=%s would publish documents the publish gate refuses (policy heavy failed P6 on arm64, policy off lets the arches overlap; docs/ARCH-EXCLUSION-VALIDATION.md): set BENCH_PUBLISH=0 for a data-only run, use the default policy (all), or BENCH_PUBLISH_FORCE=1 to override", policy)
	}
	return nil
}

// narrowCellsToColumns restricts a cell glob list ("<scenario>/<server>",
// "!<exclude>") to the named columns: every include glob keeps the scenario
// half and keeps the columns its server half matches, spelt out; excludes stay.
// The result has no include glob when no named column matches (a scope that
// schedules nothing, which the bench itself refuses).
func narrowCellsToColumns(cells string, cols []string) string {
	var out []string
	hasInclude := false
	for _, part := range strings.Split(cells, ",") {
		g := strings.TrimSpace(part)
		if g == "" {
			continue
		}
		if strings.HasPrefix(g, "!") {
			out = append(out, g)
			continue
		}
		sc, srv, ok := strings.Cut(g, "/")
		if !ok { // "*" alone matches every cell
			sc, srv = "*", "*"
		}
		for _, c := range cols {
			if m, _ := path.Match(srv, c); m {
				out = append(out, sc+"/"+c)
				hasInclude = true
			}
		}
	}
	if !hasInclude {
		return "!*/*"
	}
	return strings.Join(out, ",")
}

// baselineProjection adds the opt-in baseline columns to p's cell counts when
// BENCH_BASELINE is on (servers.BaselineEnabled) and returns p unchanged
// otherwise.
func baselineProjection(p budget.Profile) budget.Profile {
	if !servers.BaselineEnabled() {
		return p
	}
	fmt.Printf("baseline columns ON (%s): +%d saturation cells, +%d rated cells projected\n",
		servers.BaselineEnvVar, budget.BaselineRealizedCells, budget.BaselineRatedRealizedCells)
	return p.WithBaseline()
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
// this) counts the scope that will run: scopedProjection asks the runner's dry
// run for the realized cell and rated cell counts of a preset BENCH_CELLS /
// BENCH_RATED_CELLS, so neither a scoped run nor a widened rated scope slips
// past the gate on the profile's pins.
func setBenchEnvFromProfile(p budget.Profile, rated bool) {
	setCells := func(profileGlob string) {
		glob, overridden := resolveBenchCells(os.Getenv("BENCH_CELLS"), profileGlob)
		if overridden {
			fmt.Printf("  BENCH_CELLS preset %q overrides the %s profile glob %q (the budget was projected from the scope's realized %d cells / %d rated)\n",
				glob, p.Name, profileGlob, p.Cells, p.RatedCells)
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
