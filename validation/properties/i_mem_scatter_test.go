package properties

import (
	"bufio"
	"math"
	"math/rand"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// loadFixtureHeap reads testdata/imem1_obs_std_arm64_soak38441116109.csv:
// heap_inuse bytes at 1 Hz from msr1's observability/std cell of soak
// 38441116109, the cell whose one I-MEM-1 trip (77 failing ticks, first
// persisted at t=1498 s, "trough slope 1.8 KB/s ... rise 1.59 MB ... 7
// troughs") failed ValidateGate with no leak behind it.
func loadFixtureHeap(t *testing.T) []int64 {
	t.Helper()
	f, err := os.Open("testdata/imem1_obs_std_arm64_soak38441116109.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		v, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// fixtureLoadOffset is how far into the cell sustained load starts: the
// cell runs the I-MEM-2 prelude (burst, idle) first, and the slope
// warm-up counts from the end of it.
const fixtureLoadOffset = 151

// persistedAt replays heap through spec the way the evaluator does --
// one verdict per second with the cell's LoadStartedAt, a violation
// declared after spec.Persist consecutive failures -- and returns the
// second of the first declared violation, the number of declared
// violations, or (-1, 0).
func persistedAt(spec Spec, heap []int64, loadOffset int) (first, n int) {
	base := int64(1_700_000_000)
	start := time.Unix(base, 0)
	var hist []Snapshot
	streak, first := 0, -1
	for i, h := range heap {
		hist = append(hist, Snapshot{TS: base + int64(i), HeapInuseBytes: h, GoroutineCount: 100})
		ctx := Context{
			RunStartedAt:  start,
			LoadStartedAt: start.Add(time.Duration(loadOffset) * time.Second),
			Now:           time.Unix(base+int64(i), 0),
			History:       hist,
		}
		ok, msg := spec.Predicate(&hist[i], ctx)
		if IsSkip(ok, msg) {
			continue
		}
		if ok {
			streak = 0
			continue
		}
		streak++
		if streak >= spec.Persist {
			if first < 0 {
				first = i
			}
			n++
		}
	}
	return first, n
}

// withoutScatter is IMEM1 with the trough-scatter term off: the rule as
// it stood before heapScatterAlpha.
func withoutScatter() Spec {
	sp := heapSlopeSpec
	sp.scatterAlpha = 0
	s := IMEM1
	s.Predicate = func(_ *Snapshot, ctx Context) (bool, string) { return sp.judge(ctx) }
	return s
}

// The regression for soak 38441116109: this cell's troughs scatter by
// 0.6 MB (sd) with no leak, and a window of 6-9 troughs rose by 1.2-2.5
// MB, past the 1.5 MB floor, for 77 persisted ticks.
func TestIMEM1_scatteredTroughsWithoutLeakDoNotTrip(t *testing.T) {
	heap := loadFixtureHeap(t)
	if len(heap) != 2700 {
		t.Fatalf("fixture has %d samples, want 2700", len(heap))
	}
	if first, n := persistedAt(IMEM1, heap, fixtureLoadOffset); n != 0 {
		t.Fatalf("I-MEM-1 declared %d violations on a cell with no leak, first at t=%ds", n, first)
	}
}

// The fixture must actually be the failing case: without the scatter
// term the rule trips on it exactly as the gate recorded (first
// persisted violation at t=1498 s, 77 declared).
func TestIMEM1_scatterFixtureIsTheRecordedTrip(t *testing.T) {
	heap := loadFixtureHeap(t)
	first, n := persistedAt(withoutScatter(), heap, fixtureLoadOffset)
	if first != 1498 || n != 77 {
		t.Fatalf("rule without the scatter term: first=%d n=%d, want first=1498 n=77 (the soak's record)", first, n)
	}
}

// Sensitivity guard on the same fixture: a real leak on top of the same
// scatter still fails. 10 KB/s from the end of warm-up is 15 MB by the
// time the window is 25 min old; 3 KB/s is the rate probatorium#478
// asks I-MEM-1 to keep. The scatter term must not swallow either.
func TestIMEM1_scatterFixtureWithLeakStillTrips(t *testing.T) {
	for _, rate := range []float64{10 * 1024, 5 * 1024, 3 * 1024} {
		heap := loadFixtureHeap(t)
		for i := range heap {
			if dt := i - (fixtureLoadOffset + 300); dt > 0 {
				heap[i] += int64(rate * float64(dt))
			}
		}
		first, n := persistedAt(IMEM1, heap, fixtureLoadOffset)
		if n == 0 {
			t.Fatalf("a %.0f KB/s leak added to the scattered cell never tripped I-MEM-1", rate/1024)
		}
		t.Logf("%.0f KB/s on the scattered cell: first persisted violation at t=%ds (%.1f min), %d declared", rate/1024, first, float64(first)/60, n)
	}
}

// troughSeries builds a 1 Hz heap series whose 150 s bucket minima are
// level + drift*t + N(0, sigma): a flat process with the given trough
// scatter, a sawtooth riding above each trough. Buckets align with the
// judged grid (load start + 300 s).
func troughSeries(rng *rand.Rand, secs int, level, sigma, drift float64) []int64 {
	out := make([]int64, secs+1)
	grid0 := fixtureLoadOffset + 300
	var trough float64
	for i := range out {
		if i < grid0 {
			out[i] = int64(level + 4e6)
			continue
		}
		if (i-grid0)%150 == 0 {
			trough = rng.NormFloat64() * sigma
		}
		saw := float64((i-grid0)%7) * 3e5 // sawtooth above the trough; its minimum sits in every bucket
		out[i] = int64(level + trough + saw + drift*float64(i-grid0))
	}
	return out
}

// With the same trough scatter as the soak's trouble cells (sd 0.6 MB)
// and no leak, the rule trips in about as many windows as the Gaussian
// calculation behind heapScatterAlpha allows, where the rule without the
// scatter term trips several times as often. Seeds fixed.
func TestIMEM1_scatterNullOnSyntheticScatter(t *testing.T) {
	const cells = 60
	var withTerm, without int
	off := withoutScatter()
	for s := 0; s < cells; s++ {
		rng := rand.New(rand.NewSource(int64(1000 + s)))
		heap := troughSeries(rng, 2700, 20e6, 0.6e6, 0)
		if _, n := persistedAt(IMEM1, heap, fixtureLoadOffset); n > 0 {
			withTerm++
		}
		if _, n := persistedAt(off, heap, fixtureLoadOffset); n > 0 {
			without++
		}
	}
	t.Logf("%d synthetic leak-free cells with trough sd 0.6 MB: %d trip with the scatter term, %d without", cells, withTerm, without)
	if without < 5 {
		t.Fatalf("control: the rule without the scatter term tripped only %d/%d scattered cells; the synthetic scatter is not the soak's", without, cells)
	}
	if withTerm > cells/100+1 {
		t.Fatalf("%d/%d leak-free scattered cells tripped with the scatter term, want at most %d (1%% + 1)", withTerm, cells, cells/100+1)
	}
}

// Where the troughs barely scatter (sd 0.1 MB), the scatter term sits
// below the other floors, and a 3 KB/s leak from the end of warm-up
// trips as early as it did without the term, to within one trough.
func TestIMEM1_scatterTermLeavesQuietCellsAlone(t *testing.T) {
	off := withoutScatter()
	for s := 0; s < 20; s++ {
		rng := rand.New(rand.NewSource(int64(77 + s)))
		heap := troughSeries(rng, 2700, 20e6, 0.1e6, 3*1024)
		fNew, nNew := persistedAt(IMEM1, heap, fixtureLoadOffset)
		fOld, _ := persistedAt(off, heap, fixtureLoadOffset)
		if nNew == 0 || fOld < 0 {
			t.Fatalf("seed %d: 3 KB/s leak on a quiet cell: new rule %d violations, old rule first=%d", s, nNew, fOld)
		}
		if fNew-fOld > 300 {
			t.Fatalf("seed %d: new rule first trips at %ds, old at %ds: the scatter term delays a quiet cell by more than 2 troughs", s, fNew, fOld)
		}
	}
}

// scatterPts builds trough points (150 s apart) from the given values.
func scatterPts(y ...float64) []point {
	pts := make([]point, len(y))
	for i, v := range y {
		pts[i] = point{x: float64(i * 150), y: v}
	}
	return pts
}

// The scatter estimate comes from successive differences, so a linear
// trend does not move it, one step moves it little, and (the point of
// the estimator) a bend does not scale it with the span.
func TestTrimmedDiffScale(t *testing.T) {
	rng := rand.New(rand.NewSource(3))
	noise := make([]float64, 40)
	for i := range noise {
		noise[i] = rng.NormFloat64() * 5e5
	}
	flat := trimmedDiffScale(scatterPts(noise...))
	if flat < 3.5e5 || flat > 6.5e5 {
		t.Errorf("sd 500 KB iid troughs: estimate %.0f KB, want near 500", flat/1e3)
	}
	// a linear trend of 9 KB/s (1.35 MB per trough) changes nothing: the differences all shift together.
	trend := append([]float64(nil), noise...)
	for i := range trend {
		trend[i] += 9000 * 150 * float64(i)
	}
	if got := trimmedDiffScale(scatterPts(trend...)); math.Abs(got-flat) > 1e-6*flat {
		t.Errorf("a linear trend moved the estimate: %.0f -> %.0f", flat, got)
	}
	// one 5 MB step in 40 troughs is a single outlying difference: dropped by the trim.
	step := append([]float64(nil), noise...)
	for i := 20; i < len(step); i++ {
		step[i] += 5e6
	}
	if got := trimmedDiffScale(scatterPts(step...)); got > 1.1*flat {
		t.Errorf("one 5 MB step inflated the estimate %.0f -> %.0f KB", flat/1e3, got/1e3)
	}
	// a bend: flat, then 6 KB/s for the last 15 troughs. The residual about a line grows with the span here;
	// the difference estimate sees 15 differences shifted by 0.9 MB and must stay at the noise.
	bend := append([]float64(nil), noise[:30]...)
	for i := 15; i < 30; i++ {
		bend[i] += 6144 * 150 * float64(i-15)
	}
	if got := trimmedDiffScale(scatterPts(bend...)); got > 2.2*flat {
		t.Errorf("a late bend inflated the estimate %.0f -> %.0f KB (the residual approach read it as ~2 MB)", flat/1e3, got/1e3)
	}
}

func TestScatterFloor(t *testing.T) {
	line := func(n int, slope, noise float64) []point {
		var pts []point
		for i := 0; i < n; i++ {
			y := slope * float64(i*150)
			if i%2 == 1 {
				y += noise
			}
			pts = append(pts, point{x: float64(i * 150), y: y})
		}
		return pts
	}
	if f := scatterFloor(line(8, 3000, 0)); f > 1e-6 {
		t.Errorf("an exact line has no scatter: floor %v, want 0", f)
	}
	if f := scatterFloor(line(3, 3000, 0)); !math.IsInf(f, 1) {
		t.Errorf("3 troughs: floor %v, want +Inf", f)
	}
	// alternating +-: the floor scales linearly with the scatter.
	a, b := scatterFloor(line(8, 0, 1e5)), scatterFloor(line(8, 0, 2e5))
	if a <= 0 || math.Abs(b/a-2) > 1e-9 {
		t.Errorf("floor should scale with the scatter: %v then %v", a, b)
	}
	// a leak's slope does not enter the floor.
	if x, y := scatterFloor(line(8, 0, 1e5)), scatterFloor(line(8, 9000, 1e5)); math.Abs(x-y) > 1e-6*x {
		t.Errorf("slope changed the scatter floor: %v vs %v", x, y)
	}
	// fewer troughs, a larger multiple of the same scatter.
	if scatterFloor(line(6, 0, 1e5))/scatterFloor(line(14, 0, 1e5)) < 2 {
		t.Error("a 6-trough window must demand more than a 14-trough one at equal scatter")
	}
}

// The critical-value table is the 1 - alpha quantile of the statistic
// for iid normal troughs: spot-check three entries by a small
// simulation (the full table: scripts in the PR body).
func TestScatterCritCalibration(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	for _, n := range []int{7, 10, 15} {
		const draws = 60000
		over := 0
		pts := make([]point, n)
		for k := 0; k < draws; k++ {
			for i := range pts {
				pts[i] = point{x: float64(i * 150), y: rng.NormFloat64()}
			}
			var sxx float64
			mx := float64((n-1)*150) / 2
			for _, p := range pts {
				sxx += (p.x - mx) * (p.x - mx)
			}
			if slope(pts)*math.Sqrt(sxx)/trimmedDiffScale(pts) > heapScatterCrit[n] {
				over++
			}
		}
		// expected 60 exceedances at alpha 0.001; allow a wide band (binomial sd ~ 8).
		if over < 25 || over > 110 {
			t.Errorf("n=%d: %d of %d null windows exceed the critical value %.2f, want about %d", n, over, draws, heapScatterCrit[n], int(draws*heapScatterAlpha))
		}
	}
}

// scatterLeak builds a 1 Hz heap series on troughSeries' grid with a leak
// added by leak(t), t seconds after the end of warm-up.
func scatterLeak(rng *rand.Rand, secs int, sigma float64, leak func(t float64) float64) []int64 {
	h := troughSeries(rng, secs, 20e6, sigma, 0)
	grid0 := fixtureLoadOffset + 300
	for i := grid0; i < len(h); i++ {
		h[i] += int64(leak(float64(i - grid0)))
	}
	return h
}

func lateRamp(rate, onset float64) func(float64) float64 {
	return func(t float64) float64 {
		if t <= onset {
			return 0
		}
		return rate * (t - onset)
	}
}

func stairs(rate, period float64) func(float64) float64 {
	return func(t float64) float64 { return rate * period * math.Floor(t/period) }
}

// A leak that starts late, on troughs that scatter by 0.6 MB, must be
// called nearly as often as the rule without the scatter term calls it
// (the first version of the term counted the bend as scatter and lost a
// third of them). 6 KB/s from load+1500 s, 45 min cells.
func TestIMEM1_scatterTermKeepsLateStartingLeaks(t *testing.T) {
	off := withoutScatter()
	var nNew, nOff int
	for s := 0; s < 40; s++ {
		rng := rand.New(rand.NewSource(int64(5000 + s)))
		h := scatterLeak(rng, 2700, 0.6e6, lateRamp(6*1024, 1200))
		if _, n := persistedAt(IMEM1, h, fixtureLoadOffset); n > 0 {
			nNew++
		}
		if _, n := persistedAt(off, h, fixtureLoadOffset); n > 0 {
			nOff++
		}
	}
	t.Logf("late-start 6 KB/s on 0.6 MB scatter, 40 cells: %d called with the scatter term, %d without", nNew, nOff)
	if nOff < 20 {
		t.Fatalf("control: the rule without the term called only %d/40; the scenario is not a leak the old rule sees", nOff)
	}
	if float64(nNew) < 0.85*float64(nOff) {
		t.Fatalf("scatter term called %d of the %d late-start leaks the old rule calls (want >= 85%%)", nNew, nOff)
	}
}

// A leak that climbs in steps (2.7 MB every 15 min, 3 KB/s on average) on
// a quiet cell must be called within a few minutes of the old rule.
func TestIMEM1_scatterTermKeepsSteppedLeaksOnQuietCells(t *testing.T) {
	off := withoutScatter()
	var worst int
	for s := 0; s < 20; s++ {
		rng := rand.New(rand.NewSource(int64(9000 + s)))
		h := scatterLeak(rng, 2700, 0.1e6, stairs(3*1024, 900))
		fNew, nNew := persistedAt(IMEM1, h, fixtureLoadOffset)
		fOld, nOld := persistedAt(off, h, fixtureLoadOffset)
		if nOld == 0 {
			t.Fatalf("seed %d: control: the old rule never called the stepped leak", s)
		}
		if nNew == 0 {
			t.Fatalf("seed %d: the stepped leak on a quiet cell is not called with the scatter term", s)
		}
		worst = max(worst, fNew-fOld)
	}
	t.Logf("stepped 3 KB/s on a 0.1 MB cell: worst first-violation delay vs the old rule %d s", worst)
	if worst > 600 {
		t.Fatalf("worst delay %d s, want <= 600 s (10 min)", worst)
	}
}

func TestIMEM34_doNotUseTheScatterTerm(t *testing.T) {
	if goroutineSlopeSpec.scatterAlpha != 0 || rssSlopeSpec.scatterAlpha != 0 {
		t.Fatal("I-MEM-3 and I-MEM-4 must not enable the trough-scatter term")
	}
	if heapSlopeSpec.scatterAlpha != heapScatterAlpha {
		t.Fatal("I-MEM-1 must enable the trough-scatter term at heapScatterAlpha")
	}
}
