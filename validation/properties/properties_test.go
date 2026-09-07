package properties

import (
	"strings"
	"testing"
	"time"
)

// ctxWith builds a Context with the given run-age, history, and idle
// flag. The Now field is set to ctx.RunStartedAt + age.
func ctxWith(age time.Duration, history []Snapshot, idle bool) Context {
	start := time.Unix(1_700_000_000, 0)
	return Context{
		RunStartedAt:       start,
		Now:                start.Add(age),
		IdleMode:           idle,
		BaselineGoroutines: 50,
		History:            history,
	}
}

func TestICONN1_passesUnderBudget(t *testing.T) {
	snap := &Snapshot{OldestOpenConnLastByteAgeMs: 1000}
	ok, msg := ICONN1.Predicate(snap, ctxWith(0, nil, false))
	if !ok {
		t.Fatalf("expected pass, got msg=%q", msg)
	}
}

func TestICONN1_failsOverBudget(t *testing.T) {
	snap := &Snapshot{OldestOpenConnLastByteAgeMs: 60_000}
	ok, msg := ICONN1.Predicate(snap, ctxWith(0, nil, false))
	if ok {
		t.Fatal("expected fail")
	}
	if !strings.Contains(msg, "I-CONN-1") {
		t.Fatalf("expected violation message, got %q", msg)
	}
}

// driftHistory builds n consecutive 1 Hz samples whose balance is off by
// drift (same sign throughout), ending at the returned snapshot.
func driftHistory(n int, drift int64) ([]Snapshot, *Snapshot) {
	base := int64(1_700_000_000)
	h := make([]Snapshot, 0, n)
	for i := 0; i < n; i++ {
		h = append(h, Snapshot{TS: base + int64(i), AcceptedConnTotal: 100 + int64(i), ClosedConnTotal: 90 + int64(i), ActiveConns: 10 - drift})
	}
	last := h[len(h)-1]
	return h, &last
}

func TestICONN2_failsOnPersistentDrift(t *testing.T) {
	h, snap := driftHistory(connDriftPersistSamples, 5)
	ok, msg := ICONN2.Predicate(snap, ctxWith(0, h, false))
	if ok {
		t.Fatalf("expected fail after %d same-sign samples; msg=%q", connDriftPersistSamples, msg)
	}
	if !strings.Contains(msg, "I-CONN-2") {
		t.Fatalf("missing tag: %q", msg)
	}
}

// One off-by-one sample under load (counters read at different instants)
// must not fire; neither must drift that keeps flipping sign.
func TestICONN2_ignoresTransientDrift(t *testing.T) {
	snap := &Snapshot{AcceptedConnTotal: 100, ClosedConnTotal: 90, ActiveConns: 5}
	if ok, msg := ICONN2.Predicate(snap, ctxWith(0, nil, false)); !ok {
		t.Fatalf("single-sample drift must not fire; msg=%q", msg)
	}
	h, last := driftHistory(connDriftPersistSamples*2, 1)
	for i := range h {
		if i%2 == 0 {
			h[i].ActiveConns += 2 // flip the sign every other sample
		}
	}
	if ok, msg := ICONN2.Predicate(last, ctxWith(0, h, false)); !ok {
		t.Fatalf("sign-flipping drift must not fire; msg=%q", msg)
	}
	h, last = driftHistory(connDriftPersistSamples-1, 3)
	if ok, msg := ICONN2.Predicate(last, ctxWith(0, h, false)); !ok {
		t.Fatalf("drift one sample short of the persistence bar must not fire; msg=%q", msg)
	}
}

func TestICONN2_passesBalanced(t *testing.T) {
	snap := &Snapshot{AcceptedConnTotal: 100, ClosedConnTotal: 80, ActiveConns: 20}
	ok, _ := ICONN2.Predicate(snap, ctxWith(0, nil, false))
	if !ok {
		t.Fatal("expected pass")
	}
}

func TestIRFC1_eachPath(t *testing.T) {
	cases := []struct {
		name string
		s    Snapshot
	}{
		{"bad-framing", Snapshot{ResponsesBadFraming: 1}},
		{"head-with-body", Snapshot{ResponsesHeadWithBody: 1}},
		{"204-with-body", Snapshot{Responses204WithBody: 1}},
		{"304-with-body", Snapshot{Responses304WithBody: 1}},
		{"missing-chunk-end", Snapshot{ResponsesMissingChunkEnd: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, msg := IRFC1.Predicate(&tc.s, ctxWith(0, nil, false))
			if ok {
				t.Fatal("expected violation")
			}
			if !strings.Contains(msg, "I-RFC-1") {
				t.Fatalf("missing tag: %q", msg)
			}
		})
	}
}

