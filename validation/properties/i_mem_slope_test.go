package properties

import (
	"strings"
	"testing"
	"time"
)

// leakHistory builds a 1 Hz History of n samples starting at the run
// start. fill receives the sample index (seconds since run start) and
// returns the (goroutines, heap_inuse, rss) triple for that second.
func leakHistory(n int, fill func(i int) (goroutines, heap, rss int64)) []Snapshot {
	base := int64(1_700_000_000)
	h := make([]Snapshot, 0, n+1)
	for i := 0; i <= n; i++ {
		g, hp, r := fill(i)
		h = append(h, Snapshot{TS: base + int64(i), GoroutineCount: g, HeapInuseBytes: hp, RSSBytes: r})
	}
	return h
}

// slopeCtx is ctxWith positioned at the last sample of history.
func slopeCtx(history []Snapshot) Context {
	last := history[len(history)-1]
	ctx := ctxWith(0, history, false)
	ctx.Now = time.Unix(last.TS, 0)
	return ctx
}

// celeris#494: three goroutines leaked per killed SSE stream. At one kill
// per second that is a 3/s slope -- 15x the I-MEM-3 budget -- and it
// must trip within the first 10 minutes after the 5 minute warm-up.
func TestIMEM3_failsOnGoroutineLeakWithinTenMinutes(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		return 100 + 3*int64(i), 100 << 20, 0
	})
	ok, msg := IMEM3.Predicate(&h[len(h)-1], slopeCtx(h))
	if ok {
		t.Fatalf("expected violation at t=15min for a 3 goroutine/s leak; msg=%q", msg)
	}
	if !strings.Contains(msg, "I-MEM-3") {
		t.Fatalf("missing tag: %q", msg)
	}
}

// A stationary goroutine count with request-driven jitter must pass.
func TestIMEM3_passesFlatWithJitter(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		return 120 + int64(i%7) - 3, 100 << 20, 0
	})
	ok, msg := IMEM3.Predicate(&h[len(h)-1], slopeCtx(h))
	if !ok {
		t.Fatalf("flat goroutine count must pass; msg=%q", msg)
	}
}

// A cold-start ramp confined to the warm-up window is excluded: the
// count climbs steeply for 5 minutes and is flat afterwards.
func TestIMEM3_ignoresWarmupRamp(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		if i < 5*60 {
			return 10 + 5*int64(i), 100 << 20, 0
		}
		return 1510, 100 << 20, 0
	})
	ok, msg := IMEM3.Predicate(&h[len(h)-1], slopeCtx(h))
	if !ok {
		t.Fatalf("warm-up ramp must be excluded; msg=%q", msg)
	}
}

// Before 10 minutes of post-warm-up samples exist the predicate skips,
// even when the leak is already visible.
func TestIMEM3_skipsUntilTenMinutesAfterWarmup(t *testing.T) {
	for _, minutes := range []int{4, 10, 14} {
		h := leakHistory(minutes*60, func(i int) (int64, int64, int64) {
			return 100 + 3*int64(i), 100 << 20, 0
		})
		if ok, msg := IMEM3.Predicate(&h[len(h)-1], slopeCtx(h)); !ok {
			t.Fatalf("t=%dmin: must skip before the window is full; msg=%q", minutes, msg)
		}
	}
}

// RSS growing at 100 KB/s (above the 64 KB/s budget) trips I-MEM-4.
func TestIMEM4_failsOnRSSLeak(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		return 100, 100 << 20, 200<<20 + 100*1024*int64(i)
	})
	ok, msg := IMEM4.Predicate(&h[len(h)-1], slopeCtx(h))
	if ok {
		t.Fatalf("expected violation for a 100 KB/s RSS slope; msg=%q", msg)
	}
	if !strings.Contains(msg, "I-MEM-4") {
		t.Fatalf("missing tag: %q", msg)
	}
}

func TestIMEM4_passesFlatRSS(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		return 100, 100 << 20, 200<<20 + int64(i%13)*4096
	})
	if ok, msg := IMEM4.Predicate(&h[len(h)-1], slopeCtx(h)); !ok {
		t.Fatalf("flat RSS must pass; msg=%q", msg)
	}
}

// No pid => RSS never sampled => the predicate must skip, not fire on a
// zero series and not fire on the transition from 0 to a real value.
func TestIMEM4_skipsWithoutRSS(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) { return 100, 100 << 20, 0 })
	if ok, msg := IMEM4.Predicate(&h[len(h)-1], slopeCtx(h)); !ok {
		t.Fatalf("RSS=0 must skip; msg=%q", msg)
	}
}

// I-MEM-1 used to skip until the run had lasted a full hour, so a 1h
// matrix cell evaluated it (at most) once. It now judges the slope once
// 10 minutes of post-warm-up samples exist.
func TestIMEM1_failsWithinFifteenMinutes(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		return 100, 100<<20 + 2048*int64(i), 0
	})
	ok, msg := IMEM1.Predicate(&h[len(h)-1], slopeCtx(h))
	if ok {
		t.Fatalf("expected violation at t=15min for a 2 KB/s heap slope; msg=%q", msg)
	}
	if !strings.Contains(msg, "I-MEM-1") {
		t.Fatalf("missing tag: %q", msg)
	}
}

func TestIMEM1_passesFlatWithinFifteenMinutes(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		// GC sawtooth: +-8 MB around a flat mean.
		return 100, 100<<20 + int64(i%60)*256*1024 - 8<<20, 0
	})
	if ok, msg := IMEM1.Predicate(&h[len(h)-1], slopeCtx(h)); !ok {
		t.Fatalf("flat heap must pass; msg=%q", msg)
	}
}

func TestIMEM1_ignoresWarmupRamp(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		if i < 5*60 {
			return 100, 10<<20 + 1<<20*int64(i), 0
		}
		return 100, 310 << 20, 0
	})
	if ok, msg := IMEM1.Predicate(&h[len(h)-1], slopeCtx(h)); !ok {
		t.Fatalf("warm-up ramp must be excluded; msg=%q", msg)
	}
}

// Every slope predicate needs the window to span 10 minutes of samples,
// not merely 60 points: a sparse history (poll failures) must not be
// judged as if it were dense.
func TestSlopePredicates_requireSpanNotJustCount(t *testing.T) {
	// 70 samples, 1 Hz, all after warm-up -- count is fine, span is not.
	base := int64(1_700_000_000)
	h := make([]Snapshot, 0, 70)
	for i := 0; i < 70; i++ {
		h = append(h, Snapshot{TS: base + 6*60 + int64(i), GoroutineCount: 100 + 50*int64(i), HeapInuseBytes: 1 << 20 * int64(i), RSSBytes: 1 << 20 * int64(i)})
	}
	ctx := ctxWith(0, h, false)
	ctx.Now = time.Unix(h[len(h)-1].TS, 0)
	for _, s := range []Spec{IMEM1, IMEM3, IMEM4} {
		if ok, msg := s.Predicate(&h[len(h)-1], ctx); !ok {
			t.Fatalf("%s judged a 70s window; msg=%q", s.ID, msg)
		}
	}
}

func TestRegistry_SlopePredicatesAreCore(t *testing.T) {
	for _, id := range []string{"I-MEM-1", "I-MEM-3", "I-MEM-4"} {
		s, ok := ByID(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		if s.Tier != "core" {
			t.Fatalf("%s tier=%q, want core", id, s.Tier)
		}
	}
}
