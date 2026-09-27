//go:build mage

package main

import (
	"os"
	"slices"
	"strconv"
	"testing"
	"time"

	"github.com/goceleris/probatorium/budget"
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