func TestIRFC2_failsOnInjection(t *testing.T) {
	for _, s := range []Snapshot{{ResponsesCRLFInHeader: 1}, {ResponsesNULInHeader: 1}} {
		ok, _ := IRFC2.Predicate(&s, ctxWith(0, nil, false))
		if ok {
			t.Fatalf("expected violation for %+v", s)
		}
	}
}

func TestIMEM1_skipsBeforeWindow(t *testing.T) {
	snap := &Snapshot{HeapInuseBytes: 1 << 30}
	// 30 min < 1h window → skip.
	ok, _ := IMEM1.Predicate(snap, ctxWith(30*time.Minute, nil, false))
	if !ok {
		t.Fatal("expected skip during warmup")
	}
}

func TestIMEM1_failsOnSteepSlope(t *testing.T) {
	// Build a 1h history with a deterministic 2 KB/s slope (above the
	// 1 KB/s budget). Use sample density of 1/sec.
	history := make([]Snapshot, 0, 3601)
	base := int64(1_700_000_000)
	var heap int64 = 100 << 20
	for i := 0; i <= 3600; i++ {
		history = append(history, Snapshot{TS: base + int64(i), HeapInuseBytes: heap})
		heap += 2048
	}
	ctx := ctxWith(time.Hour+10*time.Second, history, false)
	ctx.Now = time.Unix(history[len(history)-1].TS, 0).Add(time.Second)
	ok, msg := IMEM1.Predicate(&history[len(history)-1], ctx)
	if ok {
		t.Fatalf("expected violation, got pass; msg=%q forever=%s", msg, Forever(ctx))
	}
	if !strings.Contains(msg, "I-MEM-1") {
		t.Fatalf("missing tag: %q", msg)
	}
}

func TestIMEM1_passesFlat(t *testing.T) {
	history := make([]Snapshot, 0, 3601)
	base := int64(1_700_000_000)
	for i := 0; i <= 3600; i++ {
		history = append(history, Snapshot{TS: base + int64(i), HeapInuseBytes: 100 << 20})
	}
	ctx := ctxWith(time.Hour+10*time.Second, history, false)
	ctx.Now = time.Unix(history[len(history)-1].TS, 0).Add(time.Second)
	ok, _ := IMEM1.Predicate(&history[len(history)-1], ctx)
	if !ok {
		t.Fatal("expected pass for flat heap")
	}
}

func TestIMEM2_skipsWhenNotIdle(t *testing.T) {
	snap := &Snapshot{GoroutineCount: 10_000}
	ok, _ := IMEM2.Predicate(snap, ctxWith(time.Hour, nil, false))
	if !ok {
		t.Fatal("expected skip when not idle")
	}
}

func TestIMEM2_failsWhenOverBudget(t *testing.T) {
	history := make([]Snapshot, 60)
	snap := &Snapshot{GoroutineCount: 200}
	ctx := ctxWith(time.Hour, history, true)
	ok, msg := IMEM2.Predicate(snap, ctx)
	if ok {
		t.Fatalf("expected violation; msg=%q", msg)
	}
}

func TestIPANIC_failsOnPersistentCount(t *testing.T) {
	// A panic that persists across ipanicPersistence consecutive snapshots
	// fires; the History carries the earlier samples.
	hist := []Snapshot{{TS: 1, PanicCount: 1}, {TS: 2, PanicCount: 1}}
	ok, _ := IPANIC.Predicate(&Snapshot{TS: 3, PanicCount: 1}, Context{History: hist})
	if ok {
		t.Fatal("expected violation")
	}
}

func TestIRACE_failsOnReport(t *testing.T) {
	ok, _ := IRACE.Predicate(&Snapshot{RaceReports: 1}, ctxWith(0, nil, false))
	if ok {
		t.Fatal("expected violation")
	}
}

