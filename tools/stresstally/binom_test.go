package main

import (
	"math"
	"testing"
)

// The reference intervals come from scipy (beta.ppf, cross-checked against
// binomtest(...).proportion_ci(method="exact") to 1e-12); the script that
// printed them is kept with the lane's evidence as cp_ref.py.
func TestClopperPearsonMatchesScipy(t *testing.T) {
	for _, c := range []struct {
		k, n   int
		lo, hi float64
	}{
		{0, 1, 0, 0.975},
		{1, 1, 0.025, 1},
		{0, 10, 0, 0.308497107818761},
		{1, 10, 0.00252857854446178, 0.445016117028195},
		{5, 100, 0.0164318791820522, 0.112834911105463},
		{0, 100, 0, 0.0362166926451764},
		{100, 100, 0.963783307354824, 1},
		{2, 7, 0.0366925661760856, 0.709579136262657},
		{1, 2000, 1.26588238685579e-05, 0.00278263983465895},
		{37, 1000, 0.0261827088437373, 0.0506411230599249},
		{999, 1000, 0.994441075720173, 0.999974682512509},
		{3, 50, 0.0125485878353341, 0.165481946603773},
	} {
		lo, hi := clopperPearson(c.k, c.n)
		if !near(lo, c.lo) || !near(hi, c.hi) {
			t.Errorf("clopperPearson(%d, %d) = [%.15g, %.15g], want [%.15g, %.15g]", c.k, c.n, lo, hi, c.lo, c.hi)
		}
	}
}

func TestClopperPearsonHasNoIntervalWithoutRuns(t *testing.T) {
	for _, c := range [][2]int{{0, 0}, {1, 0}, {-1, 5}, {6, 5}} {
		lo, hi := clopperPearson(c[0], c[1])
		if !math.IsNaN(lo) || !math.IsNaN(hi) {
			t.Errorf("clopperPearson(%d, %d) = [%v, %v], want NaN bounds", c[0], c[1], lo, hi)
		}
	}
}

func near(got, want float64) bool {
	if want == 0 {
		return math.Abs(got) < 1e-12
	}
	return math.Abs(got-want) <= 1e-9*math.Abs(want)
}
