//go:build mage

package main

import (
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/servers"
)

// benchEnvKeys are the BENCH_* variables BenchTier's env resolution reads or
// writes. Each test starts with all of them unset.
var benchEnvKeys = []string{
	"BENCH_CELLS", "BENCH_DURATION", "BENCH_WARMUP",
	"BENCH_RATED", "BENCH_RATED_CELLS", "BENCH_RATED_DURATION", "BENCH_SKIP_RATED",
}

func unsetBenchEnv(t *testing.T) {
	t.Helper()
	for _, k := range benchEnvKeys {
		t.Setenv(k, "") // restores the caller's value when the test ends
		if err := os.Unsetenv(k); err != nil {
			t.Fatal(err)
		}
	}
}

// TestBenchTierForwardsTheModelsRatedScope is probatorium#418: a rated
// BenchTier run must hand the runner the model's rated cells and the
// profile's rated pass window. It set neither: BENCH_RATED_CELLS was never
// set, so every clean cell was rated, and BENCH_RATED_DURATION was never set,
// so mage Bench forwarded its 30 s default (headline plans 20 s). The
// saturation grid stays the whole profile glob.
func TestBenchTierForwardsTheModelsRatedScope(t *testing.T) {
	for _, p := range []budget.Profile{budget.HeadlineWeekly(), budget.Full()} {
		t.Run(p.Name, func(t *testing.T) {
			unsetBenchEnv(t)
			benchTierEnv(p)

			if got := os.Getenv("BENCH_CELLS"); got != budget.CellsGlob(p) {
				t.Errorf("BENCH_CELLS = %q, want the profile's saturation glob %q", got, budget.CellsGlob(p))
			}
			r, err := resolveRatedBench()
			if err != nil {
				t.Fatalf("resolveRatedBench: %v", err)
			}
			if !r.On {
				t.Fatalf("rated profile %s: rated mode is off", p.Name)
			}
			if want := int(p.RatedDuration / time.Second); r.DurationSec != want {
				t.Errorf("rated passes measure %d s, the %s profile plans %d s (BENCH_RATED_DURATION=%q)",
					r.DurationSec, p.Name, want, os.Getenv("BENCH_RATED_DURATION"))
			}
			ev := r.extraVars()
			for _, want := range []string{
				"bench_rated=1",
				"bench_rated_duration_seconds=" + strconv.Itoa(int(p.RatedDuration/time.Second)),
				"bench_rated_cells=" + budget.RatedGlob(p),
			} {
				if !slices.Contains(ev, want) {
					t.Errorf("ansible extra-vars %q lack %q", ev, want)
				}
			}
		})
	}
}

// TestBenchTierRatedScopeEdges: presets win (a scoped run may widen, narrow
// or shorten the rated sweep), a run with the rated sweep off sets no rated
// scope and forwards no rated extra-vars, and a hand-run `mage Bench
// BENCH_RATED=1` without BENCH_RATED_CELLS rates the model's cells, not every
// cell.
func TestBenchTierRatedScopeEdges(t *testing.T) {
	t.Run("presets win", func(t *testing.T) {
		unsetBenchEnv(t)
		t.Setenv("BENCH_RATED_CELLS", "get-json/celeris-*")
		t.Setenv("BENCH_RATED_DURATION", "5s")
		benchTierEnv(budget.Full())
		r, err := resolveRatedBench()
		if err != nil {
			t.Fatal(err)
		}
		if r.Cells != "get-json/celeris-*" || r.DurationSec != 5 {
			t.Errorf("presets lost: cells %q duration %d s, want get-json/celeris-* and 5 s", r.Cells, r.DurationSec)
		}
	})
	for name, set := range map[string]func(t *testing.T) budget.Profile{
		"fast profile":       func(t *testing.T) budget.Profile { return budget.Fast() },
		"BENCH_SKIP_RATED=1": func(t *testing.T) budget.Profile { t.Setenv("BENCH_SKIP_RATED", "1"); return budget.Full() },
	} {
		t.Run(name, func(t *testing.T) {
			unsetBenchEnv(t)
			benchTierEnv(set(t))
			if v, ok := os.LookupEnv("BENCH_RATED_CELLS"); ok {
				t.Errorf("rated sweep off, but BENCH_RATED_CELLS=%q", v)
			}
			r, err := resolveRatedBench()
			if err != nil {
				t.Fatal(err)
			}
			if r.On || len(r.extraVars()) != 0 {
				t.Errorf("rated sweep off, but rated=%v extra-vars %q", r.On, r.extraVars())
			}
		})
	}
	t.Run("hand-run Bench", func(t *testing.T) {
		unsetBenchEnv(t)
		t.Setenv("BENCH_RATED", "1")
		r, err := resolveRatedBench()
		if err != nil {
			t.Fatal(err)
		}
		if r.Cells != budget.RatedCellsGlob() {
			t.Errorf("BENCH_RATED=1 without BENCH_RATED_CELLS rates %q, want the model's %q", r.Cells, budget.RatedCellsGlob())
		}
	})
}