func TestICHECKPTR_failsOnReport(t *testing.T) {
	ok, _ := ICHECKPTR.Predicate(&Snapshot{CheckptrReports: 1}, ctxWith(0, nil, false))
	if ok {
		t.Fatal("expected violation")
	}
}

func TestMWStubs_passOnZero(t *testing.T) {
	for _, s := range []Spec{IMWRateLimit, IMWSession, IMWJWT, IENGIOURing, IENGAdaptive} {
		ok, msg := s.Predicate(&Snapshot{}, ctxWith(0, nil, false))
		if !ok {
			t.Fatalf("%s expected pass on zero snapshot; msg=%q", s.ID, msg)
		}
	}
}

func TestMWStubs_failOnNegative(t *testing.T) {
	cases := []struct {
		name string
		s    Snapshot
		spec Spec
	}{
		{"ratelimit-neg-allowed", Snapshot{RateLimitAllowed: -1}, IMWRateLimit},
		{"session-neg-created", Snapshot{SessionsCreatedTotal: -1}, IMWSession},
		{"jwt-neg-ok", Snapshot{JWTValidatedOK: -1}, IMWJWT},
		{"iouring-cqe-leads-sqe", Snapshot{IOUringSQEsSubmitted: 1, IOUringCQEsCompleted: 2}, IENGIOURing},
		{"adaptive-neg", Snapshot{AdaptiveSwitches: -1}, IENGAdaptive},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, _ := tc.spec.Predicate(&tc.s, ctxWith(0, nil, false))
			if ok {
				t.Fatalf("%s expected violation on %+v", tc.spec.ID, tc.s)
			}
		})
	}
}

// The flap window is one HOUR of history; the old code subtracted 3600
// nanoseconds and could never see a delta.
func TestIENGAdaptive_flapOverAnHourFires(t *testing.T) {
	base := int64(1_700_000_000)
	h := make([]Snapshot, 0, 3600)
	for i := 0; i < 3600; i++ {
		h = append(h, Snapshot{TS: base + int64(i), AdaptiveSwitches: int64(i)})
	}
	ctx := ctxWith(time.Hour, h, false)
	ctx.Now = time.Unix(base+3599, 0)
	snap := &Snapshot{TS: base + 3599, AdaptiveSwitches: 3599}
	if ok, msg := IENGAdaptive.Predicate(snap, ctx); ok {
		t.Fatalf("3599 switches in an hour must fire; msg=%q", msg)
	}
	snap.AdaptiveSwitches = 100
	if ok, msg := IENGAdaptive.Predicate(snap, ctx); !ok {
		t.Fatalf("100 switches in an hour is within budget; msg=%q", msg)
	}
}

func TestIDRV_failsOnMiss(t *testing.T) {
	snap := &Snapshot{DriverReadsIssued: 10, DriverReadHits: 9, DriverReadMisses: 1}
	ok, _ := IDRV.Predicate(snap, ctxWith(0, nil, false))
	if ok {
		t.Fatal("expected violation")
	}
}

func TestIDRV_passesClean(t *testing.T) {
	snap := &Snapshot{DriverReadsIssued: 10, DriverReadHits: 10}
	ok, _ := IDRV.Predicate(snap, ctxWith(0, nil, false))
	if !ok {
		t.Fatal("expected pass")
	}
}

// Validation-socket counters (celeris v1.4.3+) fire on any non-zero
// value — they're hard "assertion fired" signals from the validation
// build of celeris. Each must be wired into the corresponding spec.
func TestMWValidationCounters_FailOnNonZero(t *testing.T) {
	cases := []struct {
		name string
		s    Snapshot
		spec Spec
	}{
		{"ratelimit-token-violation", Snapshot{RatelimitTokenViolations: 1}, IMWRateLimit},
		{"session-owner-mismatch", Snapshot{SessionOwnerMismatches: 1}, IMWSession},
		{"jwt-late-admit", Snapshot{JWTLateAdmits: 1}, IMWJWT},
		{"iouring-sqe-corruption", Snapshot{IouringSQECorruptions: 1}, IENGIOURing},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ok, msg := tc.spec.Predicate(&tc.s, ctxWith(0, nil, false))
			if ok {
				t.Fatalf("%s expected violation on %+v", tc.spec.ID, tc.s)
			}
			if msg == "" {
				t.Fatalf("%s violation must carry a non-empty message", tc.spec.ID)
			}
		})
	}
}

