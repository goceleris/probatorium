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

// TestProfilesAreSinglePass pins the single-pass invariant: the bench
// ALWAYS runs exactly one pass. The back-to-back / multi-run machinery was
// removed (if more passes are wanted, more benchmarks are scheduled), so
// every curated profile MUST carry Runs=1. A profile that ships Runs>1
// would silently re-introduce a multi-pass run.
func TestProfilesAreSinglePass(t *testing.T) {
	for _, p := range []Profile{Fast(), HeadlineWeekly(), Full()} {
		if p.Runs != 1 {
			t.Errorf("profile %q must be single-pass (Runs=1), got Runs=%d", p.Name, p.Runs)
		}
	}
}

// TestFastFitsWithin24h pins the routine/weekly invariant: the default
// "fast" profile (full grid, saturation-only, 35s/10s) MUST fit the 24h
// budget. If the registry grows the grid past ~1390 cells this fails loudly
// so we shorten the window (or trim coverage) deliberately rather than
// silently overrunning the weekly cluster slot.
func TestFastFitsWithin24h(t *testing.T) {
	p := Fast()
	log, ok := p.FitWithin(Budget)
	if !ok {
		t.Fatalf("fast profile must fit the %v budget; log:\n%s", Budget, log)
	}
	if p.Rated() != 0 {
		t.Errorf("fast profile must be saturation-only (Rated()=0), got %v", p.Rated())
	}
}

// TestFitWithinAcceptsSinglePassThatFits proves FitWithin returns ok for
// the single-pass config when it fits, and reports the headroom.
func TestFitWithinAcceptsSinglePassThatFits(t *testing.T) {
	p := HeadlineWeekly()
	log, ok := p.FitWithin(Budget)
	if !ok {
		t.Fatalf("single-pass headline must fit the %v budget; log:\n%s", Budget, log)
	}
	if p.WallClock() >= Budget {
		t.Fatalf("FitWithin returned ok but wall-clock %v exceeds budget %v",
			p.WallClock(), Budget)
	}
	if !strings.Contains(log, "fits") {
		t.Fatalf("fit log must call out that the config fits; got:\n%s", log)
	}
}

// TestFitWithinFailsLoudlyWhenSinglePassOverflows proves the
// no-silent-truncation rule: there is no Runs dimension left to trim, so
// when the single-pass config overflows the budget, ok is false and the
// log explains it. The caller MUST fail loudly rather than shrink the
// matrix.
func TestFitWithinFailsLoudlyWhenSinglePassOverflows(t *testing.T) {
	p := HeadlineWeekly()
	p.Duration = 10 * time.Hour // absurd window: a single pass cannot fit
	log, ok := p.FitWithin(Budget)
	if ok {
		t.Fatalf("expected no fit for a 10h-per-cell window; log:\n%s", log)
	}
	if !strings.Contains(log, "NO FIT") {
		t.Fatalf("no-fit log must call out the failure; got:\n%s", log)
	}
}

// TestArchParallelHalvesWallClock proves the #168 win is modeled: with
// both arches and ArchParallel, wall-clock is half the serial cost. The
// halving only applies at Arches==2, so the test forces both arches
// explicitly — the weekly/full profiles ship Arches:1 today (amd64-only),
// which would otherwise make the toggle a no-op.
func TestArchParallelHalvesWallClock(t *testing.T) {
	serial := HeadlineWeekly()
	serial.Arches = 2
	serial.ArchParallel = false
	parallel := HeadlineWeekly()
	parallel.Arches = 2
	parallel.ArchParallel = true
	if parallel.WallClock() != serial.WallClock()/2 {
		t.Fatalf("ArchParallel wall-clock %v != serial/2 %v",
			parallel.WallClock(), serial.WallClock()/2)
	}
}

