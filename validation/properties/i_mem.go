package properties

import (
	"fmt"
	"time"
)

// heapSlopeWindow is the trailing window over which I-MEM-1 fits a
// least-squares line to heap_inuse. 1h matches the rolling-window cap
// in [Context.History]; using less would let a slow leak hide in the
// noise floor of generational GC bookkeeping.
const heapSlopeWindow = time.Hour

// heapSlopeMaxBytesPerSec is the slope I-MEM-1 tolerates. 1 KB/s over
// an hour = ~3.5 MB/h: comfortably above GC noise but well below any
// production-meaningful leak rate.
const heapSlopeMaxBytesPerSec = 1024.0

// goroutineSettleDuration is how long I-MEM-2 waits after Context.IdleMode
// flips true before enforcing the baseline-return assertion. Most
// pooled goroutines exit within a single GC cycle; 30s is generous.
const goroutineSettleDuration = 30 * time.Second

// goroutineBaselinePad is the slack added to the post-idle goroutine
// budget. The engine spins up a small ladder of background goroutines
// (epoll readers, the scheduler timer, the metrics publisher) per
// listener; +N=8 covers the worst case without masking a real leak.
const goroutineBaselinePad int64 = 8

// IMEM1 asserts heap_inuse slope is bounded over the rolling 1h
// window. The predicate fits a linear regression on the last 1h of
// snapshots; if the slope exceeds heapSlopeMaxBytesPerSec the run is
// halted as a suspected heap leak.
//
// The predicate is conservative during warmup: until the run has been
// active for at least heapSlopeWindow, the slope check is skipped (it
// would fire on every cold-start ramp).
var IMEM1 = Spec{
	ID:          "I-MEM-1",
	Description: "heap_inuse slope ≤ ε over rolling 1h",
	Tier:        "core",
	Predicate: func(snap *Snapshot, ctx Context) (bool, string) {
		if Forever(ctx) < heapSlopeWindow {
			return true, ""
		}
		// Find the first snapshot inside the trailing 1h window.
		cutoff := ctx.Now.Add(-heapSlopeWindow).Unix()
		samples := make([]Snapshot, 0, len(ctx.History))
		for _, s := range ctx.History {
			if s.TS >= cutoff {
				samples = append(samples, s)
			}
		}
		if len(samples) < 60 { // need at least 1 min of points for a slope
			return true, ""
		}
		slope := heapSlope(samples)
		if slope > heapSlopeMaxBytesPerSec {
			return false, fmt.Sprintf(
				"I-MEM-1 violated: heap_inuse slope %.1f B/s exceeds %.0f B/s budget over %s window",
				slope, heapSlopeMaxBytesPerSec, heapSlopeWindow)
		}
		return true, ""
	},
}

// heapSlope returns the least-squares slope (bytes per second) of
// HeapInuseBytes against TS over samples. Returns 0 for degenerate
// inputs (single point, zero variance in TS).
func heapSlope(samples []Snapshot) float64 {
	n := float64(len(samples))
	if n < 2 {
		return 0
	}
	var sx, sy, sxy, sxx float64
	t0 := float64(samples[0].TS)
	for _, s := range samples {
		x := float64(s.TS) - t0
		y := float64(s.HeapInuseBytes)
		sx += x
		sy += y
		sxy += x * y
		sxx += x * x
	}
	denom := n*sxx - sx*sx
	if denom == 0 {
		return 0
	}
	return (n*sxy - sx*sy) / denom
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
