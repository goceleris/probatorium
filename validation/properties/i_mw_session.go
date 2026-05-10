package properties

import "fmt"

// IMWSession is the session middleware invariant: every Context's
// session owner must match the authenticated user that produced the
// request, and session created/expired counters must remain coherent.
//
// The strong assertion lives on the validation socket
// (SessionOwnerMismatches): celeris's validation build emits one
// every time a handler observes a session whose Owner field is not
// the authenticated user — i.e. session bleed across goroutines, the
// worst middleware bug class. Non-zero is grounds to halt the run.
var IMWSession = Spec{
	ID:          "I-MW-SESSION",
	Description: "session middleware preserves owner identity",
	Tier:        "middleware",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.SessionOwnerMismatches > 0 {
			return false, fmt.Sprintf(
				"I-MW-SESSION violated: %d session-owner mismatch(es) (validation build)",
				snap.SessionOwnerMismatches)
		}
		if snap.SessionsCreatedTotal < 0 || snap.SessionsExpiredTotal < 0 {
			return false, fmt.Sprintf(
				"I-MW-SESSION violated: counters negative (created=%d expired=%d)",
				snap.SessionsCreatedTotal, snap.SessionsExpiredTotal)
		}
		if snap.SessionsCreatedTotal == 0 && snap.SessionsExpiredTotal > 0 {
			return false, fmt.Sprintf(
				"I-MW-SESSION violated: %d sessions expired with zero ever created",
				snap.SessionsExpiredTotal)
		}
		return true, ""
	},
}
