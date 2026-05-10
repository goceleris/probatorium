package properties

import "fmt"

// IENGIOURing is the io_uring engine invariant: submitted-SQEs must
// match completed-CQEs in steady state — every prepared op completes
// (in the kernel sense) before the engine drops its reference. Catches
// the send-queue corruption bugs we hit in PR #36 (SQE drops on full
// ring) and the connState UAF in PR #244.
//
// Wave 6 STUB: needs the -tags=validation build to surface
// SQE / CQE counters. The predicate sanity-checks the counters and
// TODOs the real check.
var IENGIOURing = Spec{
	ID:          "I-ENG-IOURING",
	Description: "io_uring SQE/CQE accounting balances in steady state",
	Tier:        "engine",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		// TODO(wave-7): drift = SQEsSubmitted - CQEsCompleted should
		// settle to a small bounded value (the in-flight depth, capped
		// at ring depth). For now: counters must be non-negative and
		// completed must not lead submitted (an impossibility absent a
		// counter bug).
		if snap.IOUringSQEsSubmitted < 0 || snap.IOUringCQEsCompleted < 0 {
			return false, fmt.Sprintf(
				"I-ENG-IOURING violated: counters negative (sqe=%d cqe=%d)",
				snap.IOUringSQEsSubmitted, snap.IOUringCQEsCompleted)
		}
		if snap.IOUringCQEsCompleted > snap.IOUringSQEsSubmitted {
			return false, fmt.Sprintf(
				"I-ENG-IOURING violated: CQEs(%d) > SQEs(%d); counter wiring bug",
				snap.IOUringCQEsCompleted, snap.IOUringSQEsSubmitted)
		}
		return true, ""
	},
}
