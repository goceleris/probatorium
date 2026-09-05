package report

import (
	"fmt"
	"sort"
	"strings"
)

// Violation is one absolute-gate failure: a single gated signal that was
// nonzero (or a structural failure such as a missing cell) in one cell.
type Violation struct {
	Refapp string
	Engine string
	Arch   string
	Field  string // dotted path, e.g. "tier_1.h2c_churn.h2c_hang"
	Value  int64
	Why    string
}

func (v Violation) String() string {
	return fmt.Sprintf("%s/%s/%s %s=%d (%s)", v.Refapp, v.Engine, v.Arch, v.Field, v.Value, v.Why)
}

// GateOptions tunes the absolute gate.
type GateOptions struct {
	// ExpectedCells > 0 fails the gate when fewer cells are present: a cell
	// that crashed, was skipped, or never reported is a failure, not a pass.
	ExpectedCells int
	// RequireTier3 fails a cell whose tier 3 (seed replay) never ran.
	RequireTier3 bool
	// RequireSoak fails any cell that carries no soak summary: the soak
	// workflow sets it so a dropped block fails the run instead of
	// silently passing the soak rules (probatorium#281).
	RequireSoak bool
	// RequireProperties fails any cell whose property loop never
	// evaluated a predicate (tier_1.property_evaluations == 0): the
	// refapp's /debug/vars was unreachable for the whole cell, so every
	// I-* predicate was vacuous. This is the silent-zero state every run
	// before the in-process loop was in (properties_passed=0 on an 18 GB
	// heap, celeris#494). A cell whose tier_1.property_loop_skipped
	// names a reason (ssh driver) is waived: the loop did not fail, it
	// was not run, and the document says so. Documents older than
	// schema 5.6 have no property loop at all; mage ValidateGate
	// defaults this off for them.
	RequireProperties bool
}

// gatedTier1Keys are the sub-tally counters that are defects by definition.
// Informational counters are deliberately NOT here: *_sent, *_upgraded,
// h2c_declined, h2c_intentional_rst, h2c_hang_max_elapsed_ms (a duration),
// adv_well_rejected, ws_closed_correctly, sse_established, sse_events_read,
// sse_killed_mid_stream (the validator kills on purpose) and *_endpoint_absent
// (the refapp has no such endpoint).
//
// The scalar Tier1Summary counters gated inline in Gate follow the same
// rule: requests_5xx, requests_error, invariant_hits and
// property_violations are defects; requests_5xx_expected,
// requests_cut_at_deadline, property_evaluations, property_skips and
// property_poll_errors are informational (property_evaluations == 0 is
// gated only under RequireProperties, and waived when
// property_loop_skipped names a reason).
var gatedTier1Keys = []struct{ slice, key, why string }{
	{"adversarial", "adv_wrong_accepted", "a malformed request was accepted"},
	{"adversarial", "adv_hang_until_timeout", "a malformed request hung the server"},
	{"h2c_churn", "h2c_hang", "an h2c request was neither answered nor declined"},
	{"h2c_churn", "h2c_hang_eof", "h2c hang, cause: peer EOF"},
	{"h2c_churn", "h2c_hang_timeout", "h2c hang, cause: timeout"},
	{"h2c_churn", "h2c_hang_reset", "h2c hang, cause: reset"},
	{"h2c_churn", "h2c_hang_other", "h2c hang, cause: other"},
	{"h2c_churn", "h2c_crashed", "the server crashed under h2c churn"},
	{"ws_torture", "ws_accepted_bad_frame", "an invalid WebSocket frame was accepted"},
	{"ws_torture", "ws_hang_no_close", "a WebSocket conn never completed its close"},
	{"ws_torture", "ws_handshake_fail", "a WebSocket upgrade handshake failed"},
	{"sse_kill", "sse_handshake_fail", "an SSE handshake failed"},
	{"sse_kill", "sse_server_closed_early", "the server closed an SSE stream before the client did"},
}

