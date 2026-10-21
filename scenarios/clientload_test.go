package scenarios

import (
	"slices"
	"testing"
)

// TestEveryScenarioHasAClientLoadClass is the guard that keeps a new scenario
// from running unprotected on a BENCH_TARGET=both run (probatorium#442): the
// table must name every registered scenario and nothing else.
func TestEveryScenarioHasAClientLoadClass(t *testing.T) {
	var reg []string
	for _, s := range Registry() {
		reg = append(reg, s.Name())
	}
	got := ClassifiedScenarios()
	for _, n := range reg {
		if !slices.Contains(got, n) {
			t.Errorf("scenario %q is registered but has no entry in clientLoadTable (classify it heavy or light)", n)
		}
	}
	for _, n := range got {
		if !slices.Contains(reg, n) {
			t.Errorf("clientLoadTable names %q, which is not a registered scenario", n)
		}
	}
}

func TestClientLoadOfUnknownIsHeavy(t *testing.T) {
	if got := ClientLoadOf("no-such-row"); got != LoadHeavy {
		t.Fatalf("unclassified scenario = %q, want heavy", got)
	}
	if got := ClientLoadOf("driver-pg-read"); got != LoadLight {
		t.Fatalf("driver-pg-read = %q, want light", got)
	}
	if got := ClientLoadOf("ws-large-echo"); got != LoadHeavy {
		t.Fatalf("ws-large-echo = %q, want heavy", got)
	}
}

// TestBandwidthRowsAreHeavy pins the rows #442 measured as contaminated: they
// can never be reclassified light without this test (and the issue) changing.
func TestBandwidthRowsAreHeavy(t *testing.T) {
	for _, n := range []string{"ws-large-echo", "post-4k", "post-4k-h2", "get-json-h2", "get-simple-1024c", "get-json"} {
		if ClientLoadOf(n) != LoadHeavy {
			t.Errorf("%s must stay heavy (probatorium#442)", n)
		}
	}
}
