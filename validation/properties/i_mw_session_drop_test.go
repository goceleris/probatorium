package properties

import (
	"strings"
	"testing"
)

// A session cookie the middleware could not put on the wire is celeris#507:
// the client never learns the id it was granted, so the owner it
// authenticated as is not the owner its next request carries. celeris
// counts it in every build (middleware/session.DroppedCookies), so unlike
// the owner-mismatch assertion this one needs no validation tag -- it is the
// signal I-MW-SESSION was written for and never had.
func TestIMWSession_failsOnDroppedCookie(t *testing.T) {
	ok, msg := IMWSession.Predicate(&Snapshot{SessionCookieDrops: 1}, ctxWith(0, nil, false))
	if ok {
		t.Fatal("a dropped id-changing session cookie must be a violation")
	}
	if !strings.Contains(msg, "cookie") {
		t.Fatalf("violation message must name the dropped cookie, got %q", msg)
	}
}
