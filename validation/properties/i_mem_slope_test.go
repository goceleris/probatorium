package properties

import (
	"fmt"
	"math"
	"math/rand"
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

// firstViolation replays history through spec the way the evaluator
// does -- one evaluation per sample over the growing history, every
// stride seconds -- and returns the index (seconds) of the first
// ok=false result and its message, or -1.
func firstViolation(spec Spec, history []Snapshot, stride int) (int, string) {
	for i := 0; i < len(history); i += stride {
		ctx := slopeCtx(history[:i+1])
		if ok, msg := spec.Predicate(&history[i], ctx); !ok {
			return i, msg
		}
	}
	return -1, ""
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

// A slope just above the budget (0.3/s) still trips once the window is
// full: the rise floor must not swallow a modest, sustained leak.
func TestIMEM3_failsOnModestSustainedLeak(t *testing.T) {
	h := leakHistory(40*60, func(i int) (int64, int64, int64) {
		return 100 + int64(float64(i)*0.3), 100 << 20, 0
	})
	at, msg := firstViolation(IMEM3, h, 1)
	if at < 0 {
		t.Fatal("a 0.3/s goroutine leak never tripped I-MEM-3")
	}
	if at > 20*60 {
		t.Fatalf("0.3/s leak first tripped at t=%ds, want within 20 min; msg=%q", at, msg)
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
	// Uniform 0..40 jitter over an hour, evaluated every second.
	rng := rand.New(rand.NewSource(7))
	h = leakHistory(60*60, func(int) (int64, int64, int64) {
		return 300 + int64(rng.Intn(41)), 100 << 20, 0
	})
	if at, msg := firstViolation(IMEM3, h, 2); at >= 0 {
		t.Fatalf("uniform jitter tripped I-MEM-3 at t=%ds: %s", at, msg)
	}
}

// A single step of goroutines below the budgeted 10 min total (120)
// after warm-up -- a standby engine spinning up, a WS burst pinning a
// goroutine per connection -- is not a leak and must never fire, even
// though the four-trough regression turns it into a slope > 0.2/s.
func TestIMEM3_stepBelowBudgetedRisePasses(t *testing.T) {
	for _, step := range []int64{80, 119} {
		h := leakHistory(60*60, func(i int) (int64, int64, int64) {
			if i >= 20*60 {
				return 200 + step, 100 << 20, 0
			}
			return 200, 100 << 20, 0
		})
		if at, msg := firstViolation(IMEM3, h, 2); at >= 0 {
			t.Fatalf("+%d step tripped I-MEM-3 at t=%ds: %s", step, at, msg)
		}
	}
	// A step of 5 % of a large population is likewise tolerated.
	h := leakHistory(60*60, func(i int) (int64, int64, int64) {
		if i >= 20*60 {
			return 5000 + 240, 100 << 20, 0
		}
		return 5000, 100 << 20, 0
	})
	if at, msg := firstViolation(IMEM3, h, 2); at >= 0 {
		t.Fatalf("+240 on 5000 tripped I-MEM-3 at t=%ds: %s", at, msg)
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

// Before 10 minutes of post-warm-up samples exist the predicate SKIPS
// (not passes), even when the leak is already visible.
func TestIMEM3_skipsUntilTenMinutesAfterWarmup(t *testing.T) {
	for _, minutes := range []int{4, 10, 14} {
		h := leakHistory(minutes*60, func(i int) (int64, int64, int64) {
			return 100 + 3*int64(i), 100 << 20, 0
		})
		ok, msg := IMEM3.Predicate(&h[len(h)-1], slopeCtx(h))
		if !ok {
			t.Fatalf("t=%dmin: must skip before the window is full; msg=%q", minutes, msg)
		}
		if !IsSkip(ok, msg) {
			t.Fatalf("t=%dmin: a not-yet-judgeable window must be a Skip, got a pass (msg=%q)", minutes, msg)
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

// A one-off RSS step below the budgeted 10 min total (37.5 MB) -- the
// heap goal moving after a load burst, a buffer ring growing -- is not
// a leak; RSS has no sawtooth so nothing else would soften it.
func TestIMEM4_stepBelowBudgetedRisePasses(t *testing.T) {
	for _, step := range []int64{16 << 20, 24 << 20, 36 << 20} {
		h := leakHistory(60*60, func(i int) (int64, int64, int64) {
			rss := int64(400 << 20)
			if i >= 20*60 {
				rss += step
			}
			return 100, 100 << 20, rss
		})
		if at, msg := firstViolation(IMEM4, h, 2); at >= 0 {
			t.Fatalf("+%d MB RSS step tripped I-MEM-4 at t=%ds: %s", step>>20, at, msg)
		}
	}
}

// No pid => RSS never sampled => the predicate must SKIP, not fire on a
// zero series and not fire on the transition from 0 to a real value.
func TestIMEM4_skipsWithoutRSS(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) { return 100, 100 << 20, 0 })
	ok, msg := IMEM4.Predicate(&h[len(h)-1], slopeCtx(h))
	if !ok {
		t.Fatalf("RSS=0 must skip; msg=%q", msg)
	}
	if !IsSkip(ok, msg) {
		t.Fatalf("RSS=0 must be a Skip, not a pass (msg=%q)", msg)
	}
}

// I-MEM-1 used to skip until the run had lasted a full hour, so a 1h
// matrix cell evaluated it (at most) once. It now judges the slope once
// 10 minutes of post-warm-up samples exist: a 2 KB/s leak on a small
// heap (the 3 % relative floor is below the 600 KB absolute one) trips
// at t=15min.
func TestIMEM1_failsWithinFifteenMinutes(t *testing.T) {
	h := leakHistory(15*60, func(i int) (int64, int64, int64) {
		return 100, 16<<20 + 2048*int64(i), 0
	})
	ok, msg := IMEM1.Predicate(&h[len(h)-1], slopeCtx(h))
	if ok {
		t.Fatalf("expected violation at t=15min for a 2 KB/s heap slope; msg=%q", msg)
	}
	if !strings.Contains(msg, "I-MEM-1") {
		t.Fatalf("missing tag: %q", msg)
	}
}

// On a 100 MB heap the same 2 KB/s leak has to accumulate 3 % (3 MB)
// before the verdict is allowed -- ~25 min of window -- and the
// celeris#494 rate (18 GB over 24 h ≈ 218 KB/s) trips the moment the
// window is judgeable.
func TestIMEM1_leakClearsRelativeFloor(t *testing.T) {
	h := leakHistory(60*60, func(i int) (int64, int64, int64) {
		return 100, 100<<20 + 2048*int64(i), 0
	})
	at, msg := firstViolation(IMEM1, h, 1)
	if at < 0 {
		t.Fatal("a 2 KB/s leak on a 100 MB heap never tripped I-MEM-1 within an hour")
	}
	if at < 15*60 || at > 35*60 {
		t.Fatalf("2 KB/s on 100 MB first tripped at t=%ds, want between 15 and 35 min; msg=%q", at, msg)
	}
	h = leakHistory(20*60, func(i int) (int64, int64, int64) {
		return 100, 200<<20 + 218*1024*int64(i), 0
	})
	at, msg = firstViolation(IMEM1, h, 1)
	if at < 0 || at > 15*60+1 {
		t.Fatalf("celeris#494-class leak first tripped at t=%d, want t=15min; msg=%q", at, msg)
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

// A single heap floor step after warm-up (a cache or TTL store filling
// once) below 3 % of the live heap is not a leak verdict: the four
// trough regression turns even a 1 MB step into a 2.7 KB/s slope, so
// only the rise floor stands between it and a hard fail.
func TestIMEM1_stepBelowRelativeFloorPasses(t *testing.T) {
	for _, step := range []int64{512 << 10, 1 << 20, 2 << 20} {
		h := leakHistory(60*60, func(i int) (int64, int64, int64) {
			heap := int64(100 << 20)
			if i >= 20*60 {
				heap += step
			}
			return 100, heap, 0
		})
		if at, msg := firstViolation(IMEM1, h, 2); at >= 0 {
			t.Fatalf("+%d KB step on 100 MB tripped I-MEM-1 at t=%ds: %s", step>>10, at, msg)
		}
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

// sawtoothHeap models a healthy Go heap under steady allocation: the
// live set is L, GOGC=100 lets it grow to 2L before a collection drops
// it back, with the given period and a random phase. Sampled at 1 Hz;
// with probability dup the poller sees the refapp's 1 s memstats cache
// and repeats the previous sample (measured 41-50 % on a live refapp).
func sawtoothHeap(rng *rand.Rand, n int, L float64, period, dup float64) []Snapshot {
	phase := rng.Float64() * period
	base := int64(1_700_000_000)
	h := make([]Snapshot, 0, n+1)
	var prev int64
	for i := 0; i <= n; i++ {
		t := float64(i) + phase
		frac := t/period - math.Floor(t/period)
		v := int64(L + L*frac)
		if i > 0 && rng.Float64() < dup {
			v = prev
		}
		prev = v
		h = append(h, Snapshot{TS: base + int64(i), GoroutineCount: 100, HeapInuseBytes: v})
	}
	return h
}

// A healthy GC sawtooth at a live heap of tens of MB must never trip
// I-MEM-1 over an hour: the bucket minimum of a random-phase sawtooth
// sits L/(n+1) above the true trough with a comparable spread, and with
// only a few troughs the fitted slope's noise exceeds the absolute
// 1 KB/s budget (a plain budget check failed 5/12 clean hours at
// L=32 MB and 10/12 at 128 MB). The sampling-noise floor is what makes
// this pass; the leak tests above are the true-positive bound.
func TestIMEM1_sawtoothNeverFires(t *testing.T) {
	// 12 seeds x 2 heap sizes x 3 GC periods, one hour each, evaluated
	// every 5 s: ~60 s under -race, a few seconds without. (A one-off
	// 30-seed / 3 s-stride run of the same model also stayed at zero.)
	seeds := 12
	if testing.Short() {
		seeds = 4
	}
	for _, L := range []float64{32 << 20, 128 << 20} {
		for _, period := range []float64{0.37, 1.7, 13} {
			for seed := 0; seed < seeds; seed++ {
				rng := rand.New(rand.NewSource(int64(seed)*7919 + int64(period*1000)))
				h := sawtoothHeap(rng, 60*60, L, period, 0.5)
				if at, msg := firstViolation(IMEM1, h, 5); at >= 0 {
					t.Fatalf("L=%d MB period=%gs seed=%d: clean sawtooth tripped I-MEM-1 at t=%ds: %s",
						int(L)>>20, period, seed, at, msg)
				}
			}
		}
	}
}

// The same sawtooth WITH a leak underneath must still be caught: the
// noise floor scales with the sawtooth, so the leak has to be
// proportionate, but a 5 %/10 min growth of a 32 MB live set (~2.8
// KB/s) is well inside what a soak must flag.
func TestIMEM1_sawtoothWithLeakFires(t *testing.T) {
	rng := rand.New(rand.NewSource(11))
	L := float64(32 << 20)
	h := sawtoothHeap(rng, 60*60, L, 1.7, 0.5)
	for i := range h {
		h[i].HeapInuseBytes += int64(8 * 1024 * i) // 8 KB/s
	}
	at, msg := firstViolation(IMEM1, h, 3)
	if at < 0 {
		t.Fatal("an 8 KB/s leak under a 32 MB sawtooth never tripped I-MEM-1")
	}
	if !strings.Contains(msg, "sampling noise") {
		t.Fatalf("message must show the floors: %q", msg)
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
		ok, msg := s.Predicate(&h[len(h)-1], ctx)
		if !ok {
			t.Fatalf("%s judged a 70s window; msg=%q", s.ID, msg)
		}
		if !IsSkip(ok, msg) {
			t.Fatalf("%s: an unjudgeable window must be a Skip", s.ID)
		}
	}
}

// The slope predicates carry a one-bucket persistence so a verdict has
// to survive a complete re-bucketing before the evaluator declares it.
func TestRegistry_SlopePredicatesAreCoreAndPersist(t *testing.T) {
	for _, id := range []string{"I-MEM-1", "I-MEM-3", "I-MEM-4"} {
		s, ok := ByID(id)
		if !ok {
			t.Fatalf("%s not registered", id)
		}
		if s.Tier != "core" {
			t.Fatalf("%s tier=%q, want core", id, s.Tier)
		}
		if s.Persist != slopePersistSamples || slopePersistSamples != 150 {
			t.Fatalf("%s persist=%d, want one bucket width (150)", id, s.Persist)
		}
	}
}

// Skip is ok=true with a recognisable message; a plain pass is not a
// skip and neither is a violation.
func TestSkip_IsDistinguishable(t *testing.T) {
	ok, msg := Skip("because")
	if !ok || !IsSkip(ok, msg) || !strings.Contains(msg, "because") {
		t.Fatalf("Skip: ok=%v msg=%q", ok, msg)
	}
	if IsSkip(true, "") || IsSkip(false, "skip: no") {
		t.Fatal("a pass or a violation must not read as a skip")
	}
	if ok, msg := IMEM2.Predicate(&Snapshot{}, ctxWith(time.Hour, nil, false)); !IsSkip(ok, msg) {
		t.Fatalf("I-MEM-2 outside an idle window must skip: ok=%v msg=%q", ok, msg)
	}
}

func TestFmtBytes(t *testing.T) {
	for v, want := range map[float64]string{512: "512 B", 2048: "2.0 KB", 3 << 20: "3.00 MB", 1 << 30: "1.00 GB"} {
		if got := fmtBytes(v); got != want {
			t.Errorf("fmtBytes(%v)=%q want %q", v, got, want)
		}
	}
	if s := fmt.Sprint(fmtCount(0.3)); s != "0.30" {
		t.Errorf("fmtCount: %q", s)
	}
}