// TestScopedProjectionPricesTheRatedScope is the gate of the workflow's
// `rated_cells` input: profile=full target=both (policy all) with
// rated_cells='*' rates every cell, about twice the model's 401, and used to
// pass FitWithin on the profile's pins at 101.9 h against the 108 h budget.
// The realized counts price it (the counter is stubbed here; the real dry run is
// TestRealizedCountsMatchThePins).
func TestScopedProjectionPricesTheRatedScope(t *testing.T) {
	stub := func(cells, rated string) (int, int, error) {
		switch {
		case rated == "*" && cells == budget.CellsGlob(budget.Full()):
			return budget.FullRealizedCells, budget.FullRealizedCells, nil // every cell rated
		case cells == "post-4k/*" && rated == "*":
			return 52, 52, nil
		}
		return 0, 0, fmt.Errorf("unexpected scope %q %q", cells, rated)
	}
	old := realizedCounts
	realizedCounts = stub
	defer func() { realizedCounts = old }()

	both := func() budget.Profile {
		p := budget.Full()
		p.Runs, p.Arches, p.ArchExclusion = 1, 2, "all"
		return p
	}
	const bud = 108 * time.Hour // the workflow's full / all budget

	// no preset: the pins, which fit.
	t.Setenv("BENCH_CELLS", "")
	t.Setenv("BENCH_RATED_CELLS", "")
	p, scoped, err := scopedProjection(both())
	if err != nil || scoped {
		t.Fatalf("no preset: scoped=%v err=%v, want the profile unchanged", scoped, err)
	}
	if _, ok := p.FitWithin(bud); !ok {
		t.Fatal("the model's own full / all projection must fit 108h")
	}

	// rated_cells='*' on the whole grid: priced, and refused.
	t.Setenv("BENCH_RATED_CELLS", "*")
	p, scoped, err = scopedProjection(both())
	if err != nil || !scoped || p.RatedCells != budget.FullRealizedCells {
		t.Fatalf("rated_cells=*: scoped=%v rated=%d err=%v, want every cell rated", scoped, p.RatedCells, err)
	}
	t.Logf("full / both / all with rated_cells='*': %s projected (the pins say %s) against the %s budget", p.WallClock().Round(time.Minute), both().WallClock().Round(time.Minute), bud)
	if log, ok := p.FitWithin(bud); ok {
		t.Errorf("rated_cells='*' on full / both / all fits 108h: the gate still ignores the rated scope\n%s", log)
	}

	// a scoped run is priced by its own cells: it fits.
	t.Setenv("BENCH_CELLS", "post-4k/*")
	p, scoped, err = scopedProjection(both())
	if err != nil || !scoped || p.Cells != 52 {
		t.Fatalf("scoped: scoped=%v cells=%d err=%v", scoped, p.Cells, err)
	}
	if log, ok := p.FitWithin(bud); !ok {
		t.Errorf("a 52-cell scope must fit:\n%s", log)
	}

	// a profile without a rated sweep rates nothing whatever the glob.
	fast := budget.Fast()
	fast.Runs, fast.Arches = 1, 2
	if p, _, err := scopedProjection(fast); err != nil || p.RatedCells != 0 {
		t.Errorf("fast with a rated glob: rated=%d err=%v, want 0", p.RatedCells, err)
	}

	// the dry run failing is a refusal, not a silent fall back to the pins.
	realizedCounts = func(string, string) (int, int, error) { return 0, 0, fmt.Errorf("go: no") }
	if _, _, err := scopedProjection(both()); err == nil {
		t.Error("a failing dry run must refuse the projection")
	}
}

