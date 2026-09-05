package properties

import (
	"fmt"
	"math"
	"sort"
	"time"
)

// Slope predicates (I-MEM-1, I-MEM-3, I-MEM-4) share one shape: fit a
// least-squares line to a resource series over a trailing window of
// [Context.History] and fail when BOTH the slope exceeds a budget AND
// the fitted rise over the window clears a floor that no healthy
// process can reach by noise or by a single step. The window rules are
// common to all three:
//
//   - The first slopeWarmup (5 min) of the run is never judged. A cold
//     start ramps every series (pools fill, caches warm, the engine
//     spins up its goroutine ladder) and would fire on every cell.
//   - The window must span at least slopeMinSpan (10 min) of samples
//     AND hold at least slopeMinSamples points before a verdict. A
//     sparse history (poll failures) is not judged as if it were dense.
//   - Until both hold the predicate returns [Skip], so a 1h matrix
//     cell starts judging at t=15min rather than never -- and a 150 s
//     nightly cell, which never gets there, reports the predicate as
//     not judged instead of as passed.
//   - The fit runs over per-150s bucket MINIMA (see buckets), not raw
//     samples, so a GC sawtooth cannot masquerade as a slope.
//   - A verdict must persist for slopePersistSamples consecutive
//     evaluations (one bucket width) before the evaluator declares a
//     violation, so it has survived a complete re-bucketing of the
//     window.
//
// The 24h soak of 2026-09-04 reached an 18 GB heap and 17,704
// goroutines (celeris#494, three goroutines leaked per killed SSE
// stream) with properties_passed=0: I-MEM-1 skipped until the run had
// lasted a full hour and nothing watched the goroutine count at all.
// At 1 stream-kill/s that leak is a 3 goroutine/s slope -- 15x the
// I-MEM-3 budget -- and now trips at t=15min (+150 s persistence).
const (
	// slopeWarmup is the run-relative period excluded from every slope
	// window.
	slopeWarmup = 5 * time.Minute
	// slopeMinSpan is the minimum first-to-last sample span a window
	// must cover before its slope is judged.
	slopeMinSpan = 10 * time.Minute
	// slopeMinSamples is the minimum point count in a window.
	slopeMinSamples = 60
)

// slopeBucket is the width of the trough buckets the regression runs
// over. A GC cycle is a sawtooth (slow ramp, instant drop) and a plain
// least-squares fit over one has a positive bias of roughly
// amplitude*period/window^2 -- a 500 MB live heap with 5 s cycles over
// a 10 min window reads as ~7 KB/s with no leak at all. The per-bucket
// minimum is the post-GC trough: flat for a healthy process, rising
// under a leak. The bucket must be wider than the longest possible GC
// period: the runtime's sysmon forces a collection whenever none has
// run for 2 minutes (runtime.forcegcperiod), so 150 s guarantees at
// least one trough per bucket at any allocation rate. A 10 min window
// yields 4 trough points, the 1 h window 24.
const slopeBucket = 150 * time.Second

// slopePersistSamples is [Spec.Persist] for the slope predicates: at
// 1 Hz, one bucket width. The bucket grid is anchored on the first
// sample of the window and slides with it, so after this many ticks
// every bucket boundary has moved across a full bucket and the trough
// set has been rebuilt from scratch; a verdict that survives that is
// not an artefact of one boundary alignment.
const slopePersistSamples = int(slopeBucket / time.Second)

// slopeNoiseK scales the per-window sampling-noise estimate into the
// rise floor. The bucket minimum of a sawtooth of amplitude A sampled
// at n distinct points sits ~A/(n+1) above the true trough with a
// comparable standard deviation, so the rise between two troughs has
// a noise sigma of ~1.4*A/n; the fit is re-evaluated every second with
// sliding bucket boundaries, so the floor has to sit well beyond that.
// 8x the estimate is ~5.6 sigma of the two-trough difference; the
// sawtooth simulation in i_mem_slope_test.go (L = 32 / 128 MB live
// heap, GC periods 0.37 s / 1.7 s / 13 s, random phase, 50 % duplicate
// samples, one hour, 12 seeds each) must stay at zero violations.
const slopeNoiseK = 8.0

