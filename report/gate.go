package report

import (
	"fmt"
	"slices"
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
	// RequireInstrumented fails the RUN (not a cell) for every predicate
	// that every property-running cell listed in
	// properties_not_instrumented and that is not on the
	// [WaivedUninstrumented] record.
	//
	// Such a predicate judged nothing anywhere: the matrix says so in the
	// document, the gate passes, and the hole stays invisible. Not a
	// hypothetical -- probatorium#297 found 9 of 14 requested predicates in
	// exactly that state across all 48 cells of both the passing nightly
	// AND the failing v1.5.11 soak, I-MW-SESSION (the oracle for the
	// session bug that failed two consecutive soaks) among them. Off by
	// default so pre-5.6 documents, which carry no property fields at all,
	// behave exactly as before.
	RequireInstrumented bool

	// RequireCoverage fails the run for every predicate the property
	// loop ran but never reached a verdict on in ANY cell, unless every
	// cell that skipped it was structurally too short to judge it (see
	// [Coverage]). Documents older than schema 5.8 carry no by-design
	// list, so their short-cell oracles would all read as gaps; mage
	// ValidateGate defaults this off for them.
	RequireCoverage bool

	// ExpectInstrumented names predicates that every property-running cell
	// of THIS run must have declared, waiver or not. The waiver record says
	// "this hole may exist somewhere"; a tier built to close that hole (the
	// soak's 1 h cells for I-MEM-2, the checkptr tier for I-CHECKPTR) must
	// not be able to fall back on it: a regression in the declaration path
	// would otherwise turn a covered predicate back into a waived one with
	// the gate still green. Per cell, so the report names where it went
	// missing.
	ExpectInstrumented []string

	// H2CUpgradeRefapps names the refapps whose cells must record at least
	// one completed h1->h2c upgrade (tier_1.h2c_churn.h2c_upgraded > 0)
	// once the churn slice has sent anything at all. Nil selects
	// DefaultH2CUpgradeRefapps; a non-nil empty slice disables the check --
	// the escape hatch for re-gating a run recorded before any refapp
	// served the upgrade.
	H2CUpgradeRefapps []string
}

// WaivedUninstrumented lists the predicates allowed to be uninstrumented in
// every cell, with the reason. This list IS the point of
// [GateOptions.RequireInstrumented]: a coverage hole may exist, but it has to
// be DECLARED here, in review, instead of being discovered a soak later.
// Registering a predicate with no data source now fails the gate until
// someone either wires it up or writes down why they did not.
//
// The reasons mirror validation/checker.Uninstrumented; report/ is a leaf
// package and does not import it.
var WaivedUninstrumented = map[string]string{
	"I-RACE":        "needs a -race build of the refapps plus a stderr marker counter; out of scope for probatorium#297",
	"I-CHECKPTR":    "instrumented only in cells whose refapp is a -tags=checkptr build; a run that deploys none has no cell that can judge it",
	"I-RFC-1":       "needs the response-scraping MITM in front of each refapp",
	"I-RFC-2":       "needs the response-scraping MITM in front of each refapp",
	"I-MEM-2":       "instrumented only in cells long enough to idle the refapp twice (20 min or more; the soak's 1 h cells); a 150 s nightly cell never idles",
	"I-ENG-IOURING": "instrumented only in the io_uring cells of the instrumented tier (refapps built -tags=checkptr,validation); a normal run deploys no such refapp",
}

// ranPropertyLoop reports whether the cell's in-process property loop
// actually observed samples. Only such a cell can say anything about which
// predicates were instrumented; an ssh-driven cell (loop deliberately not
// run) or a pre-5.6 document reports nothing and must not vote.
func ranPropertyLoop(c ValidationCellResult) bool {
	return c.Tier1 != nil && c.Tier1.PropertyLoopSkipped == "" &&
		(c.Tier1.PropertyEvaluations > 0 || c.Tier1.PropertySkips > 0)
}

