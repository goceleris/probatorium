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
			WSEcho:      map[string]int64{"ws_echo_fires": 10, "ws_echo_upgraded": 10, "ws_echo_sent": 3000, "ws_echo_ok": 3000, "ws_echo_close_ok": 10},
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
		{"ws_echo_corrupt", "tier_1.ws_echo.ws_echo_corrupt", func(c *ValidationCellResult) { c.Tier1.WSEcho["ws_echo_corrupt"] = 1 }},
		{"ws_echo_reorder", "tier_1.ws_echo.ws_echo_reorder", func(c *ValidationCellResult) { c.Tier1.WSEcho["ws_echo_reorder"] = 1 }},
		{"ws_echo_missing", "tier_1.ws_echo.ws_echo_missing", func(c *ValidationCellResult) { c.Tier1.WSEcho["ws_echo_missing"] = 1 }},
		{"ws_echo_timeout", "tier_1.ws_echo.ws_echo_timeout", func(c *ValidationCellResult) { c.Tier1.WSEcho["ws_echo_timeout"] = 1 }},
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
	for _, k := range []string{"ws_echo_fires", "ws_echo_upgraded", "ws_echo_sent", "ws_echo_ok", "ws_echo_close_ok",
		"ws_echo_cut_at_deadline", "ws_echo_frame_err", "ws_echo_handshake_fail", "ws_echo_endpoint_absent",
		"ws_echo_route_probed", "ws_echo_route_present"} {
		c.Tier1.WSEcho[k] = 1_000_000
	}
	if v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true}); len(v) != 0 {
		t.Fatalf("informational counters must not be gated, got %v", v)
	}
}

// wsEchoGatedKeys are the large-echo slice's defect classes (celeris#587).
var wsEchoGatedKeys = []string{"ws_echo_corrupt", "ws_echo_reorder", "ws_echo_missing", "ws_echo_timeout"}

// gateWSEchoKey runs the gate over a clean cell with one ws_echo key set
// and reports the violations that name that key.
func gateWSEchoKey(key string, value int64) []Violation {
	c := cleanCell("auth_session_ratelimit", "iouring", "arm64")
	c.Tier1.WSEcho[key] = value
	var out []Violation
	for _, v := range Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true}) {
		if v.Field == "tier_1.ws_echo."+key {
			out = append(out, v)
		}
	}
	return out
}

// TestGate_WSEchoKeysAreLoadBearing pins the four large-echo rows, and
// carries its own negative control: with the ws_echo rows stripped from
// gatedTier1Keys the very same cells pass, which is what the positive half
// would report if the rows went missing -- so the positive half fails
// exactly when a key is ungated, and for no other reason.
func TestGate_WSEchoKeysAreLoadBearing(t *testing.T) {
	for _, k := range wsEchoGatedKeys {
		if v := gateWSEchoKey(k, 3); len(v) != 1 || v[0].Value != 3 {
			t.Errorf("gated: %s=3 must be exactly one violation naming it, got %v", k, v)
		}
	}
	// The corrupt row carries its attribution split as cause detail, not as
	// extra violations: one event, one violation, the split in Why.
	c := cleanCell("auth_session_ratelimit", "iouring", "amd64")
	c.Tier1.WSEcho["ws_echo_corrupt"] = 2
	c.Tier1.WSEcho["ws_echo_egress_interleave"] = 1
	c.Tier1.WSEcho["ws_echo_other_corrupt"] = 1
	v := Gate([]ValidationCellResult{c}, nil, GateOptions{RequireTier3: true})
	if len(v) != 1 || v[0].Field != "tier_1.ws_echo.ws_echo_corrupt" {
		t.Fatalf("corrupt + its split must be ONE violation, got %v", v)
	}
	if !strings.Contains(v[0].Why, "egress_interleave=1") || !strings.Contains(v[0].Why, "other_corrupt=1") {
		t.Errorf("Why must carry the attribution split, got %q", v[0].Why)
	}

	// Negative control: ungate the rows and the same inputs pass.
	saved := gatedTier1Keys
	defer func() { gatedTier1Keys = saved }()
	var stripped []struct{ slice, key, why string }
	for _, g := range saved {
		if g.slice != "ws_echo" {
			stripped = append(stripped, g)
		}
	}
	gatedTier1Keys = stripped
	for _, k := range wsEchoGatedKeys {
		if v := gateWSEchoKey(k, 3); len(v) != 0 {
			t.Errorf("control: with the ws_echo rows removed %s must NOT be a violation, got %v -- the positive half is not testing the rows", k, v)
		}
	}
}

