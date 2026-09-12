package properties

import (
	"strings"
	"testing"
	"time"
)

// idleSamples builds n 1 Hz samples starting at ts, all in idle window w
// with goroutine count g.
func idleSamples(ts int64, n, w int, g int64) []Snapshot {
	out := make([]Snapshot, 0, n)
	for i := 0; i < n; i++ {
		out = append(out, Snapshot{TS: ts + int64(i), IdleWindow: w, GoroutineCount: g})
	}
	return out
}

func lastOf(h []Snapshot) *Snapshot { return &h[len(h)-1] }

func TestIMEM2_SkipsUntilTheSecondWindowHasSettled(t *testing.T) {
	base := int64(1_700_000_000)

	// Window 1 establishes the baseline; nothing is judged there, however
	// high the count.
	h := idleSamples(base, 40, 1, 500)
	ctx := Context{IdleWindow: 1, IdleMode: true, History: h}
	if ok, msg := IMEM2.Predicate(lastOf(h), ctx); !IsSkip(ok, msg) || !strings.Contains(msg, "first idle window") {
		t.Fatalf("window 1 must skip, got ok=%v msg=%q", ok, msg)
	}

	// Window 2 without a baseline (window 1 was never left) skips too.
	h = idleSamples(base, 40, 2, 500)
	ctx = Context{IdleWindow: 2, IdleMode: true, History: h}
	if ok, msg := IMEM2.Predicate(lastOf(h), ctx); !IsSkip(ok, msg) || !strings.Contains(msg, "no idle baseline") {
		t.Fatalf("window 2 without baseline must skip, got ok=%v msg=%q", ok, msg)
	}

	// Window 2 with a baseline, but only 10 s in: not settled.
	h = idleSamples(base, 10, 2, 500)
	ctx = Context{IdleWindow: 2, IdleMode: true, IdleBaselineGoroutines: 44, History: h}
	if ok, msg := IMEM2.Predicate(lastOf(h), ctx); !IsSkip(ok, msg) || !strings.Contains(msg, "not settled") {
		t.Fatalf("10 s into window 2 must skip, got ok=%v msg=%q", ok, msg)
	}

	// 31 s in: judged, and 500 against 44+8 is a violation.
	h = idleSamples(base, 31, 2, 500)
	ctx.History = h
	if ok, msg := IMEM2.Predicate(lastOf(h), ctx); ok {
		t.Fatalf("settled window 2 over budget must fail, got msg=%q", msg)
	}
}

// The reference is the first idle window's settled count, not the count
// at readiness: a refapp that grew from 40 to 84 under its first load and
// kept that at idle passes at 90 (84+8) and fails at 93.
func TestIMEM2_JudgesAgainstTheFirstIdleWindowNotReadiness(t *testing.T) {
	base := int64(1_700_000_000)
	ctx := Context{IdleWindow: 2, IdleMode: true, BaselineGoroutines: 40, IdleBaselineGoroutines: 84}

	h := idleSamples(base, 31, 2, 90)
	ctx.History = h
	if ok, msg := IMEM2.Predicate(lastOf(h), ctx); !ok || IsSkip(ok, msg) {
		t.Fatalf("90 against idle baseline 84+8 must pass (readiness baseline 40 is not the reference), got ok=%v msg=%q", ok, msg)
	}

	h = idleSamples(base, 31, 2, 93)
	ctx.History = h
	ok, msg := IMEM2.Predicate(lastOf(h), ctx)
	if ok {
		t.Fatalf("93 against 84+8 must fail")
	}
	if !strings.Contains(msg, "baseline(84)+8=92") || !strings.Contains(msg, "idle window 2") {
		t.Fatalf("violation must name the idle baseline, pad and window, got %q", msg)
	}
}

// The settle clock starts at the first sample of the CURRENT window: 40 s
// of load samples followed by 20 s of window 2 is not settled, even
// though History spans 60 s.
func TestIMEM2_SettleClockStartsAtTheWindow(t *testing.T) {
	base := int64(1_700_000_000)
	h := append(idleSamples(base, 40, 0, 90), idleSamples(base+40, 20, 2, 500)...)
	ctx := Context{IdleWindow: 2, IdleMode: true, IdleBaselineGoroutines: 44, History: h}
	if ok, msg := IMEM2.Predicate(lastOf(h), ctx); !IsSkip(ok, msg) {
		t.Fatalf("20 s into window 2 must skip regardless of older history, got ok=%v msg=%q", ok, msg)
	}
	h = append(h, idleSamples(base+60, 11, 2, 500)...)
	ctx.History = h
	if ok, msg := IMEM2.Predicate(lastOf(h), ctx); ok {
		t.Fatalf("31 s into window 2 must be judged, got skip %q", msg)
	}
}

// The slope predicates' warm-up runs from the start of sustained load
// when the cell idled: the prelude's resume transient stays out of the fit.
func TestSlopeWindow_WarmsUpFromLoadStart(t *testing.T) {
	base := int64(1_700_000_000)
	h := make([]Snapshot, 0, 1801)
	for i := 0; i <= 1800; i++ {
		h = append(h, Snapshot{TS: base + int64(i), HeapInuseBytes: 1})
	}
	t0 := time.Unix(base, 0)
	ctx := Context{RunStartedAt: t0, Now: t0.Add(30 * time.Minute), History: h}

	// No idle prelude: cutoff = max(now-20m, run+5m) = t0+10m.
	got, ok := slopeWindow(ctx, 20*time.Minute, nil)
	if !ok || got[0].TS != base+600 {
		t.Fatalf("without LoadStartedAt the window must start at run+10m, got ok=%v first=%d", ok, got[0].TS-base)
	}

	// Sustained load began at t0+7m: warm-up ends at t0+12m, which now
	// dominates the trailing window.
	ctx.LoadStartedAt = t0.Add(7 * time.Minute)
	got, ok = slopeWindow(ctx, 20*time.Minute, nil)
	if !ok || got[0].TS != base+720 {
		t.Fatalf("with LoadStartedAt=+7m the window must start at +12m, got ok=%v first=%d", ok, got[0].TS-base)
	}
}
