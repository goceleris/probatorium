package properties

import (
	"math"
	"sort"
)

// heapScatterAlpha is the one-sided significance level of the slope's
// test, applied to every judged window (I-MEM-1 only).
//
// The rise floor's other terms measure size: a budgeted 10 minutes of
// leak, a fraction of the level, the within-bucket sawtooth. None of
// them measures how far the troughs themselves move from one bucket to
// the next, and in a cell whose troughs scatter by 0.5-1.4 MB (the
// observability cells, which carry no ws_echo walker for the quiet gap
// of #483 to silence) a 7-trough window rises by 1.6 MB about as often
// as by chance as the floor allows. Soak 38441116109 failed
// ValidateGate on exactly that: one observability/std/arm64 window with
// a whole-cell slope of 0.42 KB/s.
//
// The scatter term asks that the fitted rise also be a clear signal
// against the cell's own trough scatter: the slope's test statistic,
// b / (sigmaD / sqrt(Sxx)), must exceed the critical value heapScatterCrit
// gives for the window's trough count, calibrated at heapScatterAlpha.
// It is a floor inside the existing max(), so it can only remove
// violations.
//
// What it costs, so nobody reads it as free (45 minute cells of the weekend
// soaks, against the rule without the term; probatorium PR, lane IMEM1):
//   - In the noisy cells (trough sd 0.34-1.4 MB, the observability cells)
//     a 3 KB/s leak from the end of warm-up is called about 11 minutes
//     later (median 17.5 -> 28.7 min), a 2 KB/s leak in 24 of 25 cells
//     (25 before), a 1 KB/s leak in 9 (24 before), and a leak that starts
//     at minute 25 in fewer of them (3 KB/s: 12 of 21 that were called
//     before).
//   - In the quiet cells a 3 KB/s leak is called about 3.7 minutes later
//     (median 17.5 -> 21.2 min), because windows of 5-8 troughs need a
//     leak large against the scatter (heapScatterCrit is 26 at 5 troughs,
//     11 at 6, 7.7 at 7; the estimator is robust and so inefficient).
//   - A leak that climbs in steps every 15 or 30 minutes is called as
//     early as without the term on the quiet cells.
//
// The scatter estimate sigmaD comes from successive trough differences,
// not from the residuals about the fitted line. A residual grows with
// the span whenever the series bends, so a leak that starts late, or
// climbs in steps, was counted as scatter and held back (the review of
// the first version of this term). A difference sees a bend only at the
// bend, and a step as one outlying difference, which the trimmed mean of
// squares drops.
//
// alpha is the level, not a fitted constant: heapScatterCrit holds the
// 1 - alpha quantile of the statistic for iid normal troughs at equal
// spacing, by simulation (4,000,000 draws per trough count, seeds
// 1000+n; the any-look false-alarm rate over windows of 4 to 15 troughs
// is 0.75 % per cell, simulated). Real troughs are not exactly iid
// normal; the stationary-bootstrap null in the PR that introduced this
// term measures the rate on the soaks' own series.
const heapScatterAlpha = 0.001

// scatterTrim is the fraction of the squared centred differences kept
// by the scale estimate. 0.75 keeps one outlying difference in 4
// (breakdown point 25 %): more than the one or two steps a 45 minute
// cell holds, less than a staircase with a step every 600 s.
const scatterTrim = 0.75

// heapScatterCrit[n] is the critical value of the slope statistic for a
// window of n troughs, n = 4..30 (the window holds at most 24). Index 0-3
// are unused. See heapScatterAlpha for how it was obtained.
var heapScatterCrit = [...]float64{
	4: 22.342, 5: 26.005, 6: 11.139, 7: 7.737, 8: 6.220, 9: 6.547,
	10: 5.663, 11: 5.171, 12: 4.807, 13: 4.893, 14: 4.614, 15: 4.451,
	16: 4.279, 17: 4.336, 18: 4.205, 19: 4.101, 20: 3.999, 21: 4.036,
	22: 3.943, 23: 3.883, 24: 3.822, 25: 3.857, 26: 3.812, 27: 3.759,
	28: 3.719, 29: 3.737, 30: 3.699,
}

// scatterFloor is the smallest fitted rise that is significant against
// the cell's trough scatter: c(n) x sigmaD / sqrt(Sxx) x span, where
// sigmaD is trimmedDiffScale of the troughs. With fewer than 4 points,
// or no spread in x, the scatter cannot be estimated and the floor is
// +Inf (no verdict).
func scatterFloor(pts []point) float64 {
	n := len(pts)
	if n < 4 {
		return math.Inf(1)
	}
	var sx float64
	for _, p := range pts {
		sx += p.x
	}
	mx := sx / float64(n)
	var sxx float64
	for _, p := range pts {
		dx := p.x - mx
		sxx += dx * dx
	}
	if sxx == 0 {
		return math.Inf(1)
	}
	c := heapScatterCrit[len(heapScatterCrit)-1]
	if n < len(heapScatterCrit) {
		c = heapScatterCrit[n]
	}
	return c * trimmedDiffScale(pts) / math.Sqrt(sxx) * (pts[n-1].x - pts[0].x)
}

// trimmedDiffScale estimates the sd of independent trough scatter from
// the successive differences of the troughs: the differences are centred
// at their median (a linear trend adds a constant to every one), squared,
// the smallest scatterTrim of them kept, their mean divided by the
// Gaussian consistency factor of that truncation, and the square root
// divided by sqrt(2) (a difference of two independent troughs has twice
// the variance).
func trimmedDiffScale(pts []point) float64 {
	d := make([]float64, 0, len(pts)-1)
	for i := 1; i < len(pts); i++ {
		d = append(d, pts[i].y-pts[i-1].y)
	}
	sorted := append([]float64(nil), d...)
	m := medianOf(sorted)
	sq := make([]float64, len(d))
	for i, v := range d {
		sq[i] = (v - m) * (v - m)
	}
	sort.Float64s(sq)
	k := int(math.Ceil(scatterTrim * float64(len(sq))))
	var s float64
	for _, v := range sq[:k] {
		s += v
	}
	f := 1.0
	if k < len(sq) {
		p := float64(k) / float64(len(sq)) // realised retained fraction
		z := math.Sqrt2 * math.Erfinv(p)
		f = 1 - 2*z*math.Exp(-z*z/2)/math.Sqrt(2*math.Pi)/p
	}
	return math.Sqrt(s/float64(k)/f) / math.Sqrt2
}

// medianOf sorts v in place and returns its median.
func medianOf(v []float64) float64 {
	sort.Float64s(v)
	if n := len(v); n%2 == 1 {
		return v[n/2]
	} else {
		return (v[n/2-1] + v[n/2]) / 2
	}
}
