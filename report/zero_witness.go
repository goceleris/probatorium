package report

import "sort"

// ZeroWitnessMeaning maps each must-stay-zero engine counter to the defect a
// nonzero value witnesses. Exported so the gate renders the same sentence
// the harness recorded, and so a new witness is added in exactly one place.
//
// It lives in report, not in the checker that populates it, for one
// reason: two copies of this set would drift, and a witness declared on
// one side and recorded on the other is silent in both directions -- the
// gate would judge a counter nobody records, or record one nobody judges.
//
// These are not thresholds. Every one of them counts an event that cannot
// happen in a correct engine, so one is a failure.
var ZeroWitnessMeaning = map[string]string{
	"engine_transplant_adopt_slot_occupied": "an epoll->io_uring transplant found its target slot already occupied, bumped an error count, returned, and never closed the descriptor: the connection left one engine and arrived at neither (celeris#624)",
	"engine_close_missing_conn_state":       "a close decremented the live-connection gauge with no connection state attached, so OnDisconnect was skipped: the engine and the refapp now disagree about how many connections are open (celeris#624)",
	"engine_recv_double_armed":              "a second recv was armed while one was already in flight; both target the same buffer, so the kernel's second write clobbers the first's unread bytes as mid-stream corruption (celeris#484)",
	"engine_recv_cqe_unaccounted":           "a recv completion arrived that no armed recv accounts for (celeris#484)",
	"engine_recv_sq_full":                   "prepareRecv returned early because the submission queue was full, leaving needsRecv set (celeris#607's standing guard: this read zero across the entire investigation that produced it)",
	"engine_recv_stall_episodes":            "a dirty connection with needsRecv set was skipped because a send was outstanding (celeris#607's standing guard)",
}

// engineRequestCoverageFactor is how far the engine's own request count may
// fall below the walker's requests_sent before the gate calls it a counter
// defect. Ten is chosen to be unarguable in both directions: refapps that
// answer one walker operation with several requests run ABOVE the walker's
// count, and celeris#626 ran three orders of magnitude below it.
const engineRequestCoverageFactor = 10

// sortedKeys returns m's keys in a stable order, so a gate report reads the
// same way twice and a diff between two runs is a real difference.
func sortedKeys(m map[string]int64) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
