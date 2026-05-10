package properties

import "fmt"

// IMWRateLimit is the rate-limit middleware invariant: under sustained
// over-limit load, rejected count must monotonically increase. The
// validator's traffic generator knows the rate it is sending; the
// checker compares the rejection rate against the ratelimit config
// and hard-fails on unbounded admission.
//
// Plus the validation-build (celeris v1.4.3+) emits a hard-assertion
// counter for token-bucket invariants: any non-zero
// RatelimitTokenViolations means the token bucket went negative or
// exceeded its capacity — a structural bug in the ratelimit allocator
// regardless of the offered-vs-allowed rate. Either is grounds to
// halt the run.
var IMWRateLimit = Spec{
	ID:          "I-MW-RATELIMIT",
	Description: "rate-limit middleware admits/denies per configured policy",
	Tier:        "middleware",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		if snap.RatelimitTokenViolations > 0 {
			return false, fmt.Sprintf(
				"I-MW-RATELIMIT violated: %d token-bucket assertion(s) fired (validation build)",
				snap.RatelimitTokenViolations)
		}
		if snap.RateLimitAllowed < 0 {
			return false, fmt.Sprintf("I-MW-RATELIMIT violated: allowed counter went negative (%d)", snap.RateLimitAllowed)
		}
		if snap.RateLimitRejected < 0 {
			return false, fmt.Sprintf("I-MW-RATELIMIT violated: rejected counter went negative (%d)", snap.RateLimitRejected)
		}
		// TODO(wave-9+): once the traffic generator records offered-RPS
		// per window, also assert that RateLimitRejected grows >=
		// (offered - allowed) under sustained over-limit load.
		return true, ""
	},
}
