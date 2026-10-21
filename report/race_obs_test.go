package report

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
	"time"
)

// Race tier observability (probatorium#481, celeris#871): the gate's
// account of a race run must say what the cell did.
//
// Run 39560945814, cell auth_session_ratelimit adaptive amd64: 11 ws_echo
// fires, 4 of them SLOW_CLASS timeouts (3 tolerated, the 4th refused for
// being over N=3 and counted in ws_echo_timeout). The gate's line said
// "3 SLOW_CLASS ws_echo timeout(s) in 11 fire(s)", leaving out the one that
// failed, and the failure read as a wedge.

// runCell is that cell's ws_echo tally.
func runCell() ValidationCellResult {
	c := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	c.Tier1.WSEcho["ws_echo_fires"] = 11
	c.Tier1.WSEcho["ws_echo_ok"] = 807
	c.Tier1.WSEcho["ws_echo_timeout_slow"] = 3
	c.Tier1.WSEcho["ws_echo_timeout"] = 1
	c.Tier1.WSEcho["ws_echo_slow_over_n"] = 1
	return c
}

const mainHeader = "ws_echo SLOW_CLASS timeouts (ws_echo_timeout_slow, race tier): 1 cell(s) recorded latency, NOT failing up to N=3 per cell (the server read and wrote in the window; a WEDGE_CLASS or INDETERMINATE timeout is ws_echo_timeout and fails). A rise in these counts is the signal to watch:"

// A document without the new keys (every one written before this change, and
// every cell with nothing refused) prints exactly what main printed.
func TestWSEchoSlowLines_DocumentWithoutRefusedRendersAsBefore(t *testing.T) {
	c := runCell()
	delete(c.Tier1.WSEcho, "ws_echo_slow_over_n")
	delete(c.Tier1.WSEcho, "ws_echo_timeout")
	got := WSEchoSlowLines([]ValidationCellResult{c}, true)
	want := []string{
		mainHeader,
		fmt.Sprintf("  %-24s %-8s %-12s 3 SLOW_CLASS ws_echo timeout(s) in 11 fire(s) (807 echoes ok)", "auth_session_ratelimit", "adaptive", "amd64"),
	}
	if len(got) != len(want) {
		t.Fatalf("lines:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("line %d changed:\n got %q\nwant %q", i, got[i], want[i])
		}
	}
}

// Item 1: every SLOW fire is in the line, the over-N one has its own name and
// is said to fail.
func TestWSEchoSlowLines_ReportsEverySlowFireAndNamesTheOverNOne(t *testing.T) {
	lines := WSEchoSlowLines([]ValidationCellResult{runCell()}, true)
	all := strings.Join(lines, "\n")
	t.Logf("\n%s", all)
	if strings.Contains(all, "3 SLOW_CLASS ws_echo timeout(s) in 11 fire(s)") {
		t.Errorf("the line still counts 3 SLOW fires of a cell that had 4:\n%s", all)
	}
	var row string
	for _, l := range lines {
		if strings.Contains(l, "auth_session_ratelimit") {
			row = l
			break
		}
	}
	for _, want := range []string{"4 SLOW_CLASS", "in 11 fire(s)", "807 echoes ok", "3 tolerated", "1 over N=3", "FAILING"} {
		if !strings.Contains(row, want) {
			t.Errorf("row lacks %q:\n%s", want, row)
		}
	}
	// A line of its own for the failing ones, named by the field.
	var over []string
	for _, l := range lines {
		if strings.Contains(l, "ws_echo_slow_over_n") {
			over = append(over, l)
		}
	}
	if len(over) == 0 {
		t.Fatalf("no line names ws_echo_slow_over_n:\n%s", all)
	}
	ov := strings.Join(over, "\n")
	for _, want := range []string{"FAILING", "slow-class latency", "not a wedge", "N=3"} {
		if !strings.Contains(all, want) {
			t.Errorf("output lacks %q:\n%s", want, all)
		}
	}
	if !strings.Contains(ov, "ws_echo_timeout") {
		t.Errorf("the over-N line should say it is counted as ws_echo_timeout:\n%s", ov)
	}
}

