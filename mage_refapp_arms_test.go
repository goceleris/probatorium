//go:build mage

package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func armEnv(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

// Defaults are today's behaviour: nothing set adds nothing to the playbook argv.
func TestRefappArmExtraVars(t *testing.T) {
	if a, err := refappArmExtraVars(armEnv(nil)); a != nil || err != nil {
		t.Fatalf("unset: %v, %v; want nil, nil", a, err)
	}
	if a, err := refappArmExtraVars(armEnv(map[string]string{"PROBATORIUM_REFAPP_CPUS": " ", "PROBATORIUM_REFAPP_WORKLOAD_HINT": ""})); a != nil || err != nil {
		t.Fatalf("blank: %v, %v; want nil, nil", a, err)
	}
	a, err := refappArmExtraVars(armEnv(map[string]string{"PROBATORIUM_REFAPP_CPUS": "2-5"}))
	if err != nil || len(a) != 2 || a[0] != "--extra-vars" || a[1] != `{"probatorium_refapp_cpus":"2-5"}` {
		t.Fatalf("cpus: %v, %v", a, err)
	}
	a, err = refappArmExtraVars(armEnv(map[string]string{"PROBATORIUM_REFAPP_WORKLOAD_HINT": "high"}))
	if err != nil || len(a) != 2 || a[1] != `{"probatorium_refapp_workload_hint":"high"}` {
		t.Fatalf("hint: %v, %v", a, err)
	}
	// Negative controls: a value the validator would refuse never reaches a host.
	for name, env := range map[string]map[string]string{
		"one cpu":      {"PROBATORIUM_REFAPP_CPUS": "3"},
		"malformed":    {"PROBATORIUM_REFAPP_CPUS": "2;id"},
		"bad hint":     {"PROBATORIUM_REFAPP_WORKLOAD_HINT": "iouring"},
		"good + bad":   {"PROBATORIUM_REFAPP_CPUS": "2-3", "PROBATORIUM_REFAPP_WORKLOAD_HINT": "x"},
		"descending":   {"PROBATORIUM_REFAPP_CPUS": "5-2"},
		"shell-ish":    {"PROBATORIUM_REFAPP_CPUS": "$(id),1"},
		"out of range": {"PROBATORIUM_REFAPP_CPUS": "0-99999"},
	} {
		if a, err := refappArmExtraVars(armEnv(env)); err == nil || a != nil {
			t.Errorf("%s: %v, %v; want an error and no extra-vars", name, a, err)
		}
	}
}

// runValidatePlaybook's real argv: the arms reach msa2-server only, so msr1
// (arm64, where this cell has never timed out) runs as today and is the
// control; and with the arms unset neither target carries them.
func TestValidatePlaybookArgvCarriesTheArmsToMsa2ServerOnly(t *testing.T) {
	run := func(t *testing.T, env map[string]string, target string) string {
		t.Helper()
		dir := t.TempDir()
		if err := os.MkdirAll(filepath.Join(dir, "ansible"), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
			t.Fatal(err)
		}
		argv := filepath.Join(dir, "argv.txt")
		script := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\"; done >> '" + argv + "'\n"
		if err := os.WriteFile(filepath.Join(dir, "bin", "ansible-playbook"), []byte(script), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", filepath.Join(dir, "bin")+string(os.PathListSeparator)+os.Getenv("PATH"))
		t.Setenv("PROBATORIUM_STALL_CAPTURE", "")
		t.Setenv("PROBATORIUM_REFAPP_FAULT", "")
		t.Setenv("PROBATORIUM_VALIDATE_DRIVER", "")
		t.Setenv("VALIDATE_PARALLEL", "")
		t.Setenv("PROBATORIUM_REFAPP_CPUS", env["PROBATORIUM_REFAPP_CPUS"])
		t.Setenv("PROBATORIUM_REFAPP_WORKLOAD_HINT", env["PROBATORIUM_REFAPP_WORKLOAD_HINT"])
		t.Chdir(dir)
		if err := runValidatePlaybook("1m", target, "v0.0.0-test", false); err != nil {
			t.Fatalf("runValidatePlaybook(%s): %v", target, err)
		}
		b, err := os.ReadFile(argv)
		if err != nil {
			t.Fatalf("the stand-in ansible-playbook never ran: %v", err)
		}
		return string(b)
	}
	armed := map[string]string{"PROBATORIUM_REFAPP_CPUS": "2-5", "PROBATORIUM_REFAPP_WORKLOAD_HINT": "high"}
	if got := run(t, armed, "msa2-server"); !strings.Contains(got, `{"probatorium_refapp_cpus":"2-5","probatorium_refapp_workload_hint":"high"}`) {
		t.Errorf("msa2-server argv lacks the arms:\n%s", got)
	}
	if got := run(t, armed, "msr1"); strings.Contains(got, "probatorium_refapp_") {
		t.Errorf("msr1 argv carries an arm (it is the control):\n%s", got)
	}
	if got := run(t, nil, "msa2-server"); strings.Contains(got, "probatorium_refapp_cpus") || strings.Contains(got, "probatorium_refapp_workload_hint") {
		t.Errorf("unset arms still reached the argv:\n%s", got)
	}
	// A bad value stops the run before any playbook runs.
	t.Setenv("PROBATORIUM_REFAPP_CPUS", "1")
	if err := runValidatePlaybook("1m", "msa2-server", "v0.0.0-test", false); err == nil || !strings.Contains(err.Error(), "at least 2") {
		t.Errorf("a one-CPU list: err=%v, want a refusal", err)
	}
}
