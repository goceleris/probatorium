package properties

import "fmt"

// IPANIC asserts the celeris process has not panicked. The middleware
// recovery package catches handler panics and increments the counter
// without crashing the server; we still want to halt validation runs
// on the first panic so the postmortem captures the stack while the
// process is hot.
var IPANIC = Spec{
	ID:          "I-PANIC",
	Description: "panic count == 0",
	Tier:        "core",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.PanicCount > 0 {
			return false, fmt.Sprintf("I-PANIC violated: %d panic(s) observed", snap.PanicCount)
		}
		return true, ""
	},
}