// The other guard reason counts as a SLOW fire too.
func TestWSEchoSlowLines_NoCleanFireReasonIsCounted(t *testing.T) {
	c := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	c.Tier1.WSEcho["ws_echo_fires"] = 5
	c.Tier1.WSEcho["ws_echo_timeout_slow"] = 1
	c.Tier1.WSEcho["ws_echo_timeout"] = 1
	c.Tier1.WSEcho["ws_echo_slow_no_clean_fire"] = 1
	all := strings.Join(WSEchoSlowLines([]ValidationCellResult{c}, true), "\n")
	for _, want := range []string{"2 SLOW_CLASS", "1 tolerated", "ws_echo_slow_no_clean_fire", "FAILING"} {
		if !strings.Contains(all, want) {
			t.Errorf("output lacks %q:\n%s", want, all)
		}
	}
	// Only over-N is "latency past the tolerance, not a wedge": the persistence
	// rule can refuse the 2nd SLOW fire and says a server that delivers nothing.
	for _, bad := range []string{"over N=3", "past N=", "not a wedge", "ws_echo_slow_over_n:"} {
		if strings.Contains(all, bad) {
			t.Errorf("a no-clean-fire refusal must not say %q:\n%s", bad, all)
		}
	}
	if !strings.Contains(all, "none drained cleanly") {
		t.Errorf("a no-clean-fire refusal must say why:\n%s", all)
	}
	v := Gate([]ValidationCellResult{c}, nil, raceOpts())
	if len(v) != 1 || strings.Contains(v[0].Why, "not a wedge") || !strings.Contains(v[0].Why, "no fire drained cleanly") {
		t.Errorf("violation wording for a no-clean-fire refusal: %+v", v)
	}
}

// A cell with a refused SLOW fire and nothing tolerated is still listed.
func TestWSEchoSlowLines_ListsACellWithOnlyRefusedFires(t *testing.T) {
	c := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	c.Tier1.WSEcho["ws_echo_timeout"] = 1
	c.Tier1.WSEcho["ws_echo_slow_over_n"] = 1
	all := strings.Join(WSEchoSlowLines([]ValidationCellResult{c}, true), "\n")
	if !strings.Contains(all, "auth_session_ratelimit") || !strings.Contains(all, "ws_echo_slow_over_n") {
		t.Errorf("a cell whose only SLOW fire was refused is missing:\n%s", all)
	}
	if got := WSEchoSlowLines([]ValidationCellResult{c}, false); len(got) != 0 {
		t.Errorf("outside the race tier a refused-only cell cannot exist and must not be described, got %v", got)
	}
}

// The failing violation: same field, same count, same single row. Only its
// wording says what the timeout was.
func TestGate_OverNSlowTimeoutKeepsItsViolationAndSaysWhatItWas(t *testing.T) {
	v := Gate([]ValidationCellResult{runCell()}, nil, raceOpts())
	if got := fields(v); len(got) != 1 || got[0] != "tier_1.ws_echo.ws_echo_timeout" {
		t.Fatalf("violations %v, want exactly tier_1.ws_echo.ws_echo_timeout (the new counter must not add a second row for the same event)", got)
	}
	if v[0].Value != 1 {
		t.Errorf("value %d, want 1", v[0].Value)
	}
	for _, want := range []string{"SLOW_CLASS", "ws_echo_slow_over_n=1", "not a wedge"} {
		if !strings.Contains(v[0].Why, want) {
			t.Errorf("why %q lacks %q", v[0].Why, want)
		}
	}
}

// A timeout that is not a refused SLOW fire reads exactly as before.
func TestGate_OtherTimeoutsKeepTheirWording(t *testing.T) {
	c := cleanCell("a", "adaptive", "amd64")
	c.Tier1.WSEcho["ws_echo_timeout"] = 1
	v := Gate([]ValidationCellResult{c}, nil, raceOpts())
	if len(v) != 1 || v[0].Why != "a WebSocket echo connection stopped delivering before its hold expired" {
		t.Fatalf("violations %+v, want the unchanged wording", v)
	}
	// Mixed: one of two timeouts was a refused SLOW fire. The wording is about
	// that fire only and says the others were not SLOW.
	c.Tier1.WSEcho["ws_echo_timeout"] = 2
	c.Tier1.WSEcho["ws_echo_slow_over_n"] = 1
	v = Gate([]ValidationCellResult{c}, nil, raceOpts())
	if len(v) != 1 || !strings.Contains(v[0].Why, "1 of 2") || !strings.Contains(v[0].Why, "the others were WEDGE_CLASS") {
		t.Fatalf("mixed timeouts: %+v, want '1 of 2' and the others named as not SLOW", v)
	}
}

// The new counters are attribution of ws_echo_timeout, never a signal of
// their own: alone they fail nothing, and they do not enter the cross-arch
// diff.
func TestGate_RefusedCountersAreNotGatedOrDiffed(t *testing.T) {
	c := cleanCell("a", "adaptive", "amd64")
	c.Tier1.WSEcho["ws_echo_slow_over_n"] = 2
	c.Tier1.WSEcho["ws_echo_slow_no_clean_fire"] = 1
	if v := Gate([]ValidationCellResult{c}, nil, raceOpts()); len(v) != 0 {
		t.Errorf("violations %v: the attribution counters must not gate", v)
	}
	b := cleanCell("a", "adaptive", "arm64")
	if d := DiffCells([]ValidationCellResult{c, b}); len(d) != 0 {
		t.Errorf("divergences %+v: ws_echo_slow_over_n is not a cross-arch signal", d)
	}
}

