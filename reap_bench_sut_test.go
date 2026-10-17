package main

import (
	"bytes"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"
)

const reapScript = "ops/sut/reap-bench-sut"

// The cancelled bench 37226284351 left its SUT supervisor and SUT alive on
// msa2-server with the pid file and the whole bench directory gone, so the
// generic stop task's pid-file handle was useless. ops/sut/reap-bench-sut finds
// such processes by IDENTITY that survives the directory: the run tag the
// launch puts in the SUT's environment (PROBATORIUM_SUT_RUN), or a working
// directory / binary under bench_root, deleted or not.
//
// These tests start real processes and run the real script against them. They
// need Linux (/proc).

func needLinux(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("reap-bench-sut reads /proc")
	}
}

type fake struct {
	cmd  *exec.Cmd
	done chan struct{}
}

// spawn starts argv with extra env and dir, and reaps it when it dies so a
// killed fake is gone rather than a zombie.
func spawn(t *testing.T, dir string, env []string, argv ...string) *fake {
	t.Helper()
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), env...)
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v: %v", argv, err)
	}
	f := &fake{cmd: cmd, done: make(chan struct{})}
	go func() { _ = cmd.Wait(); close(f.done) }()
	t.Cleanup(func() { _ = cmd.Process.Kill(); <-f.done })
	return f
}

func (f *fake) pid() int { return f.cmd.Process.Pid }

func (f *fake) dead(within time.Duration) bool {
	select {
	case <-f.done:
		return true
	case <-time.After(within):
		return false
	}
}

func pidAlive(pid int) bool {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndex(s, ") ")
	return i >= 0 && len(s) > i+2 && s[i+2] != 'Z'
}

func reap(t *testing.T, benchRoot string, mode string) (string, int) {
	return reapMatch(t, benchRoot, mode, "any")
}

func reapMatch(t *testing.T, benchRoot, mode, match string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", reapScript)
	cmd.Env = append(os.Environ(), "BENCH_ROOT="+benchRoot, "REAP_MODE="+mode, "REAP_MATCH="+match,
		"PROBATORIUM_SUT_RUN=") // must not make the script match itself
	cmd.Env = removeEnv(cmd.Env, "PROBATORIUM_SUT_RUN")
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	err := cmd.Run()
	rc := 0
	if ee, ok := err.(*exec.ExitError); ok {
		rc = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("run %s: %v", reapScript, err)
	}
	return out.String(), rc
}

func removeEnv(env []string, key string) []string {
	var out []string
	for _, e := range env {
		if !strings.HasPrefix(e, key+"=") {
			out = append(out, e)
		}
	}
	return out
}

// An adapter whose argv matches nothing the old sweep looks for ("competitors/",
// uvicorn, the bench port) is found by its run tag alone.
func TestReapFindsATaggedSUTWithNoPidFileNoDirAndAnUnrelatedArgv(t *testing.T) {
	needLinux(t)
	root := filepath.Join(t.TempDir(), "celeris-bench") // never created: it is already gone
	sut := spawn(t, t.TempDir(), []string{"PROBATORIUM_SUT_RUN=37226284351-1"},
		"bash", "-c", `exec -a node-adapter sleep 300`)
	time.Sleep(100 * time.Millisecond)
	if !pidAlive(sut.pid()) {
		t.Fatal("fixture did not start")
	}
	out, rc := reap(t, root, "reap")
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	if !sut.dead(3 * time.Second) {
		t.Fatalf("the tagged SUT survived the reap\n%s", out)
	}
	if !strings.Contains(out, "pid="+strconv.Itoa(sut.pid())) || !strings.Contains(out, "run=37226284351-1") {
		t.Errorf("the reap must say what it stopped, with the run tag:\n%s", out)
	}
}

