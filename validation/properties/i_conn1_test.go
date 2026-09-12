package properties

import (
	"strings"
	"testing"
)

// TestICONN1HealthyIdleConnDoesNotFire is the test that matters most here,
// and the reason this predicate could not simply be switched on.
//
// Its original bound was 30_000 ms, justified as "celeris's documented worst
// case is read+write timeout (30s default)". Every refapp configures
// ReadTimeout 30s / IdleTimeout 120s, and the native engines' sweep reads:
//
//	if IdleTimeout > 0 && elapsed > IdleTimeout { close }
//	else if ReadTimeout > 0 && elapsed > ReadTimeout { close }
//
// The else-if means an idle conn at 31s fails the 120s test, falls through,
// and is reaped by the 30s ReadTimeout. So the healthy ceiling IS 30s plus
// one sweep -- exactly the old threshold. Turning the predicate on at that
// value would have failed cells on healthy traffic, everywhere.
func TestICONN1HealthyIdleConnDoesNotFire(t *testing.T) {
	for _, engine := range []string{"io_uring", "epoll"} {
		t.Run(engine, func(t *testing.T) {
			// A conn caught a few hundred ms past the reap deadline, before
			// the sweep got to it. Entirely normal.
			snap := &Snapshot{EngineName: engine, OldestOpenConnLastByteAgeMs: 30_400, OpenConnsTracked: 12}
			if ok, msg := ICONN1.Predicate(snap, Context{}); !ok {
				t.Fatalf("healthy conn 400ms past the reap deadline failed the predicate: %s", msg)
			}
		})
	}
}

// TestICONN1StdEngineHasAFourTimesLargerCeiling: net/http honours
// IdleTimeout for the keep-alive wait and never applies ReadTimeout to an
// idle conn, so the legitimate age on std is 120s, not 30s. A single bound
// cannot be right for both, which is why the bound is engine-aware.
func TestICONN1StdEngineHasAFourTimesLargerCeiling(t *testing.T) {
	// 100s idle: a violation on a native engine, entirely legal on std.
	snap := &Snapshot{EngineName: "std", OldestOpenConnLastByteAgeMs: 100_000, OpenConnsTracked: 3}
	if ok, msg := ICONN1.Predicate(snap, Context{}); !ok {
		t.Fatalf("100s idle on std failed, but std's IdleTimeout ceiling is 120s: %s", msg)
	}
	native := &Snapshot{EngineName: "epoll", OldestOpenConnLastByteAgeMs: 100_000, OpenConnsTracked: 3}
	if ok, _ := ICONN1.Predicate(native, Context{}); ok {
		t.Fatal("100s idle on epoll passed; the native ceiling is 30s, so this is a leak")
	}
}

// TestICONN1FiresOnALeak: the detection side. A connection that has gone
// far past any engine's ceiling is the phantom-socket signature.
func TestICONN1FiresOnALeak(t *testing.T) {
	for _, tc := range []struct {
		engine string
		ageMs  int64
	}{
		{"io_uring", 120_000},
		{"epoll", 120_000},
		{"std", 600_000},
	} {
		t.Run(tc.engine, func(t *testing.T) {
			snap := &Snapshot{EngineName: tc.engine, OldestOpenConnLastByteAgeMs: tc.ageMs, OpenConnsTracked: 1}
			ok, msg := ICONN1.Predicate(snap, Context{})
			if ok {
				t.Fatalf("a conn idle for %dms on %s did not fire", tc.ageMs, tc.engine)
			}
			// The message must name the age, the bound and the engine, or a
			// reader of the incident cannot tell a leak from a retuned bound.
			for _, want := range []string{"I-CONN-1 violated", tc.engine} {
				if !strings.Contains(msg, want) {
					t.Errorf("message missing %q: %s", want, msg)
				}
			}
		})
	}
}

// TestICONN1UnknownEngineTakesTheStricterBound: an absent engine name means
// the refapp is too old to be trusted, so it gets the native bound.
// Under-reporting a leak is worse than an occasional false positive.
func TestICONN1UnknownEngineTakesTheStricterBound(t *testing.T) {
	snap := &Snapshot{EngineName: "", OldestOpenConnLastByteAgeMs: 100_000, OpenConnsTracked: 1}
	if ok, _ := ICONN1.Predicate(snap, Context{}); ok {
		t.Fatal("unknown engine took the lax std bound; it must take the stricter native one")
	}
}

// TestICONN1RequiresPersistence guards the other half of the false-positive
// defence. A single sample past the bound must not fail a cell -- a
// boundary artifact clears in one sweep, whereas a leak grows without bound
// and stays failing. Persist is what encodes that difference.
func TestICONN1RequiresPersistence(t *testing.T) {
	if ICONN1.Persist < 2 {
		t.Fatalf("ICONN1.Persist = %d; a single over-threshold sample must not fail a cell, "+
			"because the reap deadline and the sample clock are independent", ICONN1.Persist)
	}
}