// heapSlopeWindow is the trailing window over which I-MEM-1 (and
// I-MEM-4) fits a least-squares line: min(1h, elapsed since warm-up).
// 1h matches the rolling-window cap in [Context.History]. A growing
// window dilutes a single legitimate floor step (a cache filling once)
// as more buckets accumulate behind it, where a fixed 10 min window
// would carry it as a slope for its whole width.
const heapSlopeWindow = time.Hour

// heapSlopeMaxBytesPerSec is the slope I-MEM-1 tolerates. 1 KB/s over
// an hour = ~3.5 MB/h: comfortably above GC noise but well below any
// production-meaningful leak rate.
const heapSlopeMaxBytesPerSec = 1024.0

// heapRiseRelFloor is the fraction of the window's mean trough level
// the fitted rise must exceed before I-MEM-1 fires: a floor step
// (a TTL store or cache filling after warm-up) smaller than 3 % of the
// live heap is not a leak verdict. A leak of 2 KB/s on a 100 MB heap
// clears it (3 MB) after ~25 min of window; the celeris#494 class
// (218 KB/s) clears it in seconds.
const heapRiseRelFloor = 0.03

// goroutineSlopeWindow is the trailing window I-MEM-3 fits over.
const goroutineSlopeWindow = 10 * time.Minute

// goroutineSlopeMaxPerSec is the goroutine-count slope I-MEM-3
// tolerates: 0.2/s = 120 goroutines over the 10 min window. Tier 1
// runs a FIXED walker concurrency per cell, so a healthy refapp's
// goroutine count is stationary (request-driven jitter only); any
// sustained slope is a leak.
const goroutineSlopeMaxPerSec = 0.2

// goroutineRiseRelFloor is the fraction of the mean trough goroutine
// count the fitted rise must exceed. A one-off step (a standby engine
// spinning up its ladder on promotion, a WS-torture burst pinning a
// goroutine per connection) is tolerated up to 5 % of the population
// and, through the absolute floor, up to the budgeted 120.
const goroutineRiseRelFloor = 0.05

// rssSlopeWindow is the trailing window I-MEM-4 fits over: the same
// growing min(1h, elapsed) as I-MEM-1. RSS has no sawtooth, so a step
// (the heap goal rising after a load burst touches fresh arena pages;
// io_uring buffer-ring growth) goes straight into a short window's
// slope; the growing window dilutes it.
const rssSlopeWindow = heapSlopeWindow

// rssSlopeMaxBytesPerSec is the RSS slope I-MEM-4 tolerates: 64 KB/s =
// ~37.5 MB over 10 min. Looser than the heap budget because RSS also
// moves with the Go runtime's scavenger and page-cache behaviour, not
// just the live heap.
const rssSlopeMaxBytesPerSec = 64 * 1024.0

// rssRiseRelFloor is the fraction of the mean trough RSS the fitted
// rise must exceed (a 5 % step of resident memory is a heap-goal move,
// not a leak verdict).
const rssRiseRelFloor = 0.05

// goroutineSettleDuration is how long I-MEM-2 waits after Context.IdleMode
// flips true before enforcing the baseline-return assertion. Most
// pooled goroutines exit within a single GC cycle; 30s is generous.
const goroutineSettleDuration = 30 * time.Second

// goroutineBaselinePad is the slack added to the post-idle goroutine
// budget. The engine spins up a small ladder of background goroutines
// (epoll readers, the scheduler timer, the metrics publisher) per
// listener; +N=8 covers the worst case without masking a real leak.
const goroutineBaselinePad int64 = 8