// everyCellLists returns the predicate IDs that appear in list(c) for EVERY
// cell that ran a property loop, sorted. Empty when no cell ran one.
func everyCellLists(cells []ValidationCellResult, list func(ValidationCellResult) []string) []string {
	seen := map[string]int{}
	total := 0
	for _, c := range cells {
		if !ranPropertyLoop(c) {
			continue
		}
		total++
		for _, id := range list(c) {
			seen[id]++
		}
	}
	if total == 0 {
		return nil
	}
	out := make([]string, 0, len(seen))
	for id, n := range seen {
		if n == total {
			out = append(out, id)
		}
	}
	sort.Strings(out)
	return out
}

// UninstrumentedEverywhere returns the predicates that every property-running
// cell reported as not-instrumented: they judged a structurally-zero input in
// the whole run and verified nothing. Waivers are NOT applied here -- the
// caller decides whether to gate or merely print.
func UninstrumentedEverywhere(cells []ValidationCellResult) []string {
	return everyCellLists(cells, func(c ValidationCellResult) []string { return c.PropertiesNotInstrumented })
}

// NotJudgedEverywhere returns the predicates that were instrumented but whose
// every evaluation was a skip in every cell -- typically the slope oracles in
// a run too short to fill their window. Reported, never gated: a 150 s
// nightly legitimately never reaches them, and failing on it would fail every
// nightly rather than teach anyone anything.
func NotJudgedEverywhere(cells []ValidationCellResult) []string {
	return everyCellLists(cells, func(c ValidationCellResult) []string { return c.PropertiesNotJudged })
}

// CoverageGap is one predicate the property loop RAN and never reached a
// verdict on in ANY cell of the matrix: every evaluation, everywhere,
// was a skip.
//
// It is a distinct finding from a [Violation]. Nothing was violated --
// the run simply holds no opinion on what that predicate asserts, and
// on an ABSOLUTE zero-signal gate silence is not zero signal. The
// v1.5.11 nightlies passed for months with I-MEM-1, I-MEM-3 and
// I-MEM-4 in this state in all 48 cells while the gate header printed
// require_properties=true (probatorium#299).
type CoverageGap struct {
	// ID is the predicate, e.g. "I-MEM-1".
	ID string
	// Cells is the number of cells whose property loop ran and reported
	// the predicate as not judged.
	Cells int
	// ByDesign is the subset of Cells that could not have judged it:
	// the cell was shorter than the predicate's declared minimum
	// observation (properties.Spec.MinObservation). A 150 s nightly
	// cell can never fit I-MEM-1's 5 min warm-up plus its 10 min span.
	ByDesign int
}

// Structural reports whether EVERY cell that failed to judge the
// predicate was incapable of judging it. Such a gap describes the tier
// (its cells are too short for this oracle), not the run: it is
// reported so a nightly PASS cannot be misread as "the leak oracles
// passed", but it does not fail the gate -- a 150 s cell legitimately
// cannot judge a slope.
//
// A gap that is NOT structural means at least one cell had the time and
// still reached no verdict: the oracle was silent when it should have
// spoken, which is a coverage failure.
func (g CoverageGap) Structural() bool { return g.Cells > 0 && g.ByDesign == g.Cells }

