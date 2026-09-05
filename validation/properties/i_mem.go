package properties

import (
	"fmt"
	"time"
)

// Slope predicates (I-MEM-1, I-MEM-3, I-MEM-4) share one shape: fit a
// least-squares line to a resource series over a trailing window of
// [Context.History] and fail when the slope exceeds a budget. The
// window rules are common to all three:
//
//   - The first slopeWarmup (5 min) of the run is never judged. A cold
//     start ramps every series (pools fill, caches warm, the engine
//     spins up its goroutine ladder) and would fire on every cell.
//   - The window must span at least slopeMinSpan (10 min) of samples
//     AND hold at least slopeMinSamples points before a verdict. A
//     sparse history (poll failures) is not judged as if it were dense.
//   - Until both hold the predicate returns ok (skip), so a 1h matrix
//     cell starts judging at t=15min rather than never.
//   - The fit runs over per-150s bucket MINIMA (see troughs), not raw
//     samples, so a GC sawtooth cannot masquerade as a slope.
//
// The 24h soak of 2026-09-04 reached an 18 GB heap and 17,704
// goroutines (celeris#494, three goroutines leaked per killed SSE
// stream) with properties_passed=0: I-MEM-1 skipped until the run had
// lasted a full hour and nothing watched the goroutine count at all.
// At 1 stream-kill/s that leak is a 3 goroutine/s slope -- 15x the
// I-MEM-3 budget -- and now trips at t=15min.
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

// heapSlopeWindow is the trailing window over which I-MEM-1 fits a
// least-squares line to heap_inuse: min(1h, elapsed since warm-up).
// 1h matches the rolling-window cap in [Context.History].
const heapSlopeWindow = time.Hour

// heapSlopeMaxBytesPerSec is the slope I-MEM-1 tolerates. 1 KB/s over
// an hour = ~3.5 MB/h: comfortably above GC noise but well below any
// production-meaningful leak rate.
const heapSlopeMaxBytesPerSec = 1024.0

// goroutineSlopeWindow is the trailing window I-MEM-3 fits over.
const goroutineSlopeWindow = 10 * time.Minute

// goroutineSlopeMaxPerSec is the goroutine-count slope I-MEM-3
// tolerates: 0.2/s = 120 goroutines over the 10 min window. Tier 1
// runs a FIXED walker concurrency per cell, so a healthy refapp's
// goroutine count is stationary (request-driven jitter only); any
// sustained slope is a leak.
const goroutineSlopeMaxPerSec = 0.2

// rssSlopeWindow is the trailing window I-MEM-4 fits over.
const rssSlopeWindow = 10 * time.Minute

// rssSlopeMaxBytesPerSec is the RSS slope I-MEM-4 tolerates: 64 KB/s =
// ~37.5 MB over the 10 min window. Looser than the heap budget because
// RSS also moves with the Go runtime's scavenger and page-cache
// behaviour, not just the live heap.
const rssSlopeMaxBytesPerSec = 64 * 1024.0

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

// slopeBucket is the width of the trough buckets slopeOf regresses
// over. A GC cycle is a sawtooth (slow ramp, instant drop) and a plain
// least-squares fit over one has a positive bias of roughly
// amplitude*period/window^2 -- a 500 MB live heap with 5 s cycles over
// a 10 min window reads as ~7 KB/s with no leak at all. The per-bucket
// minimum is the post-GC trough: flat for a healthy process, rising
// under a leak. The bucket must be wider than the longest possible GC
// period: the runtime's sysmon forces a collection whenever none has
// run for 2 minutes (runtime.forcegcperiod), so 150 s guarantees at
// least one trough per bucket at any allocation rate. A 10 min window
// yields 4 trough points, the 1 h heap window 24.
const slopeBucket = 150 * time.Second

// troughs reduces samples to one point per slopeBucket: the minimum of
// y over the bucket, at the bucket's start offset. A trailing bucket
// holding fewer than half a bucket's worth of samples is dropped so an
// incomplete bucket (which may not contain a trough yet) cannot bias
// the tail.
func troughs(samples []Snapshot, y func(Snapshot) float64) []point {
	if len(samples) == 0 {
		return nil
	}
	width := int64(slopeBucket / time.Second)
	t0 := samples[0].TS
	var out []point
	var cur int64 = -1
	var curMin float64
	var curN int
	flush := func() {
		if cur >= 0 && int64(curN) >= width/2 {
			out = append(out, point{x: float64(cur * width), y: curMin})
		}
	}
	for _, s := range samples {
		b := (s.TS - t0) / width
		v := y(s)
		if b != cur {
			flush()
			cur, curMin, curN = b, v, 0
		}
		if v < curMin {
			curMin = v
		}
		curN++
	}
	flush()
	return out
}

