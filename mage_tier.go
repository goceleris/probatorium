//go:build mage

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/report"
)

// RatedRunIDSuffix is appended to a back-to-back run-K id to form the
// sibling cell id for the rated (closed-loop, SLO-targeted) pass. The
// suffix is appended verbatim to a name already matched by
// /^run-\d+$/, so the docs ingest's run- dir regex (and the index's
// run-sort comparator) must accept the suffix explicitly. The dash
// separator keeps the rated cell human-distinguishable in the tree
// (`run-2/`, `run-2-rated/`) and keeps the rated id shorter than a
// full canonical id like `run-2.rated` so filesystem-safe everywhere.
const RatedRunIDSuffix = "-rated"

// Benchmark-tier orchestration (probatorium#172/#166/#167). BenchTier
// resolves a curated profile into a budget-asserted matrix and runs it
// back-to-back N times, publishing each pass as run-1..run-N. DetectRelease
// is the cheap ubuntu-side gate that decides N by comparing the go.mod
// celeris pin against the newest already-benchmarked version in the docs
// index.json. Both are thin shells over the pure helpers in budget/ and
// report/ so the cost model + version logic are unit-tested without the
// mage build tag.

// DetectRelease prints GITHUB_OUTPUT-compatible lines reporting whether
// the celeris version under test (go.mod pin > CELERIS_VERSION) is newer
// than the newest version already published to the docs index.json. The
// docs sync-benchmarks workflow is the single writer of that manifest, so
// it is the canonical record — no extra state branch or GH variable.
//
// Output (stdout, append to $GITHUB_OUTPUT in the workflow):
//
//	is_new_release=true|false
//	version=<celeris pin under test>
//	newest_benchmarked=<newest in docs index, or "" on cold start>
//
// Cold start (index absent / unreadable) is treated as a new release so
// the very first run seeds a baseline rather than silently skipping.
//
// Env knobs: DOCS_TOKEN (docs read token; falls back to `gh auth token`).
func DetectRelease() error {
	version, err := celerisVersion()
	if err != nil {
		return err
	}

	newest, idxErr := newestBenchmarkedVersion()
	isNew := true
	switch {
	case idxErr != nil:
		// Cold start or unreadable index: seed a baseline. Log to stderr so
		// the GITHUB_OUTPUT capture on stdout stays clean.
		fmt.Fprintf(os.Stderr, "DetectRelease: docs index unreadable (%v); treating as new release\n", idxErr)
		isNew = true
	case newest == "":
		fmt.Fprintln(os.Stderr, "DetectRelease: docs index empty; treating as new release")
		isNew = true
	default:
		isNew = report.CompareSemver(version, newest) > 0
	}

	fmt.Printf("is_new_release=%t\n", isNew)
	fmt.Printf("version=%s\n", version)
	fmt.Printf("newest_benchmarked=%s\n", newest)
	return nil
}

