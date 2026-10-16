package properties

import "math"

// heapScatterAlpha is the one-sided significance level of the slope's
// t-test, applied to every judged window (I-MEM-1 only).
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
// against the window's own residual scatter: the slope's t-statistic
// must exceed the one-sided Student-t quantile at heapScatterAlpha with
// n-2 degrees of freedom. It is a floor inside the existing max(), so
// it can only remove violations, and where the troughs scatter little
// it sits below the other terms and changes nothing.
//
// The level is chosen, not fitted to a soak. A cell is judged on up to
// a dozen distinct trough sets (4 to 15 troughs over a 45 min cell),
// each a nested superset of the one before, with 2 to 13 degrees of
// freedom. For troughs that are independent and normal, per-window
// levels of 0.01 / 0.005 / 0.002 / 0.001 give a false-alarm rate per
// cell of 6.3 % / 3.5 % / 1.6 % / 0.66 % (simulated, 30000 cells);
// 0.001 is the largest of those that keeps it at or under 1 %, which
// is the target probatorium#466 set for the oracle. Real troughs are
// not exactly independent and normal: the stationary block-permutation
// null in the PR that introduced this term measures the rate on the
// soaks' own series.
//
// The t quantile, not a fixed multiple of the standard error, because
// the earliest judged windows hold 4 or 5 troughs (2 or 3 degrees of
// freedom), where the residual scatter itself is a poor estimate and a
// fixed 3 sigma lets a chance-quiet window through.
const heapScatterAlpha = 0.001

// scatterFloor is the smallest fitted rise that is significant, at
// alpha, against the residual scatter of pts about their least-squares
// line: t(1-alpha, n-2) x SE(slope) x span, where
// SE(slope) = sqrt(RSS / (n-2) / Sxx). With fewer than 3 points, or no
// spread in x, the scatter cannot be estimated and the floor is +Inf
// (no verdict).
func scatterFloor(pts []point, alpha float64) float64 {
	n := len(pts)
	if n < 3 {
		return math.Inf(1)
	}
	var sx, sy float64
	for _, p := range pts {
		sx += p.x
		sy += p.y
	}
	fn := float64(n)
	mx, my := sx/fn, sy/fn
	var sxx, sxy float64
	for _, p := range pts {
		dx := p.x - mx
		sxx += dx * dx
		sxy += dx * (p.y - my)
	}
	if sxx == 0 {
		return math.Inf(1)
	}
	b := sxy / sxx
	var rss float64
	for _, p := range pts {
		r := p.y - my - b*(p.x-mx)
		rss += r * r
	}
	se := math.Sqrt(rss / float64(n-2) / sxx)
	span := pts[n-1].x - pts[0].x
	return studentTQuantile(1-alpha, n-2) * se * span
}

// studentTQuantile returns the p-quantile (0.5 < p < 1) of Student's t
// distribution with dof degrees of freedom. Values for dof up to
// tQuantileCacheMax at heapScatterAlpha are tabulated once at start-up,
// because the judge asks every second; any other (p, dof) is computed
// by bisection on the exact CDF.
func studentTQuantile(p float64, dof int) float64 {
	if dof < 1 {
		return math.Inf(1)
	}
	if p == 1-heapScatterAlpha && dof <= tQuantileCacheMax {
		return scatterTCache[dof]
	}
	return studentTQuantileExact(p, dof)
}

const tQuantileCacheMax = 128

var scatterTCache = func() [tQuantileCacheMax + 1]float64 {
	var c [tQuantileCacheMax + 1]float64
	for d := 1; d <= tQuantileCacheMax; d++ {
		c[d] = studentTQuantileExact(1-heapScatterAlpha, d)
	}
	return c
}()

// studentTQuantileExact inverts studentTCDF by bisection. The upper
// bracket 1e7 covers every p >= 0.5 + 1e-12 at dof >= 1 (the Cauchy
// tail: t ~ 1/(pi (1-p))).
func studentTQuantileExact(p float64, dof int) float64 {
	lo, hi := 0.0, 1e7
	for i := 0; i < 200; i++ {
		mid := (lo + hi) / 2
		if studentTCDF(mid, float64(dof)) < p {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2
}

// studentTCDF is P(T <= t) for Student's t with v degrees of freedom,
// through the regularized incomplete beta function.
func studentTCDF(t, v float64) float64 {
	x := v / (v + t*t)
	tail := 0.5 * regIncBeta(v/2, 0.5, x)
	if t > 0 {
		return 1 - tail
	}
	return tail
}

// regIncBeta is the regularized incomplete beta function I_x(a, b), by
// the continued fraction of Numerical Recipes (betacf), evaluated on
// whichever side of the symmetry point converges faster.
func regIncBeta(a, b, x float64) float64 {
	if x <= 0 {
		return 0
	}
	if x >= 1 {
		return 1
	}
	lg := func(z float64) float64 { v, _ := math.Lgamma(z); return v }
	bt := math.Exp(lg(a+b) - lg(a) - lg(b) + a*math.Log(x) + b*math.Log(1-x))
	if x < (a+1)/(a+b+2) {
		return bt * betaCF(a, b, x) / a
	}
	return 1 - bt*betaCF(b, a, 1-x)/b
}

func betaCF(a, b, x float64) float64 {
	const tiny = 1e-30
	qab, qap, qam := a+b, a+1, a-1
	c := 1.0
	d := 1 - qab*x/qap
	if math.Abs(d) < tiny {
		d = tiny
	}
	d = 1 / d
	h := d
	for m := 1; m <= 300; m++ {
		fm := float64(m)
		m2 := 2 * fm
		aa := fm * (b - fm) * x / ((qam + m2) * (a + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		h *= d * c
		aa = -(a + fm) * (qab + fm) * x / ((a + m2) * (qap + m2))
		d = 1 + aa*d
		if math.Abs(d) < tiny {
			d = tiny
		}
		c = 1 + aa/c
		if math.Abs(c) < tiny {
			c = tiny
		}
		d = 1 / d
		del := d * c
		h *= del
		if math.Abs(del-1) < 1e-14 {
			break
		}
	}
	return h
}
