package properties

import "fmt"

// IMWRateLimit is the rate-limit middleware invariant: under sustained
// over-limit load, rejected count must monotonically increase. The
// validator's traffic generator knows the rate it is sending; the
// checker compares the rejection rate against the ratelimit config
// and hard-fails on unbounded admission.
//
// Wave 6 STUB: until the -tags=validation build of celeris exports
// per-key rate-limit counters (wave 7), the predicate only enforces
// the trivial "RateLimitRejected counter is non-negative" check via
// the int64 type itself. The TODO below tracks the real assertion.
var IMWRateLimit = Spec{
	ID:          "I-MW-RATELIMIT",
	Description: "rate-limit middleware admits/denies per configured policy",
	Tier:        "middleware",
	Predicate: func(snap *Snapshot, _ Context) (bool, string) {
		// TODO(wave-7): assert that under controlled over-RPS load the
		// RateLimitRejected counter grows >= (offered - allowed) per
		// window; for now we only verify the counters are sane.
		if snap.RateLimitAllowed < 0 {
			return false, fmt.Sprintf("I-MW-RATELIMIT violated: allowed counter went negative (%d)", snap.RateLimitAllowed)
		}
		if snap.RateLimitRejected < 0 {
			return false, fmt.Sprintf("I-MW-RATELIMIT violated: rejected counter went negative (%d)", snap.RateLimitRejected)
		}
		return true, ""
	},
}
