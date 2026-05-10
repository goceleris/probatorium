package properties

import "fmt"

// connCloseDeadlineMs is the grace window between last-byte timestamp
// and close. RFC 9112 §9.6 lets a server close idle conns at will, but
// celeris's documented worst case is read+write timeout (30s default),
// so anything older than that is a leak.
const connCloseDeadlineMs = 30_000

// ICONN1 asserts every accepted connection closes within 30s of its
// last observed byte. Catches FD leaks where the engine forgets a
// peer-closed socket and the connection state machine never observes
// EOF (the classic adaptive-standby phantom-socket bug, PR #49).
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

// ICONN2 asserts accepted_total - closed_total - active == 0. Any
// non-zero result is an accounting bug somewhere in the engine's
// connection lifecycle bookkeeping — every connState transition must
// keep the three counters in lockstep.
var ICONN2 = Spec{
	ID:          "I-CONN-2",
	Description: "accepted_total - closed_total - active == 0",
	Tier:        "core",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		diff := snap.AcceptedConnTotal - snap.ClosedConnTotal - snap.ActiveConns
		if diff != 0 {
			return false, fmt.Sprintf(
				"I-CONN-2 violated: accepted(%d) - closed(%d) - active(%d) = %d (want 0); engine conn accounting drift",
				snap.AcceptedConnTotal, snap.ClosedConnTotal, snap.ActiveConns, diff)
		}
		return true, ""
	},
}
