//go:build mage

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// writeFaultRun writes a one-host run with an injected auth_session_ratelimit
// cell whose capture caught a /ws hold, and an un-injected kitchen_sink cell.
// good=false leaves the dossier's goroutine dump without the holder frame.
func writeFaultRun(t *testing.T, good bool) string {
	t.Helper()
	run := t.TempDir()
	host := filepath.Join(run, "msa2-server")
	t0 := time.Date(2026, 9, 26, 12, 0, 0, 0, time.UTC)
	ts := func(d time.Duration) string { return t0.Add(d).Format(time.RFC3339Nano) }
	tail := "[fault] hold path=/ws hold=8s start=" + ts(0) + "\n[fault] release path=/ws end=" + ts(8*time.Second) + " waiters=9\n"
	cell := filepath.Join(host, "cell-00-auth_session_ratelimit-iouring")
	d := filepath.Join(cell, "incidents", "20260926-120003-I-WS-HANDSHAKE")
	if err := os.MkdirAll(d, 0o755); err != nil {
		t.Fatal(err)
	}
	stacks := "goroutine 88 [sync.Mutex.Lock]:\ngithub.com/x/debugvars.(*FaultHold).wait(...)\n"
	if good {
		stacks += "goroutine 7 [sleep]:\ngithub.com/x/debugvars.(*FaultHold).run(...)\n"
	}
	inc, _ := json.Marshal(map[string]any{"observed_at": ts(3 * time.Second)})
	files := map[string]string{
		filepath.Join(cell, "refapp_stderr_tail.txt"): tail,
		filepath.Join(d, "refapp_stderr_tail.txt"):    tail,
		filepath.Join(d, "incident.json"):             string(inc),
		filepath.Join(d, "goroutine-stacks.txt"):      stacks,
		filepath.Join(d, "core.skipped"):              "x\n",
	}
	for p, b := range files {
		if err := os.WriteFile(p, []byte(b), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.MkdirAll(filepath.Join(host, "cell-01-kitchen_sink-iouring"), 0o755); err != nil {
		t.Fatal(err)
	}
	doc := report.Document{SchemaVersion: report.SchemaVersion, Validation: &report.ValidationResults{Cells: []report.ValidationCellResult{
		{Refapp: "auth_session_ratelimit", Engine: "iouring", Arch: "amd64", Tier1: &report.Tier1Summary{
			WSTorture: map[string]int64{"ws_handshake_fail": 3, "ws_handshake_fail_timeout": 3},
			WSSlowReads: []report.SlowFire{{TS: ts(2 * time.Second), ReadMs: 2000, Outcome: "handshake-fail-timeout",
				Err: "i/o timeout", LocalAddr: "127.0.0.1:40000", RemoteAddr: "127.0.0.1:8080"}},
		}},
		{Refapp: "kitchen_sink", Engine: "iouring", Arch: "amd64", Tier1: &report.Tier1Summary{H2CChurn: map[string]int64{"h2c_sent": 10}}},
	}}}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(host, "validate-results.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	return run
}

func TestValidateFaultControlJudgesARun(t *testing.T) {
	t.Setenv("VALIDATE_FAULT_CONTROL", "/ws:8s@30s")
	t.Setenv("VALIDATE_FAULT_CONTROL_EXPECT_CELLS", "1")
	t.Setenv("VALIDATE_FAULT_CONTROL_RESULTS", writeFaultRun(t, true))
	if err := ValidateFaultControl(); err != nil {
		t.Fatalf("good run: %v", err)
	}
	t.Setenv("VALIDATE_FAULT_CONTROL_RESULTS", writeFaultRun(t, false))
	if err := ValidateFaultControl(); err == nil || !strings.Contains(err.Error(), "1 cell(s) failed") {
		t.Fatalf("a dump without the holder must fail the control, got %v", err)
	}
	t.Setenv("VALIDATE_FAULT_CONTROL_RESULTS", writeFaultRun(t, true))
	t.Setenv("VALIDATE_FAULT_CONTROL_EXPECT_CELLS", "8")
	if err := ValidateFaultControl(); err == nil || !strings.Contains(err.Error(), "want 8") {
		t.Fatalf("a cell-count mismatch must fail, got %v", err)
	}
	// The no-fault control over the same artifact: the injected cell's /ws
	// events are now false positives, so it must fail; a clean artifact passes.
	t.Setenv("VALIDATE_FAULT_CONTROL_EXPECT_CELLS", "")
	t.Setenv("VALIDATE_FAULT_CONTROL_REFAPPS", "none")
	t.Setenv("VALIDATE_FAULT_CONTROL", "")
	if err := ValidateFaultControl(); err == nil {
		t.Fatal("no-fault mode over an artifact WITH handshake failures must fail")
	}
	clean := writeFaultRun(t, true)
	_ = os.RemoveAll(filepath.Join(clean, "msa2-server", "cell-00-auth_session_ratelimit-iouring"))
	doc := report.Document{SchemaVersion: report.SchemaVersion, Validation: &report.ValidationResults{Cells: []report.ValidationCellResult{
		{Refapp: "auth_session_ratelimit", Engine: "iouring", Arch: "amd64", Tier1: &report.Tier1Summary{
			WSTorture: map[string]int64{"ws_sent": 50}, H2CChurn: map[string]int64{"h2c_sent": 50}}}}}}
	b, _ := json.Marshal(doc)
	if err := os.WriteFile(filepath.Join(clean, "msa2-server", "validate-results.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("VALIDATE_FAULT_CONTROL_RESULTS", clean)
	if err := ValidateFaultControl(); err != nil {
		t.Fatalf("no-fault mode over a clean artifact: %v", err)
	}
	t.Setenv("VALIDATE_FAULT_CONTROL_REFAPPS", "")
	t.Setenv("VALIDATE_FAULT_CONTROL", "/login:8s@30s")
	if err := ValidateFaultControl(); err == nil || !strings.Contains(err.Error(), "no walker judges") {
		t.Fatalf("a path no walker exercises must be refused, got %v", err)
	}
}

func TestRefappFaultExtraVars(t *testing.T) {
	env := map[string]string{"PROBATORIUM_REFAPP_FAULT": " /ws:8s@30s,/:40s@60s "}
	got := refappFaultExtraVars(func(k string) string { return env[k] })
	if len(got) != 2 || got[0] != "--extra-vars" || got[1] != `{"probatorium_refapp_fault":"/ws:8s@30s,/:40s@60s"}` {
		t.Fatalf("got %q", got)
	}
	if got := refappFaultExtraVars(func(string) string { return "" }); got != nil {
		t.Fatalf("unset must add nothing, got %q", got)
	}
}
