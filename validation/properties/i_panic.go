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
		seen := 1
		for i := len(ctx.History) - 1; i >= 0 && seen < ipanicPersistence; i-- {
			h := ctx.History[i]
			if h.TS == snap.TS {
				continue // the current snapshot may already be appended
			}
			if unexpectedPanics(&h) <= 0 {
				break
			}
			seen++
		}
		if seen < ipanicPersistence {
			return true, ""
		}
		return false, fmt.Sprintf("I-PANIC violated: %d unexpected panic(s) (panic_count=%d, expected=%d)",
			unexpectedPanics(snap), snap.PanicCount, snap.ExpectedPanics)
	},
}

func unexpectedPanics(s *Snapshot) int64 { return s.PanicCount - s.ExpectedPanics }
