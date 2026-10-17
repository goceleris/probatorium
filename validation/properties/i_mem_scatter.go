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
// soaks: 25 noisy observability cells, trough sd 0.34-1.4 MB, and 162 quiet
// cells; detected/sustained counts against the rule without the term; the
// case definitions and all tables are in probatorium#520):
//   - Noisy cells, straight leak from the end of warm-up: 1 KB/s is called
//     in 9 of 25 (24 before), 2 KB/s in 24 (25), 3 KB/s about 11 minutes
//     later (median 17.5 -> 28.7 min).
//   - Noisy cells, a leak that starts late: from minute 20, 3 KB/s 25/25 ->
//     22/19; from minute 25, 3 KB/s 21/10 -> 12/3, 4 KB/s 25/22 -> 21/10,
//     6 KB/s 25/25 -> 24/21.
//   - Noisy cells, a stepped leak (3 KB/s on average): 11 minutes later
//     with a step every 15 minutes, sustained 21 -> 9 with a step every 30.
//   - Quiet cells, straight leak: 3 KB/s about 3.7 minutes later at the
//     median (6.2 at most), 1 KB/s in 146 of 162 (150 before); late-start
//     and stepped leaks up to 2.5-5 minutes later.
//   - Quiet cells, a leak that STOPS: starting at the end of warm-up and
//     plateauing after 7.5 minutes, 3 KB/s is lost in 90 cells (154/153 ->
//     64/18), 4 KB/s 162/156 -> 125/75, 6 KB/s 162/162 -> 156/153; 3 KB/s
//     for 10 minutes 162/162 -> 157/149. An instant 2 MB step is still
//     called in 156 cells, so the estimator is the cause: after the leak
//     stops the shifted differences sit in windows of 6-8 troughs where the
//     75 % trim drops only one, and the scatter estimate inflates. The
//     other floors do not catch it.
//   - A single low trough outlier (2-4 MB at minute 10-15) delays a 3-5
//     KB/s call by a median of 7.5-11.2 minutes (max 11.2-13.7) on quiet
//     cells; a single 1 MB step is called in 139 of 150.
//   - Quiet cells with 1-2 MB of added trough scatter: 3 KB/s is lost in
//     19-123 of 162, a 4 MB sawtooth loses 161; with no leak the rule
//     without the term trips falsely in 59 of those 162 and this one in 0.
//   - Short cells: with the load phase cut to 20 minutes, quiet cells lose
//     99 of 162 at 3 KB/s, 75 at 5, 32 at 10 (noisy 22-25 of 25); at 25
//     minutes none. The production 45 minute cells are unaffected.
//   - Windows of 5-8 troughs judge only a leak that is large against the
//     scatter (heapScatterCrit is 26 at 5 troughs, 11 at 6, 7.7 at 7).
//
// Nothing the floor absolves is hidden: IMEM1.Shadow runs the same judge with
// the floor switched off (legacyHeapSlopeSpec) beside the gated rule, the
// evaluator records it as an informational twin that is never gated, and
// ValidateGate prints every cell where it tripped. The maintainer accepted
// the costs above on 2026-10-17 after five pre-registered attempts (the
// residual scatter, the successive differences, the half-bucket pairs, a
// noisy-cell switch; probatorium#520 says why the last two did not work).
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
// 1000+n, testdata/scatter_calib/main.go reproduces the table; the any-look
// false-alarm rate over windows of 4 to 15 troughs is 0.75 % per cell). Real troughs are not exactly iid
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
