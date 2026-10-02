package properties

import (
	"math"
	"math/rand"
	"strings"
	"testing"
)

// studentTUpperTail is P(T > t) for Student's t with dof degrees of
// freedom, by Simpson's rule after x = sqrt(dof) tan(theta). That turns
// the density into c * cos(theta)^(dof-1) on [atan(t/sqrt(dof)), pi/2],
// a smooth integrand on a finite interval.
func studentTUpperTail(t float64, dof int) float64 {
	nu := float64(dof)
	lg1, _ := math.Lgamma((nu + 1) / 2)
	lg2, _ := math.Lgamma(nu / 2)
	c := math.Exp(lg1-lg2) / math.Sqrt(math.Pi)
	a, b := math.Atan(t/math.Sqrt(nu)), math.Pi/2
	const n = 20000 // even
	h := (b - a) / n
	f := func(th float64) float64 { return math.Pow(math.Cos(th), nu-1) }
	sum := f(a) + f(b)
	for i := 1; i < n; i++ {
		w := 2.0
		if i%2 == 1 {
			w = 4
		}
		sum += w * f(a+float64(i)*h)
	}
	return c * sum * h / 3
}

// Every entry of slopeBoundT is the one-sided 99.9 % point of its t
// distribution: the upper tail beyond it is 0.001, to the table's three
// decimals.
func TestSlopeBoundT_isTheOneSided999Quantile(t *testing.T) {
	for dof := 1; dof < len(slopeBoundT); dof++ {
		q := slopeBoundT[dof]
		// Three decimals: the true quantile lies within 0.0005 of q, so the
		// tail at q-0.0005 is above 0.001 and the tail at q+0.0005 below it.
		lo, hi := studentTUpperTail(q-0.0005, dof), studentTUpperTail(q+0.0005, dof)
		if !(lo > 0.001 && hi < 0.001) {
			t.Errorf("dof %d: slopeBoundT %.3f is not the 99.9%% point (tail %.6f at -0.0005, %.6f at +0.0005)", dof, q, lo, hi)
		}
	}
	// Sanity of the integrator itself: the t with 1 dof is Cauchy, whose
	// tail is known in closed form.
	if got, want := studentTUpperTail(1, 1), 0.25; math.Abs(got-want) > 1e-9 {
		t.Fatalf("Cauchy tail at 1 = %.12f, want %.2f", got, want)
	}
}

// The bound is the OLS slope less t standard errors, with the SE from the
// troughs' residuals: on an exact line the SE is 0 and the bound is the
// slope; with scatter it drops by t x SE.
func TestSlopeLowerBound(t *testing.T) {
	line := []point{{0, 10}, {150, 160}, {300, 310}, {450, 460}, {600, 610}}
	b := slope(line)
	lower, se, tq, dof, ok := slopeLowerBound(line, b)
	if !ok || dof != 3 || se != 0 || lower != b || tq != slopeBoundT[3] {
		t.Fatalf("exact line: lower=%g se=%g t=%g dof=%d ok=%v, want lower=slope=%g, se 0, 3 dof", lower, se, tq, dof, ok, b)
	}
	// Residuals +e, -e, +e, -e, +e about a slope-1 line.
	const e = 30.0
	pts := []point{{0, 0 + e}, {150, 150 - e}, {300, 300 + e}, {450, 450 - e}, {600, 600 + e}}
	b = slope(pts)
	lower, se, _, dof, ok = slopeLowerBound(pts, b)
	var rss, mx float64
	for _, p := range pts {
		mx += p.x / 5
	}
	var sxx float64
	for _, p := range pts {
		sxx += (p.x - mx) * (p.x - mx)
	}
	my := 0.0
	for _, p := range pts {
		my += p.y / 5
	}
	for _, p := range pts {
		r := p.y - (my + b*(p.x-mx))
		rss += r * r
	}
	wantSE := math.Sqrt(rss / 3 / sxx)
	if !ok || dof != 3 || math.Abs(se-wantSE) > 1e-12 || math.Abs(lower-(b-slopeBoundT[3]*wantSE)) > 1e-12 {
		t.Fatalf("scattered line: lower=%g se=%g dof=%d, want se=%g lower=%g", lower, se, dof, wantSE, b-slopeBoundT[3]*wantSE)
	}
	if _, _, _, _, ok := slopeLowerBound(pts[:2], 1); ok {
		t.Fatal("two points leave no residual to bound the slope with")
	}
}

