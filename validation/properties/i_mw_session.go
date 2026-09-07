package properties

import "fmt"

// IMWSession is the session middleware invariant: every request must see
// the session owner it authenticated as, the client must actually receive
// every id it is granted, and the created/expired counters must remain
// coherent.
//
// Two independent sources feed SessionOwnerMismatches. The refapps that
// install session middleware keep an id→owner ledger (they minted the id,
// so they know who it belongs to) and count every request that presents a
// known id under a different owner — session bleed across goroutines, the
// worst middleware bug class, observable in a plain build. celeris's
// validation build raises the same counter from inside the middleware.
// Either is grounds to halt the run.
//
// SessionCookieDrops is celeris's own DroppedCookies() gauge and covers the
// other half of "owner identity": an id-changing Set-Cookie the middleware
// could not put on the wire means the client never learns the session it
// just authenticated into and silently starts over on its next request
// (celeris#507, which failed two consecutive soaks while this predicate had
// no data at all).
var IMWSession = Spec{
	ID:          "I-MW-SESSION",
	Description: "session middleware preserves owner identity and delivers every id-changing cookie",
	Tier:        "middleware",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.SessionOwnerMismatches > 0 {
			return false, fmt.Sprintf(
				"I-MW-SESSION violated: %d session-owner mismatch(es): a request presented a known session id under a different owner",
				snap.SessionOwnerMismatches)
		}
		if snap.SessionCookieDrops > 0 {
			return false, fmt.Sprintf(
				"I-MW-SESSION violated: %d id-changing session cookie(s) dropped before reaching the client (celeris session.DroppedCookies; mutate the session before writing the body)",
				snap.SessionCookieDrops)
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
