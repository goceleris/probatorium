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

func TestICONN2_failsOnDrift(t *testing.T) {
	snap := &Snapshot{AcceptedConnTotal: 100, ClosedConnTotal: 90, ActiveConns: 5}
	ok, msg := ICONN2.Predicate(snap, ctxWith(0, nil, false))
	if ok {
		t.Fatalf("expected fail; msg=%q", msg)
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

func TestIPANIC_failsOnCount(t *testing.T) {
	ok, _ := IPANIC.Predicate(&Snapshot{PanicCount: 1}, ctxWith(0, nil, false))
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
	// y = 100x + 0 at integer x; slope should round-trip to 100.
	samples := []Snapshot{
		{TS: 100, HeapInuseBytes: 0},
		{TS: 101, HeapInuseBytes: 100},
		{TS: 102, HeapInuseBytes: 200},
		{TS: 103, HeapInuseBytes: 300},
	}
	got := heapSlope(samples)
	if got < 99 || got > 101 {
		t.Fatalf("expected slope ≈100, got %v", got)
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