// newestBenchmarkedVersion fetches results/index.json from the docs repo
// via the contents API and returns the highest version key in it. Returns
// ("", nil) when the index exists but carries no versions (cold start),
// and ("", err) when the fetch itself fails (network / auth / 404) — the
// caller maps both empty and error to "treat as new release".
func newestBenchmarkedVersion() (string, error) {
	token, err := resolveDocsToken()
	if err != nil {
		return "", err
	}
	// The contents API returns {"content":"<base64>","encoding":"base64"}.
	out, err := ghAPI(token, "GET", "/repos/"+docsRepo+"/contents/results/index.json", nil)
	if err != nil {
		return "", fmt.Errorf("fetch docs index.json: %w", err)
	}
	var resp struct {
		Content  string `json:"content"`
		Encoding string `json:"encoding"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", fmt.Errorf("parse contents API response: %w", err)
	}
	if resp.Content == "" {
		return "", nil
	}
	// The contents API wraps base64 at 76 cols with newlines.
	raw, err := base64.StdEncoding.DecodeString(strings.ReplaceAll(resp.Content, "\n", ""))
	if err != nil {
		return "", fmt.Errorf("decode index.json content: %w", err)
	}
	return report.NewestBenchmarkedVersion(raw)
}

// BenchTier runs the budget-asserted curated benchmark matrix N times
// back-to-back, publishing each pass as a distinct run-K cell under the
// same version/date/arch (#167). A second rated-scoped pass (#156) runs
// once per N. The runner executes BOTH the saturation pass (open-loop
// blast) AND the rated sweep (closed-loop coordinated-omission-corrected
// at fractions of measured saturation) inside a single cell, so the
// published per-cell JSON carries both panels on the same scenario.
//
// The two-pass design (separate run-K/ + run-K-rated/ subdirs, with the
// rated pass using a smaller 8-server × 3-scenario glob) is intentionally
// gone: it doubled the published-tree cell count, fragmented the data
// into two folders per iteration (run-K/ for the throughput grid,
// run-K-rated/ for the latency-at-SLO grid), split the bench across
// two UTC days when the run crossed midnight, and forced the consumer
// to merge two folders to answer "what's celeris-iouring's RPS and
// p99-at-1s-slo on the get-json scenario?". The new design answers
// that question in one folder.
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
//  2. Override Runs from BENCH_RUNS (per-cell median basis, default 3),
//     then assert the whole run fits the 24h budget via FitWithin —
//     logging exactly what was trimmed. NEVER silently truncates: if even
//     minRuns overflows, fail loudly.
//  3. Set BENCH_START_DATE to the bench start timestamp (UTC yyyymmdd)
//     and reuse it for every iteration's Publish so the whole back-to-back
//     run lands under a single date even when it crosses midnight.
//  4. For k in 1..N (BENCH_BACK_TO_BACK): one Bench + one Publish, both
//     onto run-K. BENCH_RATED=1 is set so every cell does its rated
//     sweep; BENCH_SKIP_RATED=1 disables the rated subpass for throughput-
//     only runs.
//
// N (back-to-back published cells) is orthogonal to BENCH_RUNS (per-cell
// median interleave inside one publish). N=3 outer x BENCH_RUNS=3 inner is
// the budget-pinned release config; weekly N=1.
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
//	BENCH_BACK_TO_BACK=1       N published run-K cells (release: 3)
//	BENCH_RUNS=3               per-cell median basis inside each publish
//	BENCH_TARGET=both          msa2-server | msr1 | both (both = 2 arches)
//	BENCH_SKIP_RATED=          "1" runs saturation passes only. Every
//	                           cell still runs the saturation pass; the
//	                           rated sweep is skipped per-cell.
func BenchTier() error {
	p := budget.ForProfile(os.Getenv("BENCH_PROFILE"))
	n := atoiOr(os.Getenv("BENCH_BACK_TO_BACK"), 1)
	if n < 1 {
		n = 1
	}
	p.Runs = atoiOr(envOrDefault("BENCH_RUNS", "3"), 3)
	// Arch count drives the cost model: BENCH_TARGET=both is two arches
	// (serial today — #168 ArchParallel is blocked on loadgen arm64).
	if envOrDefault("BENCH_TARGET", defaultClusterTarget) == "both" {
		p.Arches = 2
	} else {
		p.Arches = 1
	}

	runs, log, ok := p.FitWithin(budget.Budget, 1)
	fmt.Println(log)
	if !ok {
		return fmt.Errorf("no benchmark config fits the %s budget for profile %q; aborting rather than truncating the matrix:\n%s",
			budget.Budget, p.Name, log)
	}
	p.Runs = runs

	fmt.Printf("\n=== BenchTier: profile=%s back-to-back=%d runs/cell=%d cells=%d rated-cells=%d ===\n",
		p.Name, n, p.Runs, p.Cells, p.RatedCells)

	// Bench start date is set ONCE before the back-to-back loop and
	// inherited by every Publish in the iteration so a 10h bench that
	// crosses midnight lands all cells under the same date. (Prior
	// implementation used now() per Publish, which split the 3
	// iterations across 2 dates when a run straddled UTC midnight.)
	benchStartDate := time.Now().UTC().Format("20060102")
	_ = os.Setenv("BENCH_START_DATE", benchStartDate)

	for k := 1; k <= n; k++ {
		runID := fmt.Sprintf("run-%d", k)
		fmt.Printf("\n=== BenchTier: %s ===\n", runID)
		if err := os.Setenv("PUBLISH_RUN_ID", runID); err != nil {
			return err
		}
		// One bench per iteration: the runner does BOTH the saturation
		// pass and the rated sweep (per rated-scenario) inside each
		// cell. The published per-cell JSON carries both maps on the
		// same row. The rated sweep is opt-in via BENCH_SKIP_RATED=1
		// (set when the caller wants throughput-only — e.g. for the
		// weekly smoke test). Per-scenario rated data lands in
		// benchmarks[].latency_at_slo alongside the per-scenario
		// saturation data, so the dashboard's headline reads "for
		// this server × this scenario: RPS, p99-at-1s, SLO" all
		// from one Document.
		skipRated := os.Getenv("BENCH_SKIP_RATED") == "1" || os.Getenv("BENCH_SKIP_RATED") == "true"
		if skipRated {
			_ = os.Unsetenv("BENCH_RATED")
		} else {
			_ = os.Setenv("BENCH_RATED", "1")
		}
		setBenchEnvFromProfile(p, false)
		if err := Bench(); err != nil {
			return fmt.Errorf("%s bench: %w", runID, err)
		}
		if err := Publish(); err != nil {
			return fmt.Errorf("%s publish: %w", runID, err)
		}
	}
	_ = os.Unsetenv("PUBLISH_RUN_ID")
	_ = os.Unsetenv("BENCH_RATED")
	_ = os.Unsetenv("BENCH_START_DATE")
	fmt.Printf("\n=== BenchTier complete (%d back-to-back run%s) ===\n", n, pluralN(n))
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
	_ = os.Setenv("BENCH_RUNS", fmt.Sprintf("%d", p.Runs))
}

// durString renders a time.Duration as the Go-parseable string Bench()
// re-parses (e.g. "60s"). Uses the stdlib String() which round-trips.
func durString(d time.Duration) string { return d.String() }

func pluralN(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}