func TestRegistry_AllSorted(t *testing.T) {
	specs := All()
	if len(specs) == 0 {
		t.Fatal("registry empty")
	}
	seen := map[string]bool{}
	for i, s := range specs {
		if s.ID == "" || s.Predicate == nil || s.Tier == "" {
			t.Fatalf("spec[%d]=%+v missing required fields", i, s)
		}
		if seen[s.ID] {
			t.Fatalf("duplicate spec id %q", s.ID)
		}
		seen[s.ID] = true
		if i > 0 && specs[i-1].ID > s.ID {
			t.Fatalf("specs not sorted: %q > %q", specs[i-1].ID, s.ID)
		}
	}
}

func TestRegistry_ByTier(t *testing.T) {
	if got := len(ByTier("core")); got == 0 {
		t.Fatal("expected core specs")
	}
	if got := len(ByTier("middleware")); got == 0 {
		t.Fatal("expected middleware specs")
	}
	if len(ByTier("nonexistent")) != 0 {
		t.Fatal("expected empty for unknown tier")
	}
}

func TestRegistry_ByID(t *testing.T) {
	if _, ok := ByID("I-CONN-1"); !ok {
		t.Fatal("I-CONN-1 missing")
	}
	if _, ok := ByID("nope"); ok {
		t.Fatal("ByID should fail for unknown id")
	}
}

func TestHeapSlope_LinearFit(t *testing.T) {
	// y = 100x over four 150s trough buckets; slope should round-trip to 100.
	var samples []Snapshot
	for x := int64(0); x < 600; x++ {
		samples = append(samples, Snapshot{TS: 100 + x, HeapInuseBytes: 100 * x})
	}
	got := heapSlope(samples)
	if got < 99 || got > 101 {
		t.Fatalf("expected slope ≈100, got %v", got)
	}
}

// A GC sawtooth (slow ramp, instant drop) has a positive least-squares
// slope over raw samples; the trough regression must read it as flat.
func TestHeapSlope_SawtoothIsFlat(t *testing.T) {
	var samples []Snapshot
	for x := int64(0); x < 600; x++ {
		samples = append(samples, Snapshot{TS: x, HeapInuseBytes: 500<<20 + (x%5)*(100<<20)})
	}
	if got := heapSlope(samples); got > 1 {
		t.Fatalf("sawtooth must regress flat over troughs, got %v B/s", got)
	}
}

func TestHeapSlope_Degenerate(t *testing.T) {
	if heapSlope(nil) != 0 {
		t.Fatal("nil should yield 0 slope")
	}
	if heapSlope([]Snapshot{{TS: 1, HeapInuseBytes: 1}}) != 0 {
		t.Fatal("single sample should yield 0 slope")
	}
	flat := []Snapshot{{TS: 100, HeapInuseBytes: 0}, {TS: 100, HeapInuseBytes: 1000}}
	if heapSlope(flat) != 0 {
		t.Fatal("zero TS variance should yield 0 slope")
	}
}

func TestTier1Walker_PredicatesRegistered(t *testing.T) {
	for _, want := range []string{"I-ADV-ACCEPTED", "I-H2C-CRASHED", "I-WS-ACCEPTED", "I-WS-HANG", "I-LIVENESS", "I-HANG"} {
		spec, ok := ByID(want)
		if !ok {
			t.Errorf("Spec %q missing from registry", want)
			continue
		}
		if spec.Tier != "tier-1-walker" {
			t.Errorf("Spec %q tier: got %q, want tier-1-walker", want, spec.Tier)
		}
		// Predicate must be a no-op that returns true — the real
		// evaluation happens in the orchestrator's TallyCallback.
		ok, msg := spec.Predicate(&Snapshot{}, Context{})
		if !ok {
			t.Errorf("Spec %q Predicate returned !ok with msg %q; want always-true no-op", want, msg)
		}
	}
}

func TestTier1Walker_ByTierFiltersCorrectly(t *testing.T) {
	got := ByTier("tier-1-walker")
	if len(got) != 6 {
		t.Errorf("ByTier(tier-1-walker): got %d specs, want 6", len(got))
	}
	for _, s := range got {
		if s.Tier != "tier-1-walker" {
			t.Errorf("ByTier returned %q with wrong tier %q", s.ID, s.Tier)
		}
	}
}
