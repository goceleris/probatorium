package report

import (
	"strings"
	"testing"
)

func cleanCell(r, e, a string) ValidationCellResult {
	return ValidationCellResult{Refapp: r, Engine: e, Arch: a,
		Tier1: &Tier1Summary{RequestsSent: 1000, Requests2xx: 990, Requests4xx: 10,
			Requests5xxExpected: 5, RequestsCutAtDeadline: 3,
			PropertyEvaluations: 3600 * 14, PropertyPollErrors: 2,
			Adversarial: map[string]int64{"adv_sent": 10, "adv_well_rejected": 10},
			H2CChurn:    map[string]int64{"h2c_sent": 10, "h2c_upgraded": 3, "h2c_declined": 7, "h2c_intentional_rst": 3, "h2c_hang_max_elapsed_ms": 40},
			WSTorture:   map[string]int64{"ws_sent": 10, "ws_upgraded": 10, "ws_closed_correctly": 10},
			SSEKill:     map[string]int64{"sse_sent": 10, "sse_established": 10, "sse_events_read": 50, "sse_killed_mid_stream": 10},
		},
		Tier3: &Tier3Summary{SeedsAttempted: 11, SeedsPassed: 11},
	}
}

func TestGate_CleanCellsPass(t *testing.T) {
	cells := []ValidationCellResult{cleanCell("a", "std", "amd64"), cleanCell("a", "epoll", "arm64")}
	if v := Gate(cells, nil, GateOptions{ExpectedCells: 2, RequireTier3: true}); len(v) != 0 {
		t.Fatalf("clean cells must pass, got %v", v)
	}
}

// Every gated signal, alone, must produce exactly one violation naming it.
func TestGate_EachSignalIsAViolation(t *testing.T) {
	cases := []struct {
		name, field string
		mutate      func(c *ValidationCellResult)
	}{
		{"5xx", "tier_1.requests_5xx", func(c *ValidationCellResult) { c.Tier1.Requests5xx = 1 }},
		{"error", "tier_1.requests_error", func(c *ValidationCellResult) { c.Tier1.RequestsError = 1 }},
		{"invariant", "tier_1.invariant_hits", func(c *ValidationCellResult) { c.Tier1.InvariantHits = 1 }},
		{"property_violations", "tier_1.property_violations", func(c *ValidationCellResult) {
			c.Tier1.PropertyViolations = 7
			c.Tier1.PropertyViolationIDs = []string{"I-MEM-3"}
		}},
		{"dead cell", "tier_1.requests_sent", func(c *ValidationCellResult) { c.Tier1.RequestsSent = 0 }},
		{"tier1 missing", "tier_1", func(c *ValidationCellResult) { c.Tier1 = nil }},
		{"h2c_hang", "tier_1.h2c_churn.h2c_hang", func(c *ValidationCellResult) { c.Tier1.H2CChurn["h2c_hang"] = 1 }},
		{"h2c_crashed", "tier_1.h2c_churn.h2c_crashed", func(c *ValidationCellResult) { c.Tier1.H2CChurn["h2c_crashed"] = 1 }},
		{"adv_wrong_accepted", "tier_1.adversarial.adv_wrong_accepted", func(c *ValidationCellResult) { c.Tier1.Adversarial["adv_wrong_accepted"] = 1 }},
		{"adv_hang", "tier_1.adversarial.adv_hang_until_timeout", func(c *ValidationCellResult) { c.Tier1.Adversarial["adv_hang_until_timeout"] = 1 }},
		{"ws_bad_frame", "tier_1.ws_torture.ws_accepted_bad_frame", func(c *ValidationCellResult) { c.Tier1.WSTorture["ws_accepted_bad_frame"] = 1 }},
		{"ws_hang", "tier_1.ws_torture.ws_hang_no_close", func(c *ValidationCellResult) { c.Tier1.WSTorture["ws_hang_no_close"] = 1 }},
		{"ws_hs_fail", "tier_1.ws_torture.ws_handshake_fail", func(c *ValidationCellResult) { c.Tier1.WSTorture["ws_handshake_fail"] = 1 }},
		{"sse_hs_fail", "tier_1.sse_kill.sse_handshake_fail", func(c *ValidationCellResult) { c.Tier1.SSEKill["sse_handshake_fail"] = 1 }},
		{"sse_closed_early", "tier_1.sse_kill.sse_server_closed_early", func(c *ValidationCellResult) { c.Tier1.SSEKill["sse_server_closed_early"] = 1 }},
		{"seeds_failed", "tier_3.seeds_failed", func(c *ValidationCellResult) { c.Tier3.SeedsFailed = 1 }},
		{"seeds_errored", "tier_3.seeds_errored", func(c *ValidationCellResult) { c.Tier3.SeedsErrored = 1 }},
		{"tier3 not run", "tier_3.seeds_attempted", func(c *ValidationCellResult) { c.Tier3.SeedsAttempted = 0 }},
		{"tier3 missing", "tier_3.seeds_attempted", func(c *ValidationCellResult) { c.Tier3 = nil }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			c := cleanCell("r", "iouring", "amd64")
			tc.mutate(&c)
			v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true})
			if len(v) != 1 || v[0].Field != tc.field {
				t.Fatalf("want exactly one violation on %q, got %v", tc.field, v)
			}
			if v[0].Refapp != "r" || v[0].Engine != "iouring" || v[0].Arch != "amd64" {
				t.Fatalf("violation must name the cell, got %+v", v[0])
			}
		})
	}
}