func TestParseDryRunSummary(t *testing.T) {
	in := "probatorium-runner: rated mode ON\nprobatorium-runner: dry-run; the rated sweep runs on 401 of the 813 cells (-rated-cells \"x\") once each saturation pass is clean\n"
	cells, rated, err := parseDryRunSummary(in)
	if err != nil || cells != 813 || rated != 401 {
		t.Errorf("got cells=%d rated=%d err=%v", cells, rated, err)
	}
	if _, _, err := parseDryRunSummary("nothing here"); err == nil {
		t.Error("a dry run with no summary must be an error")
	}
}

// TestRealizedCountsMatchThePins runs the real dry run: the counts the gate
// uses for a preset scope agree with the pins the unscoped gate uses, widen
// with rated_cells='*', and follow BENCH_BASELINE (so the scoped projection
// must not add the baseline columns a second time).
func TestRealizedCountsMatchThePins(t *testing.T) {
	if testing.Short() {
		t.Skip("execs go run ./cmd/runner")
	}
	full := budget.Full()
	for _, c := range []struct {
		name, baseline, rated string
		cells, ratedN         int
	}{
		{"pins", "", budget.RatedGlob(full), budget.FullRealizedCells, budget.FullRatedRealizedCells},
		{"every cell rated", "", "*", budget.FullRealizedCells, budget.FullRealizedCells},
		{"one column, every cell rated", "", "*", -1, -1},
		{"pins with the baseline", "1", budget.RatedGlob(full), budget.FullRealizedCells + budget.BaselineRealizedCells, budget.FullRatedRealizedCells + budget.BaselineRatedRealizedCells},
	} {
		t.Setenv(servers.BaselineEnvVar, c.baseline)
		cells := budget.CellsGlob(full)
		if c.cells == -1 { // BENCH_COMPETITORS=celeris-epoll-h1-sync, as scopedProjection narrows it
			cells = narrowCellsToColumns(cells, []string{"celeris-epoll-h1-sync"})
			n, r, err := realizedCounts(cells, c.rated)
			if err != nil || n < 20 || n > 40 || r != n {
				t.Errorf("%s: %q realizes %d cells %d rated (err %v), want one column's 20-40 cells, all rated", c.name, cells, n, r, err)
			}
			continue
		}
		n, r, err := realizedCounts(cells, c.rated)
		if err != nil || n != c.cells || r != c.ratedN {
			t.Errorf("%s: got %d cells %d rated (err %v), want %d / %d", c.name, n, r, err, c.cells, c.ratedN)
		}
	}
}

// TestTierProjectionDoesNotCountTheBaselineTwice: a preset scope's realized
// counts already include the baseline columns (the dry run reads
// BENCH_BASELINE), so only the unscoped projection adds them.
func TestTierProjectionDoesNotCountTheBaselineTwice(t *testing.T) {
	old := realizedCounts
	defer func() { realizedCounts = old }()
	realizedCounts = func(string, string) (int, int, error) { return 100, 50, nil } // as the runner counts them, baseline on
	t.Setenv(servers.BaselineEnvVar, "1")
	t.Setenv("BENCH_CELLS", "post-4k/*")
	t.Setenv("BENCH_RATED_CELLS", "")
	p, err := tierProjection(budget.Full())
	if err != nil || p.Cells != 100 || p.RatedCells != 50 {
		t.Errorf("scoped with the baseline: cells=%d rated=%d err=%v, want the runner's 100 / 50", p.Cells, p.RatedCells, err)
	}
	t.Setenv("BENCH_CELLS", "")
	p, err = tierProjection(budget.Full())
	if want := budget.Full().WithBaseline(); err != nil || p.Cells != want.Cells || p.RatedCells != want.RatedCells {
		t.Errorf("unscoped with the baseline: cells=%d rated=%d err=%v, want %d / %d", p.Cells, p.RatedCells, err, want.Cells, want.RatedCells)
	}
}

