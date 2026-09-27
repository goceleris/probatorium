package debugvars

import "testing"

// Every hold has the same waiter frame ((*FaultHold).wait) and the same
// holder frame (run), so a goroutine dump taken while two holds overlap
// cannot say which path's requests it shows: the #588 checker would credit
// one path's dossier with the other path's waiters. Overlapping holds are
// refused; holds that only touch (one ends as the next starts) are not
// (probatorium#412 review round 3).
func TestParseFaultsRefusesOverlappingHolds(t *testing.T) {
	for _, bad := range []string{
		"/ws:8s@30s,/:40s@35s", // the second starts inside the first
		"/:40s@30s,/ws:8s@40s", // the second lies inside the first
		"/ws:8s@30s,/:8s@30s",  // same window
	} {
		if _, err := ParseFaults(bad); err == nil {
			t.Errorf("%q: overlapping holds parsed, want an error", bad)
		}
	}
	for _, ok := range []string{"/ws:8s@30s,/:40s@60s", "/ws:8s@30s,/:40s@38s"} {
		if _, err := ParseFaults(ok); err != nil {
			t.Errorf("%q: disjoint holds refused: %v", ok, err)
		}
	}
}
