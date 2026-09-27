package validation

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// A terminal tick (the refapp crashed or is wedged) runs on Tier 1's own
// goroutines, and whatever it does not wait for happens next: the final tick
// returns, driveTier1 returns -- its deferred SIGTERM stops the refapp -- and
// runTierProperty forgets the side-listener accessor. Until the re-review of
// probatorium#412 round 3 the tick returned as soon as the wedge was in the
// channel, so Run's loop took the I-HANG dossier AFTER all of that: 0 of 17
// I-HANG dossiers since b7d91bf had a goroutine dump, every one read
// pprof_source=engine, and in one the refapp had logged its SIGTERM before
// the dossier was written. A record-only incident delivered on that tick
// (1c00c1c) was captured the same way.
//
// Run's loop ends the run (cancel) right after it captured a hard incident,
// and I-LIVENESS / I-HANG are hard, so the terminal tick now waits for the run
// to end once it delivered one: every dossier the tick delivered is then
// taken while Tier 1 -- and the refapp, and its side listener -- is still up.

// tickReturned reports, without blocking, whether the tick has returned.
func tickReturned(done <-chan struct{}) bool {
	select {
	case <-done:
		return true
	default:
		return false
	}
}

// The loop takes the record-only incident, then the wedge; the tick holds
// Tier 1 through both captures and returns only once the loop ends the run.
func TestTerminalTickHoldsTier1UntilTheLoopEndsTheRun(t *testing.T) {
	t.Setenv(refappFaultEnv, "/ws:8s@30s,/:40s@60s")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		o := &Orchestrator{}
		violations := make(chan Incident, 1)
		cb := o.tallyIncidentCallback(ctx, violations, func() int { return 4242 })

		var snap tier1TallySnapshot
		snap.H2CChurn.Hang = 15   // a record-only incident pending on the terminal tick
		snap.Liveness.Hung = true // and the wedge
		tickDone := make(chan struct{})
		go func() { cb(snap); close(tickDone) }()
		synctest.Wait()

		if inc := <-violations; inc.PredicateID != properties.IH2CHang.ID {
			t.Fatalf("premise: the loop should first receive the record-only %s, got %q", properties.IH2CHang.ID, inc.PredicateID)
		}
		synctest.Wait() // the loop is capturing the record-only dossier
		if tickReturned(tickDone) {
			t.Fatal("the terminal tick returned while the loop was still capturing the record-only dossier it delivered: Tier 1 returns, SIGTERMs the refapp and drops its side listener under the capture")
		}
		if inc := <-violations; inc.PredicateID != properties.IHang.ID {
			t.Fatalf("premise: the loop should then receive %s, got %q", properties.IHang.ID, inc.PredicateID)
		}
		synctest.Wait() // the loop is capturing the wedge's dossier
		if tickReturned(tickDone) {
			t.Fatal("the terminal tick returned once the wedge was in the channel, before the loop captured it: the I-HANG dossier is taken after the SIGTERM, with no side listener and no goroutine dump")
		}
		cancel() // Run: the capture is done, end the run
		synctest.Wait()
		if !tickReturned(tickDone) {
			t.Fatal("the run ended but the terminal tick still holds Tier 1")
		}
	})
}

// The loop never ends the run after taking the wedge (it cannot happen in
// Run, but the wait must not depend on it): the tick gives up after
// terminalIncidentWait, so Tier 1 still returns.
func TestTerminalTickWaitForTheRunToEndIsBounded(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		o := &Orchestrator{}
		violations := make(chan Incident, 1)
		cb := o.tallyIncidentCallback(ctx, violations, func() int { return 4242 })

		var snap tier1TallySnapshot
		snap.Liveness.Hung = true
		begin := time.Now()
		tickDone := make(chan struct{})
		go func() { cb(snap); close(tickDone) }()
		synctest.Wait()
		if inc := <-violations; inc.PredicateID != properties.IHang.ID {
			t.Fatalf("premise: the loop should receive %s, got %q", properties.IHang.ID, inc.PredicateID)
		}
		limit := terminalIncidentWait + time.Second
		select {
		case <-tickDone:
		case <-time.After(limit):
			t.Fatalf("a run that never ends holds the terminal tick past %s", limit)
		}
		if took := time.Since(begin); took < terminalIncidentWait {
			t.Fatalf("the tick returned after %s, before terminalIncidentWait (%s): it did not wait for the loop's capture", took, terminalIncidentWait)
		}
	})
}