// Counters that describe designed behaviour must never trip the gate.
func TestGate_InformationalCountersAreNotGated(t *testing.T) {
	c := cleanCell("obs", "std", "arm64")
	c.Tier1.Requests5xxExpected = 1_000_000
	c.Tier1.RequestsPanicExpected = 1_000_000
	c.Tier1.RequestsCutAtDeadline = 500
	c.Tier1.Requests4xx = 3_000_000
	c.Tier1.H2CChurn["h2c_declined"] = 1_000_000
	c.Tier1.H2CChurn["h2c_intentional_rst"] = 1_000_000
	c.Tier1.H2CChurn["h2c_hang_max_elapsed_ms"] = 9_999
	c.Tier1.SSEKill["sse_killed_mid_stream"] = 1_000_000
	c.Tier1.SSEKill["sse_endpoint_absent"] = 1_000_000
	c.Tier1.WSTorture["ws_endpoint_absent"] = 1_000_000
	c.Tier1.Adversarial["adv_well_rejected"] = 1_000_000
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true}); len(v) != 0 {
		t.Fatalf("informational counters must not be gated, got %v", v)
	}
}

// A property violation names the predicate IDs so the gate output says
// WHICH invariant failed, and a cell whose loop evaluated nothing is a
// failure when the run requires properties (the silent-zero state of
// every run before the in-process loop).
func TestGate_PropertyLoop(t *testing.T) {
	c := cleanCell("auth_session_ratelimit", "iouring", "arm64")
	c.Tier1.PropertyViolations = 1200
	c.Tier1.PropertyViolationIDs = []string{"I-MEM-1", "I-MEM-3"}
	v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireProperties: true})
	if len(v) != 1 || v[0].Field != "tier_1.property_violations" || v[0].Value != 1200 {
		t.Fatalf("want one property_violations violation, got %v", v)
	}
	if !strings.Contains(v[0].Why, "I-MEM-1") || !strings.Contains(v[0].Why, "I-MEM-3") {
		t.Fatalf("Why must name the predicate IDs, got %q", v[0].Why)
	}

	c = cleanCell("r", "std", "amd64")
	c.Tier1.PropertyEvaluations = 0
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true}); len(v) != 0 {
		t.Fatalf("zero evaluations must pass when properties are not required, got %v", v)
	}
	v = Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireProperties: true})
	if len(v) != 1 || v[0].Field != "tier_1.property_evaluations" {
		t.Fatalf("zero evaluations must fail under RequireProperties, got %v", v)
	}

	// Poll errors and the evaluation count itself are informational.
	c = cleanCell("r", "std", "amd64")
	// A cell whose loop was deliberately not run (ssh driver) says so and
	// is waived from the zero-evaluations check.
	c.Tier1.PropertyEvaluations = 0
	c.Tier1.PropertyLoopSkipped = "ssh driver: remote /debug/vars is loopback-only"
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireProperties: true}); len(v) != 0 {
		t.Fatalf("a skipped loop with a reason on record must not fail RequireProperties, got %v", v)
	}
	c.Tier1.PropertyLoopSkipped = ""
	c.Tier1.PropertyPollErrors = 1_000_000
	c.Tier1.PropertySkips = 1_000_000
	c.Tier1.PropertyEvaluations = 1
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireProperties: true}); len(v) != 0 {
		t.Fatalf("poll errors must not be gated, got %v", v)
	}
}

