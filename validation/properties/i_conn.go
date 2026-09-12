package properties

import "fmt"

// Connection-close deadlines, per engine.
//
// The original single 30_000 ms bound was justified as "celeris's documented
// worst case is read+write timeout (30s default)". Measured against the
// deployed refapps that is wrong in both directions, and turning the
// predicate on at that value would have failed every soak.
//
// All eight refapps configure ReadTimeout 30s / IdleTimeout 120s. On the
// native engines checkTimeouts reads:
//
//	if IdleTimeout > 0 && elapsed > IdleTimeout { close }
//	else if ReadTimeout > 0 && elapsed > ReadTimeout { close }
//
// The else-if is load-bearing: an idle conn at 31s fails the 120s test,
// falls through, and is reaped by the 30s ReadTimeout. So the healthy
// ceiling is min(Idle, Read) = 30s plus up to one sweep -- which is EXACTLY
// the old threshold. A healthy connection sampled at 30.05s would fail a
// predicate that has no persistence requirement.
//
// On std the ceiling is 120s, not 30s: net/http honours IdleTimeout for the
// keep-alive wait and never applies ReadTimeout to an idle conn. Same
// config, four times the legitimate age.
//
// Hence one bound per engine, each above its own ceiling with headroom, and
// a persistence requirement so a boundary artifact that clears in one sweep
// cannot fail a cell. A real leak grows without bound, blows through either
// number, and stays failing. Losing ~15s of sensitivity on a one-hour cell
// costs nothing.
const (
	// connCloseDeadlineNativeMs bounds epoll and io_uring: 30s ReadTimeout
	// ceiling + 50% headroom for the sweep and sampling jitter.
	connCloseDeadlineNativeMs = 45_000
	// connCloseDeadlineStdMs bounds the std engine: 120s IdleTimeout
	// ceiling + 25%.
	connCloseDeadlineStdMs = 150_000
	// connClosePersistSamples is how many consecutive samples must exceed
	// the bound. At 1 Hz this is five seconds -- longer than any sweep
	// boundary, far shorter than a leak.
	connClosePersistSamples = 5
)

// ConnCloseDeadlineNativeMs exposes the native-engine bound so the Tier 1
// walkers can assert their stream hold times stay well under it. See
// validation.TestStreamHoldsStayUnderTheConnCloseBound for why that
// relationship needs a test rather than a comment.
func ConnCloseDeadlineNativeMs() int64 { return connCloseDeadlineNativeMs }

// connCloseDeadlineMsFor returns the bound for the engine that produced a
// snapshot. An unknown or absent engine name gets the stricter native bound:
// under-reporting a leak is worse than an occasional false positive, and an
// absent name means the refapp is too old to be trusted about anything else
// either.
func connCloseDeadlineMsFor(engine string) int64 {
	if engine == "std" {
		return connCloseDeadlineStdMs
	}
	return connCloseDeadlineNativeMs
}

// connDriftPersistSamples is how many consecutive samples the
// accepted - closed - active balance must stay off zero WITH THE SAME
// SIGN before I-CONN-2 fires. The three counters are read at slightly
// different instants by the refapp's /debug/vars handler (two hook
// atomics plus the engine's active gauge), so under load a single
// sample is routinely off by the connections that were mid-accept or
// mid-close between the reads. That skew flips sign randomly and
// returns to zero within a tick; a real accounting bug (a transition
// that bumps one counter and not the other) is a permanent, same-sign
// offset.
const connDriftPersistSamples = 30

// ICONN1 asserts every accepted connection closes within the engine's
// close deadline of its last observed byte. Catches FD leaks where the
// engine forgets a peer-closed socket and the connection state machine
// never observes EOF (the classic adaptive-standby phantom-socket bug,
// PR #49).
//
// The input is a per-connection last-byte table the refapps keep
// (validation/refapp/internal/debugvars/conntable.go). It lives there
// rather than in the engine on purpose: an engine-side gauge is a minimum
// over the live-conn set, and a connection the engine has FORGOTTEN is in
// no live set -- which is precisely the class this predicate exists to
// catch.
var ICONN1 = Spec{
	ID:          "I-CONN-1",
	Description: "every accepted conn closes within the engine's close deadline of its last byte",
	Tier:        "core",
	Persist:     connClosePersistSamples,
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		deadline := connCloseDeadlineMsFor(snap.EngineName)
		if snap.OldestOpenConnLastByteAgeMs > deadline {
			return false, fmt.Sprintf(
				"I-CONN-1 violated: oldest open conn last-byte age %dms > %dms (engine %q); suggests FD leak or stuck reader",
				snap.OldestOpenConnLastByteAgeMs, deadline, snap.EngineName)
		}
		return true, ""
	},
}

// connDrift is the I-CONN-2 balance for one sample.
func connDrift(s Snapshot) int64 {
	return s.AcceptedConnTotal - s.ClosedConnTotal - s.ActiveConns
}

// ICONN2 asserts accepted_total - closed_total - active == 0. Any
// persistent non-zero result is an accounting bug somewhere in the
// engine's connection lifecycle bookkeeping — every connState
// transition must keep the three counters in lockstep.
//
// Inputs (refapp /debug/vars): accepted/closed are counted by the
// refapp in celeris.Config.OnConnect / OnDisconnect (fired by every
// engine on real accept/close, never on adaptive transplants), active
// is the engine's ActiveConnections gauge (adaptive sums both
// sub-engines). The balance is judged only when it has been off zero
// with the same sign for connDriftPersistSamples consecutive samples
// -- see that constant for why a single off-by-one sample is noise.
var ICONN2 = Spec{
	ID:          "I-CONN-2",
	Description: "accepted_total - closed_total - active == 0 (persistent same-sign drift over 30 samples)",
	Tier:        "core",
	Predicate: func(snap *Snapshot, ctx Context) (bool, string) {
		diff := connDrift(*snap)
		if diff == 0 {
			return true, ""
		}
		// Count the trailing run of same-sign non-zero drift. History
		// carries the current sample as its last element when the
		// caller appends before evaluating; either way the current
		// sample counts once.
		run := 1
		for i := len(ctx.History) - 1; i >= 0; i-- {
			h := ctx.History[i]
			if h.TS == snap.TS {
				continue
			}
			d := connDrift(h)
			if d == 0 || (d > 0) != (diff > 0) {
				break
			}
			run++
		}
		if run < connDriftPersistSamples {
			return true, ""
		}
		return false, fmt.Sprintf(
			"I-CONN-2 violated: accepted(%d) - closed(%d) - active(%d) = %d (want 0) for %d consecutive samples; engine conn accounting drift",
			snap.AcceptedConnTotal, snap.ClosedConnTotal, snap.ActiveConns, diff, run)
	},
}