// TestForProfileResolves checks the BENCH_PROFILE resolution: known
// names map to their config, unknown/empty fall back to "full". Both
// "headline" and "full" cover the same full grid (every server × every
// scenario, capability-gated); they differ only by the per-cell window
// (headline's shorter window fits 24h, full's longer window is the
// exhaustive sweep) — see ForProfile's docstring.
func TestForProfileResolves(t *testing.T) {
	if ForProfile("headline").Name != "headline" {
		t.Errorf("ForProfile(headline) should resolve headline")
	}
	if ForProfile("full").Name != "full" {
		t.Errorf("ForProfile(full) should resolve full")
	}
	if ForProfile("fast").Name != "fast" {
		t.Errorf("ForProfile(fast) should resolve fast")
	}
	if ForProfile("").Name != "fast" {
		t.Errorf("ForProfile(empty) should fall back to fast (the full-grid, <24h saturation default), got %q (the fallback must still be a full-coverage */* grid, never a curated subset)",
			ForProfile("").Name)
	}
	if ForProfile("bogus").Name != "fast" {
		t.Errorf("ForProfile(unknown) should fall back to fast, got %q (an unknown name must not silently downgrade coverage)", ForProfile("bogus").Name)
	}
}

// TestForProfileDefaultHasFullCoverage pins the "no missing tests"
// invariant: the default (no env / empty string) must yield a profile
// whose Globs cover every registered server. Otherwise a weekly run
// would silently drop servers from the publish, and we'd publish a
// headline-scoped report without telling the user.
func TestForProfileDefaultHasFullCoverage(t *testing.T) {
	def := ForProfile("")
	if def.Name != "fast" {
		t.Fatalf("ForProfile(\"\").Name: want %q, got %q (the default must be the full-coverage saturation matrix)",
			"fast", def.Name)
	}
	if len(def.Globs) == 0 || def.Globs[0] != "*/*" {
		t.Fatalf("default profile must cover the full grid (Globs '*/*'), got %v", def.Globs)
	}
	if def.Cells < 400 {
		t.Errorf("default profile Cells: want >= 400 (the full matrix is ~1111 capability-gated), got %d. "+
			"A value this low means the default was silently scoped down to a curated subset.",
			def.Cells)
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

// TestColumnWallClock pins the per-column projection the ansible hang
// guard is sized from. The "v3.8 rated column" case uses the REAL run
// config (33 capability-gated scenarios on celeris-epoll-h1-sync,
// 90s/20s saturation, 4 x 30s rated passes each re-running the full
// warmup): the projection must exceed the old 8540s guard
// (28 x (20+90) + 10, doubled, +120s) that SIGTERMed every healthy
// rated column 28 cells into 33.
func TestColumnWallClock(t *testing.T) {
	const v38OldGuard = 8540 * time.Second // (28*(90+20)+10)*2 + 120

	cases := []struct {
		name                      string
		scenarios, ratedPasses    int
		warmup, duration, ratedDu time.Duration
		want                      time.Duration
	}{
		{
			// 33 x (20+90+5+12 + 4*(20+30)) = 33 x 327s
			name: "v3.8 rated column", scenarios: 33, ratedPasses: 4,
			warmup: 20 * time.Second, duration: 90 * time.Second, ratedDu: 30 * time.Second,
			want: 33 * 327 * time.Second,
		},
		{
			// rated off: 33 x (20+90+5+12)
			name: "saturation only", scenarios: 33, ratedPasses: 0,
			warmup: 20 * time.Second, duration: 90 * time.Second, ratedDu: 30 * time.Second,
			want: 33 * 127 * time.Second,
		},
		{
			// empty schedule floors at one scenario so `timeout 0` (=
			// guard disabled) can never be derived from it.
			name: "zero scenarios floors to one", scenarios: 0, ratedPasses: 0,
			warmup: 5 * time.Second, duration: 30 * time.Second, ratedDu: 30 * time.Second,
			want: 52 * time.Second,
		},
	}
	for _, tc := range cases {
		got := ColumnWallClock(tc.scenarios, tc.ratedPasses, tc.warmup, tc.duration, tc.ratedDu)
		if got != tc.want {
			t.Errorf("%s: ColumnWallClock = %v, want %v", tc.name, got, tc.want)
		}
	}

	rated := ColumnWallClock(33, 4, 20*time.Second, 90*time.Second, 30*time.Second)
	if rated <= v38OldGuard {
		t.Errorf("rated column projection %v must exceed the v3.8 guard %v that killed the run; "+
			"a smaller projection means the data-loss bug is back", rated, v38OldGuard)
	}
}
