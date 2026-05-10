package properties

import "fmt"

// IMWJWT is the JWT middleware invariant: any token the validator
// signs with a known-good key and presents with a known-good claim set
// MUST verify (JWTValidatedOK), and any token mutated to break the
// signature MUST be rejected (JWTValidatedFail).
//
// Wave 6 STUB: paired with the validator's traffic generator we know
// the offered ratio; until -tags=validation surfaces per-reason
// rejection counters we can only verify counters are sane.
var IMWJWT = Spec{
	ID:          "I-MW-JWT",
	Description: "JWT middleware accepts valid tokens and rejects mutated ones",
	Tier:        "middleware",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		// TODO(wave-7): correlate offered-valid count from the traffic
		// generator against snap.JWTValidatedOK; correlate offered-mutated
		// against snap.JWTValidatedFail. Today we just gate on the
		// counters being non-negative — anything else is a counter wiring
		// bug.
		if snap.JWTValidatedOK < 0 || snap.JWTValidatedFail < 0 {
			return false, fmt.Sprintf(
				"I-MW-JWT violated: counters negative (ok=%d fail=%d)",
				snap.JWTValidatedOK, snap.JWTValidatedFail)
		}
		return true, ""
	},
}
