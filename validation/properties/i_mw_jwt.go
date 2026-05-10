package properties

import "fmt"

// IMWJWT is the JWT middleware invariant: no token with `exp < now()`
// may ever be admitted by the middleware. Plus any valid token MUST
// verify, and any mutated/expired token MUST be rejected.
//
// The hard assertion lives on the validation socket
// (JWTLateAdmits): celeris's validation build emits one every time
// the JWT verifier accepts a token whose exp claim is already in the
// past — i.e. a clock-skew or epsilon-window bug in the validator.
// Non-zero is grounds to halt the run.
var IMWJWT = Spec{
	ID:          "I-MW-JWT",
	Description: "JWT middleware rejects every token past its expiry",
	Tier:        "middleware",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.JWTLateAdmits > 0 {
			return false, fmt.Sprintf(
				"I-MW-JWT violated: %d expired token(s) accepted (validation build)",
				snap.JWTLateAdmits)
		}
		if snap.JWTValidatedOK < 0 || snap.JWTValidatedFail < 0 {
			return false, fmt.Sprintf(
				"I-MW-JWT violated: counters negative (ok=%d fail=%d)",
				snap.JWTValidatedOK, snap.JWTValidatedFail)
		}
		return true, ""
	},
}
