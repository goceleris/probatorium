package main

import (
	"os"
	"regexp"
	"testing"

	"github.com/goceleris/probatorium/report"
)

// The race tier's ws_echo tolerance (probatorium#481) is explicit: the
// validator takes -race-tier, and every other tier runs without it.
func TestWSEchoSlowToleranceIsExplicitAndRaceTierOnly(t *testing.T) {
	if got := wsEchoSlowTolerance(false); got != 0 {
		t.Errorf("without -race-tier the tolerance is %d, want 0: nightly and soak keep failing on a ws_echo timeout", got)
	}
	if got := wsEchoSlowTolerance(true); got != 3 || report.WSEchoSlowTolerance != 3 {
		t.Errorf("-race-tier tolerance is %d (report.WSEchoSlowTolerance=%d), want N=3 in both", got, report.WSEchoSlowTolerance)
	}
}

func TestRaceTierFlagReachesBothCellConfigs(t *testing.T) {
	for _, f := range []string{"main.go", "matrix.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if !regexp.MustCompile(`WSEchoSlowTolerance:\s+wsEchoSlowTolerance\(cfg\.RaceTier\)`).Match(b) {
			t.Errorf("%s builds a validation.Config without WSEchoSlowTolerance", f)
		}
	}
	var c Config
	if c.RaceTier {
		t.Error("RaceTier must default to off")
	}
}
