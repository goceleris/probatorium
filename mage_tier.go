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
// once per N for the SLO panel.
//
// Flow:
//
//  1. Resolve BENCH_PROFILE (headline|full) into a budget.Profile with the
//     curated cell globs + per-cell tuning.
//  2. Override Runs from BENCH_RUNS (per-cell median basis, default 3),
//     then assert the whole run fits the 24h budget via FitWithin —
//     logging exactly what was trimmed. NEVER silently truncates: if even
//     minRuns overflows, fail loudly.
//  3. For k in 1..N (BENCH_BACK_TO_BACK): publish the saturation pass as
//     run-k, then the rated subset pass as run-k.
//
// N (back-to-back published cells) is orthogonal to BENCH_RUNS (per-cell
// median interleave inside one publish). N=3 outer x BENCH_RUNS=3 inner is
// the budget-pinned release config; weekly N=1.
//
// Env knobs (in addition to every BENCH_*/PUBLISH_*/DOCS_TOKEN knob):
//
//	BENCH_PROFILE=headline     headline | full curated matrix
//	BENCH_BACK_TO_BACK=1       N published run-K cells (release: 3)
//	BENCH_RUNS=3               per-cell median basis inside each publish
//	BENCH_TARGET=both          msa2-server | msr1 | both (both = 2 arches)
func BenchTier() error {
	p := budget.ForProfile(os.Getenv("BENCH_PROFILE"))
	n := atoiOr(os.Getenv("BENCH_BACK_TO_BACK"), 1)
	if n < 1 {
		n = 1
	}
	p.Runs = atoiOr(envOrDefault("BENCH_RUNS", "3"), 3)
	// Arch count drives the cost model: BENCH_TARGET=both is two arches
	// (serial today — #168 ArchParallel is blocked on loadgen arm64).
	if envOrDefault("BENCH_TARGET", "both") == "both" {
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

	for k := 1; k <= n; k++ {
		runID := fmt.Sprintf("run-%d", k)
		fmt.Printf("\n=== BenchTier: %s (saturation pass) ===\n", runID)
		if err := os.Setenv("PUBLISH_RUN_ID", runID); err != nil {
			return err
		}
		// Saturation pass: curated cell glob, rated OFF, per-cell tuning.
		setBenchEnvFromProfile(p, false)
		if err := Bench(); err != nil {
			return fmt.Errorf("%s saturation bench: %w", runID, err)
		}
		if err := Publish(); err != nil {
			return fmt.Errorf("%s saturation publish: %w", runID, err)
		}

		// Rated pass: curated subset, rated ON, same run-K cell. Skipped
		// when the profile carries no rated subset.
		if p.RatedCells > 0 && len(p.RatedGlobs) > 0 {
			fmt.Printf("\n=== BenchTier: %s (rated pass) ===\n", runID)
			setBenchEnvFromProfile(p, true)
			if err := Bench(); err != nil {
				return fmt.Errorf("%s rated bench: %w", runID, err)
			}
			if err := Publish(); err != nil {
				return fmt.Errorf("%s rated publish: %w", runID, err)
			}
			_ = os.Unsetenv("BENCH_RATED")
		}
	}
	_ = os.Unsetenv("PUBLISH_RUN_ID")
	fmt.Printf("\n=== BenchTier complete (%d back-to-back run%s) ===\n", n, pluralN(n))
	return nil
}

// setBenchEnvFromProfile pushes the resolved profile's per-cell tuning +
// cell glob into the BENCH_* env Bench() reads. rated=true scopes the cell
// glob to the curated rated subset and flips BENCH_RATED on with the
// rated-pass durations; rated=false runs the full saturation glob with
// rated off.
func setBenchEnvFromProfile(p budget.Profile, rated bool) {
	if rated {
		_ = os.Setenv("BENCH_RATED", "1")
		_ = os.Setenv("BENCH_CELLS", budget.RatedGlob(p))
		_ = os.Setenv("BENCH_DURATION", durString(p.RatedDuration))
		_ = os.Setenv("BENCH_WARMUP", durString(p.RatedWarmup))
		_ = os.Setenv("BENCH_RATED_DURATION", durString(p.RatedDuration))
	} else {
		_ = os.Unsetenv("BENCH_RATED")
		_ = os.Setenv("BENCH_CELLS", budget.CellsGlob(p))
		_ = os.Setenv("BENCH_DURATION", durString(p.Duration))
		_ = os.Setenv("BENCH_WARMUP", durString(p.Warmup))
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
