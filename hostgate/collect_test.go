package hostgate

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

// These run the REAL collector on the test machine's /proc, so the line
// format collect.sh prints and the one Parse reads cannot drift apart. They
// need Linux, bash and ss; elsewhere they skip (the parser tests above carry
// the logic).

func runCollector(t *testing.T, env ...string) string {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("collect.sh reads /proc")
	}
	for _, tool := range []string{"bash", "ss"} {
		if _, err := exec.LookPath(tool); err != nil {
			t.Skipf("%s not installed", tool)
		}
	}
	cmd := exec.Command("bash", "collect.sh")
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("collect.sh: %v\n%s", err, out)
	}
	return string(out)
}

func TestCollectorOutputParses(t *testing.T) {
	out := runCollector(t, "HOSTGATE_MIN_PROC_AGE=0")
	s, err := Parse(strings.NewReader(out))
	if err != nil {
		t.Fatalf("Parse(collect.sh output): %v\n%s", err, out)
	}
	if s.Host == "" || s.Epoch < 1_700_000_000 {
		t.Errorf("host/epoch = %q/%d", s.Host, s.Epoch)
	}
	var self *Proc
	for i := range s.Procs {
		if s.Procs[i].PID == os.Getpid() {
			self = &s.Procs[i]
		}
	}
	if self == nil {
		t.Fatalf("the test process (pid %d) is not in the collector's process list:\n%s", os.Getpid(), out)
	}
	if self.Exe == "" || self.Cwd == "" || self.User == "" {
		t.Errorf("incomplete proc record: %+v", self)
	}
}

// A binary deleted from /tmp that outlives its run is rvtest's signature. Make
// one, run the real collector, and require the real rule to name it. The
// control is the same process BEFORE the binary is deleted.
func TestCollectorSeesADeletedBinaryUnderTmp(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	sleepBin, err := exec.LookPath("sleep")
	if err != nil {
		t.Skip("no sleep")
	}
	dir, err := os.MkdirTemp("/tmp", "hostgate-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	bin := filepath.Join(dir, "rvtest-like")
	data, err := os.ReadFile(sleepBin)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bin, data, 0o755); err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(bin, "300")
	cmd.Dir = dir
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cmd.Process.Kill(); _ = cmd.Wait() }()
	time.Sleep(100 * time.Millisecond)

	pol := DefaultPolicy()
	pol.MaxAge = 0 // the child is seconds old; the rule under test is not the age
	evalChild := func() []Violation {
		s, err := Parse(strings.NewReader(runCollector(t, "HOSTGATE_MIN_PROC_AGE=0")))
		if err != nil {
			t.Fatal(err)
		}
		var mine []Violation
		for _, v := range Evaluate(s, pol) {
			if v.Rule == RuleStaleProcess && strings.Contains(strings.Join(v.Lines, " "), "pid="+itoa(cmd.Process.Pid)+" ") {
				mine = append(mine, v)
			}
		}
		return mine
	}

	// Control: the binary still exists. /tmp/hostgate-test-*/ is not a harness
	// directory and `sleep` is not a harness name, so nothing may be reported.
	if got := evalChild(); len(got) != 0 {
		t.Fatalf("a live binary in an unrelated /tmp directory was reported: %v", got)
	}
	if err := os.Remove(bin); err != nil {
		t.Fatal(err)
	}
	got := evalChild()
	if len(got) != 1 || !strings.Contains(got[0].Lines[0], "binary deleted from /tmp") || !strings.Contains(got[0].Lines[0], "(deleted)") {
		t.Fatalf("a binary deleted from /tmp must be reported once with its reason: %v", got)
	}
}

func TestCollectorSeesAGuardedListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip(err)
	}
	defer func() { _ = ln.Close() }()
	port := ln.Addr().(*net.TCPAddr).Port
	s, err := Parse(strings.NewReader(runCollector(t)))
	if err != nil {
		t.Fatal(err)
	}
	pol := DefaultPolicy()
	// Control: the port is not guarded, so the same listener is fine.
	for _, v := range Evaluate(s, pol) {
		if v.Rule == RuleListener {
			t.Fatalf("an unguarded listener was reported: %v", v)
		}
	}
	pol.GuardedPorts = append(pol.GuardedPorts, port)
	var hit int
	for _, v := range Evaluate(s, pol) {
		if v.Rule == RuleListener && strings.Contains(v.Lines[0], "pid="+itoa(os.Getpid())) {
			hit++
		}
	}
	if hit != 1 {
		t.Errorf("a listener on a guarded port must be reported once, naming this pid; got %d", hit)
	}
}

// A supervisor is `bash -c <script>` and the script embeds the fixture DSN. The
// collector must never copy it.
func TestCollectorDoesNotCopyShellScriptsOrURLCredentials(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("needs /proc")
	}
	secret := "hunter2-bench-password"
	sh := exec.Command("bash", "-c", "sleep 300 # postgres://bench:"+secret+"@127.0.0.1:54321/bench")
	if err := sh.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sh.Process.Kill(); _ = sh.Wait() }()
	py := exec.Command("bash", "-c", `exec -a "adapter postgres://bench:`+secret+`@127.0.0.1:54321/bench" sleep 300`)
	if err := py.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { _ = py.Process.Kill(); _ = py.Wait() }()
	time.Sleep(100 * time.Millisecond)
	out := runCollector(t, "HOSTGATE_MIN_PROC_AGE=0")
	if strings.Contains(out, secret) {
		t.Errorf("the collector copied a credential:\n%s", regexp.MustCompile(`(?m)^.*`+secret+`.*$`).FindString(out))
	}
	if !strings.Contains(out, "\t"+itoa(py.Process.Pid)+"\t") || !strings.Contains(out, "***@") {
		t.Errorf("the credential-bearing process should still be listed, with the password masked")
	}
}

func itoa(n int) string { return strconv.Itoa(n) }