// Coverage returns one [CoverageGap] per predicate that no cell in the
// run ever judged, sorted by ID.
//
// Only cells whose property loop actually ran are evidence. A cell that
// skipped the loop (ssh driver) reports empty lists, and reading those
// as "this cell judged everything" would erase the gaps the other cells
// reported. A cell that lists a predicate as not INSTRUMENTED is
// likewise neither evidence of coverage nor a cell that failed to judge
// it: vacuous instrumentation is its own finding (probatorium#297).
func Coverage(cells []ValidationCellResult) []CoverageGap {
	notJudged := map[string]int{}
	byDesign := map[string]int{}
	var ran []ValidationCellResult
	for _, c := range cells {
		if !propertyLoopRan(c) {
			continue
		}
		ran = append(ran, c)
		skipped := make(map[string]bool, len(c.PropertiesNotJudged))
		for _, id := range c.PropertiesNotJudged {
			skipped[id] = true
			notJudged[id]++
		}
		for _, id := range c.PropertiesNotJudgedByDesign {
			if skipped[id] {
				byDesign[id]++
			}
		}
	}
	// Second pass, over the candidates the first one collected: every
	// predicate this cell evaluated and did not report as skipped
	// reached a verdict here, so the run has an opinion on it. The
	// candidate set has to be complete first -- a cell that judged a
	// predicate is evidence no matter whether it was walked before or
	// after the cells that did not.
	judged := make(map[string]bool, len(notJudged))
	for _, c := range ran {
		silent := make(map[string]bool, len(c.PropertiesNotJudged)+len(c.PropertiesNotInstrumented))
		for _, id := range c.PropertiesNotJudged {
			silent[id] = true
		}
		for _, id := range c.PropertiesNotInstrumented {
			silent[id] = true
		}
		for id := range notJudged {
			if !silent[id] {
				judged[id] = true
			}
		}
	}
	out := make([]CoverageGap, 0, len(notJudged))
	for id, n := range notJudged {
		if judged[id] {
			continue
		}
		out = append(out, CoverageGap{ID: id, Cells: n, ByDesign: byDesign[id]})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// propertyLoopRan reports whether the cell's in-process property loop
// observed anything. A loop that was deliberately not run (ssh driver)
// or never got a sample has nothing to say about coverage either way.
func propertyLoopRan(c ValidationCellResult) bool {
	t := c.Tier1
	return t != nil && t.PropertyLoopSkipped == "" && t.PropertyEvaluations+t.PropertySkips > 0
}

// DefaultH2CUpgradeRefapps names the refapps configured to answer the
// HTTP/1.1 -> h2c upgrade, and is what a nil GateOptions.H2CUpgradeRefapps
// falls back to.
//
// It mirrors validation/refapp/<slug>/main.go: a refapp belongs here iff it
// passes celeris.Auto as its Protocol, because celeris infers
// EnableH2Upgrade from Protocol and the std engine reads Protocol alone.
// Each refapp is a separate Go module, so the root module cannot link them
// and read this off the config; TestRefappH2CUpgradeSetMatchesGateDefault in
// package validation parses their sources and fails if the two ever drift.
var DefaultH2CUpgradeRefapps = []string{"kitchen_sink"}

// gatedTier1Keys are the sub-tally counters that are defects by definition.
// Informational counters are deliberately NOT here: *_sent, *_upgraded,
// h2c_declined, h2c_intentional_rst, h2c_hang_max_elapsed_ms (a duration),
// adv_well_rejected, ws_closed_correctly, sse_established, sse_events_read,
// sse_killed_mid_stream (the validator kills on purpose) and *_endpoint_absent
// (the refapp has no such endpoint). h2c_upgraded is informational as a
// MAGNITUDE only -- its being zero is gated separately in Gate, because a
// refapp that should upgrade and never did means the whole slice measured
// nothing (probatorium#279).
//
// The CAUSE splits are also excluded, and deliberately so: h2c_hang_{eof,
// timeout,reset,other} sum to h2c_hang, and ws_handshake_fail_{eof,timeout,
// reset,status,other} sum to ws_handshake_fail. Gating both a total and its
// parts reports one defect as two violations — the v1.5.11 soak printed "5
// violations" for what were only THREE distinct events, because a single h2c
// hang was counted once as h2c_hang and again as h2c_hang_timeout. The cause
// counters are diagnostic detail attached to a gated total, not independent
// signals; gating the total alone keeps the violation count equal to the
// event count.
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
	{"h2c_churn", "h2c_crashed", "the server crashed under h2c churn"},
	{"ws_torture", "ws_accepted_bad_frame", "an invalid WebSocket frame was accepted"},
	{"ws_torture", "ws_hang_no_close", "a WebSocket conn never completed its close"},
	{"ws_torture", "ws_handshake_fail", "a WebSocket upgrade handshake failed"},
	{"sse_kill", "sse_handshake_fail", "an SSE handshake failed"},
	{"sse_kill", "sse_server_closed_early", "the server sent FIN on an SSE stream before the client did"},
	// Split out of sse_server_closed_early, which classified purely on
	// timing and so asserted "the server closed it" for any read error.
	// Both of these still fail: the split is for attribution, not tolerance.
	{"sse_kill", "sse_peer_reset_early", "an SSE stream was reset before the client hung up (engine or transport -- see sse_early_errs)"},
	{"sse_kill", "sse_read_err_early", "an SSE stream failed with an unclassified read error (see sse_early_errs)"},
}

// causeCounters maps a gated total to the cause counters that sum to it.
// The causes are reported as detail on the total's violation instead of as
// violations of their own — see the gatedTier1Keys comment.
var causeCounters = map[string][]string{
	"h2c_hang":          {"h2c_hang_eof", "h2c_hang_timeout", "h2c_hang_reset", "h2c_hang_other"},
	"ws_handshake_fail": {"ws_handshake_fail_eof", "ws_handshake_fail_timeout", "ws_handshake_fail_reset", "ws_handshake_fail_status", "ws_handshake_fail_other"},
}

// causeSuffix renders the non-zero cause breakdown for a gated total, e.g.
// " (timeout=1)". Empty when the tally carries no cause detail, so a run
// produced before the split was added reads exactly as it did before.
func causeSuffix(m map[string]int64, key string) string {
	causes, ok := causeCounters[key]
	if !ok {
		return ""
	}
	var parts []string
	for _, c := range causes {
		if v := m[c]; v > 0 {
			parts = append(parts, fmt.Sprintf("%s=%d", strings.TrimPrefix(c, key+"_"), v))
		}
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
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
//
// Under RequireCoverage it also fails on SILENCE: a predicate the property
// loop ran but never reached a verdict on in any cell (see [Coverage]). A
// counter that stayed zero and an oracle that never spoke look identical in
// a tally, and only one of them is evidence.
//
// It also fails a cell whose oracles were VACUOUS -- one that reported zero
// because it measured nothing, not because the server was clean. The dead
// cell (requests_sent == 0), the property loop that never evaluated
// (RequireProperties) and the h2c slice that never completed an upgrade
// (H2CUpgradeRefapps) are all that same class.
func Gate(cells []ValidationCellResult, soaks map[string]*SoakSummary, opts GateOptions) []Violation {
	var out []Violation
	add := func(c ValidationCellResult, field string, v int64, why string) {
		out = append(out, Violation{Refapp: c.Refapp, Engine: c.Engine, Arch: c.Arch, Field: field, Value: v, Why: why})
	}
	// Nil means "the built-in list"; a non-nil empty slice means "don't
	// check", so an archived run can still be re-gated on its own terms.
	upgradeRefapps := opts.H2CUpgradeRefapps
	if upgradeRefapps == nil {
		upgradeRefapps = DefaultH2CUpgradeRefapps
	}
	mustUpgrade := make(map[string]bool, len(upgradeRefapps))
	for _, r := range upgradeRefapps {
		mustUpgrade[r] = true
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
			// A walker re-logs in only when a request comes back 401. On a
			// healthy run that happens after a deliberate logout, so
			// relogins are bounded by logouts plus the one pre-walk login
			// per walker. Unbounded relogins mean the server is not
			// honouring the session at all -- which is exactly what
			// celeris#507 did for months at ~96% 4xx, invisible to this
			// gate because requests_4xx lumps 401/404/429 together
			// (probatorium#292).
			//
			// Only judged when the tally carries the counters, so runs
			// produced before they existed read exactly as they did.
			if t.WalkerRelogins > 0 && t.WalkerLogins > 0 {
				// A deliberate logout costs exactly one 401 and one
				// re-login, and the matrices walk through their logout
				// state on purpose — auth_session_ratelimit reaches it
				// from three other states, so a healthy cell logs out
				// thousands of times. Judging relogins against the walker
				// count alone therefore failed every run once this check
				// landed, while sessions were provably working. What is
				// still a true signal is re-logins the walk did NOT ask
				// for: on top of every logout, allow ten per walker for
				// ordinary expiry over a long cell.
				budget := t.WalkerLogouts + t.WalkerLogins*10
				if t.WalkerRelogins > budget {
					add(c, "tier_1.walker_relogins", t.WalkerRelogins,
						fmt.Sprintf("walkers re-logged in %d times against %d deliberate logout(s) "+
							"and %d login(s) (budget %d): the server is not honouring the session",
							t.WalkerRelogins, t.WalkerLogouts, t.WalkerLogins, budget))
				}
			}
			if t.PropertyViolations > 0 {
				add(c, "tier_1.property_violations", t.PropertyViolations,
					"property predicate(s) violated: "+strings.Join(t.PropertyViolationIDs, ", "))
			}
			if opts.RequireProperties && t.PropertyEvaluations == 0 && t.PropertyLoopSkipped == "" {
				add(c, "tier_1.property_evaluations", 0, "the property loop never evaluated a predicate (refapp /debug/vars unreachable?)")
			}
			// Vacuous h2c slice. The three churn modes all peel away from a
			// COMPLETED upgrade; against a server that declines, every one
			// of them is a plain GET, and h2c_hang / h2c_crashed judge a
			// path the engine never entered. Only cells that actually sent
			// preambles are judged: below concurrency 10 the slice is not
			// scheduled at all, which says nothing about the server.
			if sent := t.H2CChurn["h2c_sent"]; mustUpgrade[c.Refapp] && sent > 0 && t.H2CChurn["h2c_upgraded"] == 0 {
				add(c, "tier_1.h2c_churn.h2c_upgraded", 0, fmt.Sprintf(
					"vacuous h2c slice: %d upgrade preambles, not one 101 -- h2c_hang/h2c_crashed judged nothing", sent))
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
					add(c, "tier_1."+g.slice+"."+g.key, v, g.why+causeSuffix(m, g.key))
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
	for _, id := range opts.ExpectInstrumented {
		for _, c := range cells {
			if !ranPropertyLoop(c) || !slices.Contains(c.PropertiesNotInstrumented, id) {
				continue
			}
			add(c, "properties_not_instrumented."+id, 1,
				"this tier expects the predicate instrumented in every cell (VALIDATE_GATE_EXPECT_INSTRUMENTED) and the cell never declared it; the waiver on record does not apply here")
		}
	}
	if opts.RequireInstrumented {
		for _, id := range UninstrumentedEverywhere(cells) {
			if _, waived := WaivedUninstrumented[id]; waived {
				continue
			}
			out = append(out, Violation{Refapp: "*", Engine: "*", Arch: "*",
				Field: "properties_not_instrumented." + id, Value: 0,
				Why: "the predicate was requested by tier but not instrumented in every cell that ran a property loop: it verified nothing anywhere. Wire it up, or add it to report.WaivedUninstrumented with a reason"})
		}
	}
	if opts.RequireCoverage {
		for _, g := range Coverage(cells) {
			if g.Structural() {
				// The tier cannot judge this oracle in cells this
				// short. mage ValidateGate prints it either way; failing
				// on it would fail the nightly for its own definition.
				continue
			}
			out = append(out, Violation{Refapp: "*", Engine: "*", Arch: "*",
				Field: "properties.coverage." + g.ID, Value: int64(g.Cells),
				Why: fmt.Sprintf("%s reached no verdict in ANY cell (%d cell(s) never judged it, %d of them had the time): the predicate was requested by the tier and stayed silent, which on an absolute gate is not the same as zero signal",
					g.ID, g.Cells, g.Cells-g.ByDesign)})
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