func TestGate_MissingCellsFail(t *testing.T) {
	v := Gate([]ValidationCellResult{cleanCell("a", "std", "amd64")}, nil, GateOptions{ExpectedCells: 2, RequireTier3: true})
	if len(v) != 1 || v[0].Field != "cells" || v[0].Value != 1 {
		t.Fatalf("one missing cell must be one violation on cells, got %v", v)
	}
}

func TestGate_Tier3NotRequiredForSmoke(t *testing.T) {
	c := cleanCell("a", "std", "amd64")
	c.Tier3 = nil
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{}); len(v) != 0 {
		t.Fatalf("tier 3 absence must be allowed when not required, got %v", v)
	}
}

func TestGate_SoakLeakIndicatorsFail(t *testing.T) {
	soaks := map[string]*SoakSummary{"msa2-server": {GoroutineLeakDetected: true, RestartedProcesses: 2}, "msr1": {}}
	v := Gate([]ValidationCellResult{cleanCell("a", "std", "amd64")}, soaks, GateOptions{})
	if len(v) != 2 || v[0].Field != "soak_summary.goroutine_leak_detected" || v[1].Field != "soak_summary.restarted_processes" || v[1].Value != 2 {
		t.Fatalf("soak leak + restarts must each be a violation, got %v", v)
	}
	if v[0].Arch != "msa2-server" {
		t.Fatalf("soak violation must name the host, got %+v", v[0])
	}
}

func TestGate_OutputIsSortedAndComplete(t *testing.T) {
	b := cleanCell("b", "std", "amd64")
	b.Tier1.Requests5xx = 3
	a := cleanCell("a", "std", "amd64")
	a.Tier1.RequestsError = 2
	a.Tier1.H2CChurn["h2c_hang"] = 1
	v := Gate([]ValidationCellResult{b, a}, nil, GateOptions{})
	if len(v) != 3 || v[0].Refapp != "a" || v[0].Field != "tier_1.h2c_churn.h2c_hang" || v[1].Field != "tier_1.requests_error" || v[2].Refapp != "b" {
		t.Fatalf("violations must be sorted by cell then field and all reported, got %v", v)
	}
}