// slopeOf returns the least-squares slope (units per second) of the
// bucket-trough series of y over samples. Returns 0 for degenerate
// inputs (fewer than two buckets, zero variance in x).
func slopeOf(samples []Snapshot, y func(Snapshot) float64) float64 {
	return slope(troughs(samples, y))
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

// IMEM1 asserts heap_inuse slope is bounded over the trailing window
// min(1h, elapsed) excluding the first 5 minutes of warm-up. The
// predicate fits a linear regression on the window; if the slope
// exceeds heapSlopeMaxBytesPerSec the run is halted as a suspected
// heap leak. Skips until 10 minutes of post-warm-up samples exist.
var IMEM1 = Spec{
	ID:          "I-MEM-1",
	Description: "heap_inuse slope ≤ 1 KB/s over trailing min(1h, elapsed) after 5 min warm-up (judged from 10 min of samples)",
	Tier:        "core",
	Predicate: func(_ *Snapshot, ctx Context) (bool, string) {
		samples, ok := slopeWindow(ctx, heapSlopeWindow, nil)
		if !ok {
			return true, ""
		}
		s := heapSlope(samples)
		if s > heapSlopeMaxBytesPerSec {
			return false, fmt.Sprintf(
				"I-MEM-1 violated: heap_inuse slope %.1f B/s exceeds %.0f B/s budget (%s, %d -> %d bytes)",
				s, heapSlopeMaxBytesPerSec, windowSpan(samples),
				samples[0].HeapInuseBytes, samples[len(samples)-1].HeapInuseBytes)
		}
		return true, ""
	},
}

// IMEM3 asserts the goroutine count slope is bounded over the trailing
// 10 minutes after the 5 minute warm-up. This is the always-on
// complement of I-MEM-2 (which needs an idle window the orchestrator
// never enters under load): a per-connection / per-stream goroutine
// that is never reaped shows up as a sustained positive slope while
// Tier 1's fixed-concurrency load keeps a healthy count stationary.
// Would have caught celeris#494 at t=15min instead of at 17,704
// goroutines after 24h.
var IMEM3 = Spec{
	ID:          "I-MEM-3",
	Description: "goroutine count slope ≤ 0.2/s over trailing 10 min after 5 min warm-up",
	Tier:        "core",
	Predicate: func(_ *Snapshot, ctx Context) (bool, string) {
		samples, ok := slopeWindow(ctx, goroutineSlopeWindow, nil)
		if !ok {
			return true, ""
		}
		s := slopeOf(samples, func(s Snapshot) float64 { return float64(s.GoroutineCount) })
		if s > goroutineSlopeMaxPerSec {
			return false, fmt.Sprintf(
				"I-MEM-3 violated: goroutine slope %.2f/s exceeds %.2f/s budget (%s, %d -> %d goroutines)",
				s, goroutineSlopeMaxPerSec, windowSpan(samples),
				samples[0].GoroutineCount, samples[len(samples)-1].GoroutineCount)
		}
		return true, ""
	},
}

// IMEM4 asserts the refapp's resident set (VmRSS) slope is bounded over
// the trailing 10 minutes after the 5 minute warm-up. Catches growth
// the Go heap counters cannot see -- off-heap buffers, io_uring
// registered memory, cgo, a runaway page cache of mmap'd files -- and
// cross-checks I-MEM-1 from the kernel's point of view.
//
// RSS is sampled from /proc/<pid>/status by the orchestrator (linux,
// local driver only). When no pid is known the field stays 0 and the
// predicate skips; samples with RSSBytes == 0 are excluded from the
// window so a late-starting sampler never produces a fake ramp.
var IMEM4 = Spec{
	ID:          "I-MEM-4",
	Description: "RSS slope ≤ 64 KB/s over trailing 10 min after 5 min warm-up (skipped when RSS is not sampled)",
	Tier:        "core",
	Predicate: func(snap *Snapshot, ctx Context) (bool, string) {
		if snap.RSSBytes <= 0 {
			return true, ""
		}
		samples, ok := slopeWindow(ctx, rssSlopeWindow, func(s Snapshot) bool { return s.RSSBytes > 0 })
		if !ok {
			return true, ""
		}
		s := slopeOf(samples, func(s Snapshot) float64 { return float64(s.RSSBytes) })
		if s > rssSlopeMaxBytesPerSec {
			return false, fmt.Sprintf(
				"I-MEM-4 violated: RSS slope %.1f B/s exceeds %.0f B/s budget (%s, %d -> %d bytes)",
				s, rssSlopeMaxBytesPerSec, windowSpan(samples),
				samples[0].RSSBytes, samples[len(samples)-1].RSSBytes)
		}
		return true, ""
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
			return true, ""
		}
		// The orchestrator sets ctx.IdleMode for every snapshot inside
		// an idle window; count how many of the trailing samples in
		// History have a TS within the settle period. (The IdleMode
		// field on Snapshot is the orchestrator's signal; if not yet
		// fed through we conservatively skip.)
		needed := int(goroutineSettleDuration / time.Second)
		if len(ctx.History) < needed {
			return true, ""
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
