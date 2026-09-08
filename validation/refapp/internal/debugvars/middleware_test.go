package debugvars

import (
	"testing"
	"time"
)

// The document must carry every middleware-oracle counter the checker's
// parser reads, whether or not the refapp installs the middleware: a key
// that is absent parses as zero, which is indistinguishable from "clean"
// (probatorium#297). What tells the two apart is
// celeris.instrumented_properties, which lists only what this refapp can
// actually judge.
func TestDebugVars_MiddlewareCountersInDocument(t *testing.T) {
	dv := New()
	doc := dv.Document()
	for _, k := range []string{
		"celeris.session_owner_mismatches",
		"celeris.sessions_created_total",
		"celeris.sessions_expired_total",
		"celeris.session_cookie_drops",
		"celeris.ratelimit_allowed",
		"celeris.ratelimit_rejected",
		"celeris.ratelimit_token_violations",
		"celeris.jwt_validated_ok",
		"celeris.jwt_validated_fail",
		"celeris.jwt_late_admits",
	} {
		v, ok := doc[k].(int64)
		if !ok {
			t.Errorf("%s: missing or not an int64: %#v", k, doc[k])
			continue
		}
		if v != 0 {
			t.Errorf("%s: fresh Vars must start at 0, got %d", k, v)
		}
	}
	if got, ok := doc["celeris.instrumented_properties"].(string); !ok || got != "" {
		t.Errorf("celeris.instrumented_properties on an undeclared refapp: %#v", doc["celeris.instrumented_properties"])
	}
}

// Declare is what makes a cell's judgement non-vacuous, so the list must be
// sorted, de-duplicated and stable across polls.
func TestDeclare_SortedAndDeduplicated(t *testing.T) {
	dv := New()
	dv.Declare("I-MW-SESSION", "I-MW-RATELIMIT")
	dv.Declare("I-MW-SESSION")
	if got := dv.Document()["celeris.instrumented_properties"]; got != "I-MW-RATELIMIT,I-MW-SESSION" {
		t.Fatalf("instrumented_properties=%#v", got)
	}
}

// The session ledger is the I-MW-SESSION oracle: it binds the id the refapp
// handed out to the user it authenticated, and every later request must
// present the same pair. A request that arrives with someone else's owner
// under that id is session bleed.
func TestSessionLedger_OwnerMismatch(t *testing.T) {
	dv := New()
	dv.SessionLogin("sid-1", "walker-a")
	dv.SessionLogin("sid-2", "walker-b")
	if !dv.SessionCheck("sid-1", "walker-a") || !dv.SessionCheck("sid-2", "walker-b") {
		t.Fatal("a matching (id, owner) pair must not be a mismatch")
	}
	if dv.SessionOwnerMismatches() != 0 {
		t.Fatalf("mismatches=%d after two clean checks", dv.SessionOwnerMismatches())
	}
	// sid-1 comes back carrying walker-b's identity: the bleed.
	if dv.SessionCheck("sid-1", "walker-b") {
		t.Fatal("a crossed (id, owner) pair must be reported as a mismatch")
	}
	if dv.SessionOwnerMismatches() != 1 {
		t.Fatalf("mismatches=%d want 1", dv.SessionOwnerMismatches())
	}
	// An id the ledger never saw (or has evicted) is unknown, not wrong.
	if !dv.SessionCheck("sid-unknown", "walker-a") {
		t.Fatal("an unknown id must not be judged")
	}
	if dv.SessionOwnerMismatches() != 1 {
		t.Fatalf("an unknown id must not count: mismatches=%d", dv.SessionOwnerMismatches())
	}
	// Counters: two logins, one logout.
	dv.SessionLogout("sid-2")
	doc := dv.Document()
	if doc["celeris.sessions_created_total"] != int64(2) || doc["celeris.sessions_expired_total"] != int64(1) {
		t.Fatalf("created/expired = %v/%v", doc["celeris.sessions_created_total"], doc["celeris.sessions_expired_total"])
	}
	if doc["celeris.session_owner_mismatches"] != int64(1) {
		t.Fatalf("document mismatches=%v", doc["celeris.session_owner_mismatches"])
	}
}

// A re-login on the same id rebinds the owner: the walker that owns the
// cookie is authoritative, so the ledger must follow it rather than report
// its own stale entry as a bleed.
func TestSessionLedger_ReloginRebinds(t *testing.T) {
	dv := New()
	dv.SessionLogin("sid", "walker-a")
	dv.SessionLogin("sid", "walker-b")
	if !dv.SessionCheck("sid", "walker-b") || dv.SessionOwnerMismatches() != 0 {
		t.Fatalf("re-login must rebind, mismatches=%d", dv.SessionOwnerMismatches())
	}
}

// The shadow bucket is the I-MW-RATELIMIT oracle: it re-derives the
// token-bucket bound the middleware promises, so an admission the bound
// cannot explain is a violation regardless of what the middleware reports.
func TestTokenShadow_AdmitsToTheBoundAndNoFurther(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	// RPS 10, burst 4 -> capacity 4*RateLimitShadowSlack.
	s := NewTokenShadow(10, 4)
	capacity := 4 * RateLimitShadowSlack
	for i := 0; i < capacity; i++ {
		if !s.Admit("k", now) {
			t.Fatalf("admission %d of %d must fit the initial burst", i+1, capacity)
		}
	}
	if s.Admit("k", now) {
		t.Fatal("an admission past burst with no elapsed time must be a violation")
	}
	// Half a second later the bucket has refilled by rps*0.5 = 5 tokens
	// (below capacity, so the refill rate is what is under test).
	later := now.Add(500 * time.Millisecond)
	for i := 0; i < 5; i++ {
		if !s.Admit("k", later) {
			t.Fatalf("refill admission %d must fit RPS=10 over 0.5s", i+1)
		}
	}
	if s.Admit("k", later) {
		t.Fatal("the 6th admission in that half-second must be a violation")
	}
	// Keys are independent: one busy key must not indict another.
	if !s.Admit("other", later) {
		t.Fatal("a fresh key starts with a full bucket")
	}
}
