package properties

import "fmt"

// IDRV is the driver-consistency invariant: every read the validator
// issued after a write must hit the value most recently written for
// that key (read-after-write consistency, single-writer model).
//
// The validator's traffic generator maintains a shadow map of
// key→value writes; after every read it diffs the response payload
// against the shadow's expectation. Mismatches increment
// DriverReadMisses. The hit/miss counters here are the projection of
// that shadow check.
//
// Wave 6 STUB: the shadow map lives in the validator-checker, which
// populates DriverReadHits / Misses directly. The predicate's only
// duty is to halt the run on the first miss.
var IDRV = Spec{
	ID:          "I-DRV",
	Description: "driver reads return the most-recent shadow-tracked write",
	Tier:        "driver",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.DriverReadMisses > 0 {
			return false, fmt.Sprintf(
				"I-DRV violated: %d driver read-after-write inconsistencies (writes=%d reads=%d hits=%d)",
				snap.DriverReadMisses, snap.DriverWritesIssued, snap.DriverReadsIssued, snap.DriverReadHits)
		}
		// Sanity: read count should equal hits + misses; misses already
		// trip the violation above, so this catches an internal accounting
		// bug in the validator itself.
		if snap.DriverReadsIssued > 0 && snap.DriverReadHits > snap.DriverReadsIssued {
			return false, fmt.Sprintf(
				"I-DRV validator-internal: hits(%d) > reads_issued(%d)",
				snap.DriverReadHits, snap.DriverReadsIssued)
		}
		return true, ""
	},
}
