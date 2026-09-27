package validation

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// With the run still live and a loop that never drains, every wait of a
// terminal tick -- the pending record-only incident's and the wedge's -- is
// bounded by terminalIncidentWait.
func TestFinalTickWaitIsBounded(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	synctest.Test(t, func(t *testing.T) {
		// The bubble's fake clock: the real bound, no real wait.
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		o := &Orchestrator{}
		violations := make(chan Incident, 1)
		cb := o.tallyIncidentCallback(ctx, violations, func() int { return 4242 })

		violations <- Incident{PredicateID: properties.IWSStall.ID} // never drained
		var snap tier1TallySnapshot
		snap.H2CChurn.Hang = 1
		snap.Liveness.Hung = true
		begin := time.Now()
		done := make(chan struct{})
		go func() { cb(snap); close(done) }()
		limit := 2*terminalIncidentWait + time.Second
		select {
		case <-done:
		case <-time.After(limit):
			t.Fatalf("a loop that never drains holds the tick past %s (two waits of terminalIncidentWait)", limit)
		}
		if took := time.Since(begin); took < terminalIncidentWait {
			t.Fatalf("the tick returned after %s, before one terminalIncidentWait (%s): it did not wait", took, terminalIncidentWait)
		}
	})
}
