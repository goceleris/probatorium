package main

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/hostgate"
)

// ansible/host-gate.yml has to be RUN, not just syntax-checked: it is the part
// that carries collect.sh's text (tabs, the `end ok` trailer) through the
// `script` and `copy` modules to a file, and that must write NOTHING for a
// host that did not answer. Drive the real playbook over a local connection
// (a host that answers) and over a refused ssh connection (one that does not),
// then run the real parser and rules over what landed. Needs Linux, ansible
// and ss; skips otherwise.
func TestHostGatePlaybookCarriesTheCollectorOutputAndWritesNothingForADeadHost(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("collect.sh reads /proc")
	}
	for _, tool := range []string{"ansible-playbook", "ss", "ssh"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port

	out := t.TempDir()
	inv := filepath.Join(t.TempDir(), "inventory.ini")
	// "dead" is ssh to a port nothing listens on: refused at once, so the test
	// does not wait for a timeout.
	body := "[cluster]\nhere ansible_connection=local\n" +
		"dead ansible_connection=ssh ansible_host=127.0.0.1 ansible_port=1 ansible_user=nobody " +
		"ansible_ssh_common_args='-o BatchMode=yes -o ConnectTimeout=3 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null'\n"
	if err := os.WriteFile(inv, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command("ansible-playbook", "-i", inv, "ansible/host-gate.yml",
		"-e", "ansible_become=false", "-e", "hostgate_local_dir="+out)
	cmd.Env = append(os.Environ(), "ANSIBLE_LOCALHOST_WARNING=False", "ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_HOME="+t.TempDir())
	if b, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("host-gate.yml must not fail because one host is dead (the gate does, afterwards): %v\n%s", err, b)
	}

	if _, err := os.Stat(filepath.Join(out, "dead.txt")); err == nil {
		t.Error("a host that never answered has a record; the gate would read it as a clean host")
	}
	raw, err := os.ReadFile(filepath.Join(out, "here.txt"))
	if err != nil {
		t.Fatalf("the reachable host has no record: %v", err)
	}
	if !strings.HasSuffix(string(raw), "end\tok\n") || !strings.Contains(string(raw), "\t") {
		t.Fatalf("the record lost its tabs or its end marker passing through ansible:\n%q", raw)
	}

	read := func(h string) ([]byte, error) { return os.ReadFile(filepath.Join(out, h+".txt")) }
	// Control: with the listener's port unguarded the live host is clean...
	pol := hostgate.DefaultPolicy()
	pol.MaxIOSomeAvg60 = 0 // the CI machine's own IO pressure is not under test
	pol.MaxAge = 1 << 62   // nor are its long-running processes
	res := hostgate.CheckAll([]string{"here", "dead"}, read, pol)
	if got := strings.Join(res.Hosts(), ","); got != "dead" {
		t.Fatalf("only the dead host may fail, got %q: %v", got, res.Violations)
	}
	if res.Violations[0].Rule != hostgate.RuleNoData {
		t.Errorf("the dead host must fail as no-data: %v", res.Violations)
	}
	// ...and guarding it reports THIS process, found through the playbook.
	pol.GuardedPorts = append(pol.GuardedPorts, port)
	res = hostgate.CheckAll([]string{"here"}, read, pol)
	var hit bool
	for _, v := range res.Violations {
		if v.Rule == hostgate.RuleListener && strings.Contains(strings.Join(v.Lines, " "), "pid="+strconv.Itoa(os.Getpid())) {
			hit = true
		}
	}
	if !hit {
		t.Errorf("a listener on a guarded port was not reported through the playbook: %v", res.Violations)
	}
}
