package report

import (
	"fmt"
	"sort"
	"time"
)

// Arch exclusion (schema 5.22, probatorium#442).
//
// A BENCH_TARGET=both run drives the x86 SUT and the arm64 SUT from the one
// loadgen host, msa2-client, at the same time. Two arches loading the client
// together contaminated the heavy rows of the 0829 and 36441790519 runs (the
// 2x10G bond carries both arches' traffic; the client's CPU and latency path is
// shared). The runner therefore takes a host-wide lock around every heavy cell
// (cmd/runner/clientlock.go), and each pass records the interval it drove load
// in. This file is the check that proves it from a finished run: it reads the
// two arches' load windows and reports every pair the exclusion policy forbids.

// Load-window kinds.
const (
	// LoadKindSaturation is the open-loop saturation pass, warmup included.
	LoadKindSaturation = "saturation"
	// LoadKindRated is one rated pass (its own full-speed warmup plus the
	// paced window), recorded whether or not the runner kept its result.
	LoadKindRated = "rated"
)

// Exclusion policies.
const (
	// ExclusionHeavy: no heavy pass of one arch overlaps a heavy pass of the
	// other. Light passes (the driver rows and other low-demand rows) are free.
	ExclusionHeavy = "heavy"
	// ExclusionAll: no pass of one arch overlaps any pass of the other.
	ExclusionAll = "all"
	// ExclusionOff: nothing is excluded (what every both-arch run did before
	// 5.22). The check still counts overlaps and reports them, never as a
	// violation.
	ExclusionOff = "off"
)

