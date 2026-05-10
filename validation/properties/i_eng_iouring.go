package properties

import "fmt"

// IENGIOURing is the io_uring engine invariant: SQE write index is
// strictly monotonic and never produces a torn write — catches the
// PR #36 send-queue corruption bug class. The hard assertion lives on
// the validation socket: IouringSQECorruptions is incremented by
// celeris's per-ring assertion in `engine/iouring/sqe.go` whenever
// the tail index moves backward or stalls past a submission boundary.
//
// Plus the legacy SQE-vs-CQE accounting check from /debug/vars stays
// in place as a structural sanity gate (negative counters or CQEs
// leading SQEs are impossible absent a wiring bug).
var IENGIOURing = Spec{
	ID:          "I-ENG-IOURING",
	Description: "io_uring SQE write-index monotonic; SQE/CQE accounting balances",
	Tier:        "engine",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.IouringSQECorruptions > 0 {
			return false, fmt.Sprintf(
				"I-ENG-IOURING violated: %d SQE corruption(s) detected (validation build)",
				snap.IouringSQECorruptions)
		}
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
