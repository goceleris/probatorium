package main

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/report"
)

// The standing NIC interrupt placement for msr1 (goceleris/celeris#1036): the enp1s0 TX-completion vector is
// pinned to CPU 1 before every bench run, verified by readback, and recorded. These tests pin the pieces CI can
// see without a NIC: the shell logic against a fake /proc and /sys, the shape of the record it prints, and the
// wiring that makes the placement msr1-only and opt-out.

func needBash(t *testing.T) string {
	t.Helper()
	bash, err := exec.LookPath("bash")
	if err != nil {
		t.Skip("bash not installed")
	}
	return bash
}

// TestNICIRQPinScript runs ansible/tests/test_nic_irq_pin.sh: pin applied, readback mismatch fails, idempotent
// second run, refusals (unknown vector, CPU list, missing effective_affinity_list), verify mode never writes.
func TestNICIRQPinScript(t *testing.T) {
	bash := needBash(t)
	out, err := exec.Command(bash, filepath.Join("ansible", "tests", "test_nic_irq_pin.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_nic_irq_pin.sh failed: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte(" 0 failed")) {
		t.Fatalf("test_nic_irq_pin.sh reported failures:\n%s", out)
	}
}

// TestNICIRQPinScriptRecordMatchesReportSchema decodes what the script prints, strictly, into the type
// environment.nic_irq_placement is read as: a field the script adds without the Go side knowing fails here.
func TestNICIRQPinScriptRecordMatchesReportSchema(t *testing.T) {
	bash := needBash(t)
	root := t.TempDir()
	mk := func(p, body string) {
		t.Helper()
		full := filepath.Join(root, p)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	mk("sys/class/net/enp1s0/device/msi_irqs/167", "")
	mk("sys/class/net/enp49s0/device/msi_irqs/169", "")
	mk("proc/interrupts", "  CPU0\n 167: 0 ITS-MSI 2 Edge      enp1s0-8\n 169: 0 ITS-MSI 4 Edge      enp49s0-8\n")
	mk("proc/irq/167/smp_affinity_list", "0-11\n")
	if err := os.Symlink("smp_affinity_list", filepath.Join(root, "proc/irq/167/effective_affinity_list")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	cmd := exec.Command(bash, filepath.Join("ansible", "files", "nic_irq_pin.sh"), "pin", "enp1s0", "enp1s0-8", "1")
	cmd.Env = append(os.Environ(), "NIC_IRQ_ROOT="+root)
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	if err := cmd.Run(); err != nil {
		t.Fatalf("pin failed: %v", err)
	}
	dec := json.NewDecoder(&stdout)
	dec.DisallowUnknownFields()
	var p report.NICIRQPlacement
	if err := dec.Decode(&p); err != nil {
		t.Fatalf("the script's record does not decode into report.NICIRQPlacement: %v", err)
	}
	if p.Interface != "enp1s0" || p.Vector != "enp1s0-8" || p.Binding != "msi_irqs" || p.CPU != 1 || !p.Changed || !p.Verified || len(p.IRQs) != 1 {
		t.Fatalf("record = %+v", p)
	}
	if v := p.IRQs[0]; v.IRQ != 167 || v.Before.Affinity != "0-11" || v.After.Affinity != "1" || v.After.Effective != "1" {
		t.Errorf("irq record = %+v, want 167 pinned 0-11 -> 1 with effective 1", v)
	}
	if got, _ := os.ReadFile(filepath.Join(root, "proc/irq/167/smp_affinity_list")); strings.TrimSpace(string(got)) != "1" {
		t.Errorf("smp_affinity_list = %q, want 1", got)
	}
}

// TestNICIRQPlacementPlay runs the real tasks files on a local connection against the fake tree
// (ansible/tests/test_nic_irq_placement_play.sh): record written, second run idempotent, mismatch fails the
// play, an empty or unset variable is a no-op. Opt-in with PROBATORIUM_ANSIBLE_TESTS=1.
func TestNICIRQPlacementPlay(t *testing.T) {
	bash := needBash(t)
	// Opt-in: the version of ansible on a developer box or a generic runner is not the one the cluster pins,
	// so only the ansible CI job (which installs the pinned one and runs this script directly) counts.
	if os.Getenv("PROBATORIUM_ANSIBLE_TESTS") != "1" {
		t.Skip("set PROBATORIUM_ANSIBLE_TESTS=1 to run the play test (needs ansible-playbook); the ansible CI job runs it")
	}
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Fatal("PROBATORIUM_ANSIBLE_TESTS=1 but ansible-playbook is not installed")
	}
	out, err := exec.Command(bash, filepath.Join("ansible", "tests", "test_nic_irq_placement_play.sh")).CombinedOutput()
	if err != nil {
		t.Fatalf("test_nic_irq_placement_play.sh failed: %v\n%s", err, out)
	}
}

// TestNICIRQPlacementWiring pins what makes the placement what the maintainer decided: msr1 only, enp1s0's
// vector to CPU 1, enp49s0 untouched, gated on a variable that empty (or unset) turns into a no-op, with the
// why and the issue link next to the variable, and the end-of-run readback before the artefacts are tarred.
func TestNICIRQPlacementWiring(t *testing.T) {
	read := func(p ...string) string {
		t.Helper()
		b, err := os.ReadFile(filepath.Join(p...))
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	inv := read("ansible", "inventory.yml")

	// The variable lives on the msr1 host entry and nowhere else.
	host := func(name string) string {
		re := regexp.MustCompile(`(?ms)^        ` + name + `:\n(.*?)(?:^        \S|^    \S|\z)`)
		m := re.FindStringSubmatch(inv)
		if m == nil {
			t.Fatalf("inventory.yml has no %s host entry", name)
		}
		return m[1]
	}
	msr1 := host("msr1")
	for _, want := range []string{
		"nic_irq_placement:",
		"iface: enp1s0",
		"vector: enp1s0-8",
		"cpu: 1",
		"goceleris/celeris#1036",
		"effective_affinity_list",
		"comparability",
	} {
		if !strings.Contains(msr1, want) {
			t.Errorf("the msr1 inventory entry lacks %q", want)
		}
	}
	if strings.Count(inv, "nic_irq_placement:") != 1 {
		t.Errorf("nic_irq_placement must be set on msr1 only, inventory.yml sets it %d times", strings.Count(inv, "nic_irq_placement:"))
	}
	// enp49s0 is named only in the comment that says it is NOT touched, never as a value.
	for _, line := range strings.Split(inv, "\n") {
		if strings.Contains(line, "enp49s0") && !strings.HasPrefix(strings.TrimSpace(line), "#") {
			t.Errorf("enp49s0 appears outside a comment in inventory.yml: %q", line)
		}
	}
	for _, h := range []string{"msa2-server", "msa2-client"} {
		if strings.Contains(host(h), "nic_irq_placement") {
			t.Errorf("%s must not carry nic_irq_placement", h)
		}
	}

	bench := read("ansible", "bench.yml")
	gate := regexp.MustCompile(`(?s)include_tasks: tasks/(nic_irq_placement\w*)\.yml\n\s+when:\n\s+- inventory_hostname == bench_target\n\s+- \(nic_irq_placement \| default\(\{\}\)\) \| length > 0`)
	got := map[string]bool{}
	for _, m := range gate.FindAllStringSubmatch(bench, -1) {
		got[m[1]] = true
	}
	for _, f := range []string{"nic_irq_placement", "nic_irq_placement_end"} {
		if !got[f] {
			t.Errorf("bench.yml does not include tasks/%s.yml gated on the bench target and a non-empty nic_irq_placement", f)
		}
	}
	// pin before the first cell (Stage play), end readback before the run dir is tarred (Collect play).
	pin := strings.Index(bench, "tasks/nic_irq_placement.yml")
	run := strings.Index(bench, "- name: Run interleaved bench")
	end := strings.Index(bench, "tasks/nic_irq_placement_end.yml")
	tar := strings.Index(bench, "- name: Tar bench artefacts on remote")
	if pin < 0 || run < 0 || end < 0 || tar < 0 || pin >= run || run >= end || end >= tar {
		t.Errorf("order in bench.yml: pin %d < run %d < end readback %d < tar %d does not hold", pin, run, end, tar)
	}

	tasks := read("ansible", "tasks", "nic_irq_placement.yml")
	for _, want := range []string{"nic_irq_pin.sh pin", "nic-irq-placement.json", "failed_when: nic_irq_pin.rc"} {
		if !strings.Contains(tasks, want) {
			t.Errorf("tasks/nic_irq_placement.yml lacks %q", want)
		}
	}
	// THROWAWAY ab/irqplace-v1: cleanup.yml carries the IRQPLACE cancel-path restore, which pins the EXPLICIT standing CPU 1 with a readback
	// (never a mask) and only while its armed marker exists. Its own names are not what this guard is about.
	if strings.Contains(strings.ReplaceAll(strings.ReplaceAll(read("ansible", "cleanup.yml"), "irqplace", ""), "IRQPLACE", ""), "irq") {
		t.Error("cleanup.yml must not restore the IRQ placement: it is the standing configuration, and a restored mask does not put the vector back")
	}
}