// slopeWindow returns the History samples inside the trailing window
// that also fall after the warm-up period, filtered by keep (nil keeps
// every sample). ok is false while the window is not yet judgeable:
// unknown run start, fewer than slopeMinSamples points, or a
// first-to-last span shorter than slopeMinSpan.
func slopeWindow(ctx Context, window time.Duration, keep func(Snapshot) bool) (samples []Snapshot, ok bool) {
	if ctx.RunStartedAt.IsZero() {
		return nil, false
	}
	cutoff := ctx.Now.Add(-window)
	if warm := ctx.RunStartedAt.Add(slopeWarmup); warm.After(cutoff) {
		cutoff = warm
	}
	cutoffTS := cutoff.Unix()
	samples = make([]Snapshot, 0, len(ctx.History))
	for _, s := range ctx.History {
		if s.TS < cutoffTS {
			continue
		}
		if keep != nil && !keep(s) {
			continue
		}
		samples = append(samples, s)
	}
	if len(samples) < slopeMinSamples {
		return samples, false
	}
	span := samples[len(samples)-1].TS - samples[0].TS
	if span < int64(slopeMinSpan/time.Second) {
		return samples, false
	}
	return samples, true
}

// point is one (x seconds, y) pair fed to the regression.
type point struct{ x, y float64 }

// bucket is the per-slopeBucket summary of a series: the trough (min),
// the peak (max), the sample count and the number of DISTINCT
// consecutive values (a refapp caches memstats for 1 s and the poller
// runs at 1 Hz, so roughly half the samples repeat their predecessor
// and carry no information about the trough).
type bucket struct {
	x        float64
	min, max float64
	n        int
	distinct int
}

// buckets reduces samples to one bucket per slopeBucket, anchored on
// the first sample. A trailing bucket holding fewer than half a
// bucket's worth of samples is dropped so an incomplete bucket (which
// may not contain a trough yet) cannot bias the tail.
func buckets(samples []Snapshot, y func(Snapshot) float64) []bucket {
	if len(samples) == 0 {
		return nil
	}
	width := int64(slopeBucket / time.Second)
	t0 := samples[0].TS
	var out []bucket
	var cur int64 = -1
	var b bucket
	var last float64
	flush := func() {
		if cur >= 0 && int64(b.n) >= width/2 {
			out = append(out, b)
		}
	}
	for _, s := range samples {
		idx := (s.TS - t0) / width
		v := y(s)
		if idx != cur {
			flush()
			cur = idx
			b = bucket{x: float64(idx * width), min: v, max: v, distinct: 1}
			last = v
		} else {
			if v < b.min {
				b.min = v
			}
			if v > b.max {
				b.max = v
			}
			if v != last {
				b.distinct++
				last = v
			}
		}
		b.n++
	}
	flush()
	return out
}

// troughs returns the per-bucket minima as regression points.
func troughs(bs []bucket) []point {
	out := make([]point, 0, len(bs))
	for _, b := range bs {
		out = append(out, point{x: b.x, y: b.min})
	}
	return out
}

// slopeOf returns the least-squares slope (units per second) of the
// bucket-trough series of y over samples. Returns 0 for degenerate
// inputs (fewer than two buckets, zero variance in x).
func slopeOf(samples []Snapshot, y func(Snapshot) float64) float64 {
	return slope(troughs(buckets(samples, y)))
}