// The 10-06 shape with no tag at all (a SUT launched by the previous harness
// version): cwd in the deleted bench directory.
func TestReapFindsAnUntaggedProcessWhoseCwdIsTheDeletedBenchRoot(t *testing.T) {
	needLinux(t)
	root, err := os.MkdirTemp("/tmp", "celeris-bench-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	sup := spawn(t, root, nil, "bash", "-c", `sleep 300 & wait`)
	time.Sleep(100 * time.Millisecond)
	if err := os.RemoveAll(root); err != nil { // pid file and directory gone
		t.Fatal(err)
	}
	out, rc := reap(t, root, "reap")
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	if !sup.dead(3*time.Second) || !strings.Contains(out, "cwd") {
		t.Fatalf("supervisor in a deleted bench root survived, or the reason was not reported:\n%s", out)
	}
}

// A respawn supervisor must not get a chance to relaunch the SUT between the
// two kills.
func TestReapStopsASupervisorWithoutLettingItRespawn(t *testing.T) {
	needLinux(t)
	root := filepath.Join(t.TempDir(), "celeris-bench")
	sup := spawn(t, t.TempDir(), []string{"PROBATORIUM_SUT_RUN=1-1"},
		"bash", "-c", `n=0; while [ $n -lt 50 ]; do sleep 300; n=$((n+1)); done`)
	time.Sleep(150 * time.Millisecond)
	out, rc := reap(t, root, "reap")
	if rc != 0 || !sup.dead(3*time.Second) {
		t.Fatalf("rc=%d dead=%v\n%s", rc, sup.dead(0), out)
	}
	time.Sleep(1200 * time.Millisecond) // a respawn would show up by now
	b, _ := exec.Command("bash", "-c", `grep -la 'PROBATORIUM_SUT_RUN=1-1' /proc/[0-9]*/environ 2>/dev/null | while read f; do p=${f#/proc/}; p=${p%/environ}; s=$(cut -d' ' -f3 /proc/$p/stat 2>/dev/null); [ "$s" != Z ] && echo $p; done`).Output()
	if strings.TrimSpace(string(b)) != "" {
		t.Errorf("tagged processes still alive after the reap: %s", b)
	}
}

// Precision, the negative control: the old sweep killed anything whose argv
// contained "competitors/". The reap must leave an innocent process that merely
// says so, and one with an unrelated cwd, alone.
func TestReapLeavesInnocentProcessesAlone(t *testing.T) {
	needLinux(t)
	root, err := os.MkdirTemp("/tmp", "celeris-bench-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// A sibling directory sharing the root's name as a PREFIX is not inside it.
	sibling := root + "-other"
	if err := os.MkdirAll(sibling, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(sibling) })

	says := spawn(t, t.TempDir(), nil, "bash", "-c", `: ./competitors/gin; sleep 300`)
	plain := spawn(t, t.TempDir(), []string{"PROBATORIUM_SOMETHING_ELSE=1"}, "sleep", "300")
	near := spawn(t, sibling, nil, "sleep", "300")
	// The msr1 rvtest shape: cwd /tmp, binary /tmp/<name>. It is under /tmp,
	// not under bench_root, and carries no tag; the reap must not take it (it is
	// the gate's to report, and a person's to stop).
	rvtestBin := filepath.Join(t.TempDir(), "rvtest")
	if data, err := os.ReadFile("/bin/sleep"); err == nil {
		_ = os.WriteFile(rvtestBin, data, 0o755)
	}
	rvLike := spawn(t, "/tmp", nil, rvtestBin, "300")
	lookalike := spawn(t, t.TempDir(), []string{"X=PROBATORIUM_SUT_RUN=1"}, "sleep", "300") // value, not a variable
	time.Sleep(150 * time.Millisecond)

	out, rc := reap(t, root, "reap")
	if rc != 0 {
		t.Fatalf("rc=%d\n%s", rc, out)
	}
	for name, f := range map[string]*fake{"argv says competitors/": says, "other env var": plain, "sibling dir with the same prefix": near, "rvtest shape (cwd /tmp, binary in /tmp)": rvLike, "tag text inside another variable": lookalike} {
		if f.dead(300 * time.Millisecond) {
			t.Errorf("the reap killed an innocent process (%s)\n%s", name, out)
		}
	}
	if !strings.Contains(out, "stopped 0") {
		t.Errorf("nothing matched, so it must say it stopped 0:\n%s", out)
	}
}

// The per-cell stop asks for tag-only matching: it runs mid-bench, where a
// helper that merely sits in bench_root must survive.
func TestReapTagOnlyModeLeavesAProcessThatOnlySitsInBenchRoot(t *testing.T) {
	needLinux(t)
	root, err := os.MkdirTemp("/tmp", "celeris-bench-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	helper := spawn(t, root, nil, "sleep", "300") // cwd in bench_root, no tag
	sut := spawn(t, t.TempDir(), []string{"PROBATORIUM_SUT_RUN=5-1"}, "sleep", "300")
	time.Sleep(150 * time.Millisecond)
	out, rc := reapMatch(t, root, "reap", "tag")
	if rc != 0 || !sut.dead(3*time.Second) {
		t.Fatalf("tag mode must stop the tagged SUT (rc=%d):\n%s", rc, out)
	}
	if helper.dead(300 * time.Millisecond) {
		t.Fatalf("tag mode killed a process that only sits in bench_root:\n%s", out)
	}
	// Control: the default mode takes it.
	if out, _ := reap(t, root, "reap"); !helper.dead(3 * time.Second) {
		t.Fatalf("default mode must take a process in bench_root:\n%s", out)
	}
	if _, rc := reapMatch(t, root, "reap", "nonsense"); rc != 2 {
		t.Errorf("a bad REAP_MATCH must be refused, rc=%d", rc)
	}
}

// list mode is how the identity can be inspected without touching anything.
func TestReapListModeReportsAndStopsNothing(t *testing.T) {
	needLinux(t)
	root := filepath.Join(t.TempDir(), "celeris-bench")
	sut := spawn(t, t.TempDir(), []string{"PROBATORIUM_SUT_RUN=9-9"}, "sleep", "300")
	time.Sleep(100 * time.Millisecond)
	out, rc := reap(t, root, "list")
	if rc != 0 || !strings.Contains(out, "pid="+strconv.Itoa(sut.pid())) {
		t.Fatalf("list must report the process (rc=%d):\n%s", rc, out)
	}
	if sut.dead(300 * time.Millisecond) {
		t.Fatal("list mode killed the process")
	}
}

// The script must never match its own caller chain: a test binary (or an
// ansible session) launched from inside the bench root is an ancestor.
func TestReapSparesItsOwnAncestors(t *testing.T) {
	needLinux(t)
	root, err := os.MkdirTemp("/tmp", "celeris-bench-test-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	// parent bash lives in the bench root and runs the reap as its child.
	cmd := exec.Command("bash", "-c", `cd "$ROOT" && bash `+filepath.Join(mustGetwd(t), reapScript)+`; echo parent-survived`)
	cmd.Env = append(removeEnv(os.Environ(), "PROBATORIUM_SUT_RUN"), "ROOT="+root, "BENCH_ROOT="+root, "REAP_MODE=reap")
	out, err := cmd.CombinedOutput()
	if err != nil || !strings.Contains(string(out), "parent-survived") {
		t.Fatalf("the reap killed the shell that invoked it: %v\n%s", err, out)
	}
}

func mustGetwd(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	return wd
}

// The playbooks that carry the reap are the part a Go-only run exercises not at
// all (RULE from 2026-09-27: a Go-only dry run exercises none of them). When
// ansible is installed, drive the real stop-sut.yml over a local connection
// against a real tagged process: this proves the `script` module finds the
// script, passes BENCH_ROOT, and that the task wiring stops the process, and
// that a clean host reports nothing. cleanup.yml is deliberately NOT driven
// from a Go test: it kills every docker container on the machine it runs on.
func ansibleLocal(t *testing.T, playbook string, extra ...string) string {
	t.Helper()
	needLinux(t)
	if _, err := exec.LookPath("ansible-playbook"); err != nil {
		t.Skip("ansible-playbook not installed")
	}
	inv := filepath.Join(t.TempDir(), "inventory.ini")
	if err := os.WriteFile(inv, []byte("[cluster]\nlocalhost ansible_connection=local\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	args := append([]string{"-i", inv, playbook, "-e", "ansible_become=false", "-e", "reap_sut_strict=true"}, extra...)
	cmd := exec.Command("ansible-playbook", args...)
	cmd.Env = append(removeEnv(os.Environ(), "PROBATORIUM_SUT_RUN"), "ANSIBLE_LOCALHOST_WARNING=False", "ANSIBLE_INVENTORY_UNPARSED_WARNING=False", "ANSIBLE_HOME="+t.TempDir())
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("ansible-playbook %s: %v\n%s", playbook, err, out)
	}
	return string(out)
}

func TestStopSutPlaybookReapsATaggedSUTOverALocalConnection(t *testing.T) {
	root := filepath.Join(t.TempDir(), "celeris-bench")
	sut := spawn(t, t.TempDir(), []string{"PROBATORIUM_SUT_RUN=37226284351-1"}, "bash", "-c", `exec -a node-adapter sleep 300`)
	time.Sleep(100 * time.Millisecond)
	out := ansibleLocal(t, "ansible/stop-sut.yml", "-e", "bench_root="+root)
	if !sut.dead(3 * time.Second) {
		t.Fatalf("stop-sut.yml left the tagged SUT alive:\n%s", out)
	}
	if !strings.Contains(out, "run=37226284351-1") {
		t.Errorf("the play must show what it stopped:\n%s", out)
	}
	// Control: the same play over a host with nothing to stop is clean.
	if out := ansibleLocal(t, "ansible/stop-sut.yml", "-e", "bench_root="+root); !strings.Contains(out, "failed=0") {
		t.Errorf("a clean host failed the play:\n%s", out)
	}
}
