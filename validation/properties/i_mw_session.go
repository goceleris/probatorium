package properties

import "fmt"

// IMWSession is the session middleware invariant: created and expired
// session counts converge — every CreatedTotal eventually balances
// against ExpiredTotal + ActiveSessions. Catches the worst session
// store bugs (leaks where IDs accumulate without ever expiring,
// double-issue races).
//
// Wave 6 STUB: needs ActiveSessions counter from -tags=validation
// build to make the full assertion. For now we sanity-check the
// counters and TODO the real check to wave 7.
var IMWSession = Spec{
	ID:          "I-MW-SESSION",
	Description: "session created/expired counters converge",
	Tier:        "middleware",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		// TODO(wave-7): assert CreatedTotal == ExpiredTotal + ActiveSessions
		// once the validation-tagged build exposes ActiveSessions. Until
		// then, reject any obviously absurd state — negative counters
		// imply a bug in the counter wiring, and a non-zero expired
		// without any created is structurally impossible.
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
