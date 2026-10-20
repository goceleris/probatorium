package validation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// The two-instant capture is armed only in a stall-capture run: in a routine
// run the hook is nil, so fireWSLargeEcho is exactly what it was.
func TestWSEchoDiagHookIsNilOutsideAStallCaptureRun(t *testing.T) {
	ch := make(chan Incident, 1)
	pid := func() int { return 7 }
	if h := wsEchoDiagHook(func(string) string { return "" }, ch, pid, nil, "/proc"); h != nil {
		t.Fatal("routine run: the ws_echo diagnosis is armed")
	}
	for name, env := range map[string]map[string]string{
		"fault run": {refappFaultEnv: "/ws:8s@30s"},
		"knob":      {stallCaptureEnv: "1"},
	} {
		if h := wsEchoDiagHook(func(k string) string { return env[k] }, ch, pid, nil, "/proc"); h == nil {
			t.Fatalf("%s: the ws_echo diagnosis is not armed", name)
		}
	}
}

// The record reaches the orchestrator as a record-only I-WS-ECHO-STALL
// incident that never pauses the refapp (no gcore) and carries the verdict in
// its message, so incident.json alone names the class.
func TestWSEchoDiagDeliverSendsARecordOnlyIncident(t *testing.T) {
	ch := make(chan Incident, 1)
	h := wsEchoDiagHook(func(k string) string { return map[string]string{stallCaptureEnv: "1"}[k] }, ch, func() int { return 42 }, nil, "/proc")
	rep := wsEchoDiagReport{Verdict: wsEchoVerdict{Class: verdictWedge, Line: "WS_ECHO_VERDICT=WEDGE_CLASS reason=x"}, Text: "WS_ECHO_VERDICT=WEDGE_CLASS reason=x\n### t0\n"}
	h.Deliver(context.Background(), rep)
	inc := <-ch
	if inc.PredicateID != "I-WS-ECHO-STALL" || !inc.RecordOnly || !inc.SkipCore || inc.RefappPID != 42 {
		t.Errorf("incident %+v, want a record-only, no-core I-WS-ECHO-STALL for pid 42", inc)
	}
	if !strings.HasPrefix(inc.Message, "WS_ECHO_VERDICT=WEDGE_CLASS ") {
		t.Errorf("message %q does not carry the verdict line", inc.Message)
	}
	if inc.Attachments["ws-echo-diag.txt"] != rep.Text {
		t.Errorf("attachment ws-echo-diag.txt = %q, want the whole record", inc.Attachments["ws-echo-diag.txt"])
	}
}

// A full incident slot makes the diagnosed fire wait for it rather than drop
// the record, and a cancelled run releases it.
func TestWSEchoDiagDeliverWaitsForTheSlotAndHonoursCancel(t *testing.T) {
	ch := make(chan Incident, 1)
	ch <- Incident{PredicateID: "busy"}
	h := wsEchoDiagHook(func(k string) string { return map[string]string{stallCaptureEnv: "1"}[k] }, ch, func() int { return 1 }, nil, "/proc")

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { h.Deliver(ctx, wsEchoDiagReport{}); close(done) }()
	select {
	case <-done:
		t.Fatal("Deliver returned with the slot full: the record was dropped")
	case <-time.After(150 * time.Millisecond):
	}
	<-ch // the orchestrator takes the busy incident; the waiting sender is served
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Deliver did not return once the slot freed")
	}
	if inc := <-ch; inc.PredicateID != "I-WS-ECHO-STALL" {
		t.Errorf("slot holds %q, want the delivered record", inc.PredicateID)
	}

	ch <- Incident{PredicateID: "busy"}
	done = make(chan struct{})
	go func() { h.Deliver(ctx, wsEchoDiagReport{}); close(done) }()
	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a cancelled run must release a Deliver waiting on a full slot")
	}
}

// The dossier writer puts the attachment beside incident.json, and only as a
// bare file name.
func TestWriteIncidentDossierWritesAttachments(t *testing.T) {
	o := newTestOrch(t, "local")
	dir, err := o.writeIncidentDossier(Incident{
		PredicateID: "I-WS-ECHO-STALL", Message: "m", ObservedAt: time.Now(),
		Attachments: map[string]string{
			"ws-echo-diag.txt": "WS_ECHO_VERDICT=SLOW_CLASS\n",
			"../escape.txt":    "x",
			"sub/dir.txt":      "x",
			"":                 "x",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, "ws-echo-diag.txt"))
	if err != nil || string(b) != "WS_ECHO_VERDICT=SLOW_CLASS\n" {
		t.Fatalf("attachment not written: %q %v", b, err)
	}
	for _, bad := range []string{filepath.Join(dir, "..", "escape.txt"), filepath.Join(dir, "sub")} {
		if _, err := os.Stat(bad); err == nil {
			t.Errorf("%s exists: a path-shaped attachment name was honoured", bad)
		}
	}
}