// TestScopedProjectionHonoursCompetitors: BENCH_COMPETITORS narrows the columns
// the bench runs, so a rated_cells='*' dispatch of one column is priced as that
// column's cells, not the whole grid's 813 (review round 2).
func TestScopedProjectionHonoursCompetitors(t *testing.T) {
	old := realizedCounts
	defer func() { realizedCounts = old }()
	var gotCells, gotRated string
	realizedCounts = func(c, r string) (int, int, error) { gotCells, gotRated = c, r; return 27, 27, nil }
	t.Setenv("BENCH_CELLS", "")
	t.Setenv("BENCH_RATED_CELLS", "*")
	t.Setenv("BENCH_COMPETITORS", "celeris-epoll-h1-sync")
	p := budget.Full()
	p.Runs, p.Arches, p.ArchExclusion = 1, 2, "all"
	q, scoped, err := scopedProjection(p)
	if err != nil || !scoped || q.Cells != 27 {
		t.Fatalf("scoped=%v cells=%d err=%v", scoped, q.Cells, err)
	}
	if gotCells != "*/celeris-epoll-h1-sync" || gotRated != "*" {
		t.Errorf("the dry run was asked for cells %q rated %q, want the one column", gotCells, gotRated)
	}
	if _, ok := q.FitWithin(108 * time.Hour); !ok {
		t.Error("one column of the full profile must fit 108h")
	}
	// competitors=all (the workflow's default) and an unset profile scope change nothing.
	t.Setenv("BENCH_COMPETITORS", "all")
	t.Setenv("BENCH_RATED_CELLS", "")
	if _, scoped, _ := scopedProjection(p); scoped {
		t.Error("competitors=all with no scope must stay the profile's pins")
	}
	// a competitor list alone (no cells, no rated glob) is a scope too.
	t.Setenv("BENCH_COMPETITORS", "axum")
	if _, scoped, _ := scopedProjection(p); !scoped {
		t.Error("an explicit competitors list must be projected from the dry run")
	}
}

func TestNarrowCellsToColumns(t *testing.T) {
	for _, c := range []struct{ cells, want string }{
		{"*/*", "*/a,*/b"},
		{"*", "*/a,*/b"},
		{"get-*/celeris-*", "get-*/celeris-a"},
		{"post-4k/*,churn-close/b", "post-4k/a,post-4k/b,churn-close/b"},
		{"*/*,!ws-large-echo/*", "*/a,*/b,!ws-large-echo/*"},
		{"post-4k/zzz", "!*/*"},
	} {
		cols := []string{"a", "b"}
		if c.cells == "get-*/celeris-*" {
			cols = []string{"celeris-a", "axum"}
		}
		if got := narrowCellsToColumns(c.cells, cols); got != c.want {
			t.Errorf("narrow(%q, %v) = %q, want %q", c.cells, cols, got, c.want)
		}
	}
}

// TestPublishPolicyGate: a mage BenchTier outside the workflow refuses, before
// the bench, a policy heavy or off run that would publish.
func TestPublishPolicyGate(t *testing.T) {
	t.Setenv("BENCH_PUBLISH_FORCE", "")
	for _, c := range []struct {
		policy string
		ok     bool
	}{{"all", true}, {"", true}, {"heavy", false}, {"off", false}} {
		if err := publishPolicyGate(c.policy); (err == nil) != c.ok {
			t.Errorf("policy %q: err %v, want ok=%v", c.policy, err, c.ok)
		}
	}
	t.Setenv("BENCH_PUBLISH_FORCE", "1")
	if err := publishPolicyGate("heavy"); err != nil {
		t.Errorf("BENCH_PUBLISH_FORCE=1 must override: %v", err)
	}
	// wired before publishPreflight, and only when the run publishes.
	b, _ := os.ReadFile("mage_tier.go")
	src := string(b)
	i, j := strings.Index(src, "publishPolicyGate(archPolicy)"), strings.Index(src, "publishPreflight(); err")
	if i < 0 || j < 0 || i > j || !strings.Contains(src[:i], "if !skipPublish {") {
		t.Error("BenchTier must call publishPolicyGate(archPolicy) under !skipPublish, before publishPreflight")
	}
}

// TestScopedLogLineSaysWhatTheProjectionCounted: the BENCH_CELLS preset log no
// longer claims the profile's pins were counted or that the run is strictly smaller.
func TestScopedLogLineSaysWhatTheProjectionCounted(t *testing.T) {
	b, _ := os.ReadFile("mage_tier.go")
	src := string(b)
	if strings.Contains(src, "strictly smaller") || strings.Contains(src, "projection counted the profile's") {
		t.Error("the BENCH_CELLS preset log line still describes the old projection")
	}
	if !strings.Contains(src, "projected from the scope's realized %d cells / %d rated") {
		t.Error("the BENCH_CELLS preset log line must say the budget was projected from the scope's realized counts")
	}
	a, _ := os.ReadFile("mage_bench_archexcl.go")
	if strings.Contains(string(a), "else heavy") {
		t.Error("BenchArchOverlap's doc comment still says the policy falls back to heavy")
	}
}
