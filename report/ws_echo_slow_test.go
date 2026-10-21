package report

import (
	"strings"
	"testing"
)

// Race tier semantics (probatorium#481, celeris#871): the gate fails on
// liveness defects and records latency. A SLOW_CLASS ws_echo timeout is booked
// by the validator as ws_echo_timeout_slow, which the gate tolerates only when
// the run is the race tier (GateOptions.ToleratesWSEchoSlow, set by that
// workflow alone), only in a cell that shows a -race build, and only up to
// WSEchoSlowTolerance per cell. ws_echo_timeout itself is gated as ever.

func slowCell(n int64) ValidationCellResult {
	c := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	c.Tier1.WSEcho["ws_echo_timeout_slow"] = n
	return c
}

func raceOpts() GateOptions {
	return GateOptions{ExpectedCells: 1, RequireTier3: true, ToleratesWSEchoSlow: true}
}

func fields(v []Violation) []string {
	var out []string
	for _, x := range v {
		out = append(out, x.Field)
	}
	return out
}

func TestGate_SlowTimeoutIsGreenInTheRaceTier(t *testing.T) {
	for n := int64(1); n <= WSEchoSlowTolerance; n++ {
		if v := Gate([]ValidationCellResult{slowCell(n)}, nil, raceOpts()); len(v) != 0 {
			t.Fatalf("ws_echo_timeout_slow=%d in the race tier must not fail the gate, got %v", n, v)
		}
	}
}

// Every other tier keeps failing: no tolerance option, no pass.
func TestGate_SlowTimeoutIsRedOutsideTheRaceTier(t *testing.T) {
	opts := raceOpts()
	opts.ToleratesWSEchoSlow = false
	v := Gate([]ValidationCellResult{slowCell(1)}, nil, opts)
	if got := fields(v); len(got) != 1 || got[0] != "tier_1.ws_echo.ws_echo_timeout_slow" {
		t.Fatalf("violations %v, want exactly tier_1.ws_echo.ws_echo_timeout_slow: nightly/soak keep failing on a SLOW ws_echo timeout", got)
	}
	if !strings.Contains(v[0].Why, "does not tolerate") {
		t.Errorf("why %q should say the tier does not tolerate it", v[0].Why)
	}
}

// A mis-plumbed tolerance cannot waive a cell that is not a -race build.
func TestGate_SlowTimeoutIsRedInACellWithoutRaceEvidence(t *testing.T) {
	c := slowCell(1)
	c.PropertiesNotInstrumented = []string{"I-RACE"}
	v := Gate([]ValidationCellResult{c}, nil, raceOpts())
	if got := fields(v); len(got) != 1 || got[0] != "tier_1.ws_echo.ws_echo_timeout_slow" {
		t.Fatalf("violations %v, want ws_echo_timeout_slow: no I-RACE declared, so no -race evidence", got)
	}
	// A cell whose property loop never ran has no evidence either.
	c = slowCell(1)
	c.Tier1.PropertyEvaluations = 0
	if v := Gate([]ValidationCellResult{c}, nil, raceOpts()); len(v) == 0 {
		t.Fatal("a cell whose property loop never ran cannot show a -race build; the gate passed it")
	}
}

// The guard also holds at the gate: more than N in one cell is red.
func TestGate_MoreThanNSlowTimeoutsInACellIsRed(t *testing.T) {
	v := Gate([]ValidationCellResult{slowCell(WSEchoSlowTolerance + 1)}, nil, raceOpts())
	if got := fields(v); len(got) != 1 || got[0] != "tier_1.ws_echo.ws_echo_timeout_slow" {
		t.Fatalf("violations %v, want ws_echo_timeout_slow over N=%d", got, WSEchoSlowTolerance)
	}
	if v[0].Value != WSEchoSlowTolerance+1 || !strings.Contains(v[0].Why, "more than 3") {
		t.Errorf("violation %+v should carry the count and say 'more than 3'", v[0])
	}
}

// ws_echo_timeout stays gated, in the race tier too: a failing timeout beside
// tolerated slow ones fails exactly as today.
func TestGate_FailingTimeoutStillRedInTheRaceTier(t *testing.T) {
	c := slowCell(1)
	c.Tier1.WSEcho["ws_echo_timeout"] = 1
	v := Gate([]ValidationCellResult{c}, nil, raceOpts())
	if got := fields(v); len(got) != 1 || got[0] != "tier_1.ws_echo.ws_echo_timeout" {
		t.Fatalf("violations %v, want exactly tier_1.ws_echo.ws_echo_timeout", got)
	}
}

// Every other ws_echo signal is untouched by the tolerance.
func TestGate_OtherWSEchoSignalsStayRedInTheRaceTier(t *testing.T) {
	for _, key := range []string{"ws_echo_corrupt", "ws_echo_reorder", "ws_echo_missing"} {
		c := slowCell(1)
		c.Tier1.WSEcho[key] = 1
		if v := Gate([]ValidationCellResult{c}, nil, raceOpts()); len(v) != 1 || v[0].Field != "tier_1.ws_echo."+key {
			t.Errorf("%s: violations %v, want exactly that key", key, v)
		}
	}
}

// ValidateDiff is strict in the race workflow: an amd64-only SLOW timeout
// must not read as a cross-arch divergence, or the run stays red after the
// gate passes.
func TestDiff_SlowTimeoutIsNotACrossArchDivergence(t *testing.T) {
	a, b := slowCell(2), cleanCell("auth_session_ratelimit", "adaptive", "arm64")
	if d := DiffCells([]ValidationCellResult{a, b}); len(d) != 0 {
		t.Fatalf("divergences %+v, want none: ws_echo_timeout_slow is latency, not a defect to compare", d)
	}
	if d := DiffValidation(&ValidationResults{Tier1: a.Tier1}, &ValidationResults{Tier1: b.Tier1}, "amd64", "arm64"); len(d) != 0 {
		t.Fatalf("divergences %+v, want none", d)
	}
}

// Printed in the gate's output, NOT gating, whatever the verdict.
func TestWSEchoSlowLines(t *testing.T) {
	cells := []ValidationCellResult{slowCell(2), cleanCell("kitchen_sink", "epoll", "arm64")}
	lines := strings.Join(WSEchoSlowLines(cells, true), "\n")
	for _, want := range []string{"ws_echo_timeout_slow", "NOT failing", "auth_session_ratelimit", "adaptive", "amd64", "2", "N=3", "1 cell(s)"} {
		if !strings.Contains(lines, want) {
			t.Errorf("output lacks %q:\n%s", want, lines)
		}
	}
	if strings.Contains(lines, "kitchen_sink") {
		t.Errorf("a cell with no slow timeout is listed:\n%s", lines)
	}
	none := strings.Join(WSEchoSlowLines([]ValidationCellResult{cleanCell("a", "std", "amd64")}, true), "\n")
	if !strings.Contains(none, "none recorded") {
		t.Errorf("a race run with no slow timeout must say so:\n%s", none)
	}
	if got := WSEchoSlowLines([]ValidationCellResult{cleanCell("a", "std", "amd64")}, false); len(got) != 0 {
		t.Errorf("outside the race tier with nothing recorded the gate prints nothing, got %v", got)
	}
	// Outside the race tier a recorded one is a violation, and the line says so.
	out := strings.Join(WSEchoSlowLines([]ValidationCellResult{slowCell(1)}, false), "\n")
	if !strings.Contains(out, "auth_session_ratelimit") || strings.Contains(out, "NOT failing") {
		t.Errorf("outside the race tier a recorded timeout must not be described as non-failing:\n%s", out)
	}
}