// Gate applies the ABSOLUTE zero-signal gate to every cell and, when present,
// to each host's soak summary.
//
// It is the complement of DiffCells / DiffValidation: those are RELATIVE
// (cross-engine, cross-arch) and by construction cannot see a defect that is
// present on every engine -- agreement looks like health. This gate fails on
// ANY nonzero true-signal counter in ANY cell, on any dead cell (no requests
// sent), on any missing cell, on a tier that never ran, and on soak leak
// indicators. Designed 5xx (requests_5xx_expected) and tier-deadline cutoffs
// (requests_cut_at_deadline) are informational and are not gated.
func Gate(cells []ValidationCellResult, soaks map[string]*SoakSummary, opts GateOptions) []Violation {
	var out []Violation
	add := func(c ValidationCellResult, field string, v int64, why string) {
		out = append(out, Violation{Refapp: c.Refapp, Engine: c.Engine, Arch: c.Arch, Field: field, Value: v, Why: why})
	}
	if opts.ExpectedCells > 0 && len(cells) < opts.ExpectedCells {
		out = append(out, Violation{Refapp: "*", Engine: "*", Arch: "*", Field: "cells", Value: int64(len(cells)),
			Why: fmt.Sprintf("expected %d cells, got %d: a cell crashed, was skipped, or never reported", opts.ExpectedCells, len(cells))})
	}
	for _, c := range cells {
		if t := c.Tier1; t == nil {
			add(c, "tier_1", 0, "tier 1 never ran")
		} else {
			if t.RequestsSent == 0 {
				add(c, "tier_1.requests_sent", 0, "dead cell: no requests were sent")
			}
			if t.Requests5xx > 0 {
				add(c, "tier_1.requests_5xx", t.Requests5xx, "unexpected 5xx (designed 5xx are tallied in requests_5xx_expected)")
			}
			if t.RequestsError > 0 {
				add(c, "tier_1.requests_error", t.RequestsError, "transport/timeout errors (deadline cutoffs are tallied in requests_cut_at_deadline)")
			}
			if t.InvariantHits > 0 {
				add(c, "tier_1.invariant_hits", t.InvariantHits, "the refapp reported an invariant violation")
			}
			if t.PropertyViolations > 0 {
				add(c, "tier_1.property_violations", t.PropertyViolations,
					"property predicate(s) violated: "+strings.Join(t.PropertyViolationIDs, ", "))
			}
			if opts.RequireProperties && t.PropertyEvaluations == 0 && t.PropertyLoopSkipped == "" {
				add(c, "tier_1.property_evaluations", 0, "the property loop never evaluated a predicate (refapp /debug/vars unreachable?)")
			}
			for _, g := range gatedTier1Keys {
				var m map[string]int64
				switch g.slice {
				case "adversarial":
					m = t.Adversarial
				case "h2c_churn":
					m = t.H2CChurn
				case "ws_torture":
					m = t.WSTorture
				case "sse_kill":
					m = t.SSEKill
				}
				if v := m[g.key]; v > 0 {
					add(c, "tier_1."+g.slice+"."+g.key, v, g.why)
				}
			}
		}
		if t3 := c.Tier3; t3 == nil || t3.SeedsAttempted == 0 {
			if opts.RequireTier3 {
				add(c, "tier_3.seeds_attempted", 0, "tier 3 (seed replay) never ran")
			}
		} else {
			if t3.SeedsFailed > 0 {
				add(c, "tier_3.seeds_failed", t3.SeedsFailed, "replayed seed(s) failed")
			}
			if t3.SeedsErrored > 0 {
				add(c, "tier_3.seeds_errored", t3.SeedsErrored, "replayed seed(s) errored")
			}
		}
	}
	for _, c := range cells {
		if c.Soak == nil {
			if opts.RequireSoak {
				add(c, "soak_summary.missing", 0, "soak mode, but the cell carries no soak summary")
			}
			continue
		}
		if c.Soak.GoroutineLeakDetected {
			add(c, "soak_summary.goroutine_leak_detected", 1, "a goroutine leak was detected over the soak")
		}
		if c.Soak.RestartedProcesses > 0 {
			add(c, "soak_summary.restarted_processes", int64(c.Soak.RestartedProcesses), "a server process died and was restarted during the soak")
		}
	}
	hosts := make([]string, 0, len(soaks))
	for h := range soaks {
		hosts = append(hosts, h)
	}
	sort.Strings(hosts)
	for _, h := range hosts {
		s := soaks[h]
		if s == nil {
			continue
		}
		sc := ValidationCellResult{Refapp: "soak", Engine: "*", Arch: h}
		if s.GoroutineLeakDetected {
			add(sc, "soak_summary.goroutine_leak_detected", 1, "a goroutine leak was detected over the soak")
		}
		if s.RestartedProcesses > 0 {
			add(sc, "soak_summary.restarted_processes", int64(s.RestartedProcesses), "a server process died and was restarted during the soak")
		}
	}
	sort.Slice(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Refapp != b.Refapp {
			return a.Refapp < b.Refapp
		}
		if a.Engine != b.Engine {
			return a.Engine < b.Engine
		}
		if a.Arch != b.Arch {
			return a.Arch < b.Arch
		}
		return a.Field < b.Field
	})
	return out
}
