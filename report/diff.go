package report

import (
	"fmt"
	"sort"
	"strings"
)

// Divergence is one finding from comparing two ValidationResults
// from the same run on two different architectures (or hosts).
// Material divergence in a "must-be-zero" counter is an arch-only
// bug — the canonical example is iouring SQE write paths behaving
// differently on amd64 vs aarch64.
//
// Kind names the comparison policy that flagged this finding:
//
//   - ZeroAsymmetric — one side has count==0, the other has count>0.
//     Highest-priority: a predicate violation that fires on ONE arch
//     and not the other almost always points at an arch-specific bug.
//   - RatioWide      — both sides non-zero but differ by > threshold.
//     Used for "should-be-the-same-rate" counters where the exact
//     value is workload-dependent but the ratio should be near-equal.
//
// Lower-confidence Kind values are intentionally NOT emitted by the
// default comparison policy — RPS and per-second metrics naturally
// drift between arches due to hardware. Only counters that encode
// invariant outcomes (well-rejected, accepted-bad, hangs, crashes)
// are policy-watched.
type Divergence struct {
	Kind     string `json:"kind"`
	Slice    string `json:"slice"`
	Counter  string `json:"counter"`
	ValA     int64  `json:"val_a"`
	ValB     int64  `json:"val_b"`
	HostA    string `json:"host_a,omitempty"`
	HostB    string `json:"host_b,omitempty"`
	Severity string `json:"severity"`
}

// DivergenceKind constants.
const (
	KindZeroAsymmetric = "zero-asymmetric"
	KindRatioWide      = "ratio-wide"
)

// Severity constants.
const (
	SeverityHigh = "high"
	SeverityMed  = "med"
	SeverityLow  = "low"
)

// invariantCounters is the canonical list of "must be zero on both
// arches" Tier 1 sub-counters. Each entry is (slice, counter); a
// non-zero count on EITHER side is interesting (predicate-tier
// surfaces it locally), but a count of 0 on one side and >0 on the
// other is the cross-arch divergence we hunt here.
//
// Authoritative reference: validation/{adversarial,h2c,ws,sse}.go
// counter docs.
var invariantCounters = []struct {
	slice    string
	counter  string
	severity string
}{
	{"adversarial", "adv_wrong_accepted", SeverityHigh},
	{"adversarial", "adv_hang_until_timeout", SeverityMed},
	{"h2c_churn", "h2c_crashed", SeverityHigh},
	{"h2c_churn", "h2c_hang", SeverityMed},
	{"ws_torture", "ws_accepted_bad_frame", SeverityHigh},
	{"ws_torture", "ws_hang_no_close", SeverityMed},
	{"sse_kill", "sse_handshake_fail", SeverityLow},
}

// DiffValidation compares two ValidationResults (typically one per
// arch on the same celeris commit) and returns the divergences
// detected by the default policy.
//
// hostA and hostB label the two sides in the resulting Divergence
// records (typically "msa2-server-amd64" and "msr1-arm64"). Pass
// empty strings if labels are unknown.
//
// Returns an empty slice when no divergences are found — that's the
// "OK" path. Callers (CLI / mage target) treat empty as success.
//
// The function is order-independent: DiffValidation(a, b, ...) and
// DiffValidation(b, a, ...) produce divergences that name the same
// arches consistently per HostA/HostB.
func DiffValidation(a, b *ValidationResults, hostA, hostB string) []Divergence {
	if a == nil || b == nil {
		return nil
	}
	var out []Divergence
	// Tier 1 sub-counters: walk the invariant list, flag asymmetric
	// zeros.
	if a.Tier1 != nil && b.Tier1 != nil {
		for _, inv := range invariantCounters {
			valA := pickCounter(a.Tier1, inv.slice, inv.counter)
			valB := pickCounter(b.Tier1, inv.slice, inv.counter)
			if isZeroAsymmetric(valA, valB) {
				out = append(out, Divergence{
					Kind:     KindZeroAsymmetric,
					Slice:    inv.slice,
					Counter:  inv.counter,
					ValA:     valA,
					ValB:     valB,
					HostA:    hostA,
					HostB:    hostB,
					Severity: inv.severity,
				})
			}
		}
	}
	// Tier 3: seeds_failed is an arch-symmetric must-be-zero
	// counter. Any non-zero asymmetry is the worst case (one arch
	// found a bug, the other didn't).
	if a.Tier3 != nil && b.Tier3 != nil {
		if isZeroAsymmetric(a.Tier3.SeedsFailed, b.Tier3.SeedsFailed) {
			out = append(out, Divergence{
				Kind:     KindZeroAsymmetric,
				Slice:    "tier_3",
				Counter:  "seeds_failed",
				ValA:     a.Tier3.SeedsFailed,
				ValB:     b.Tier3.SeedsFailed,
				HostA:    hostA,
				HostB:    hostB,
				Severity: SeverityHigh,
			})
		}
	}
	// Stable sort so the diff output is reproducible (high severity
	// first, then by slice, then counter).
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Severity != out[j].Severity {
			return severityRank(out[i].Severity) < severityRank(out[j].Severity)
		}
		if out[i].Slice != out[j].Slice {
			return out[i].Slice < out[j].Slice
		}
		return out[i].Counter < out[j].Counter
	})
	return out
}

// pickCounter reads a typed sub-tally counter out of Tier1Summary.
// Returns zero when the slice or counter key is absent — that's
// consistent with the "did this slice run?" semantics.
func pickCounter(t *Tier1Summary, slice, counter string) int64 {
	if t == nil {
		return 0
	}
	var m map[string]int64
	switch slice {
	case "adversarial":
		m = t.Adversarial
	case "h2c_churn":
		m = t.H2CChurn
	case "ws_torture":
		m = t.WSTorture
	case "sse_kill":
		m = t.SSEKill
	}
	return m[counter]
}

// isZeroAsymmetric returns true when exactly one of a, b is zero.
// Both-zero (clean) and both-non-zero (predicate violation on both
// arches — same bug, not a divergence) are NOT flagged here.
func isZeroAsymmetric(a, b int64) bool {
	return (a == 0) != (b == 0)
}

// severityRank maps severity strings to a sort priority. Lower
// number = higher priority.
func severityRank(s string) int {
	switch s {
	case SeverityHigh:
		return 0
	case SeverityMed:
		return 1
	case SeverityLow:
		return 2
	}
	return 3
}

// FormatDivergences renders a list of divergences as a human-
// readable report. Empty list returns "OK — no cross-arch divergence
// detected" so the report reads cleanly whether divergences exist
// or not.
//
// Fixed-width text (not markdown) so it can be read in a CI log
// without rendering, matching the diffBenchResults format
// convention.
func FormatDivergences(divs []Divergence, hostA, hostB string) string {
	if len(divs) == 0 {
		return "OK — no cross-arch divergence detected"
	}
	var b strings.Builder
	if hostA == "" {
		hostA = "A"
	}
	if hostB == "" {
		hostB = "B"
	}
	fmt.Fprintf(&b, "Cross-arch divergence: %s vs %s\n", hostA, hostB)
	fmt.Fprintf(&b, "%-8s %-12s %-24s %12s %12s\n", "SEV", "SLICE", "COUNTER", hostA, hostB)
	fmt.Fprintln(&b, strings.Repeat("-", 78))
	for _, d := range divs {
		fmt.Fprintf(&b, "%-8s %-12s %-24s %12d %12d\n",
			strings.ToUpper(d.Severity), d.Slice, d.Counter, d.ValA, d.ValB)
	}
	return b.String()
}
