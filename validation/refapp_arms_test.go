package validation

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/validation/refapparms"
)

func envMap(m map[string]string) func(string) string { return func(k string) string { return m[k] } }

func foundTaskset(string) (string, error)  { return "/usr/bin/taskset", nil }
func absentTaskset(string) (string, error) { return "", errors.New("not found") }

// Defaults are today's behaviour: with the variable unset there is no
// prefix, so the refapp starts exactly as before.
func TestRefappCPUsPrefix(t *testing.T) {
	if p, err := refappCPUsPrefix(envMap(nil), foundTaskset); p != nil || err != nil {
		t.Fatalf("unset: prefix=%v err=%v, want nil, nil", p, err)
	}
	if p, err := refappCPUsPrefix(envMap(map[string]string{refapparms.CPUsEnv: "  "}), foundTaskset); p != nil || err != nil {
		t.Fatalf("blank: prefix=%v err=%v, want nil, nil", p, err)
	}
	p, err := refappCPUsPrefix(envMap(map[string]string{refapparms.CPUsEnv: " 2-5 "}), foundTaskset)
	if err != nil || strings.Join(p, " ") != "/usr/bin/taskset -c 2-5" {
		t.Fatalf("2-5: prefix=%v err=%v, want taskset -c 2-5", p, err)
	}
	// Negative controls: none of these may start an unconfined cell that is
	// then read as arm A.
	for name, tc := range map[string]struct {
		env  string
		look func(string) (string, error)
	}{
		"one cpu":      {"3", foundTaskset},
		"malformed":    {"2-", foundTaskset},
		"injection":    {"2,3;id", foundTaskset},
		"no taskset":   {"2-5", absentTaskset},
		"descending":   {"5-2", foundTaskset},
		"out of range": {"0-99999", foundTaskset},
	} {
		if p, err := refappCPUsPrefix(envMap(map[string]string{refapparms.CPUsEnv: tc.env}), tc.look); err == nil || p != nil {
			t.Errorf("%s (%q): prefix=%v err=%v, want an error and no prefix", name, tc.env, p, err)
		}
	}
}

func TestArmsActive(t *testing.T) {
	if armsActive(envMap(nil)) {
		t.Error("a routine run reports an arm active: it would write a new file per cell")
	}
	for _, k := range []string{refapparms.CPUsEnv, refapparms.WorkloadHintEnv, stallCaptureEnv} {
		v := "1"
		if k == refapparms.CPUsEnv {
			v = "2-3"
		}
		if !armsActive(envMap(map[string]string{k: v})) {
			t.Errorf("%s set but no arm active", k)
		}
	}
}

// The readback names the arm and carries the refapp's thread table, which is
// how the analysis checks that an input reached the refapp.
func TestWriteRefappReadback(t *testing.T) {
	dir := t.TempDir()
	env := envMap(map[string]string{refapparms.CPUsEnv: "2-3"})
	if err := writeRefappReadback(context.Background(), dir, env, procFixture(t), 100, ""); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(dir, refappReadbackFile))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("refapp_sched.txt:\n%s", b)
	for _, want := range []string{"ARM refapp_cpus=2-3 workload_hint=none ", "REFAPP_THREADS n=5 leader_cpus_allowed_list=0", "VALIDATOR_CPUS "} {
		if !strings.Contains(string(b), want) {
			t.Errorf("readback lacks %q:\n%s", want, b)
		}
	}
	dir2 := t.TempDir()
	if err := writeRefappReadback(context.Background(), dir2, envMap(nil), t.TempDir(), 0, ""); err != nil {
		t.Fatal(err)
	}
	b, _ = os.ReadFile(filepath.Join(dir2, refappReadbackFile))
	if !strings.Contains(string(b), "ARM refapp_cpus=none workload_hint=none ") {
		t.Errorf("an unarmed cell must say so:\n%s", b)
	}
}

// Arm A applies on the local driver only: the ssh driver launches elsewhere
// and a loopback taskset here would confine nothing.
func TestNewDriverLocalHonoursArmA(t *testing.T) {
	t.Setenv(refapparms.CPUsEnv, "3")
	o := newTestOrch(t, "local")
	if _, err := o.newDriver(); err == nil {
		t.Fatal("a one-CPU list built a driver")
	}
	t.Setenv(refapparms.CPUsEnv, "")
	if _, err := o.newDriver(); err != nil {
		t.Fatalf("unset: %v", err)
	}
	t.Setenv(refapparms.CPUsEnv, "2-3")
	ssh := newTestOrch(t, "ssh")
	if _, err := ssh.newDriver(); err == nil || !strings.Contains(err.Error(), "ssh driver") {
		t.Fatalf("ssh driver with arm A: err=%v, want a refusal naming the ssh driver", err)
	}
}