// The relative diff sees the same keys, so a corruption on one arch only is
// a cross-arch divergence.
func TestDiffValidation_WSEchoKeysAreCompared(t *testing.T) {
	a := &ValidationResults{Tier1: &Tier1Summary{WSEcho: map[string]int64{"ws_echo_corrupt": 4, "ws_echo_reorder": 0, "ws_echo_missing": 1, "ws_echo_timeout": 0}}}
	b := &ValidationResults{Tier1: &Tier1Summary{WSEcho: map[string]int64{"ws_echo_corrupt": 0, "ws_echo_reorder": 0, "ws_echo_missing": 1, "ws_echo_timeout": 2}}}
	got := map[string]Divergence{}
	for _, d := range DiffValidation(a, b, "amd64", "arm64") {
		got[d.Counter] = d
	}
	if d, ok := got["ws_echo_corrupt"]; !ok || d.Slice != "ws_echo" || d.ValA != 4 || d.ValB != 0 || d.Severity != SeverityHigh {
		t.Errorf("ws_echo_corrupt asymmetric must diverge HIGH, got %+v", got)
	}
	if d, ok := got["ws_echo_timeout"]; !ok || d.ValA != 0 || d.ValB != 2 || d.Severity != SeverityMed {
		t.Errorf("ws_echo_timeout asymmetric must diverge MED, got %+v", got)
	}
	if _, ok := got["ws_echo_missing"]; ok {
		t.Errorf("symmetric ws_echo_missing must not diverge (Gate catches presence), got %+v", got["ws_echo_missing"])
	}
	if _, ok := got["ws_echo_reorder"]; ok {
		t.Errorf("both-zero ws_echo_reorder must not diverge, got %+v", got["ws_echo_reorder"])
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

// The gate is where a human reads the verdict, so "dead cell" has to say
// which kind of dead. A cell whose refapp never started and a cell whose
// oracle killed it both arrive with requests_sent == 0; until schema 5.11
// the difference existed only in the validator's run log (probatorium#359).
func TestGateDeadCellCarriesTheRecordedReason(t *testing.T) {
	cells := []ValidationCellResult{
		{
			Refapp: "auth_jwt_csrf", Engine: "adaptive", Arch: "amd64",
			Tier1:         &Tier1Summary{},
			Status:        ValidationCellNotRun,
			FailureReason: "cell run: validation: I-LIVENESS violated by tier-1-property: refapp process died mid-run: ambiguous configuration: Addr but Listener is bound",
		},
		{
			// No status recorded: a document from before 5.11, or a path
			// that does not go through the matrix runner. Must still gate,
			// and must not claim the cell was ok.
			Refapp: "kitchen_sink", Engine: "std", Arch: "amd64",
			Tier1: &Tier1Summary{},
		},
	}
	viol := Gate(cells, nil, GateOptions{})
	var got []string
	for _, v := range viol {
		if v.Field == "tier_1.requests_sent" {
			got = append(got, v.Why)
		}
	}
	if len(got) != 2 {
		t.Fatalf("got %d dead-cell violations, want 2: %v", len(got), viol)
	}
	if !strings.Contains(got[0], "not_run") || !strings.Contains(got[0], "I-LIVENESS") {
		t.Errorf("dead-cell verdict does not carry the recorded reason: %q", got[0])
	}
	if !strings.Contains(got[1], "dead cell") {
		t.Errorf("unrecorded cell lost its verdict: %q", got[1])
	}
	if strings.Contains(got[1], "(") {
		t.Errorf("unrecorded cell invented a reason: %q", got[1])
	}
}

// A reason long enough to wrap has to be cut, or one bad cell makes the
// gate's table unreadable.
func TestGateDeadCellReasonIsBounded(t *testing.T) {
	long := strings.Repeat("x", 600)
	why := deadCellWhy(ValidationCellResult{Status: ValidationCellNotRun, FailureReason: long})
	if len(why) > deadCellReasonMax+80 {
		t.Errorf("dead-cell verdict is %d chars: %q", len(why), why)
	}
	if !strings.HasSuffix(why, "...)") {
		t.Errorf("truncation is not marked: %q", why)
	}
}

// A cell that never ran has no engine counters, because it had no engine.
// The two unconditional engine checks added in schema 5.11 (#368) must not
// manufacture a verdict out of that absence: "the engine counted 0 requests
// while the walker sent 0, a factor of +Inf" is noise standing where the
// real finding -- the refapp would not start -- should be.
//
// The fixture deliberately reaches BOTH checks: its property loop ran (every
// evaluation a skip, which is what a loop polling a refapp that never came
// up records), so ranPropertyLoop is true and neither check is short-
// circuited by the guard that precedes them.
func TestGateInventsNoEngineVerdictForACellThatNeverRan(t *testing.T) {
	notRun := ValidationCellResult{
		Refapp: "auth_jwt_csrf", Engine: "adaptive", Arch: "amd64",
		Status:        ValidationCellNotRun,
		FailureReason: "cell run: validation: I-LIVENESS violated by tier-1-property: refapp process died mid-run",
		Tier1: &Tier1Summary{
			PropertySkips: 12, // the loop ran and judged nothing
			// No traffic and no engine: every counter below is absent,
			// not zero-because-measured.
			RequestsSent:        0,
			EngineRequestsTotal: 0,
			EngineZeroWitness: map[string]int64{
				"engine_recv_double_armed":              0,
				"engine_close_missing_conn_state":       0,
				"engine_transplant_adopt_slot_occupied": 0,
			},
		},
	}
	// Non-vacuity: if the fixture did not reach the engine checks, this
	// test would pass against code that fires on every empty cell.
	if !ranPropertyLoop(notRun) {
		t.Fatal("fixture does not reach the engine checks — the test would prove nothing")
	}

	viol := Gate([]ValidationCellResult{notRun}, nil, GateOptions{})
	var fields []string
	for _, v := range viol {
		fields = append(fields, v.Field)
		if strings.Contains(v.Field, "engine_") {
			t.Errorf("a cell that never ran produced an engine verdict: %s = %d (%s)", v.Field, v.Value, v.Why)
		}
	}
	// It must still fail, as the dead cell it is, with its reason attached.
	var dead string
	for _, v := range viol {
		if v.Field == "tier_1.requests_sent" {
			dead = v.Why
		}
	}
	if dead == "" {
		t.Fatalf("the cell stopped failing the gate entirely; violations: %v", fields)
	}
	if !strings.Contains(dead, "not_run") || !strings.Contains(dead, "I-LIVENESS") {
		t.Errorf("dead-cell verdict lost the recorded reason: %q", dead)
	}
}

// ... and the coverage check still bites on the defect it was written for:
// a cell that DID run, whose engine under-counted by three orders of
// magnitude (celeris#626). Without this, the test above could be satisfied
// by a check that never fires at all.
func TestGateEngineRequestCoverageStillFiresOnACellThatRan(t *testing.T) {
	ran := ValidationCellResult{
		Refapp: "auth_jwt_csrf", Engine: "epoll", Arch: "amd64",
		Status: ValidationCellOK,
		Tier1: &Tier1Summary{
			RequestsSent: 3_306_726, Requests2xx: 3_306_726,
			PropertyEvaluations: 1200,
			EngineRequestsTotal: 3089,
		},
	}
	var got bool
	for _, v := range Gate([]ValidationCellResult{ran}, nil, GateOptions{}) {
		if v.Field == "tier_1.engine_requests_total" {
			got = true
		}
	}
	if !got {
		t.Error("the engine-request-coverage check did not fire on celeris#626's own numbers")
	}
}