// slope returns the least-squares slope of y against x over points.
// Returns 0 for degenerate inputs (single point, zero variance in x).
func slope(points []point) float64 {
	n := float64(len(points))
	if n < 2 {
		return 0
	}
	var sx, sy, sxy, sxx float64
	for _, p := range points {
		sx += p.x
		sy += p.y
		sxy += p.x * p.y
		sxx += p.x * p.x
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / denom
}

// troughLevel is the mean trough over the buckets: the "size" of the
// series the relative floor scales with.
func troughLevel(bs []bucket) float64 {
	if len(bs) == 0 {
		return 0
	}
	var sum float64
	for _, b := range bs {
		sum += b.min
	}
	return sum / float64(len(bs))
}

// samplingNoise estimates how far a bucket minimum can sit from the
// true trough by chance: the median over buckets of (peak - trough) /
// distinct samples. For a GC sawtooth of amplitude A sampled at n
// distinct points that is ~A/n, the expected gap between the lowest
// sample and the real trough (and its standard deviation). For a
// series without a sawtooth (goroutines, RSS, a pure leak) the
// per-bucket range is tiny and the estimate vanishes.
func samplingNoise(bs []bucket) float64 {
	if len(bs) == 0 {
		return 0
	}
	v := make([]float64, 0, len(bs))
	for _, b := range bs {
		d := b.distinct
		if d < 1 {
			d = 1
		}
		v = append(v, (b.max-b.min)/float64(d))
	}
	sort.Float64s(v)
	if n := len(v); n%2 == 1 {
		return v[n/2]
	} else {
		return (v[n/2-1] + v[n/2]) / 2
	}
}

// heapSlope returns the trough slope (bytes per second) of
// HeapInuseBytes over samples.
func heapSlope(samples []Snapshot) float64 {
	return slopeOf(samples, func(s Snapshot) float64 { return float64(s.HeapInuseBytes) })
}

// windowSpan formats the judged window for violation messages.
func windowSpan(samples []Snapshot) string {
	first, last := samples[0], samples[len(samples)-1]
	return fmt.Sprintf("%d samples over %s", len(samples), time.Duration(last.TS-first.TS)*time.Second)
}

// slopeSpec is one slope predicate's parameters; judge is the shared
// verdict.
type slopeSpec struct {
	id       string
	what     string // series name for messages
	window   time.Duration
	budget   float64 // units per second
	relFloor float64 // fraction of the trough level
	y        func(Snapshot) float64
	keep     func(Snapshot) bool
	fmtV     func(float64) string // formats a series value
}

// skipWindow is the [Skip] reason while a slope window is not judgeable.
var skipWindow = fmt.Sprintf("slope window not judgeable yet (needs %s warm-up, then %s / %d samples)",
	slopeWarmup, slopeMinSpan, slopeMinSamples)

// judge evaluates the slope predicate over ctx. The verdict is a
// violation only when the trough slope exceeds the budget AND the
// fitted rise over the window (slope x span) clears the floor
//
//	max(budget x slopeMinSpan, relFloor x trough level, slopeNoiseK x sampling noise)
//
// so that (a) a single step smaller than the budgeted 10 min total,
// (b) a step proportionate to the series' size, and (c) the sampling
// noise of a GC sawtooth at this heap size can each not produce a
// verdict on their own. The message carries every number so a triage
// can see which floor was cleared and by how much.
func (sp slopeSpec) judge(ctx Context) (bool, string) {
	samples, ok := slopeWindow(ctx, sp.window, sp.keep)
	if !ok {
		return Skip(skipWindow)
	}
	bs := buckets(samples, sp.y)
	s := slope(troughs(bs))
	if s <= sp.budget {
		return true, ""
	}
	// Two estimates of how much the series rose across the window: the
	// fitted line over the trough span, and the raw last-minus-first
	// trough. A leak drives both; a single step of D shows up as a
	// fitted rise of ~1.6 D (four troughs, two low, two high) but a raw
	// rise of exactly D, and sawtooth noise moves them differently.
	// The verdict uses the smaller one, so a step counts as its own
	// size and both estimates have to clear the floor.
	pts := troughs(bs)
	fitted := s * (pts[len(pts)-1].x - pts[0].x)
	raw := pts[len(pts)-1].y - pts[0].y
	rise := math.Min(fitted, raw)
	level := troughLevel(bs)
	noise := samplingNoise(bs)
	floorBudget := sp.budget * slopeMinSpan.Seconds()
	floorRel := sp.relFloor * level
	floorNoise := slopeNoiseK * noise
	floor := math.Max(floorBudget, math.Max(floorRel, floorNoise))
	if rise < floor {
		return true, ""
	}
	return false, fmt.Sprintf(
		"%s violated: %s trough slope %s/s exceeds %s/s budget; rise %s (fitted %s, trough-to-trough %s) over %s (%d troughs) clears the floor %s = max(budget x %s = %s, %.0f%% of level %s = %s, %gx sampling noise %s = %s); %s -> %s",
		sp.id, sp.what, sp.fmtV(s), sp.fmtV(sp.budget), sp.fmtV(rise), sp.fmtV(fitted), sp.fmtV(raw), windowSpan(samples), len(bs),
		sp.fmtV(floor), slopeMinSpan, sp.fmtV(floorBudget), sp.relFloor*100, sp.fmtV(level), sp.fmtV(floorRel),
		slopeNoiseK, sp.fmtV(noise), sp.fmtV(floorNoise),
		sp.fmtV(sp.y(samples[0])), sp.fmtV(sp.y(samples[len(samples)-1])))
}

// fmtBytes renders a byte quantity for messages.
func fmtBytes(v float64) string {
	switch {
	case math.Abs(v) >= 1<<30:
		return fmt.Sprintf("%.2f GB", v/(1<<30))
	case math.Abs(v) >= 1<<20:
		return fmt.Sprintf("%.2f MB", v/(1<<20))
	case math.Abs(v) >= 1<<10:
		return fmt.Sprintf("%.1f KB", v/(1<<10))
	}
	return fmt.Sprintf("%.0f B", v)
}

// fmtCount renders a count for messages.
func fmtCount(v float64) string { return fmt.Sprintf("%.2f", v) }

var heapSlopeSpec = slopeSpec{
	id: "I-MEM-1", what: "heap_inuse", window: heapSlopeWindow,
	budget: heapSlopeMaxBytesPerSec, relFloor: heapRiseRelFloor,
	y: func(s Snapshot) float64 { return float64(s.HeapInuseBytes) }, fmtV: fmtBytes,
}

var goroutineSlopeSpec = slopeSpec{
	id: "I-MEM-3", what: "goroutine count", window: goroutineSlopeWindow,
	budget: goroutineSlopeMaxPerSec, relFloor: goroutineRiseRelFloor,
	y: func(s Snapshot) float64 { return float64(s.GoroutineCount) }, fmtV: fmtCount,
}

var rssSlopeSpec = slopeSpec{
	id: "I-MEM-4", what: "RSS", window: rssSlopeWindow,
	budget: rssSlopeMaxBytesPerSec, relFloor: rssRiseRelFloor,
	y:    func(s Snapshot) float64 { return float64(s.RSSBytes) },
	keep: func(s Snapshot) bool { return s.RSSBytes > 0 }, fmtV: fmtBytes,
}

// IMEM1 asserts heap_inuse trough slope is bounded over the trailing
// window min(1h, elapsed) excluding the first 5 minutes of warm-up,
// and that the fitted rise clears the rise floor (see slopeSpec.judge).
// Skips until 10 minutes of post-warm-up samples exist.
//
// Refapps whose in-memory TTL stores keep growing past warm-up (a
// session per cookieless request until celeris#487's fix reaches the
// pinned celeris) are EXPECTED true positives here, not noise.
var IMEM1 = Spec{
	ID: "I-MEM-1",
	Description: fmt.Sprintf("heap_inuse trough slope ≤ %s/s over trailing min(1h, elapsed) after %s warm-up, rise ≥ max(%s, %.0f%% of level, %gx sampling noise)",
		fmtBytes(heapSlopeMaxBytesPerSec), slopeWarmup, fmtBytes(heapSlopeMaxBytesPerSec*slopeMinSpan.Seconds()), heapRiseRelFloor*100, slopeNoiseK),
	Tier:      "core",
	Persist:   slopePersistSamples,
	Predicate: func(_ *Snapshot, ctx Context) (bool, string) { return heapSlopeSpec.judge(ctx) },
}

// IMEM3 asserts the goroutine count trough slope is bounded over the
// trailing 10 minutes after the 5 minute warm-up. This is the
// always-on complement of I-MEM-2 (which needs an idle window the
// orchestrator never enters under load): a per-connection / per-stream
// goroutine that is never reaped shows up as a sustained positive
// slope while Tier 1's fixed-concurrency load keeps a healthy count
// stationary. Would have caught celeris#494 at t≈17.5min instead of
// at 17,704 goroutines after 24h.
var IMEM3 = Spec{
	ID: "I-MEM-3",
	Description: fmt.Sprintf("goroutine count trough slope ≤ %g/s over trailing %s after %s warm-up, rise ≥ max(%.0f, %.0f%% of level, %gx jitter)",
		goroutineSlopeMaxPerSec, goroutineSlopeWindow, slopeWarmup, goroutineSlopeMaxPerSec*slopeMinSpan.Seconds(), goroutineRiseRelFloor*100, slopeNoiseK),
	Tier:      "core",
	Persist:   slopePersistSamples,
	Predicate: func(_ *Snapshot, ctx Context) (bool, string) { return goroutineSlopeSpec.judge(ctx) },
}

// IMEM4 asserts the refapp's resident set (VmRSS) trough slope is
// bounded over the trailing min(1h, elapsed) after the 5 minute
// warm-up. Catches growth the Go heap counters cannot see -- off-heap
// buffers, io_uring registered memory, cgo, a runaway page cache of
// mmap'd files -- and cross-checks I-MEM-1 from the kernel's point of
// view.
//
// RSS is sampled from /proc/<pid>/status by the orchestrator (linux,
// local driver only). When no pid is known the field stays 0 and the
// predicate skips; samples with RSSBytes == 0 are excluded from the
// window so a late-starting sampler never produces a fake ramp.
var IMEM4 = Spec{
	ID: "I-MEM-4",
	Description: fmt.Sprintf("RSS trough slope ≤ %s/s over trailing min(1h, elapsed) after %s warm-up, rise ≥ max(%s, %.0f%% of level) (skipped when RSS is not sampled)",
		fmtBytes(rssSlopeMaxBytesPerSec), slopeWarmup, fmtBytes(rssSlopeMaxBytesPerSec*slopeMinSpan.Seconds()), rssRiseRelFloor*100),
	Tier:    "core",
	Persist: slopePersistSamples,
	Predicate: func(snap *Snapshot, ctx Context) (bool, string) {
		if snap.RSSBytes <= 0 {
			return Skip("RSS not sampled (no pid, non-linux host, or remote refapp)")
		}
		return rssSlopeSpec.judge(ctx)
	},
}

// IMEM2 asserts goroutine count returns to baseline+pad once the
// orchestrator declares an idle window. Catches the classic
// "stuck-handler" leak where a panicking middleware leaves its
// per-request goroutine wedged.
//
// Gated on ctx.IdleMode (only the orchestrator can enter idle mode,
// after letting the load die down) plus a 30s settle period after
// idle-mode start. The settle period uses ctx.History as a proxy: if
// the most recent in-history snapshot already has IdleMode==true for
// at least goroutineSettleDuration of contiguous samples, the
// assertion is live.
var IMEM2 = Spec{
	ID:          "I-MEM-2",
	Description: "goroutines return to baseline+8 after 30s idle",
	Tier:        "core",
	Predicate: func(snap *Snapshot, ctx Context) (bool, string) {
		if !ctx.IdleMode {
			return Skip("no idle window (Context.IdleMode is false)")
		}
		// The orchestrator sets ctx.IdleMode for every snapshot inside
		// an idle window; count how many of the trailing samples in
		// History have a TS within the settle period. (The IdleMode
		// field on Snapshot is the orchestrator's signal; if not yet
		// fed through we conservatively skip.)
		needed := int(goroutineSettleDuration / time.Second)
		if len(ctx.History) < needed {
			return Skip("idle window has not settled yet")
		}
		budget := ctx.BaselineGoroutines + goroutineBaselinePad
		if snap.GoroutineCount > budget {
			return false, fmt.Sprintf(
				"I-MEM-2 violated: idle goroutine count %d exceeds baseline(%d)+%d=%d",
				snap.GoroutineCount, ctx.BaselineGoroutines, goroutineBaselinePad, budget)
		}
		return true, ""
	},
}
