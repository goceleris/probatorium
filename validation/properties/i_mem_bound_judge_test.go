package properties

import (
	"fmt"
	"math"
	"strings"
	"testing"
)

// stepTroughHeap is a 1 Hz heap_inuse history whose every 150 s bucket
// after the 5 min warm-up holds one constant value, troughs[k]. The judged
// troughs are then exactly troughs, at x = 150 k s, with no sampling noise.
// The history ends on the last second of the last bucket.
func stepTroughHeap(troughs []float64) []Snapshot {
	return leakHistory(300+150*len(troughs)-1, func(i int) (int64, int64, int64) {
		return 100, int64(troughs[max(i-300, 0)/150]), 0
	})
}

// zigzagTroughs is a 3000 B/s line from 1 MiB, with residuals +e, -e, +e, ...
func zigzagTroughs(n int, e float64) []float64 {
	y := make([]float64, n)
	for k := range y {
		r := e
		if k%2 == 1 {
			r = -e
		}
		y[k] = 1<<20 + 3000*150*float64(k) + r
	}
	return y
}

// olsOfTroughs is the test's own arithmetic on trough values at x = 150 k s:
// the OLS slope b, its lower bound b - t x SE with SE from the residuals, the
// rise the verdict uses (the smaller of fitted and trough-to-trough) and the
// mean level.
func olsOfTroughs(y []float64, t float64) (b, lower, rise, level float64) {
	n := float64(len(y))
	var mx float64
	for k, v := range y {
		mx += 150 * float64(k) / n
		level += v / n
	}
	var sxx, sxy, rss float64
	for k, v := range y {
		dx := 150*float64(k) - mx
		sxx += dx * dx
		sxy += dx * (v - level)
	}
	b = sxy / sxx
	for k, v := range y {
		r := v - (level + b*(150*float64(k)-mx))
		rss += r * r
	}
	se := math.Sqrt(rss / (n - 2) / sxx)
	return b, b - t*se, math.Min(b*150*(n-1), y[len(y)-1]-y[0]), level
}

// I-MEM-1's bound is compared with the budget, not with zero, and it is
// computed on the same troughs the slope is fitted to. Both trough sets below
// fire the verdict without the bound: the slope is over the budget and the
// rise clears the floor. The one whose lower bound lies between 0 and the
// budget must pass. The one whose lower bound clears the budget must fail,
// and its message must carry that bound, from all 10 troughs (8 dof).
func TestIMEM1_judgeBoundIsAgainstTheBudget(t *testing.T) {
	const t8 = 4.501 // slopeBoundT[8], checked by TestSlopeBoundT_isTheOneSided999Quantile
	budget := heapSlopeMaxBytesPerSec
	floor := budget * slopeMinSpan.Seconds()
	for _, c := range []struct {
		name    string
		e       float64
		lo, hi  float64 // where the lower bound must lie, in budgets
		violate bool
	}{
		{"lower bound between 0 and the budget", 600_000, 0.2, 0.8, false},
		{"lower bound over the budget", 420_000, 1.15, 1.5, true},
	} {
		y := zigzagTroughs(10, c.e)
		b, lower, rise, level := olsOfTroughs(y, t8)
		// The fixture is what its name says, and the verdict without the
		// bound fires on it: otherwise a pass would prove nothing.
		if b <= budget || rise < floor || heapRiseRelFloor*level >= floor {
			t.Fatalf("%s: slope %.0f B/s, rise %.0f B, 3%% of level %.0f B: the old verdict would not fire (budget %.0f, floor %.0f)", c.name, b, rise, heapRiseRelFloor*level, budget, floor)
		}
		if lower < c.lo*budget || lower > c.hi*budget {
			t.Fatalf("%s: the fixture's lower bound is %.0f B/s, want %.2f-%.2f budgets", c.name, lower, c.lo, c.hi)
		}
		h := stepTroughHeap(y)
		ok, msg := IMEM1.Predicate(&h[len(h)-1], slopeCtx(h))
		if !c.violate {
			if !ok {
				t.Errorf("%s (%.0f B/s, slope %.0f B/s): I-MEM-1 fails, but a lower bound under the budget must not: %s", c.name, lower, b, msg)
			}
			continue
		}
		if ok {
			t.Errorf("%s (%.0f B/s, slope %.0f B/s): I-MEM-1 passes, but a lower bound over the budget must fail it", c.name, lower, b)
			continue
		}
		for _, want := range []string{fmt.Sprintf("lower bound %s/s", fmtBytes(lower)), "(t, 8 dof)", "(10 troughs)"} {
			if !strings.Contains(msg, want) {
				t.Errorf("%s: the message must carry %q, the bound from all 10 troughs: %s", c.name, want, msg)
			}
		}
	}
}
