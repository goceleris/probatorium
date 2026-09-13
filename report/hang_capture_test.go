package report

import (
	"encoding/json"
	"strings"
	"testing"
)

// TestGate_HangViolationCarriesTheFirstSlowFire: the violation the gate
// prints for h2c_hang / ws_handshake_fail names the first failed record of
// the ring -- instant, elapsed, error, address -- so the gate output alone
// says what the v1.5.11 gate output could not. The cause suffix stays.
func TestGate_HangViolationCarriesTheFirstSlowFire(t *testing.T) {
	c := cleanCell("auth_session_ratelimit", "iouring", "arm64")
	c.Tier1.H2CChurn["h2c_hang"] = 1
	c.Tier1.H2CChurn["h2c_hang_timeout"] = 1
	c.Tier1.H2CSlowReads = []SlowFire{
		{TS: "2026-09-06T04:31:00Z", ReadMs: 1400, Outcome: "declined"}, // slow but answered: not the event
		{TS: "2026-09-06T04:32:31.5Z", ReadMs: 20000, Outcome: "hang-timeout", Err: "read tcp 127.0.0.1:41234->127.0.0.1:8080: i/o timeout", LocalAddr: "127.0.0.1:41234", ValidatorSkewMs: 0},
	}
	c.Tier1.WSTorture["ws_handshake_fail"] = 1
	c.Tier1.WSTorture["ws_handshake_fail_status"] = 1
	c.Tier1.WSSlowReads = []SlowFire{
		{TS: "2026-09-06T05:00:00Z", ReadMs: 1300, Outcome: "handshake-fail-status", Status: "HTTP/1.1 400 Bad Request", LocalAddr: "127.0.0.1:5"},
	}
	v := Gate([]ValidationCellResult{c}, nil, GateOptions{})
	if len(v) != 2 {
		t.Fatalf("two events must yield exactly two violations (the ring is detail, not a counter), got %d: %+v", len(v), v)
	}
	byField := map[string]string{}
	for _, x := range v {
		byField[x.Field] = x.Why
	}
	h := byField["tier_1.h2c_churn.h2c_hang"]
	for _, want := range []string{"(timeout=1)", "2026-09-06T04:32:31.5Z", "read=20000ms", "outcome=hang-timeout", "i/o timeout", "local=127.0.0.1:41234"} {
		if !strings.Contains(h, want) {
			t.Errorf("h2c_hang violation lacks %q: %q", want, h)
		}
	}
	if strings.Contains(h, "04:31:00") {
		t.Errorf("the answered slow read is not the event; the message must skip it: %q", h)
	}
	w := byField["tier_1.ws_torture.ws_handshake_fail"]
	for _, want := range []string{"(status=1)", "outcome=handshake-fail-status", `status="HTTP/1.1 400 Bad Request"`} {
		if !strings.Contains(w, want) {
			t.Errorf("ws_handshake_fail violation lacks %q: %q", want, w)
		}
	}
	// No ring (a document from before schema 5.9): the message is what it was.
	old := cleanCell("a", "epoll", "amd64")
	old.Tier1.H2CChurn["h2c_hang"] = 1
	ov := Gate([]ValidationCellResult{old}, nil, GateOptions{})
	if len(ov) != 1 || strings.Contains(ov[0].Why, "first:") {
		t.Fatalf("pre-5.9 document must read as before: %+v", ov)
	}
}

// TestTier1SummaryCaptureSurvivesJSON is the guard the design asks for: a
// refactor that drops the rings, the histograms, ready_at or the stderr
// tail from the wire shape fails here, and the gated totals stay gated.
func TestTier1SummaryCaptureSurvivesJSON(t *testing.T) {
	s := Tier1Summary{
		H2CChurn:     map[string]int64{"h2c_hang": 1},
		WSTorture:    map[string]int64{"ws_handshake_fail": 1},
		H2CLatency:   &WalkerLatency{Read: LatencyBuckets{Lt10s: 1, Timeout: 1}},
		WSLatency:    &WalkerLatency{Dial: LatencyBuckets{Lt100ms: 9}},
		H2CSlowReads: []SlowFire{{TS: "t", ReadMs: 20000, Outcome: "hang-timeout", Err: "e", LocalAddr: "l", RemoteAddr: "r"}},
		WSSlowReads:  []SlowFire{{TS: "t", ReadMs: 2000, Outcome: "handshake-fail-timeout", Err: "e"}},
		ReadyAt:      "2026-09-13T00:00:00Z",

		RefappStderrTail: []string{"warn"},
	}
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"h2c_latency", "ws_latency", "h2c_slow_reads", "ws_slow_reads", "ready_at", "refapp_stderr_tail"} {
		if _, ok := doc[key]; !ok {
			t.Errorf("tier_1.%s missing from the wire shape: the next h2c_hang would be unattributable again", key)
		}
	}
	lat := doc["h2c_latency"].(map[string]any)["read"].(map[string]any)
	for _, k := range []string{"lt_100ms", "lt_1s", "lt_2s", "lt_5s", "lt_10s", "lt_20s", "ge_20s", "timeout"} {
		if _, ok := lat[k]; !ok {
			t.Errorf("read histogram lacks bucket %q", k)
		}
	}
	fire := doc["h2c_slow_reads"].([]any)[0].(map[string]any)
	for _, k := range []string{"ts", "dial_ms", "write_ms", "read_ms", "outcome", "err", "local_addr", "remote_addr", "n_read"} {
		if _, ok := fire[k]; !ok {
			t.Errorf("slow fire record lacks %q", k)
		}
	}
	var rt Tier1Summary
	if err := json.Unmarshal(b, &rt); err != nil {
		t.Fatal(err)
	}
	if rt.H2CLatency.Read.Timeout != 1 || len(rt.H2CSlowReads) != 1 || rt.H2CSlowReads[0].Err != "e" {
		t.Fatalf("round trip lost data: %+v", rt)
	}
	// The totals the rings hang off are gated.
	gated := map[string]bool{}
	for _, g := range gatedTier1Keys {
		gated[g.key] = true
	}
	for _, k := range []string{"h2c_hang", "ws_handshake_fail"} {
		if !gated[k] {
			t.Errorf("%s is not gated; the capture attaches evidence to a gated total, it does not replace the gate", k)
		}
	}
	// And the informational counters are not.
	for _, k := range []string{"h2c_dial_fail", "h2c_write_fail", "h2c_slow_reads_total", "ws_dial_fail", "ws_write_fail", "ws_slow_reads_total"} {
		if gated[k] {
			t.Errorf("%s must stay informational: a dial failure is as often the validator host as the server", k)
		}
	}
	// An empty summary omits every capture key.
	eb, _ := json.Marshal(Tier1Summary{})
	for _, key := range []string{"h2c_latency", "h2c_slow_reads", "ready_at", "refapp_stderr_tail"} {
		if strings.Contains(string(eb), `"`+key+`"`) {
			t.Errorf("empty summary must omit %s", key)
		}
	}
}
