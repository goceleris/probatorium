package properties

import "fmt"

// connCloseDeadlineMs is the grace window between last-byte timestamp
// and close. RFC 9112 §9.6 lets a server close idle conns at will, but
// celeris's documented worst case is read+write timeout (30s default),
// so anything older than that is a leak.
const connCloseDeadlineMs = 30_000

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

// ICONN1 asserts every accepted connection closes within 30s of its
// last observed byte. Catches FD leaks where the engine forgets a
// peer-closed socket and the connection state machine never observes
// EOF (the classic adaptive-standby phantom-socket bug, PR #49).
//
// NOT INSTRUMENTED: nothing populates OldestOpenConnLastByteAgeMs yet
// (it needs a per-connection last-byte table in the refapp), so this
// predicate cannot fire today. Kept registered so the gap stays
// visible in plan.json.
var ICONN1 = Spec{
	ID:          "I-CONN-1",
	Description: "every accepted conn closes within 30s of last byte",
	Tier:        "core",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.OldestOpenConnLastByteAgeMs > connCloseDeadlineMs {
			return false, fmt.Sprintf(
				"I-CONN-1 violated: oldest open conn last-byte age %dms > %dms; suggests FD leak or stuck reader",
				snap.OldestOpenConnLastByteAgeMs, connCloseDeadlineMs)
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
