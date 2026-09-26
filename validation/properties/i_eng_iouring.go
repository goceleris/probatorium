package properties

import "fmt"

// IENGIOURing is the io_uring engine invariant: a ring's SQ tail never moves
// backward — the PR #36 send-queue corruption bug class. Torn SQE writes are
// not judged; nothing publishes them. The assertion lives in celeris itself:
// under -tags=validation, validateSQEWrite (engine/iouring/validation_check.go,
// called from the ring's SQE getter in ring.go) increments
// validation.IouringSQECorruptions whenever a ring's SQ tail moves backward.
// The refapp publishes the counter on /debug/vars and
// validation/checker/poll.go copies it into this snapshot.
//
// It used to carry two more branches, "counters negative" and "CQEs lead
// SQEs", over Snapshot.IOUringSQEsSubmitted and .IOUringCQEsCompleted.
// Nothing ever wrote those two fields: they were declared as stubs for a
// validation build that shipped without them, so both branches compared 0
// with 0 on every evaluation and could not fail, while the predicate's
// description promised "SQE/CQE accounting balances" and every run counted
// their evaluations (probatorium#395). Restoring that check needs celeris to
// publish SQE and CQE totals first (celeris#688); engine.EngineMetrics has no
// such fields today.
var IENGIOURing = Spec{
	ID:          "I-ENG-IOURING",
	Description: "io_uring SQE write-index monotonic (validation build)",
	Tier:        "engine",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.IouringSQECorruptions > 0 {
			return false, fmt.Sprintf(
				"I-ENG-IOURING violated: %d SQE corruption(s) detected (validation build)",
				snap.IouringSQECorruptions)
		}
		return true, ""
	},
}