// h2c_hang and ws_handshake_fail keep failing the gate; their spans are
// record only and the violation is unchanged.
func TestGate_SpansDoNotChangeTheH2CAndWSViolations(t *testing.T) {
	stamp := EventStamp{TS: "2026-10-21T09:36:10.123Z", Cause: "reset"}
	c := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	c.Tier1.H2CChurn["h2c_hang"] = 8
	c.Tier1.H2CChurn["h2c_hang_eof"] = 5
	c.Tier1.H2CChurn["h2c_hang_reset"] = 3
	c.Tier1.WSTorture["ws_handshake_fail"] = 3
	c.Tier1.WSTorture["ws_handshake_fail_reset"] = 3
	without := Gate([]ValidationCellResult{c}, nil, raceOpts())
	c.Tier1.H2CHangSpan = &EventSpan{Count: 8, First: stamp, Last: stamp}
	c.Tier1.WSHandshakeFailSpan = &EventSpan{Count: 3, First: stamp, Last: stamp}
	with := Gate([]ValidationCellResult{c}, nil, raceOpts())
	if len(without) != 2 || fmt.Sprint(without) != fmt.Sprint(with) {
		t.Fatalf("without spans %v, with %v: h2c_hang and ws_handshake_fail must fail identically", without, with)
	}
}

// Schema 5.23 is additive: the new fields validate, round-trip, and are
// omitted when empty so every older reader and document is unaffected.
func TestSchema523SpansAndSchedRoundTrip(t *testing.T) {
	if SchemaVersion != "5.23" {
		t.Fatalf("SchemaVersion %q, want 5.23", SchemaVersion)
	}
	sch := loadSchema(t)
	st := EventStamp{TS: "2026-10-21T09:36:10.123456789Z", SinceReadyMs: 31000, Cause: "eof", Err: "EOF", LocalAddr: "127.0.0.1:50000", RemoteAddr: "127.0.0.1:2059"}
	c := cleanCell("auth_session_ratelimit", "adaptive", "amd64")
	c.Tier1.H2CHangSpan = &EventSpan{Count: 2, First: st, Last: st}
	c.Tier1.WSHandshakeFailSpan = &EventSpan{Count: 1, First: st, Last: st}
	c.Tier1.RefappSched = &RefappSched{At: "2026-10-21T09:36:03Z", Threads: 5, LeaderCPUsAllowedList: "0",
		ThreadMasks: map[string]int{"0": 1, "0-15": 4}, GOMAXPROCSNote: "no side listener"}
	now := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	doc := minimalDoc(t)
	doc.Validation = &ValidationResults{StartedAt: now, FinishedAt: now.Add(time.Minute),
		Cells: []ValidationCellResult{c}}
	raw, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	validateAny(t, sch, raw)
	var back Document
	if err := json.Unmarshal(raw, &back); err != nil {
		t.Fatal(err)
	}
	t1 := back.Validation.Cells[0].Tier1
	if t1.H2CHangSpan == nil || t1.H2CHangSpan.First != st || t1.H2CHangSpan.Count != 2 ||
		t1.WSHandshakeFailSpan == nil || t1.RefappSched == nil || t1.RefappSched.ThreadMasks["0-15"] != 4 {
		t.Errorf("round trip lost the new fields: %+v", t1)
	}
	// An older document, without any of them, still parses and validates.
	plain := cleanCell("a", "std", "amd64")
	olderDoc := minimalDoc(t)
	olderDoc.SchemaVersion = "5.22"
	olderDoc.Validation = &ValidationResults{StartedAt: now, FinishedAt: now, Cells: []ValidationCellResult{plain}}
	rawPlain, _ := json.Marshal(olderDoc)
	for _, k := range []string{"h2c_hang_span", "ws_handshake_fail_span", "refapp_sched"} {
		if strings.Contains(string(rawPlain), k) {
			t.Errorf("%s is serialised for a cell without it", k)
		}
	}
	validateAny(t, sch, rawPlain)
}

// A cell with both reasons says both.
func TestWSEchoSlowLines_BothReasonsInOneCell(t *testing.T) {
	c := runCell()
	c.Tier1.WSEcho["ws_echo_slow_no_clean_fire"] = 1
	c.Tier1.WSEcho["ws_echo_timeout"] = 2
	all := strings.Join(WSEchoSlowLines([]ValidationCellResult{c}, true), "\n")
	for _, want := range []string{"5 SLOW_CLASS", "2 FAILING", "not a wedge", "none drained cleanly"} {
		if !strings.Contains(all, want) {
			t.Errorf("output lacks %q:\n%s", want, all)
		}
	}
	v := Gate([]ValidationCellResult{c}, nil, raceOpts())
	if len(v) != 1 || !strings.Contains(v[0].Why, "not a wedge") || !strings.Contains(v[0].Why, "no fire drained cleanly") {
		t.Errorf("both reasons should be in the violation: %+v", v)
	}
}
