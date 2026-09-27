package validation

import (
	"context"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// probatorium#412 review round 3: the property tier's incident channel
// holds ONE incident, and on a crash or a wedge the final synchronous tally
// tick in driveTier1 is often the only offer of I-LIVENESS / I-HANG. Since
// offerOnce retries a dropped offer, a record-only walker incident that an
// earlier busy loop dropped is offered again on every tick -- and the tick
// offered the record-only incidents before the hard ones, so on that final
// tick the retried record took the free slot and the wedge was dropped for
// good: no abort, no gcore, no goroutine dump of the wedged process. A full
// slot on that tick (the loop inside a slow capture with one incident
// queued) dropped the wedge too.

// tallyIDs drains ch without blocking and returns the predicate IDs in
// arrival order.
func tallyIDs(ch chan Incident) []string {
	var ids []string
	for {
		select {
		case inc := <-ch:
			ids = append(ids, inc.PredicateID)
		default:
			return ids
		}
	}
}

func hasID(ids []string, id string) bool {
	for _, x := range ids {
		if x == id {
			return true
		}
	}
	return false
}

// The final tick of a wedged cell offers I-HANG before a record-only
// incident an earlier busy loop dropped.
func TestFinalTickOffersTheWedgeBeforeARetriedRecord(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	o := &Orchestrator{}
	violations := make(chan Incident, 1)
	cb := o.tallyIncidentCallback(context.Background(), violations, func() int { return 4242 })

	var snap tier1TallySnapshot
	snap.H2CChurn.Hang = 1
	// A tick while the loop is busy: an in-stall incident is queued, so the
	// record-only I-H2C-HANG offer is dropped (and left unmarked, to retry).
	violations <- Incident{PredicateID: properties.IH2CStall.ID}
	cb(snap)
	if ids := tallyIDs(violations); len(ids) != 1 || ids[0] != properties.IH2CStall.ID {
		t.Fatalf("premise: the busy tick should have left only the queued in-stall incident, got %v", ids)
	}

	// The final synchronous tick of a wedged cell: the slot is free.
	snap.Liveness.Hung = true
	cb(snap)
	ids := tallyIDs(violations)
	if !hasID(ids, properties.IHang.ID) {
		t.Fatalf("the final tick of a wedged cell delivered %v: I-HANG lost the one slot to a retried record-only incident, so the wedge is never aborted or dumped", ids)
	}
}

// A hard walker incident (a routine run's I-WS-ECHO) is offered before a
// record-only one pending on the same tick.
func TestTickOffersAHardWalkerIncidentBeforeARecord(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	o := &Orchestrator{}
	violations := make(chan Incident, 1)
	cb := o.tallyIncidentCallback(context.Background(), violations, func() int { return 4242 })

	var snap tier1TallySnapshot
	snap.H2CChurn.Hang = 1
	snap.WSEcho.Timeout = 1
	cb(snap)
	if ids := tallyIDs(violations); len(ids) != 1 || ids[0] != properties.IWSEcho.ID {
		t.Fatalf("one free slot, a hard I-WS-ECHO and a record-only I-H2C-HANG pending: delivered %v, want [%s]", ids, properties.IWSEcho.ID)
	}
}

// On the final tick the slot is FULL -- the loop is inside a slow capture
// with one incident queued behind it. The wedge waits for the slot instead
// of being dropped, and arrives once the loop takes the queued incident.
func TestFinalTickWaitsForTheSlotToDeliverTheWedge(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	o := &Orchestrator{}
	violations := make(chan Incident, 1)
	cb := o.tallyIncidentCallback(context.Background(), violations, func() int { return 4242 })

	violations <- Incident{PredicateID: properties.IWSStall.ID}
	var snap tier1TallySnapshot
	snap.Liveness.Hung = true
	done := make(chan struct{})
	go func() { cb(snap); close(done) }()
	// Give the tick time to find the slot full. A tick that dropped the
	// wedge returns here; one that waits for the slot does not.
	select {
	case <-done:
	case <-time.After(300 * time.Millisecond):
	}
	if first := <-violations; first.PredicateID != properties.IWSStall.ID {
		t.Fatalf("premise: the queued incident comes first, got %q", first.PredicateID)
	}
	select {
	case inc := <-violations:
		if inc.PredicateID != properties.IHang.ID {
			t.Fatalf("after the queued incident the loop received %q, want %s", inc.PredicateID, properties.IHang.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the final tick met a full slot and dropped I-HANG: the loop never receives the wedge")
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the tick did not return after its incident was delivered")
	}
}

// The wait never outlives the run: when the orchestrator ends the run
// (cancel() before wg.Wait() on a hard fail, or the run's deadline) the
// tick returns at once, so Tier 1 can return and the WaitGroup completes.
func TestFinalTickWaitEndsWithTheRun(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	o := &Orchestrator{}
	violations := make(chan Incident, 1)
	ctx, cancel := context.WithCancel(context.Background())
	cb := o.tallyIncidentCallback(ctx, violations, func() int { return 4242 })

	violations <- Incident{PredicateID: properties.IWSStall.ID} // never drained
	var snap tier1TallySnapshot
	snap.Liveness.Crashed = true
	done := make(chan struct{})
	go func() { cb(snap); close(done) }()
	time.Sleep(100 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the run ended but the tick is still waiting for the slot: Tier 1 cannot return, and the orchestrator's wg.Wait deadlocks")
	}
}
