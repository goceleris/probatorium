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
	defer f.Close()
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

func TestStudentTQuantile_matchesReference(t *testing.T) {
	// Reference values: R qt() / standard tables.
	for _, c := range []struct {
		p    float64
		dof  int
		want float64
	}{
		{0.975, 10, 2.2281388519649385},
		{0.999, 2, 22.327124770119874},
		{0.999, 5, 5.893429532},
		{0.999, 12, 3.929633},
		{0.99, 30, 2.457262},
		{1 - heapScatterAlpha, 1, 318.308839},
		{0.95, 1000, 1.646379},
	} {
		got := studentTQuantile(c.p, c.dof)
		if math.Abs(got-c.want) > 1e-5*c.want {
			t.Errorf("t(%v, %d) = %.9g, want %.9g", c.p, c.dof, got, c.want)
		}
	}
	// the cached path agrees with the exact one at the registered level.
	for d := 1; d <= tQuantileCacheMax; d++ {
		if g, w := studentTQuantile(1-heapScatterAlpha, d), studentTQuantileExact(1-heapScatterAlpha, d); g != w {
			t.Fatalf("dof %d: cache %v != exact %v", d, g, w)
		}
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
	if f := scatterFloor(line(8, 3000, 0), 0.001); f > 1e-6 {
		t.Errorf("an exact line has no scatter: floor %v, want 0", f)
	}
	if f := scatterFloor(line(2, 3000, 0), 0.001); !math.IsInf(f, 1) {
		t.Errorf("2 troughs: floor %v, want +Inf", f)
	}
	// alternating +-: residual scatter grows with the noise, linearly.
	a, b := scatterFloor(line(8, 0, 1e5), 0.001), scatterFloor(line(8, 0, 2e5), 0.001)
	if a <= 0 || math.Abs(b/a-2) > 1e-9 {
		t.Errorf("floor should scale with the scatter: %v then %v", a, b)
	}
	// a leak's slope does not enter the floor.
	if x, y := scatterFloor(line(8, 0, 1e5), 0.001), scatterFloor(line(8, 9000, 1e5), 0.001); math.Abs(x-y) > 1e-6*x {
		t.Errorf("slope changed the scatter floor: %v vs %v", x, y)
	}
	// fewer troughs, fewer degrees of freedom: a larger multiple of the same scatter.
	if scatterFloor(line(5, 0, 1e5), 0.001)/scatterFloor(line(14, 0, 1e5), 0.001) < 1.5 {
		t.Error("a 5-trough window must demand more than a 14-trough one at equal scatter")
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