// scatteredTroughHeap is a heap whose post-GC trough moves from one 150 s
// bucket to the next by itself: a flat level, a per-bucket offset drawn
// from N(0, sigma), a GC sawtooth of the given amplitude on top, and a
// leak of leakBps from the end of warm-up. That is the shape of the
// ws_echo cells of soak 36433207097 (trough sigma 0.6-1.6 MB on the
// native engines; probatorium#466).
func scatteredTroughHeap(rng *rand.Rand, seconds int, level, sigma, amp, leakBps float64) []Snapshot {
	off := make([]float64, seconds/150+2)
	for i := range off {
		off[i] = rng.NormFloat64() * sigma
	}
	return leakHistory(seconds, func(i int) (int64, int64, int64) {
		v := level + amp*float64(i%7)/7
		if i >= 300 {
			v += off[(i-300)/150] + leakBps*float64(i-300)
		}
		return 100, int64(v), 0
	})
}

// violatesPersisted reports whether spec fails on persist consecutive
// one-second evaluations anywhere in history: what the evaluator needs
// before it declares a violation.
func violatesPersisted(spec Spec, history []Snapshot, persist int) (bool, string) {
	run := 0
	for i := range history {
		ctx := slopeCtx(history[:i+1])
		if ok, msg := spec.Predicate(&history[i], ctx); !ok {
			run++
			if run >= persist {
				return true, msg
			}
		} else {
			run = 0
		}
	}
	return false, ""
}

// imem1WithoutBound is I-MEM-1 as it was before probatorium#466: the same
// spec with the lower-bound check off. The tests below run both, so each
// shows the scatter model actually produces the failure being fixed.
func imem1WithoutBound() Spec {
	sp := heapSlopeSpec
	sp.boundSlope = false
	s := IMEM1
	s.Predicate = func(_ *Snapshot, ctx Context) (bool, string) { return sp.judge(ctx) }
	return s
}

// Trough scatter with no trend underneath must not fail I-MEM-1, and the
// same series must fail I-MEM-1 without the bound in some seeds: that is
// the false positive of probatorium#466, reproduced from the published
// scatter rather than from any one cell.
func TestIMEM1_troughScatterAloneDoesNotFire(t *testing.T) {
	seeds := 12
	if testing.Short() {
		seeds = 4
	}
	oldFired := 0
	for seed := 1; seed <= seeds; seed++ {
		h := scatteredTroughHeap(rand.New(rand.NewSource(int64(seed))), 45*60, 8<<20, 800<<10, 4<<20, 0)
		if bad, msg := violatesPersisted(IMEM1, h, slopePersistSamples); bad {
			t.Errorf("seed %d: trough scatter alone failed I-MEM-1: %s", seed, msg)
		}
		if bad, _ := violatesPersisted(imem1WithoutBound(), h, slopePersistSamples); bad {
			oldFired++
		}
	}
	if oldFired == 0 {
		t.Fatalf("the scatter model never failed I-MEM-1 without the bound in %d seeds: it does not reproduce probatorium#466, so its pass proves nothing", seeds)
	}
	t.Logf("without the bound, %d of %d seeds failed", oldFired, seeds)
}

// Under the same scatter a leak well above the budget is still caught,
// and the message carries the bound.
func TestIMEM1_boundStillCatchesLeakUnderScatter(t *testing.T) {
	for seed := 1; seed <= 4; seed++ {
		h := scatteredTroughHeap(rand.New(rand.NewSource(int64(seed))), 45*60, 8<<20, 800<<10, 4<<20, 4096)
		bad, msg := violatesPersisted(IMEM1, h, slopePersistSamples)
		if !bad {
			t.Fatalf("seed %d: a 4 KB/s leak under 800 KB trough scatter never failed I-MEM-1 in 45 min", seed)
		}
		if !strings.Contains(msg, "lower bound") || !strings.Contains(msg, "dof") {
			t.Fatalf("message must show the bound: %q", msg)
		}
	}
}

// The bound is I-MEM-1's only: I-MEM-3 and I-MEM-4 judge exactly as before.
func TestSlopeBound_onlyIMEM1(t *testing.T) {
	if !heapSlopeSpec.boundSlope || goroutineSlopeSpec.boundSlope || rssSlopeSpec.boundSlope {
		t.Fatalf("boundSlope: I-MEM-1 %v, I-MEM-3 %v, I-MEM-4 %v; want only I-MEM-1",
			heapSlopeSpec.boundSlope, goroutineSlopeSpec.boundSlope, rssSlopeSpec.boundSlope)
	}
	if !strings.Contains(IMEM1.Description, "lower confidence bound") {
		t.Fatalf("I-MEM-1's description must state the bound: %q", IMEM1.Description)
	}
}