// LoadWindow is one interval in which one arch's runner drove load through
// msa2-client: a saturation pass or a rated pass. Times come from the runner's
// own clock on msa2-client, the same clock for both arches, so no skew enters
// the comparison. Schema 5.22.
type LoadWindow struct {
	Competitor string `json:"competitor"`
	Scenario   string `json:"scenario"`
	// Kind is LoadKindSaturation or LoadKindRated.
	Kind string `json:"kind"`
	// TargetRPS is the offered load of a rated pass; zero for saturation.
	TargetRPS float64 `json:"target_rps,omitempty"`
	// Class is the scenario's client-load class, "heavy" or "light"
	// (scenarios.ClientLoadOf).
	Class string    `json:"class"`
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// ArchExclusion is what a both-arch run records about its own arch exclusion,
// stamped into BOTH arches' documents at merge time (each arch is published
// alone, so each carries the verdict). Absent on single-arch runs and on every
// document written before 5.22: for a document that comes from a both-arch run
// (its sibling results file exists, or the run was dispatched with
// target=both) the absence means the arches ran concurrently with no exclusion.
type ArchExclusion struct {
	// Policy the run enforced: heavy, all, or off.
	Policy string `json:"policy"`
	// PeerArch is the other arch of the run ("amd64" or "arm64").
	PeerArch string `json:"peer_arch"`
	// CellOrder is the order this arch walked each column's cells: "forward"
	// (the registry order) or "reverse".
	CellOrder string `json:"cell_order"`
	// Checked is the number of this arch's windows compared with the peer's.
	Checked int `json:"checked"`
	// PeerChecked is the number of the peer's windows compared.
	PeerChecked int `json:"peer_checked"`
	// HeavyHeavyOverlaps counts (this arch's window, peer's window) pairs that
	// intersect where both are heavy. Under policy heavy or all it is a
	// violation, and Violations is at least this.
	HeavyHeavyOverlaps int `json:"heavy_heavy_overlaps"`
	// HeavyLightOverlaps counts intersecting pairs where exactly one is heavy.
	// It is the load a heavy pass shared the client with under policy heavy: the
	// residual the policy accepts. Zero-valued under policy all.
	HeavyLightOverlaps int `json:"heavy_light_overlaps"`
	// LightLightOverlaps counts intersecting pairs where both are light.
	LightLightOverlaps int `json:"light_light_overlaps"`
	// Violations counts the pairs the policy forbids.
	Violations int `json:"violations"`
	// MissingTimestamps counts windows with a zero or inverted bound; they
	// cannot be checked, so a run with any is not verified clean.
	MissingTimestamps int `json:"missing_timestamps"`
	// LockWaitSeconds is the total time this arch's runners waited for the
	// client lock (heavy cells queued behind the peer's heavy cell).
	LockWaitSeconds float64 `json:"lock_wait_seconds"`
}

// OverlapPair is one intersecting (a, b) window pair.
type OverlapPair struct {
	A, B    LoadWindow
	Overlap time.Duration
}

// OverlapReport is the outcome of CheckArchOverlap.
type OverlapReport struct {
	Policy string
	// ChecksA / ChecksB are the windows compared (those with usable bounds).
	ChecksA, ChecksB                   int
	MissingA, MissingB                 int
	HeavyHeavy, HeavyLight, LightLight int
	// Violations are the pairs the policy forbids, longest overlap first and
	// capped at MaxListedViolations (Total counts them all).
	Violations []OverlapPair
	Total      int
}

// MaxListedViolations caps OverlapReport.Violations.
const MaxListedViolations = 25

// Clean reports whether the run is verified free of forbidden overlaps: no
// violation and no window that could not be checked.
func (r OverlapReport) Clean() bool {
	return r.Total == 0 && r.MissingA == 0 && r.MissingB == 0
}

// String is the one-paragraph summary mage prints.
func (r OverlapReport) String() string {
	s := fmt.Sprintf("arch overlap (policy %s): %d x %d windows checked, %d forbidden overlap(s); heavy+heavy %d, heavy+light %d, light+light %d; %d+%d window(s) without usable timestamps",
		r.Policy, r.ChecksA, r.ChecksB, r.Total, r.HeavyHeavy, r.HeavyLight, r.LightLight, r.MissingA, r.MissingB)
	for _, v := range r.Violations {
		s += fmt.Sprintf("\n  %s/%s %s [%s..%s]  x  %s/%s %s [%s..%s]  overlap %s",
			v.A.Competitor, v.A.Scenario, v.A.Kind, v.A.Start.Format(time.RFC3339), v.A.End.Format(time.RFC3339),
			v.B.Competitor, v.B.Scenario, v.B.Kind, v.B.Start.Format(time.RFC3339), v.B.End.Format(time.RFC3339),
			v.Overlap.Round(time.Millisecond))
	}
	return s
}

func windowUsable(w LoadWindow) bool {
	return !w.Start.IsZero() && !w.End.IsZero() && w.End.After(w.Start)
}

func isHeavy(w LoadWindow) bool { return w.Class != "light" } // an unknown class is heavy

// forbidden reports whether policy forbids a and b running at the same time.
func forbidden(policy string, a, b LoadWindow) bool {
	switch policy {
	case ExclusionAll:
		return true
	case ExclusionHeavy:
		return isHeavy(a) && isHeavy(b)
	}
	return false
}

// CheckArchOverlap compares the two arches' load windows of one both-arch run
// and reports every pair that intersects, with the pairs policy forbids listed.
// Two windows intersect when they share a positive length of time; windows that
// only touch do not. Windows with a zero or inverted bound are counted as
// missing, never silently skipped: a clean report needs none.
func CheckArchOverlap(a, b []LoadWindow, policy string) OverlapReport {
	r := OverlapReport{Policy: policy}
	ua := make([]LoadWindow, 0, len(a))
	for _, w := range a {
		if windowUsable(w) {
			ua = append(ua, w)
		} else {
			r.MissingA++
		}
	}
	ub := make([]LoadWindow, 0, len(b))
	for _, w := range b {
		if windowUsable(w) {
			ub = append(ub, w)
		} else {
			r.MissingB++
		}
	}
	r.ChecksA, r.ChecksB = len(ua), len(ub)
	sort.Slice(ub, func(i, j int) bool { return ub[i].Start.Before(ub[j].Start) })
	var viol []OverlapPair
	for _, x := range ua {
		for _, y := range ub {
			if !y.Start.Before(x.End) {
				break // ub is sorted by start: no later window can reach x
			}
			if !x.Start.Before(y.End) {
				continue
			}
			lo, hi := x.Start, x.End
			if y.Start.After(lo) {
				lo = y.Start
			}
			if y.End.Before(hi) {
				hi = y.End
			}
			ov := hi.Sub(lo)
			if ov <= 0 {
				continue
			}
			switch hx, hy := isHeavy(x), isHeavy(y); {
			case hx && hy:
				r.HeavyHeavy++
			case hx || hy:
				r.HeavyLight++
			default:
				r.LightLight++
			}
			if forbidden(policy, x, y) {
				r.Total++
				viol = append(viol, OverlapPair{A: x, B: y, Overlap: ov})
			}
		}
	}
	sort.Slice(viol, func(i, j int) bool { return viol[i].Overlap > viol[j].Overlap })
	if len(viol) > MaxListedViolations {
		viol = viol[:MaxListedViolations]
	}
	r.Violations = viol
	return r
}

// ArchExclusionFor builds the record stamped into the document of the arch
// whose windows are own (the peer's are peer). lockWaitSeconds is that arch's
// total client-lock wait.
func ArchExclusionFor(policy, peerArch, order string, own, peer []LoadWindow, lockWaitSeconds float64) *ArchExclusion {
	rep := CheckArchOverlap(own, peer, policy)
	return &ArchExclusion{
		Policy:             policy,
		PeerArch:           peerArch,
		CellOrder:          order,
		Checked:            rep.ChecksA,
		PeerChecked:        rep.ChecksB,
		HeavyHeavyOverlaps: rep.HeavyHeavy,
		HeavyLightOverlaps: rep.HeavyLight,
		LightLightOverlaps: rep.LightLight,
		Violations:         rep.Total,
		MissingTimestamps:  rep.MissingA + rep.MissingB,
		LockWaitSeconds:    lockWaitSeconds,
	}
}

// Problem returns why the record is not a clean verdict, or "" when it is:
// a forbidden overlap, windows that could not be checked, nothing compared at
// all (a run with no windows reads clean to a naive count), or a run whose
// policy was off.
//
// An arch whose peer drove no load (its host down, every cell a DNF before
// loadgen started) is not refused for it: a peer that recorded no pass cannot
// have loaded the client, and a peer that measured something but recorded no
// window is already counted as a window without timestamps (MissingTimestamps,
// loadWindowsFromRaw), which is refused. The record keeps PeerChecked = 0, so
// the document says what the verdict rests on.
func (e *ArchExclusion) Problem() string {
	if e == nil {
		return ""
	}
	switch {
	case e.Violations > 0:
		return fmt.Sprintf("arch exclusion (policy %s) violated: %d overlapping window pair(s) of the two arches (probatorium#442)", e.Policy, e.Violations)
	case e.MissingTimestamps > 0:
		return fmt.Sprintf("arch exclusion (policy %s) not verified: %d window(s) have no usable timestamps", e.Policy, e.MissingTimestamps)
	case e.Policy != ExclusionOff && e.Checked == 0 && e.PeerChecked == 0:
		return fmt.Sprintf("arch exclusion (policy %s) not verified: no window of either arch was compared, so no overlap could have been seen", e.Policy)
	case e.Policy == ExclusionOff:
		return "arch exclusion policy is off: the two arches ran concurrently (probatorium#442)"
	}
	return ""
}
