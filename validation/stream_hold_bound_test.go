package validation

import (
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// TestStreamHoldsStayUnderTheConnCloseBound couples two constants in
// different packages that must stay related, and explains why, because the
// relationship is not obvious from either side.
//
// I-CONN-1 reads a per-connection last-byte table the refapps keep. A
// DETACHED stream — WebSocket or SSE — stops producing requests the moment
// it upgrades, so its entry stops being refreshed and begins to age.
//
// It is not refreshed from the stream handler, and that is deliberate on
// both counts:
//
//   - It cannot be. On the native engines websocket.Conn.RemoteAddr()
//     returns nil (the middleware holds no net.Conn there), so a handler has
//     no key to stamp with. Plumbing one through is a celeris API change.
//   - It should not be, even if it could. A refreshed stamp, or equivalently
//     a websocket.Config.IdleTimeout, would make a WEDGED detached
//     connection reap itself at the deadline instead of ageing — which is
//     exactly the phantom-socket signature this predicate exists to catch.
//     celeris's own config docs note a deadline is "what would have made
//     [celeris#527, #482] survivable"; survivable is right for production
//     and wrong for a torture target, where the bug should be visible.
//
// The consequence is a constraint rather than a defect: a stream held
// LEGITIMATELY for longer than the predicate's deadline would fire on
// healthy traffic. Today the walkers hold for at most two seconds against a
// forty-five second deadline, so the margin is twenty-fold. This test is
// what turns "someone will remember" into "the build says so".
func TestStreamHoldsStayUnderTheConnCloseBound(t *testing.T) {
	// The strictest bound any engine gets. Native engines take it; std's is
	// larger, so satisfying this satisfies both.
	bound := time.Duration(properties.ConnCloseDeadlineNativeMs()) * time.Millisecond

	for _, tc := range []struct {
		what string
		hold time.Duration
	}{
		{"WebSocket torture fire", wsMaxHold},
		{"SSE kill fire", sseMaxHold},
	} {
		// Half the bound, not the bound itself: a hold that merely fits
		// leaves no room for scheduling delay on a loaded cluster node, and
		// the failure mode is a red soak rather than a flaky test.
		if tc.hold*2 >= bound {
			t.Errorf("%s holds for %v, which is not comfortably under I-CONN-1's %v deadline.\n"+
				"A detached stream is not stamped in the refapp's last-byte table, so a hold near "+
				"the deadline ages into a violation on healthy traffic. Either shorten the hold, or "+
				"raise the deadline and re-check it against the engines' own reap ceilings.",
				tc.what, tc.hold, bound)
		}
	}
}
