package validation

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// The tick that sees the refapp crashed or wedged ends the cell, and so does
// the orchestrator's incident loop: Run receives until the first hard
// incident, then cancels, waits for the tiers and returns, and nothing reads
// violations again. Two things must survive that tick:
//   - the wedge (I-HANG / I-LIVENESS) itself, even when the one slot is taken
//     by a record-only incident (re-review 1 of probatorium#412 round 2), and
//   - the record-only incidents still pending on it. The round-3 live run
//     (round3/588/20260927T103106Z-5b6b53a, adaptive cell) lost the gated
//     I-H2C-HANG dossier this way: the h2c reads timed out 9 ms after a
//     periodic tick, the wedge detector cancelled the tier before the next
//     one, and on the final tick the wedge went first, so the loop ended
//     before it ever received the I-H2C-HANG.

// runLoop mimics Run's incident loop: once start is closed it receives until
// the first hard incident (the hard-fail path returns), recording every
// predicate it received. It gives up after 5 s with nothing to receive.
func runLoop(ch chan Incident, start <-chan struct{}) (*[]string, <-chan struct{}) {
	var ids []string
	done := make(chan struct{})
	go func() {
		defer close(done)
		<-start
		for {
			select {
			case inc := <-ch:
				ids = append(ids, inc.PredicateID)
				if !inc.RecordOnly {
					return
				}
			case <-time.After(5 * time.Second):
				return
			}
		}
	}()
	return &ids, done
}

func waitDone(t *testing.T, what string, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(10 * time.Second):
		t.Fatalf("%s did not finish", what)
	}
}

// The live-run shape: a fault-control run, the loop busy with an in-stall
// dossier (its incident still in the slot), and a gated walker counter that
// moved after the last periodic tick. The final tick must hand the loop the
// record-only I-H2C-HANG before the wedge that ends it.
func TestFinalTickRecordReachesTheLoopBeforeTheWedgeEndsIt(t *testing.T) {
	t.Setenv(refappFaultEnv, "/ws:8s@30s,/:40s@60s")
	o := &Orchestrator{}
	violations := make(chan Incident, 1)
	cb := o.tallyIncidentCallback(context.Background(), violations, func() int { return 4242 })

	violations <- Incident{PredicateID: properties.IH2CStall.ID, RecordOnly: true} // the loop is busy
	start := make(chan struct{})
	got, loopDone := runLoop(violations, start)

	var snap tier1TallySnapshot
	snap.H2CChurn.Hang = 15   // the reads timed out after the last periodic tick
	snap.Liveness.Hung = true // and the wedge detector ended the tier
	tickDone := make(chan struct{})
	go func() { cb(snap); close(tickDone) }()
	time.Sleep(100 * time.Millisecond) // the tick meets the full slot
	close(start)
	waitDone(t, "the incident loop", loopDone)
	waitDone(t, "the final tick", tickDone)

	want := []string{properties.IH2CStall.ID, properties.IH2CHang.ID, properties.IHang.ID}
	if !slices.Equal(*got, want) {
		t.Fatalf("the loop received %v, want %v: a record-only incident pending on the final tick must reach the loop before the wedge ends it", *got, want)
	}
}

// A record-only incident a busy loop dropped earlier is retried on the final
// tick. It must not cost the wedge its delivery: both reach the loop, the
// record first (the loop keeps receiving after it), then the wedge.
func TestFinalTickDeliversARetriedRecordAndTheWedge(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	o := &Orchestrator{}
	violations := make(chan Incident, 1)
	cb := o.tallyIncidentCallback(context.Background(), violations, func() int { return 4242 })

	var snap tier1TallySnapshot
	snap.H2CChurn.Hang = 1
	violations <- Incident{PredicateID: properties.IH2CStall.ID, RecordOnly: true}
	cb(snap) // a busy tick: the record-only I-H2C-HANG is dropped and left unmarked
	if ids := tallyIDs(violations); len(ids) != 1 || ids[0] != properties.IH2CStall.ID {
		t.Fatalf("premise: the busy tick should have left only the queued in-stall incident, got %v", ids)
	}

	start := make(chan struct{})
	got, loopDone := runLoop(violations, start)
	snap.Liveness.Hung = true
	tickDone := make(chan struct{})
	go func() { cb(snap); close(tickDone) }()
	time.Sleep(100 * time.Millisecond) // the loop is still inside a capture
	close(start)
	waitDone(t, "the incident loop", loopDone)
	waitDone(t, "the final tick", tickDone)

	want := []string{properties.IH2CHang.ID, properties.IHang.ID}
	if !slices.Equal(*got, want) {
		t.Fatalf("the loop received %v, want %v: the final tick of a wedged cell must deliver the retried record AND the wedge", *got, want)
	}
}
