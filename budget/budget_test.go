package budget

import (
	"strings"
	"testing"
	"time"
)

// TestWeeklyConfigFitsBudget is the CI invariant (#166): the exact config
// the benchmark-tier workflow runs weekly must fit the 24h cluster
// window. Runs on the cluster-free test.yml job so the budget is enforced
// on every PR, before any 24h window is ever spent.
func TestWeeklyConfigFitsBudget(t *testing.T) {
	p := HeadlineWeekly()
	if got := p.WallClock(); got >= Budget {
		t.Fatalf("weekly headline wall-clock %v >= %v budget (saturation=%v rated=%v)",
			got, Budget, p.Saturation(), p.Rated())
	}
}

// TestNewReleaseConfigFitsBudget pins the back-to-back release config
// (#167): N=3 published run-K cells, BENCH_RUNS=3 per cell. The release
// path is the most expensive one — if it overflows, the workflow must
// trim Runs via FitWithin and log it, never silently truncate the matrix.
func TestNewReleaseConfigFitsBudget(t *testing.T) {
	p := HeadlineWeekly()
	p.Runs = 3 // back-to-back release N, #167
	if got := p.WallClock(); got >= Budget {
		t.Fatalf("release headline (runs=3) wall-clock %v >= %v budget", got, Budget)
	}
}

// TestFitWithinChoosesLargestFittingRuns proves FitWithin returns the
// largest Runs that fits and never silently exceeds the budget.
func TestFitWithinChoosesLargestFittingRuns(t *testing.T) {
	p := HeadlineWeekly()
	p.Runs = 9 // ask for far more than fits
	runs, log, ok := p.FitWithin(Budget, 1)
	if !ok {
		t.Fatalf("expected some Runs to fit; log:\n%s", log)
	}
	if runs < 1 {
		t.Fatalf("chosen runs must be >= 1, got %d", runs)
	}
	trial := p
	trial.Runs = runs
	if trial.WallClock() >= Budget {
		t.Fatalf("FitWithin chose runs=%d whose wall-clock %v exceeds budget %v",
			runs, trial.WallClock(), Budget)
	}
	// One more run must not fit (it returned the largest fitting Runs).
	if runs < p.Runs {
		bigger := p
		bigger.Runs = runs + 1
		if bigger.WallClock() < Budget {
			t.Fatalf("FitWithin chose runs=%d but runs=%d also fits (%v < %v)",
				runs, runs+1, bigger.WallClock(), Budget)
		}
	}
}

// TestFitWithinFailsLoudlyWhenNothingFits proves the no-silent-truncation
// rule: when even minRuns overflows, ok is false and the log explains it.
func TestFitWithinFailsLoudlyWhenNothingFits(t *testing.T) {
	p := HeadlineWeekly()
	p.Duration = 10 * time.Hour // absurd window: nothing fits, even runs=1
	runs, log, ok := p.FitWithin(Budget, 1)
	if ok {
		t.Fatalf("expected no fit for a 10h-per-cell window, got runs=%d", runs)
	}
	if runs != 0 {
		t.Fatalf("no-fit must return runs=0, got %d", runs)
	}
	if !strings.Contains(log, "NO FIT") {
		t.Fatalf("no-fit log must call out the failure; got:\n%s", log)
	}
}

// TestArchParallelHalvesWallClock proves the #168 win is modeled: with
// both arches and ArchParallel, wall-clock is half the serial cost.
func TestArchParallelHalvesWallClock(t *testing.T) {
	serial := HeadlineWeekly()
	serial.ArchParallel = false
	parallel := HeadlineWeekly()
	parallel.ArchParallel = true
	if parallel.WallClock() != serial.WallClock()/2 {
		t.Fatalf("ArchParallel wall-clock %v != serial/2 %v",
			parallel.WallClock(), serial.WallClock()/2)
	}
}

// TestForProfileResolves checks the BENCH_PROFILE resolution: known names
// map to their config, unknown/empty fall back to the weekly headline.
func TestForProfileResolves(t *testing.T) {
	if ForProfile("headline").Name != "headline" {
		t.Errorf("ForProfile(headline) should resolve headline")
	}
	if ForProfile("full").Name != "full" {
		t.Errorf("ForProfile(full) should resolve full")
	}
	if ForProfile("").Name != "headline" {
		t.Errorf("ForProfile(empty) should fall back to headline")
	}
	if ForProfile("bogus").Name != "headline" {
		t.Errorf("ForProfile(unknown) should fall back to headline")
	}
}

// TestGlobsAreNonEmpty guards the BENCH_CELLS wiring: a profile with no
// globs would silently bench nothing.
func TestGlobsAreNonEmpty(t *testing.T) {
	h := HeadlineWeekly()
	if len(h.Globs) == 0 {
		t.Errorf("headline Globs must be non-empty")
	}
	if len(h.RatedGlobs) == 0 {
		t.Errorf("headline RatedGlobs must be non-empty")
	}
	if RatedGlob(h) == "" || CellsGlob(h) == "" {
		t.Errorf("RatedGlob/CellsGlob must join to non-empty strings")
	}
	if !strings.Contains(CellsGlob(h), "/") {
		t.Errorf("CellsGlob must carry <scenario>/<server> pairs, got %q", CellsGlob(h))
	}
}
