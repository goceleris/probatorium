package properties

import "fmt"

// ICHECKPTR asserts the -checkptr build of celeris has not fired. The
// checker greps celeris's stderr for "fatal error: checkptr" markers
// and increments CheckptrReports. checkptr findings are usually
// unsafe.Pointer / uintptr math bugs in the engine's SIMD / iouring
// fast paths; any hit is fatal.
var ICHECKPTR = Spec{
	ID:          "I-CHECKPTR",
	Description: "checkptr (-d=checkptr) has not fired",
	Tier:        "core",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.CheckptrReports > 0 {
			return false, fmt.Sprintf(
				"I-CHECKPTR violated: %d checkptr reports observed; review SIMD / unsafe.Pointer paths",
				snap.CheckptrReports)
		}
		return true, ""
	},
}
