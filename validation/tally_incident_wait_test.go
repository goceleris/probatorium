package validation

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// With the run still live and a loop that never drains, every wait of a
// terminal tick -- the pending record-only incident's and the wedge's -- is
// bounded by terminalIncidentWait.
func TestFinalTickWaitIsBounded(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	old := terminalIncidentWait
	terminalIncidentWait = 200 * time.Millisecond
	t.Cleanup(func() { terminalIncidentWait = old })
	o := &Orchestrator{}
	violations := make(chan Incident, 1)
	cb := o.tallyIncidentCallback(context.Background(), violations, func() int { return 4242 })

	violations <- Incident{PredicateID: properties.IWSStall.ID} // never drained
	var snap tier1TallySnapshot
	snap.H2CChurn.Hang = 1
	snap.Liveness.Hung = true
	done := make(chan struct{})
	go func() { cb(snap); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("a loop that never drains holds the tick past terminalIncidentWait")
	}
}
