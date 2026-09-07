package properties

import "fmt"

// ipanicPersistence is how many consecutive snapshots must show unexpected
// panics before I-PANIC fires. A designed panic is counted by the server
// (panic_count) a moment before the Tier 1 walker records its 5xx
// (ExpectedPanics), so a single snapshot can legitimately see the server one
// ahead; three consecutive samples cannot.
const ipanicPersistence = 3

// IPANIC asserts the celeris process has not panicked UNEXPECTEDLY. The
// middleware/recovery package catches handler panics and increments the
// counter without crashing the server; the corpus may DESIGN some panics
// (`expect: panic` states such as observability's induced-panic route),
// which the Tier 1 walker counts into Snapshot.ExpectedPanics. The predicate
// judges PanicCount - ExpectedPanics and requires the excess to persist
// across ipanicPersistence consecutive snapshots.
var IPANIC = Spec{
	ID:          "I-PANIC",
	Description: "unexpected panic count == 0",
	Tier:        "core",
	Predicate: func(snap *Snapshot, ctx Context) (bool, string) {
		if unexpectedPanics(snap) <= 0 {
			return true, ""
		}
		// The Evaluator appends the current snapshot to ctx.History before
		// evaluating, so count the trailing run of excess samples there; a
		// direct call with a History that does not end in this snapshot
		// counts the snapshot itself as one more sample.
		seen := 0
		for i := len(ctx.History) - 1; i >= 0 && seen < ipanicPersistence; i-- {
			if unexpectedPanics(&ctx.History[i]) <= 0 {
				break
			}
			seen++
		}
		if n := len(ctx.History); n == 0 || !sameSample(&ctx.History[n-1], snap) {
			seen++
		}
		if seen < ipanicPersistence {
			return true, ""
		}
		return false, fmt.Sprintf("I-PANIC violated: %d unexpected panic(s) (panic_count=%d, expected=%d)",
			unexpectedPanics(snap), snap.PanicCount, snap.ExpectedPanics)
	},
}

// sameSample reports whether h is the very snapshot being evaluated (the
// Evaluator appends it to History first). Snapshot timestamps have 1 s
// resolution, so compare the counters too.
func sameSample(h, s *Snapshot) bool {
	return h.TS == s.TS && h.PanicCount == s.PanicCount && h.ExpectedPanics == s.ExpectedPanics &&
		h.GoroutineCount == s.GoroutineCount && h.HeapInuseBytes == s.HeapInuseBytes
}

func unexpectedPanics(s *Snapshot) int64 { return s.PanicCount - s.ExpectedPanics }