func TestGate_SoakSummaryPerCell(t *testing.T) {
	// Not required: a cell without a soak block passes.
	c := cleanCell("r", "iouring", "arm64")
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true}); len(v) != 0 {
		t.Fatalf("unrequired soak block produced violations: %+v", v)
	}
	// Required: the missing block is a violation naming the cell (probatorium#281).
	v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireSoak: true})
	if len(v) != 1 || v[0].Field != "soak_summary.missing" || v[0].Refapp != "r" || v[0].Engine != "iouring" {
		t.Fatalf("missing soak block not gated: %+v", v)
	}
	// Present and clean: passes even when required.
	c.Soak = &SoakSummary{Duration: 3600e9}
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireSoak: true}); len(v) != 0 {
		t.Fatalf("clean soak block produced violations: %+v", v)
	}
	// Leak / restart indicators inside the cell's block are gated.
	c.Soak = &SoakSummary{GoroutineLeakDetected: true, RestartedProcesses: 2}
	v = Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true, RequireSoak: true})
	fields := map[string]int64{}
	for _, x := range v {
		fields[x.Field] = x.Value
	}
	if fields["soak_summary.goroutine_leak_detected"] != 1 || fields["soak_summary.restarted_processes"] != 2 {
		t.Fatalf("per-cell soak indicators not gated: %+v", v)
	}
}

func TestSchemaAtLeast(t *testing.T) {
	cases := []struct {
		v, want string
		ok      bool
	}{
		{"5.6", "5.6", true}, {"5.7", "5.6", true}, {"6.0", "5.6", true}, {"5.10", "5.6", true},
		{"5.5", "5.6", false}, {"4.9", "5.6", false}, {"", "5.6", false}, {"junk", "5.6", false}, {"5", "5.6", false},
		{SchemaVersion, "5.6", true},
	}
	for _, c := range cases {
		if got := SchemaAtLeast(c.v, c.want); got != c.ok {
			t.Errorf("SchemaAtLeast(%q, %q)=%v want %v", c.v, c.want, got, c.ok)
		}
	}
}

// TestGate_CauseSplitIsNotDoubleCounted pins the reporting contract: a cause
// counter is detail on its gated total, not a violation of its own.
//
// The v1.5.11 soak printed "5 violations" for three distinct events because a
// single h2c hang was counted once as h2c_hang and again as h2c_hang_timeout.
// One defect must produce exactly one violation, with the cause carried in the
// message so nothing diagnostic is lost.
func TestGate_CauseSplitIsNotDoubleCounted(t *testing.T) {
	c := cleanCell("a", "iouring", "amd64")
	c.Tier1.H2CChurn["h2c_hang"] = 1
	c.Tier1.H2CChurn["h2c_hang_timeout"] = 1

	v := Gate([]ValidationCellResult{c}, nil, GateOptions{})
	if len(v) != 1 {
		t.Fatalf("one hang must yield exactly one violation, got %d: %+v", len(v), v)
	}
	if v[0].Field != "tier_1.h2c_churn.h2c_hang" {
		t.Errorf("violation should be on the total, got %q", v[0].Field)
	}
	if !strings.Contains(v[0].Why, "timeout=1") {
		t.Errorf("the cause must survive in the message, got %q", v[0].Why)
	}
}

// TestGate_WSHandshakeCauseSplitIsNotDoubleCounted is the same contract for
// the WebSocket handshake cause split.
func TestGate_WSHandshakeCauseSplitIsNotDoubleCounted(t *testing.T) {
	c := cleanCell("a", "iouring", "amd64")
	c.Tier1.WSTorture["ws_handshake_fail"] = 1
	c.Tier1.WSTorture["ws_handshake_fail_eof"] = 1

	v := Gate([]ValidationCellResult{c}, nil, GateOptions{})
	if len(v) != 1 {
		t.Fatalf("one handshake failure must yield exactly one violation, got %d: %+v", len(v), v)
	}
	if !strings.Contains(v[0].Why, "eof=1") {
		t.Errorf("the cause must survive in the message, got %q", v[0].Why)
	}
}

