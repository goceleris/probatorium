package checker

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// A refapp that installs session / ratelimit / jwt middleware publishes the
// oracle counters on /debug/vars. Without them I-MW-SESSION, I-MW-RATELIMIT
// and I-MW-JWT judge a structurally-zero input in every cell -- which is
// exactly how probatorium#297 kept celeris#507 out of reach of its own
// oracle through two soaks.
func TestParseDebugVars_MiddlewareCounters(t *testing.T) {
	body := []byte(`{
		"goroutines": 12,
		"celeris.session_owner_mismatches": 3,
		"celeris.sessions_created_total": 900,
		"celeris.sessions_expired_total": 400,
		"celeris.session_cookie_drops": 7,
		"celeris.ratelimit_allowed": 5000,
		"celeris.ratelimit_rejected": 120,
		"celeris.ratelimit_token_violations": 2,
		"celeris.jwt_validated_ok": 80,
		"celeris.jwt_validated_fail": 900,
		"celeris.jwt_late_admits": 1,
		"celeris.instrumented_properties": "I-MW-JWT,I-MW-RATELIMIT,I-MW-SESSION"
	}`)
	var snap properties.Snapshot
	if err := ParseDebugVars(body, &snap); err != nil {
		t.Fatalf("ParseDebugVars: %v", err)
	}
	want := properties.Snapshot{
		GoroutineCount:           12,
		SessionOwnerMismatches:   3,
		SessionsCreatedTotal:     900,
		SessionsExpiredTotal:     400,
		SessionCookieDrops:       7,
		RateLimitAllowed:         5000,
		RateLimitRejected:        120,
		RatelimitTokenViolations: 2,
		JWTValidatedOK:           80,
		JWTValidatedFail:         900,
		JWTLateAdmits:            1,
		InstrumentedProperties:   "I-MW-JWT,I-MW-RATELIMIT,I-MW-SESSION",
	}
	if snap != want {
		t.Fatalf("snapshot mismatch:\n got %+v\nwant %+v", snap, want)
	}
}

// The validation socket must not be able to MASK a refapp-side counter:
// both sources feed the same fields, so the larger reading wins the way
// PanicCount already does.
func TestPollValidationSocket_DoesNotMaskRefappCounters(t *testing.T) {
	body := []byte(`{"session_owner_mismatches":0,"jwt_late_admits":0,"ratelimit_token_violations":0}`)
	hc := NewSocketClient(fakeValidationSocket(t, body), 500*time.Millisecond)
	snap := properties.Snapshot{SessionOwnerMismatches: 4, JWTLateAdmits: 2, RatelimitTokenViolations: 1}
	PollValidationSocket(context.Background(), hc, &snap)
	if snap.SessionOwnerMismatches != 4 || snap.JWTLateAdmits != 2 || snap.RatelimitTokenViolations != 1 {
		t.Fatalf("an all-zero socket must not clear refapp-side counters, got %+v", snap)
	}
}

// A predicate whose only data source is a refapp that installs the matching
// middleware must be reported as not-instrumented in the cells whose refapp
// does NOT declare it -- otherwise a structurally-zero counter reads as a
// pass, which is the vacuity probatorium#297 is about.
func TestEvaluator_DeclaredOnlyPredicates(t *testing.T) {
	specs := []properties.Spec{properties.IMWSession, properties.IPANIC}
	now := time.Unix(1_700_000_000, 0)

	// Refapp without session middleware: never declares the ID.
	ev := NewEvaluator(specs)
	ev.Observe(properties.Snapshot{TS: now.Unix()}, now)
	tally := ev.Tally()
	if !slices.Contains(tally.NotInstrumented, "I-MW-SESSION") {
		t.Fatalf("undeclared I-MW-SESSION must be not_instrumented, got %+v", tally.NotInstrumented)
	}
	if tally.Passed() != 1 {
		t.Fatalf("only I-PANIC may count as passed, got %d", tally.Passed())
	}

	// Refapp that declares it: the predicate judges and passes.
	ev = NewEvaluator(specs)
	ev.Observe(properties.Snapshot{TS: now.Unix(), InstrumentedProperties: "I-MW-SESSION"}, now)
	tally = ev.Tally()
	if slices.Contains(tally.NotInstrumented, "I-MW-SESSION") {
		t.Fatalf("declared I-MW-SESSION must be instrumented, got %+v", tally.NotInstrumented)
	}
	if tally.Passed() != 2 {
		t.Fatalf("both predicates must count as passed, got %d", tally.Passed())
	}
}

// The three middleware predicates must no longer be listed as globally
// uninstrumented: they now have a refapp-side data source and are gated on
// the per-cell declaration instead.
func TestUninstrumented_MiddlewarePredicatesAreInstrumentable(t *testing.T) {
	for _, id := range []string{"I-MW-SESSION", "I-MW-JWT", "I-MW-RATELIMIT"} {
		if reason, ok := Uninstrumented[id]; ok {
			t.Errorf("%s must be instrumentable, still listed as uninstrumented: %s", id, reason)
		}
		if _, ok := DeclaredOnly[id]; !ok {
			t.Errorf("%s must be declared-only (instrumented in the refapps that install the middleware)", id)
		}
	}
	// I-RACE needs a -race build of the refapps and is deliberately still
	// out of reach; the reason stays on record.
	if _, ok := Uninstrumented["I-RACE"]; !ok {
		t.Error("I-RACE must stay on the uninstrumented record with its reason")
	}
	// I-CHECKPTR moved to declared-only: a -tags=checkptr refapp declares it
	// and the liveness crash scan feeds it at cell end. It must NOT be back
	// on the blanket waiver, and it must NOT be judged in a normal build.
	if _, ok := Uninstrumented["I-CHECKPTR"]; ok {
		t.Error("I-CHECKPTR is blanket-waived again; a -tags=checkptr cell could never count as covered")
	}
	if _, ok := DeclaredOnly["I-CHECKPTR"]; !ok {
		t.Error("I-CHECKPTR must be declared-only, so a normal build's structural zero is not a pass")
	}
	// I-MEM-2 moved to declared-only: the property loop declares it when
	// the orchestrator's second idle window begins. A cell that never idled
	// must report it as not instrumented, never as passed.
	if _, ok := Uninstrumented["I-MEM-2"]; ok {
		t.Error("I-MEM-2 is blanket-waived again; a cell that idled twice could never count as covered")
	}
	if _, ok := DeclaredOnly["I-MEM-2"]; !ok {
		t.Error("I-MEM-2 must be declared-only, so a cell that never idled is not a pass")
	}
	// I-DRV moved to declared-only: the driver refapps read every write back
	// and publish the tally; everything else has structural zeros.
	if _, ok := Uninstrumented["I-DRV"]; ok {
		t.Error("I-DRV is blanket-waived again; the driver cells could never count as covered")
	}
	if _, ok := DeclaredOnly["I-DRV"]; !ok {
		t.Error("I-DRV must be declared-only, so a refapp with no store is not a pass")
	}
}
