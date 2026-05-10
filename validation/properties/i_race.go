package properties

import "fmt"

// IRACE asserts the -race build of celeris has not reported a data
// race during this run. The validator-checker greps the celeris stderr
// stream for "WARNING: DATA RACE" and increments RaceReports on every
// match. Any positive count halts the run; race-detector output is
// canonical signal.
var IRACE = Spec{
	ID:          "I-RACE",
	Description: "race detector has not fired",
	Tier:        "core",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.RaceReports > 0 {
			return false, fmt.Sprintf(
				"I-RACE violated: %d race-detector reports observed; run was built with -race",
				snap.RaceReports)
		}
		return true, ""
	},
}