// TestGate_ReloginStormFails pins probatorium#292: a walker re-logging in on
// every request is the signature of a server that is not honouring sessions,
// and it must fail the gate.
//
// celeris#507 did exactly this for months -- auth_session_ratelimit ran at
// ~96% 4xx because the session cookie was never issued, so every /me was a 401
// and the walker re-logged in each time. requests_4xx lumps 401/404/429
// together, so nothing distinguished it from a healthy rate-limited run.
func TestGate_ReloginStormFails(t *testing.T) {
	c := cleanCell("auth_session_ratelimit", "iouring", "amd64")
	c.Tier1.WalkerLogins = 24        // one pre-walk login per walker
	c.Tier1.WalkerRelogins = 500_000 // a re-login on essentially every request

	v := Gate([]ValidationCellResult{c}, nil, GateOptions{})
	if len(v) != 1 || v[0].Field != "tier_1.walker_relogins" {
		t.Fatalf("a relogin storm must fail the gate, got %+v", v)
	}
	if !strings.Contains(v[0].Why, "not honouring the session") {
		t.Errorf("the message should name the cause, got %q", v[0].Why)
	}
}

// TestGate_OrdinarySessionExpiryPasses guards the other side: a long soak cell
// legitimately re-logs in when a session expires, and that must not fail.
func TestGate_OrdinarySessionExpiryPasses(t *testing.T) {
	c := cleanCell("auth_session_ratelimit", "iouring", "amd64")
	c.Tier1.WalkerLogins = 24
	c.Tier1.WalkerRelogins = 48 // a couple of expiries per walker over an hour

	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{}); len(v) != 0 {
		t.Fatalf("ordinary session expiry must not fail the gate, got %+v", v)
	}
}

// TestGate_DeliberateLogoutsExplainRelogins pins the other half of
// probatorium#292: the matrices walk through their logout state on purpose,
// and each logout costs exactly one 401 and one re-login. Judged against the
// walker count alone, every nightly failed with ~90k relogins on all six
// auth_session_ratelimit cells while sessions were provably working.
func TestGate_DeliberateLogoutsExplainRelogins(t *testing.T) {
	c := cleanCell("auth_session_ratelimit", "iouring", "amd64")
	c.Tier1.WalkerLogins = 19
	c.Tier1.WalkerLogouts = 90_000
	c.Tier1.WalkerRelogins = 90_014 // every logout, plus a few real expiries

	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{}); len(v) != 0 {
		t.Fatalf("relogins explained by deliberate logouts must not fail the gate, got %+v", v)
	}
}

// TestGate_ReloginStormBeyondLogoutsFails guards that the logout allowance did
// not defang the check: re-logins the walk never asked for are still a
// failure, even when it also logged out a lot.
func TestGate_ReloginStormBeyondLogoutsFails(t *testing.T) {
	c := cleanCell("auth_session_ratelimit", "iouring", "amd64")
	c.Tier1.WalkerLogins = 19
	c.Tier1.WalkerLogouts = 5_000
	c.Tier1.WalkerRelogins = 500_000 // a re-login on essentially every request

	v := Gate([]ValidationCellResult{c}, nil, GateOptions{})
	if len(v) != 1 || v[0].Field != "tier_1.walker_relogins" {
		t.Fatalf("unexplained relogins must still fail the gate, got %+v", v)
	}
}

// TestGate_ReloginCountersAbsentIsSilent keeps older artifacts readable: runs
// produced before these counters existed must gate exactly as they did.
func TestGate_ReloginCountersAbsentIsSilent(t *testing.T) {
	c := cleanCell("a", "std", "amd64") // both counters zero
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{}); len(v) != 0 {
		t.Fatalf("absent relogin counters must not fail, got %+v", v)
	}
}

// TestGate_VacuousH2CSliceIsAFailure pins probatorium#279: a cell whose
// refapp is configured to answer the h1->h2c upgrade must record at least
// one 101, or the whole h2c slice measured nothing.
//
// Zero upgrades means every churn mode degenerated into a plain declined
// GET, so h2c_hang and h2c_crashed judged a code path the server never
// entered -- and read as health. That is the same class as the dead-cell
// (requests_sent == 0) rule, and it is the state the v1.5.11 nightly and
// 24h soak shipped in: 172,656 preambles, zero upgrades, gate green.
func TestGate_VacuousH2CSliceIsAFailure(t *testing.T) {
	c := cleanCell("kitchen_sink", "iouring", "amd64")
	c.Tier1.H2CChurn = map[string]int64{
		"h2c_sent": 3597, "h2c_upgraded": 0, "h2c_declined": 2464, "h2c_intentional_rst": 1133,
	}
	v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true})
	if len(v) != 1 || v[0].Field != "tier_1.h2c_churn.h2c_upgraded" {
		t.Fatalf("want exactly one violation on tier_1.h2c_churn.h2c_upgraded, got %v", v)
	}
	if !strings.Contains(v[0].Why, "3597") {
		t.Fatalf("Why must name the preamble count so the reading is attributable, got %q", v[0].Why)
	}

	// One upgrade is enough: the slice is exercising the path, and the
	// magnitude is informational (the churn modes RST most of them).
	c.Tier1.H2CChurn["h2c_upgraded"] = 1
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true}); len(v) != 0 {
		t.Fatalf("a cell that completed an upgrade must pass, got %v", v)
	}

	// A refapp that serves HTTP/1.1 only is not judged on upgrades:
	// declining is a valid answer per RFC 9113 3.4, and seven of the
	// eight refapps stay on HTTP1 on purpose as the control group.
	h1 := cleanCell("driver_postgres", "iouring", "amd64")
	h1.Tier1.H2CChurn = map[string]int64{"h2c_sent": 3597, "h2c_upgraded": 0, "h2c_declined": 3597}
	if v := Gate([]ValidationCellResult{h1}, nil, GateOptions{RequireTier3: true}); len(v) != 0 {
		t.Fatalf("an HTTP1-only refapp must not be judged on upgrades, got %v", v)
	}

	// The slice not being scheduled (concurrency < 10) says nothing about
	// the server -- only a cell that actually sent preambles is judged.
	idle := cleanCell("kitchen_sink", "std", "arm64")
	idle.Tier1.H2CChurn = map[string]int64{"h2c_sent": 0}
	if v := Gate([]ValidationCellResult{idle}, nil, GateOptions{RequireTier3: true}); len(v) != 0 {
		t.Fatalf("an unscheduled h2c slice must not be judged, got %v", v)
	}
}

// The refapp set is nil-means-default so the nightly enforces the check
// without opting in, and non-nil-empty-means-off so an archived run
// recorded before any refapp served Auto can still be re-gated.
func TestGate_H2CUpgradeRefappsOverride(t *testing.T) {
	vacuous := func(refapp string) ValidationCellResult {
		c := cleanCell(refapp, "epoll", "arm64")
		c.Tier1.H2CChurn = map[string]int64{"h2c_sent": 100, "h2c_upgraded": 0, "h2c_declined": 100}
		return c
	}
	if len(DefaultH2CUpgradeRefapps) == 0 {
		t.Fatal("DefaultH2CUpgradeRefapps must name at least one refapp, or nothing exercises the upgrade")
	}
	dflt := vacuous(DefaultH2CUpgradeRefapps[0])
	if v := Gate([]ValidationCellResult{dflt}, nil, GateOptions{}); len(v) != 1 {
		t.Fatalf("a nil refapp set must fall back to DefaultH2CUpgradeRefapps, got %v", v)
	}
	if v := Gate([]ValidationCellResult{dflt}, nil, GateOptions{H2CUpgradeRefapps: []string{}}); len(v) != 0 {
		t.Fatalf("an explicit empty refapp set must disable the check, got %v", v)
	}
	other := vacuous("observability")
	if v := Gate([]ValidationCellResult{other}, nil, GateOptions{H2CUpgradeRefapps: []string{"observability"}}); len(v) != 1 {
		t.Fatalf("an explicit refapp set must be honoured, got %v", v)
	}
}
