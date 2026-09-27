package main

import (
	"archive/zip"
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"
)

// The cluster host job's shell scripts, run for real on the CI runner with
// fixtures standing in for the host (a fake sysfs and procfs, fake go, gh
// and perf on PATH). The workflow never sets the knobs they use
// (STRESS_SYSFS, STRESS_PROCFS, STRESS_*_SECONDS, STRESS_GO_TMPDIR).

// linuxOnly skips where the scripts cannot run: they read /proc and use
// util-linux and coreutils (prlimit, taskset, setsid, sha256sum). CI runs
// these tests on ubuntu.
func linuxOnly(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("the cluster host scripts are Linux-only; CI runs this on ubuntu")
	}
}

func script(t *testing.T, name string) string {
	t.Helper()
	p, err := filepath.Abs(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(p); err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return p
}

// writeExec writes an executable file.
func writeExec(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func writeFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// bashEnv is the test's environment with env applied on top ("" unsets).
func bashEnv(env map[string]string) []string {
	var out []string
	for _, kv := range os.Environ() {
		k, _, _ := strings.Cut(kv, "=")
		if _, ok := env[k]; !ok {
			out = append(out, kv)
		}
	}
	for k, v := range env {
		if v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}

// runBash runs `bash -c code _ args...` in dir and returns its combined
// output and exit status.
func runBash(t *testing.T, dir string, env map[string]string, code string, args ...string) (string, int) {
	t.Helper()
	cmd := exec.Command("bash", append([]string{"-c", code, "_"}, args...)...)
	cmd.Dir, cmd.Env = dir, bashEnv(env)
	out, err := cmd.CombinedOutput()
	var ee *exec.ExitError
	switch {
	case err == nil:
		return string(out), 0
	case errors.As(err, &ee):
		return string(out), ee.ExitCode()
	}
	t.Fatalf("bash: %v\n%s", err, out)
	return "", -1
}

// withPath puts dir first on PATH.
func withPath(dir string) string { return dir + string(os.PathListSeparator) + os.Getenv("PATH") }

// makeWritable undoes Go's read-only module cache so t.TempDir can clean up.
func makeWritable(root string) {
	_ = filepath.WalkDir(root, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.IsDir() {
			_ = os.Chmod(p, 0o755)
		}
		return nil
	})
}

// fileProxy is a GOPROXY=file:// serving one module, so `go mod download`
// builds a real module cache with no network.
func fileProxy(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	v := filepath.Join(dir, "example.com", "rocache", "@v")
	mod := "module example.com/rocache\n\ngo 1.21\n"
	writeFile(t, filepath.Join(v, "list"), "v1.0.0\n")
	writeFile(t, filepath.Join(v, "v1.0.0.info"), `{"Version":"v1.0.0","Time":"2026-01-01T00:00:00Z"}`)
	writeFile(t, filepath.Join(v, "v1.0.0.mod"), mod)
	var zb bytes.Buffer
	zw := zip.NewWriter(&zb)
	for name, body := range map[string]string{"go.mod": mod, "rocache.go": "package rocache\n"} {
		w, err := zw.Create("example.com/rocache@v1.0.0/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	writeFile(t, filepath.Join(v, "v1.0.0.zip"), zb.String())
	return "file://" + dir
}

// runnerWipe is what teardown's "Wipe runner dir" and the next bootstrap's
// "Wipe stale runner dir" do: ansible's file: state=absent, as the runner's
// own user, which for a directory is Python's shutil.rmtree.
func runnerWipe(t *testing.T, root string) error {
	t.Helper()
	py, err := exec.LookPath("python3")
	if err != nil {
		t.Fatalf("python3 (what ansible's file module runs): %v", err)
	}
	out, err := exec.Command(py, "-c", "import shutil, sys; shutil.rmtree(sys.argv[1])", root).CombinedOutput()
	if err != nil {
		return fmt.Errorf("%v: %s", err, bytes.TrimSpace(out))
	}
	return nil
}

// The Go module cache is read-only unless -modcacherw. A host job's Go state
// lives under its runner dir, which teardown and the next bootstrap delete as
// the runner's user without chmod: a read-only module cache there fails both,
// and every later cluster bootstrap on that host with them.
func TestGoStateNeverLeavesAnUndeletableModuleCache(t *testing.T) {
	linuxOnly(t)
	if _, err := exec.LookPath("go"); err != nil {
		t.Fatalf("go: %v", err)
	}
	gostate := script(t, "gostate.sh")
	proxy := fileProxy(t)
	work := t.TempDir()
	t.Cleanup(func() { makeWritable(work) }) // runs before TempDir's removal
	consumer := filepath.Join(work, "consumer")
	writeFile(t, filepath.Join(consumer, "go.mod"), "module consumer\n\ngo 1.21\n")
	env := func(runnerTemp string) map[string]string {
		return map[string]string{"RUNNER_TEMP": runnerTemp, "GOPROXY": proxy, "GOSUMDB": "off", "GOFLAGS": "",
			"GOTOOLCHAIN": "local", "STRESS_GO_TMPDIR": filepath.Join(work, "gotmp-"+filepath.Base(filepath.Dir(runnerTemp)))}
	}
	const get = `cd "$2" && go mod download example.com/rocache@v1.0.0`

	// The fixture is faithful: Go without the host job's settings leaves a
	// module cache the runner-dir wipe cannot delete.
	oldRoot := filepath.Join(work, "old")
	oldTemp := filepath.Join(oldRoot, "_work", "_temp")
	if out, code := runBash(t, "", env(oldTemp), `set -e; export GOMODCACHE="$RUNNER_TEMP/go/mod"; mkdir -p "$GOMODCACHE"; `+get, "", consumer); code != 0 {
		t.Fatalf("go mod download: exit %d\n%s", code, out)
	}
	fi, err := os.Stat(filepath.Join(oldTemp, "go", "mod", "example.com", "rocache@v1.0.0"))
	if err != nil || fi.Mode().Perm()&0o200 != 0 {
		t.Fatalf("fixture: expected Go to leave a read-only module directory: %v %v", err, fi)
	}
	if runnerWipe(t, oldRoot) == nil {
		t.Fatal("fixture: the runner-dir wipe deleted a read-only module cache; this test would prove nothing")
	}

	// The host job's own Go state: nothing in it may stop the wipe, even
	// when no step of the job runs after go (a lost runner, a killed job).
	newRoot := filepath.Join(work, "new")
	newTemp := filepath.Join(newRoot, "_work", "_temp")
	if out, code := runBash(t, "", env(newTemp), `set -e; . "$1"; gostate_init; `+get, gostate, consumer); code != 0 {
		t.Fatalf("go mod download in the host job's Go state: exit %d\n%s", code, out)
	}
	if err := runnerWipe(t, newRoot); err != nil {
		t.Errorf("teardown's runner-dir wipe fails on the host job's Go state (a read-only module cache under RUNNER_TEMP): %v", err)
	}

	// The host job's always() step wipes a read-only cache that is already
	// there, so the runner-dir wipes never meet one.
	if out, code := runBash(t, "", env(oldTemp), `bash "$1" wipe`, gostate); code != 0 {
		t.Errorf("gostate.sh wipe: exit %d\n%s", code, out)
	}
	if _, err := os.Stat(filepath.Join(oldTemp, "go")); !errors.Is(err, fs.ErrNotExist) {
		t.Errorf("gostate.sh wipe left %s in place (%v)", filepath.Join(oldTemp, "go"), err)
	}
	if err := runnerWipe(t, oldRoot); err != nil {
		t.Errorf("after gostate.sh wipe, the runner-dir wipe still fails: %v", err)
	}
}

// t.TempDir lives under TMPDIR, and celeris tests put Unix sockets there
// (adaptive/prebound_listener_test.go: t.TempDir()+"/adaptive.sock"). A
// socket path has 107 usable bytes; on GitHub TMPDIR is /tmp. On the cluster
// it must not be long enough to push such a path over, or a test skips on one
// host only.
func TestGoStateKeepsTMPDIRShort(t *testing.T) {
	gostate := script(t, "gostate.sh")
	const tail = "/TestAdaptiveRejectsNonTCPListener4294967295/001/adaptive.sock"
	github := len("/tmp" + tail)
	for _, host := range []string{"msa2-server", "msr1"} {
		rt := "/tmp/actions-runner-" + host + "/_work/_temp"
		out, code := runBash(t, "", map[string]string{"RUNNER_TEMP": rt, "STRESS_GO_TMPDIR": ""}, `. "$1"; gostate_env; printf %s "$TMPDIR"`, gostate)
		if code != 0 {
			t.Fatalf("%s: exit %d\n%s", host, code, out)
		}
		sock := len(out + tail)
		if sock > 107 || sock-github > 8 {
			t.Errorf("%s: TMPDIR %q makes a t.TempDir socket path %d bytes (GitHub: %d; the limit is 107)", host, out, sock, github)
		}
	}
}

// alive reports whether pid is a live (not zombie) process.
func alive(pid int) bool {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return false
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	return i < 0 || i+2 >= len(s) || s[i+2] != 'Z'
}

// A runner GitHub gives up on can leave its job running on the host; the
// teardown then kills the listener and deletes the runner dir. Whatever the
// host job started must die with that dir, not run on into the next cluster
// run for up to its -timeout.
func TestHostJobKillsItsShardWhenTheRunnerDirGoes(t *testing.T) {
	linuxOnly(t)
	root := t.TempDir()
	tools := filepath.Join(root, "tools")
	for _, f := range []string{"cluster-host.sh", "gostate.sh"} {
		b, err := os.ReadFile(script(t, f))
		if err != nil {
			t.Fatal(err)
		}
		writeExec(t, filepath.Join(tools, f), string(b))
	}
	// A shard whose test binary would run for 5 minutes.
	writeExec(t, filepath.Join(tools, "shard.sh"), "#!/usr/bin/env bash\nsleep 300 &\necho $! >\"$SHARD_PIDFILE\"\nwait\n")
	runner := filepath.Join(root, "actions-runner-h")
	rt := filepath.Join(runner, "_work", "_temp")
	if err := os.MkdirAll(rt, 0o755); err != nil {
		t.Fatal(err)
	}
	pidfile := filepath.Join(root, "shard.pid")
	cmd := exec.Command("bash", filepath.Join(tools, "cluster-host.sh"))
	cmd.Dir = root
	cmd.Env = bashEnv(map[string]string{
		"RUNNER_TEMP": rt, "STRESS_ARCH": "x86", "STRESS_HOST": "h", "STRESS_MODE": "stress", "STRESS_SEQUENCE": "stress:1:1",
		"STRESS_CASE_SHAS": "stress=" + testSHA, "STRESS_LOG_DIR": filepath.Join(rt, "stress", "logs"),
		"STRESS_FACTS": filepath.Join(rt, "stress", "host", "facts.txt"), "STRESS_BUSY_SECONDS": "0",
		"STRESS_WATCHDOG_SECONDS": "1", "STRESS_GO_TMPDIR": filepath.Join(root, "gotmp"), "SHARD_PIDFILE": pidfile,
	})
	var out bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &out
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	shardPID := 0
	t.Cleanup(func() {
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		if shardPID > 0 {
			_ = syscall.Kill(shardPID, syscall.SIGKILL)
		}
	})
	for deadline := time.Now().Add(60 * time.Second); shardPID == 0; time.Sleep(100 * time.Millisecond) {
		if b, err := os.ReadFile(pidfile); err == nil && strings.TrimSpace(string(b)) != "" {
			shardPID, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		select {
		case err := <-done:
			t.Fatalf("cluster-host.sh exited before its shard started (%v):\n%s", err, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shard never started:\n%s", out.String())
		}
	}
	// The teardown's wipe.
	if err := os.RemoveAll(runner); err != nil {
		t.Fatal(err)
	}
	gone := time.Now().Add(20 * time.Second)
	for alive(shardPID) && time.Now().Before(gone) {
		time.Sleep(200 * time.Millisecond)
	}
	if alive(shardPID) {
		t.Fatalf("the shard's workload (pid %d) still runs 20 s after its runner dir was deleted:\n%s", shardPID, out.String())
	}
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Errorf("cluster-host.sh still runs after its runner dir was deleted:\n%s", out.String())
	}
}

// syncBuffer is a bytes.Buffer a test may read while a command writes it.
type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// procFields is what /proc/PID/stat holds after the command name: state,
// ppid, pgrp, session, ... (nil when PID is gone).
func procFields(pid int) []string {
	b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return nil
	}
	s := string(b)
	i := strings.LastIndexByte(s, ')')
	if i < 0 {
		return nil
	}
	return strings.Fields(s[i+1:])
}

// procTable lists every process as pid -> its /proc/PID/stat fields.
func procTable() map[int][]string {
	out := map[int][]string{}
	ents, _ := os.ReadDir("/proc")
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if f := procFields(pid); len(f) > 3 {
			out[pid] = f
		}
	}
	return out
}

// sessionLeft lists the live (not zombie) processes of session sid, as
// "pid (comm)".
func sessionLeft(sid int) []string {
	var left []string
	for pid, f := range procTable() {
		if f[0] != "Z" && f[3] == strconv.Itoa(sid) {
			comm, _ := os.ReadFile(fmt.Sprintf("/proc/%d/comm", pid))
			left = append(left, fmt.Sprintf("%d (%s)", pid, strings.TrimSpace(string(comm))))
		}
	}
	slices.Sort(left)
	return left
}

// sessionGoneWithin waits up to d for session sid to have no live process
// and returns what still runs then.
func sessionGoneWithin(sid int, d time.Duration) []string {
	deadline := time.Now().Add(d)
	for {
		left := sessionLeft(sid)
		if len(left) == 0 || time.Now().After(deadline) {
			return left
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// killEverything SIGKILLs whatever a test's host job left behind: session
// sid, every process whose command line names root, and every process in
// the process group or session of one of those (a watchdog's sleep).
func killEverything(sid int, root string) {
	table := procTable()
	ids := map[string]bool{}
	if sid > 0 {
		ids[strconv.Itoa(sid)] = true
	}
	for pid := range table {
		if cmdline, _ := os.ReadFile(fmt.Sprintf("/proc/%d/cmdline", pid)); bytes.Contains(cmdline, []byte(root)) {
			ids[strconv.Itoa(pid)] = true
		}
	}
	for pid, f := range table {
		if pid != os.Getpid() && (ids[strconv.Itoa(pid)] || ids[f[2]] || ids[f[3]]) {
			_ = syscall.Kill(pid, syscall.SIGKILL)
		}
	}
}

const (
	stressClusterWorkflow = "../../.github/workflows/celeris-stress-cluster.yml"
	hostStepName          = "go test on the bare-metal host"
)

// hostStepRun is the `run:` line of the workflow's host step, the one that
// runs cluster-host.sh.
func hostStepRun(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(filepath.FromSlash(stressClusterWorkflow))
	if err != nil {
		t.Fatal(err)
	}
	in := false
	for _, l := range strings.Split(string(b), "\n") {
		s := strings.TrimSpace(l)
		if strings.HasPrefix(s, "- ") {
			name, ok := strings.CutPrefix(s, "- name: ")
			in = ok && name == hostStepName
		} else if run, ok := strings.CutPrefix(s, "run: "); ok && in {
			if !strings.Contains(run, "cluster-host.sh") || strings.HasPrefix(run, "|") || strings.HasPrefix(run, ">") {
				t.Fatalf("the host step's run is %q; this test runs a one-line run that runs cluster-host.sh", run)
			}
			return run
		}
	}
	t.Fatalf("no step %q with a run line in %s", hostStepName, stressClusterWorkflow)
	return ""
}

// cancelledHost is a stress host job in the layout of the workflow's host
// step (the scripts under probatorium/tools/stresstally), with a fake
// shard.sh: its "test binary" runs for 5 minutes and, like a wedged one,
// ignores SIGINT and SIGTERM, so only SIGKILL stops it. The shard records the
// signals a command it starts (go test, on the host) starts with ignored, and
// the workload's pid. (Not its own: bash ignores SIGQUIT itself, and gives a
// command it starts the disposition it found.)
type cancelledHost struct {
	cmd         *exec.Cmd
	out         *syncBuffer
	done        chan struct{} // closed once cmd.Wait returned
	shardSID    int
	shardSigIgn uint64 // SigIgn of a command the shard starts
}

// hostStart is how a cancelled host job's first process starts.
type hostStart int

const (
	// hostScript: bash cluster-host.sh, every signal at its default.
	hostScript hostStart = iota
	// hostStep: the workflow's host step, run as a runner that was started
	// with every signal at its default runs it (as systemd starts one).
	hostStep
	// hostStepOnCluster: the workflow's host step, run as the cluster's
	// runner runs it, with SIGINT, SIGQUIT and SIGHUP ignored (see
	// clusterRunnerStart).
	hostStepOnCluster
)

// clusterRunnerStart runs "$@" with the signals the cluster's runner leaves
// ignored in every step it runs. ansible/runner-setup.yml starts the runner
// as `nohup ./run.sh ... &` from a non-interactive shell: that asynchronous
// list starts with SIGINT and SIGQUIT ignored, nohup adds SIGHUP, and a
// signal ignored across exec stays ignored. The runner (.NET) installs no
// handler for a SIGINT or SIGQUIT it found ignored, so its steps inherit
// them. P6b (run 36338683576) saw the result: the host script's INT trap
// never fired on either arch, and the step ended on the runner's SIGTERM,
// 7.5 s after its SIGINT (probatorium#435). exec keeps the pid, so the
// command's process is the step's, as it is the runner's child on the host.
const clusterRunnerStart = `trap '' INT QUIT HUP; exec "$@"`

// hupIntQuit is SIGHUP, SIGINT and SIGQUIT as bits of a /proc SigIgn mask.
const hupIntQuit = 1<<(syscall.SIGHUP-1) | 1<<(syscall.SIGINT-1) | 1<<(syscall.SIGQUIT-1)

func startCancelledHost(t *testing.T, start hostStart, watchdogSeconds string) *cancelledHost {
	t.Helper()
	if start == hostStepOnCluster {
		// The stand-in must do what it stands in for.
		out, err := exec.Command("sh", "-c", clusterRunnerStart, "sh", "awk", "/^SigIgn:/ {print $2}", "/proc/self/status").Output()
		if ign, perr := strconv.ParseUint(strings.TrimSpace(string(out)), 16, 64); err != nil || perr != nil || ign&hupIntQuit != hupIntQuit {
			t.Fatalf("clusterRunnerStart leaves SigIgn %q (%v, %v), not SIGHUP, SIGINT and SIGQUIT ignored", out, err, perr)
		}
	}
	root := t.TempDir()
	tools := filepath.Join(root, "probatorium", "tools", "stresstally")
	for _, f := range []string{"cluster-host.sh", "gostate.sh"} {
		b, err := os.ReadFile(script(t, f))
		if err != nil {
			t.Fatal(err)
		}
		writeExec(t, filepath.Join(tools, f), string(b))
	}
	pidfile := filepath.Join(root, "shard.pid")
	writeExec(t, filepath.Join(tools, "shard.sh"), `#!/usr/bin/env bash
awk '/^SigIgn:/ {print $2}' /proc/self/status >"$SHARD_PIDFILE.sigign"
trap '' INT TERM
sleep 300 </dev/null >/dev/null 2>&1 &
echo $! >"$SHARD_PIDFILE"
wait
`)
	rt := filepath.Join(root, "runner", "_work", "_temp")
	if err := os.MkdirAll(rt, 0o755); err != nil {
		t.Fatal(err)
	}
	h := &cancelledHost{out: &syncBuffer{}, done: make(chan struct{})}
	switch start {
	case hostStep, hostStepOnCluster:
		// The runner writes the step's run to a file and runs it with the
		// step's shell (shell: bash).
		stepFile := filepath.Join(root, "step.sh")
		writeFile(t, stepFile, hostStepRun(t)+"\n")
		argv := []string{"bash", "--noprofile", "--norc", "-e", "-o", "pipefail", stepFile}
		if start == hostStepOnCluster {
			argv = append([]string{"sh", "-c", clusterRunnerStart, "sh"}, argv...)
		}
		h.cmd = exec.Command(argv[0], argv[1:]...)
		// The runner reads the step's output until it closes, at most 5 s
		// after the step's process exited.
		h.cmd.WaitDelay = 5 * time.Second
	default:
		h.cmd = exec.Command("bash", filepath.Join(tools, "cluster-host.sh"))
	}
	h.cmd.Dir = root
	h.cmd.Env = bashEnv(map[string]string{
		"RUNNER_TEMP": rt, "STRESS_ARCH": "x86", "STRESS_HOST": "h", "STRESS_MODE": "stress", "STRESS_SEQUENCE": "stress:1:1",
		"STRESS_CASE_SHAS": "stress=" + testSHA, "STRESS_LOG_DIR": filepath.Join(rt, "stress", "logs"),
		"STRESS_FACTS": filepath.Join(rt, "stress", "host", "facts.txt"), "STRESS_BUSY_SECONDS": "0",
		"STRESS_WATCHDOG_SECONDS": watchdogSeconds, "STRESS_GO_TMPDIR": filepath.Join(root, "gotmp"), "SHARD_PIDFILE": pidfile,
	})
	h.cmd.Stdout, h.cmd.Stderr = h.out, h.out
	h.cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := h.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	go func() { waitErr = h.cmd.Wait(); close(h.done) }()
	t.Cleanup(func() {
		_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL)
		killEverything(h.shardSID, root)
		select {
		case <-h.done:
		case <-time.After(10 * time.Second):
		}
	})
	workload := 0
	for deadline := time.Now().Add(60 * time.Second); workload == 0; time.Sleep(100 * time.Millisecond) {
		if b, err := os.ReadFile(pidfile); err == nil && strings.TrimSpace(string(b)) != "" {
			workload, _ = strconv.Atoi(strings.TrimSpace(string(b)))
			break
		}
		select {
		case <-h.done:
			t.Fatalf("the host job exited before its shard started (%v):\n%s", waitErr, h.out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the shard never started:\n%s", h.out.String())
		}
	}
	f := procFields(workload)
	if f == nil {
		t.Fatalf("the shard's workload (pid %d) is already gone:\n%s", workload, h.out.String())
	}
	h.shardSID, _ = strconv.Atoi(f[3])
	if own := procFields(os.Getpid()); own == nil || own[3] == f[3] {
		t.Fatalf("the shard runs in the test's own session (%s), not one of its own", f[3])
	}
	// On GitHub the shard runs in the step's foreground, so its go test
	// starts with SIGINT and SIGQUIT at their defaults. On the cluster it must
	// too.
	b, err := os.ReadFile(pidfile + ".sigign")
	if err != nil {
		t.Fatal(err)
	}
	ign, err := strconv.ParseUint(strings.TrimSpace(string(b)), 16, 64)
	if err != nil || ign&(1<<(syscall.SIGINT-1)|1<<(syscall.SIGQUIT-1)) != 0 {
		t.Errorf("the shard's commands start with SigIgn %s (%v): SIGINT or SIGQUIT ignored, which a shard's go test on GitHub never has", strings.TrimSpace(string(b)), err)
	}
	h.shardSigIgn = ign
	return h
}

// runnerCancel does to the step's process what the runner does when the job
// is cancelled or times out (actions/runner src/Runner.Sdk/ProcessInvoker.cs,
// Unix): SIGINT to that process alone; if the step has not ended 7.5 s
// later, SIGTERM; 2.5 s after that, SIGKILL. The step has ended once its
// process exited and its output closed, or 5 s after the exit
// (cmd.WaitDelay); the runner then moves on to the job's always() steps.
// It returns the signal the step ended on.
func (h *cancelledHost) runnerCancel(t *testing.T) string {
	t.Helper()
	for _, s := range []struct {
		name string
		sig  syscall.Signal
		wait time.Duration
	}{{"SIGINT", syscall.SIGINT, 7500 * time.Millisecond}, {"SIGTERM", syscall.SIGTERM, 2500 * time.Millisecond}, {"SIGKILL", syscall.SIGKILL, 30 * time.Second}} {
		_ = h.cmd.Process.Signal(s.sig)
		select {
		case <-h.done:
			return s.name
		case <-time.After(s.wait):
		}
	}
	t.Fatalf("the step has not ended 30 s after the runner's SIGKILL:\n%s", h.out.String())
	return ""
}

// A cancel or a timeout must kill the running shard before the job's
// always() steps run: the Go-state wipe (gostate.sh wipe) is one of them, and
// a go test still running under it can recreate what it deletes, in
// $RUNNER_TEMP/go or in /tmp/cstress, outside the runner dir. The P6 cancel
// of the cluster proof campaign (run 36323766722) ran the wipe with go and
// iouring.test still alive; only the runner's end-of-job orphan cleanup
// killed them. A shard runs in a session of its own, so no signal to the host
// script's process group reaches it: the host script must kill it on SIGINT
// and SIGTERM, and a watchdog in a session of its own must kill it when the
// host script dies of SIGKILL. Its re-proof, P6b (run 36338683576), then
// showed that on the cluster the host script never gets the SIGINT: the
// cluster's runner starts every step with SIGINT ignored.
func TestHostJobKillsItsShardOnCancel(t *testing.T) {
	linuxOnly(t)
	// The trap's arms: a watchdog that looks once a minute cannot be what
	// kills the shard within their bound.
	for name, sig := range map[string]syscall.Signal{"SIGINT": syscall.SIGINT, "SIGTERM": syscall.SIGTERM} {
		t.Run(name+" to the host script's process group", func(t *testing.T) {
			h := startCancelledHost(t, hostScript, "60")
			sent := time.Now()
			if err := syscall.Kill(-h.cmd.Process.Pid, sig); err != nil {
				t.Fatal(err)
			}
			if left := sessionGoneWithin(h.shardSID, 5*time.Second); len(left) > 0 {
				t.Errorf("5 s after %s to the host script's process group, the shard's session still runs %v:\n%s", name, left, h.out.String())
			}
			for alive(h.cmd.Process.Pid) && time.Since(sent) < 10*time.Second {
				time.Sleep(50 * time.Millisecond)
			}
			if alive(h.cmd.Process.Pid) {
				t.Errorf("the host script still runs 10 s after %s:\n%s", name, h.out.String())
			}
		})
	}
	// No trap runs on SIGKILL: the watchdog, in a session of its own, must.
	t.Run("SIGKILL to the host script's process group", func(t *testing.T) {
		h := startCancelledHost(t, hostScript, "1")
		if err := syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL); err != nil {
			t.Fatal(err)
		}
		if left := sessionGoneWithin(h.shardSID, 10*time.Second); len(left) > 0 {
			t.Errorf("10 s after SIGKILL to the host script's process group (watchdog every 1 s), the shard's session still runs %v:\n%s", left, h.out.String())
		}
	})
	// What the runner really does: signals to the step's process only, never
	// to a group, and the always() steps start as soon as the step has
	// ended. The watchdog looks once a minute here, so the step itself must
	// kill the shard.
	//
	// The step must end on the runner's SIGINT, not on its SIGTERM 7.5 s
	// later, also where the runner starts every step with SIGINT ignored, as
	// the cluster's does (clusterRunnerStart): bash cannot trap a signal
	// ignored when it started, so there the host step must reset it before
	// it runs cluster-host.sh. P6b (run 36338683576) ended on SIGTERM.
	for name, start := range map[string]hostStart{
		"the runner's cancel of the workflow's host step":                                   hostStep,
		"the cluster runner's cancel of the workflow's host step (SIGINT ignored at start)": hostStepOnCluster,
	} {
		t.Run(name, func(t *testing.T) {
			h := startCancelledHost(t, start, "60")
			sent := time.Now()
			ended := h.runnerCancel(t)
			took := time.Since(sent).Round(10 * time.Millisecond)
			if left := sessionGoneWithin(h.shardSID, time.Second); len(left) > 0 {
				t.Errorf("the step ended on %s and the runner moved on to the always() steps (the Go-state wipe among them), but 1 s later the shard's session still runs %v:\n%s",
					ended, left, h.out.String())
			}
			if ended != "SIGINT" || !strings.Contains(h.out.String(), "::warning::cluster-host.sh got SIGINT") {
				t.Errorf("the step ended on %s, %v after the runner's SIGINT, and the host script's trap did not report SIGINT: the SIGINT did nothing:\n%s", ended, took, h.out.String())
			}
			// Nor may a command the shard starts inherit what the runner left
			// ignored: on GitHub a shard's go test starts with SIGHUP at its
			// default too.
			if h.shardSigIgn&hupIntQuit != 0 {
				t.Errorf("the shard's commands start with SigIgn %x: SIGHUP, SIGINT or SIGQUIT still ignored", h.shardSigIgn)
			}
			if strings.Contains(h.out.String(), "started with SIGINT ignored") {
				t.Errorf("the host script says it started with SIGINT ignored:\n%s", h.out.String())
			}
		})
	}
}

// fakeGH is `gh api [--paginate] PATH [--jq FILTER]` over fixture files: a
// path with /jobs reads $FAKE_GH_JOBS, /runners $FAKE_GH_RUNNERS, /runs
// $FAKE_GH_RUNS. --jq prints as gh does: strings raw, anything else as
// compact JSON, one value per line (jq -rc).
const fakeGH = `#!/usr/bin/env bash
path="" filter=""
while [ $# -gt 0 ]; do
	case "$1" in
	api | --paginate) ;;
	--jq) shift; filter=$1 ;;
	-*) ;;
	*) path=$1 ;;
	esac
	shift
done
printf '%s\n' "$path" >>"${FAKE_GH_LOG:-/dev/null}"
case "$path" in
*/jobs*) f=${FAKE_GH_JOBS-} ;;
*/runners*) f=${FAKE_GH_RUNNERS-} ;;
*/runs*) f=${FAKE_GH_RUNS-} ;;
*) f="" ;;
esac
[ -n "$f" ] && [ -f "$f" ] || { echo "fake gh: no fixture for $path" >&2; exit 1; }
if [ -n "$filter" ]; then jq -rc "$filter" "$f"; else cat "$f"; fi
`

func runnerJSON(name, status string, labels ...string) string {
	var ls []string
	for _, l := range labels {
		ls = append(ls, fmt.Sprintf(`{"name":%q}`, l))
	}
	return fmt.Sprintf(`{"name":%q,"status":%q,"busy":false,"labels":[%s]}`, name, status, strings.Join(ls, ","))
}

// Each host job waits for one named runner; the bootstrap must not succeed
// unless the runner of every PLANNED host is online.
func TestRunnersOnlineWaitsForThePlannedHosts(t *testing.T) {
	linuxOnly(t)
	check := script(t, "runners-online.sh")
	bin := t.TempDir()
	writeExec(t, filepath.Join(bin, "gh"), fakeGH)
	both := `{"include":[{"arch":"x86","host":"msa2-server"},{"arch":"arm64","host":"msr1"}]}`
	x86 := `{"include":[{"arch":"x86","host":"msa2-server"}]}`
	server := runnerJSON("celeris-msa2-server", "online", "self-hosted", "celeris-cluster", "msa2-server")
	client := runnerJSON("celeris-msa2-client", "online", "self-hosted", "celeris-cluster", "msa2-client")
	for name, c := range map[string]struct {
		matrix, runners string
		code            int
		want            string
	}{
		"both online":         {both, server + "," + client + "," + runnerJSON("celeris-msr1", "online", "self-hosted", "celeris-cluster", "msr1"), 0, ""},
		"msr1 offline":        {both, server + "," + client + "," + runnerJSON("celeris-msr1", "offline", "self-hosted", "celeris-cluster", "msr1"), 1, "msr1"},
		"msr1 missing":        {both, server + "," + client, 1, "msr1"},
		"msr1 not in cluster": {both, server + "," + client + "," + runnerJSON("celeris-msr1", "online", "self-hosted", "msr1"), 1, "msr1"},
		"x86 plan only":       {x86, server + "," + runnerJSON("celeris-msr1", "offline", "self-hosted", "celeris-cluster", "msr1"), 0, ""},
		"no runners":          {x86, "", 1, "msa2-server"},
	} {
		t.Run(name, func(t *testing.T) {
			fix := filepath.Join(t.TempDir(), "runners.json")
			writeFile(t, fix, `{"total_count":3,"runners":[`+c.runners+`]}`)
			out, code := runBash(t, t.TempDir(), map[string]string{"PATH": withPath(bin), "GH_TOKEN": "x", "REPO": "goceleris/probatorium",
				"MATRIX": c.matrix, "FAKE_GH_RUNNERS": fix, "RUNNERS_ONLINE_TRIES": "2", "RUNNERS_ONLINE_PAUSE": "0"}, `bash "$1"`, check)
			if code != c.code {
				t.Errorf("exit %d, want %d\n%s", code, c.code, out)
			}
			if c.code != 0 && (!strings.Contains(out, "::error::") || !strings.Contains(out, c.want)) {
				t.Errorf("the refusal does not name %s as an error:\n%s", c.want, out)
			}
		})
	}
}

// The eviction audit reports the runs cancelled while this run entered
// matrix-tier-cluster: between the guard's look and a little after the guard
// job ended (the moment the cluster job is queued into the group), not every
// cancel until the bootstrap finally ran, which can be hours later.
func TestEvictionAuditReportsOnlyTheEntryWindow(t *testing.T) {
	linuxOnly(t)
	audit := script(t, "eviction-audit.sh")
	bin := t.TempDir()
	writeExec(t, filepath.Join(bin, "gh"), fakeGH)
	run := func(id int, path, updated string) string {
		return fmt.Sprintf(`{"id":%d,"path":%q,"updated_at":%q,"display_title":"run %d","status":"completed","conclusion":"cancelled"}`, id, path, updated, id)
	}
	runs := filepath.Join(t.TempDir(), "runs.json")
	writeFile(t, runs, `{"workflow_runs":[`+strings.Join([]string{
		run(1, ".github/workflows/matrix-nightly-tier.yml", "2026-09-27T10:00:50Z"), // at the entry: reported
		run(2, ".github/workflows/benchmark-tier.yml", "2026-09-27T12:30:00Z"),      // hours later: not
		run(3, ".github/workflows/matrix-race-tier.yml", "2026-09-27T09:59:00Z"),    // before the look: not
		run(4, ".github/workflows/test.yml", "2026-09-27T10:01:00Z"),                // not a cluster workflow
		run(5, ".github/workflows/celeris-stress.yml", "2026-09-27T10:02:10Z"),      // a stress run at the entry: reported
		run(77, ".github/workflows/celeris-stress.yml", "2026-09-27T10:00:30Z"),     // this run
		run(6, ".github/workflows/matrix-weekend-tier.yml", "2026-09-27T10:03:30Z"), // after the window: not
	}, ",")+`]}`)
	jobs := filepath.Join(t.TempDir(), "jobs.json")
	writeFile(t, jobs, `{"jobs":[{"name":"plan","status":"completed","completed_at":"2026-09-27T09:59:40Z"},`+
		`{"name":"cluster-guard","status":"completed","conclusion":"success","completed_at":"2026-09-27T10:00:40Z"}]}`)
	env := map[string]string{"PATH": withPath(bin), "GH_TOKEN": "x", "REPO": "goceleris/probatorium", "RUN_ID": "77",
		"CHECKED_AT": "2026-09-27T10:00:00Z", "FAKE_GH_RUNS": runs, "FAKE_GH_JOBS": jobs}
	out, code := runBash(t, t.TempDir(), env, `bash "$1"`, audit)
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	reported := map[string]bool{}
	for _, m := range regexp.MustCompile(`::error::run (\d+) `).FindAllStringSubmatch(out, -1) {
		reported[m[1]] = true
	}
	if !reported["1"] || !reported["5"] || len(reported) != 2 {
		t.Errorf("reported runs %v, want exactly 1 and 5 (cancelled between the guard's look and the entry):\n%s", reported, out)
	}
	if !strings.Contains(out, "may have") {
		t.Errorf("a cancel in the window is not proof of an eviction; the report must say it may be one:\n%s", out)
	}
}

// fakePerf is `perf stat -x, -e EVENTS -o FILE -- CMD...` on a heterogeneous
// arm64 host (two A720 PMUs and an A520 one), as recent perf prints it:
// one line per PMU, the event qualified by the PMU, uncounted PMUs marked.
const fakePerf = `#!/usr/bin/env bash
out=""
while [ $# -gt 0 ]; do
	case "$1" in
	-o) out=$2; shift 2 ;;
	--) shift; break ;;
	*) shift ;;
	esac
done
cat >"$out" <<'CSV'
# started on Sun Sep 27 10:00:00 2026

<not counted>,,armv8_cortex_a520/instructions/u,0,0.00,,
1000,,armv8_cortex_a720/instructions/u,5000,100.00,,
234,,armv8_cortex_a720_1/instructions/u,5000,100.00,,
2000,,armv8_cortex_a720/cycles/u,5000,100.00,,
<not supported>,,armv8_cortex_a520/cycles/u,0,0.00,,
12.50,msec,task-clock,12500000,100.00,0.999,CPUs utilized
CSV
exec "$@"
`

// On a heterogeneous host perf counts each core type on its own PMU; the
// observation's count is their sum, not "none" because no line starts with
// "instructions".
func TestObsSumsEveryPMU(t *testing.T) {
	linuxOnly(t)
	obs := script(t, "obs.sh")
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "bin", "perf"), fakePerf)
	writeExec(t, filepath.Join(dir, "pkg.test"), "#!/usr/bin/env bash\necho PASS\n")
	file := filepath.Join(dir, "obs")
	out, code := runBash(t, dir, map[string]string{"PATH": withPath(filepath.Join(dir, "bin")), "STRESS_OBS_FILE": file, "STRESS_PERF": "ok",
		"STRESS_OBS_SEQ": "1", "STRESS_CASE": "A", "STRESS_SHARD": "1"}, `bash "$1" "$2" -test.v=true`, obs, filepath.Join(dir, "pkg.test"))
	if code != 0 {
		t.Fatalf("exit %d\n%s", code, out)
	}
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	f := fields(strings.TrimPrefix(strings.TrimSpace(string(b)), "stress-obs: "))
	if f["instructions_u"] != "1234" || f["cycles_u"] != "2000" || f["task_clock_ms"] != "12.50" || f["perf"] != "ok" {
		t.Errorf("stress-obs %v: want instructions_u=1234 (1000+234) cycles_u=2000 task_clock_ms=12.50", f)
	}
}

// The host job's perf probe must call that host usable.
func TestPerfProbeCountsOnAHybridHost(t *testing.T) {
	linuxOnly(t)
	host := script(t, "cluster-host.sh")
	dir := t.TempDir()
	writeExec(t, filepath.Join(dir, "bin", "perf"), fakePerf)
	out, code := runBash(t, dir, map[string]string{"PATH": withPath(filepath.Join(dir, "bin")), "TMPDIR": dir, "RUNNER_TEMP": dir},
		`. "$1"; perf_probe; printf 'perf_state=%s\n' "$perf_state"`, host)
	if code != 0 || !strings.Contains(out, "perf_state=ok\n") {
		t.Errorf("exit %d; want perf_state=ok on a host whose perf counts per PMU:\n%s", code, out)
	}
}

// cpuSpec is one CPU of a fake sysfs.
type cpuSpec struct {
	n        int
	siblings string
	capacity int    // 0: no cpu_capacity file
	midr     string // "": no MIDR (not arm64)
	maxKHz   int    // 0: no cpufreq
	offline  bool
}

func fakeSysfs(t *testing.T, cpus []cpuSpec) string {
	t.Helper()
	root := t.TempDir()
	for _, c := range cpus {
		d := filepath.Join(root, fmt.Sprintf("cpu%d", c.n))
		online := "1\n"
		if c.offline {
			online = "0\n"
		}
		writeFile(t, filepath.Join(d, "online"), online)
		writeFile(t, filepath.Join(d, "topology", "thread_siblings_list"), c.siblings+"\n")
		if c.capacity > 0 {
			writeFile(t, filepath.Join(d, "cpu_capacity"), strconv.Itoa(c.capacity)+"\n")
		}
		if c.midr != "" {
			writeFile(t, filepath.Join(d, "regs", "identification", "midr_el1"), c.midr+"\n")
		}
		if c.maxKHz > 0 {
			writeFile(t, filepath.Join(d, "cpufreq", "cpuinfo_max_freq"), strconv.Itoa(c.maxKHz)+"\n")
		}
	}
	return root
}

// cluster lists n CPUs from first, each its own core.
func cluster(first, n, capacity int, midr string, khz int) []cpuSpec {
	var out []cpuSpec
	for c := first; c < first+n; c++ {
		out = append(out, cpuSpec{n: c, siblings: strconv.Itoa(c), capacity: capacity, midr: midr, maxKHz: khz})
	}
	return out
}

const (
	midrA720 = "0x00000000410fd811"
	midrA520 = "0x00000000410fd801"
)

// Every observation is pinned to CPUs of ONE core class: the fastest, cpu0's
// core excluded. A class is what the kernel reports as the same core: the
// same capacity, and on arm64 also the same core type (MIDR) and cluster
// frequency, because a kernel may report every core of a big.LITTLE SoC at
// capacity 1024.
func TestPickCPUsKeepsToOneCoreClass(t *testing.T) {
	host := script(t, "cluster-host.sh")
	var smt []cpuSpec // 8 cores, 16 threads, siblings N and N+8, no capacity file
	for c := range 16 {
		smt = append(smt, cpuSpec{n: c, siblings: fmt.Sprintf("%d,%d", c%8, c%8+8)})
	}
	cix := slices.Concat(cluster(0, 4, 400, midrA520, 1800000), cluster(4, 4, 870, midrA720, 2400000), cluster(8, 4, 1024, midrA720, 2800000))
	flat := slices.Concat(cluster(0, 4, 1024, midrA720, 2800000), cluster(4, 4, 1024, midrA720, 2400000), cluster(8, 4, 1024, midrA520, 1800000))
	offline := cluster(0, 6, 0, "", 0)
	offline[5].offline = true
	for name, c := range map[string]struct {
		cpus []cpuSpec
		n    int
		want string // the list, or a piece of the refusal
		ok   bool
	}{
		"x86 SMT, 4":                        {smt, 4, "4,5,6,7", true},
		"x86 SMT, 8 is more than 7 cores":   {smt, 8, "only 7 primary CPU(s)", false},
		"arm64 capacities, the big cluster": {cix, 4, "8,9,10,11", true},
		"arm64 capacities, 5 of 4 big":      {cix, 5, "only 4 primary CPU(s)", false},
		"arm64 flat capacity, by core type": {flat, 2, "2,3", true},
		"arm64 flat, cpu0's big cluster":    {flat, 4, "only 3 primary CPU(s)", false},
		"an offline cpu is not used":        {offline, 4, "1,2,3,4", true},
	} {
		t.Run(name, func(t *testing.T) {
			sysfs := fakeSysfs(t, c.cpus)
			errs := t.TempDir()
			out, code := runBash(t, "", map[string]string{"STRESS_SYSFS": sysfs, "RUNNER_TEMP": t.TempDir()},
				`. "$1"; rc=0; pick_cpus "$2" 2>"$3/err" || rc=$?; printf 'rc=%s\n' "$rc"; cat "$3/err"`, host, strconv.Itoa(c.n), errs)
			if code != 0 {
				t.Fatalf("exit %d\n%s", code, out)
			}
			lines := strings.SplitN(out, "\n", 2)
			if c.ok && (lines[0] != c.want || !strings.Contains(out, "rc=0")) {
				t.Errorf("picked %q, want %q\n%s", lines[0], c.want, out)
			}
			if !c.ok && (strings.Contains(out, "rc=0") || !strings.Contains(out, c.want)) {
				t.Errorf("want a refusal naming %q\n%s", c.want, out)
			}
		})
	}
}

// A loud host refuses every observation of the timing; a quiet one is
// recorded as quiet. (The refusal existed before this test; it was only ever
// exercised on its passing path.)
func TestQuietWaitRefusesALoudHost(t *testing.T) {
	host := script(t, "cluster-host.sh")
	for load, want := range map[string]string{"3.50": "loud:load1=3.50", "0.10": "quiet:load1=0.10"} {
		procfs := t.TempDir()
		writeFile(t, filepath.Join(procfs, "loadavg"), load+" 1.00 1.00 1/100 1\n")
		writeFile(t, filepath.Join(procfs, "stat"), "cpu  100 0 100 1000 0 0 0 0 0 0\n")
		out, code := runBash(t, "", map[string]string{"STRESS_PROCFS": procfs, "STRESS_BUSY_SECONDS": "0", "STRESS_QUIET_SECONDS": "0",
			"STRESS_QUIET_POLL": "0", "RUNNER_TEMP": t.TempDir()},
			`. "$1"; pre_refuse=""; quiet_wait; printf 'quiet=%s\nrefuse=%s\n' "$quiet" "$pre_refuse"`, host)
		if code != 0 || !strings.Contains(out, "quiet="+want) {
			t.Errorf("load %s: exit %d, want quiet=%s...\n%s", load, code, want, out)
		}
		if refused := !strings.Contains(out, "refuse=\n"); refused != (load == "3.50") {
			t.Errorf("load %s: refused=%v\n%s", load, refused, out)
		}
	}
}

// fakeGo is the go command of a timing host job: `go version`, `go test -c`
// (a prebuild: writes the test binary) and `go test ... -exec=WRAPPER` (runs
// the test binary once through the wrapper, as go test does). It appends
// "<dir> <args>" to $FAKE_GO_LOG.
const fakeGo = `#!/usr/bin/env bash
printf '%s %s\n' "$PWD" "$*" >>"$FAKE_GO_LOG"
case "$1" in
version) echo "go version go1.27.0 linux/fake"; exit 0 ;;
test) ;;
*) exit 2 ;;
esac
out="" exec_with="" build=0
prev=""
for a in "$@"; do
	case "$a" in
	-c) build=1 ;;
	-exec=*) exec_with=${a#-exec=} ;;
	esac
	[ "$prev" != "-o" ] || out=$a
	prev=$a
done
if [ "$build" = 1 ]; then cp "$FAKE_GO_TESTBIN" "$out"; exit 0; fi
rc=0
if [ -n "$exec_with" ]; then "$exec_with" "$FAKE_GO_TESTBIN" -test.v=true || rc=$?; else "$FAKE_GO_TESTBIN" || rc=$?; fi
if [ "$rc" = 0 ]; then printf 'ok  \tgithub.com/goceleris/celeris/engine/iouring\t0.010s\n'; else printf 'FAIL\tgithub.com/goceleris/celeris/engine/iouring\t0.010s\n'; fi
exit "$rc"
`

const fakeTestBin = "#!/usr/bin/env bash\nprintf '=== RUN   TestSockaddrString\\n--- PASS: TestSockaddrString (0.00s)\\nPASS\\n'\n"

// memlockNow is this process's RLIMIT_MEMLOCK soft limit as prlimit takes it
// ("unlimited" or bytes): a shard asked for it needs no privilege to get it.
func memlockNow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile("/proc/self/limits")
	if err != nil {
		t.Fatal(err)
	}
	for _, l := range strings.Split(string(b), "\n") {
		if strings.HasPrefix(l, "Max locked memory") {
			if f := strings.Fields(strings.TrimPrefix(l, "Max locked memory")); len(f) >= 1 {
				return f[0]
			}
		}
	}
	t.Fatal("no memlock line in /proc/self/limits")
	return ""
}

func machineArch(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("uname", "-m").Output()
	if err != nil {
		t.Fatal(err)
	}
	switch strings.TrimSpace(string(out)) {
	case "x86_64":
		return "x86"
	case "aarch64":
		return "arm64"
	}
	t.Skipf("uname -m %s is neither arch the stress workflow plans", out)
	return ""
}

func shortHostname(t *testing.T) string {
	t.Helper()
	out, err := exec.Command("hostname", "-s").Output()
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

// timingHost is a working directory laid out as a timing host job's: a
// celeris checkout per arm (all at one commit), fake go on PATH, and the
// environment the workflow's host step gives cluster-host.sh.
type timingHost struct {
	dir, bin, goLog, sha string
	env                  map[string]string
}

func newTimingHost(t *testing.T, arms ...string) timingHost {
	t.Helper()
	linuxOnly(t)
	h := timingHost{dir: t.TempDir()}
	h.bin = filepath.Join(h.dir, "bin")
	h.goLog = filepath.Join(h.dir, "go.log")
	writeExec(t, filepath.Join(h.bin, "go"), fakeGo)
	writeExec(t, filepath.Join(h.dir, "pkg.test"), fakeTestBin)
	first := filepath.Join(h.dir, "celeris-"+arms[0])
	git := func(dir string, args ...string) string {
		cmd := exec.Command("git", append([]string{"-c", "user.name=t", "-c", "user.email=t@example.com", "-c", "init.defaultBranch=main"}, args...)...)
		cmd.Dir = dir
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
		return strings.TrimSpace(string(out))
	}
	if err := os.MkdirAll(first, 0o755); err != nil {
		t.Fatal(err)
	}
	git(first, "init", "-q")
	git(first, "commit", "-q", "--allow-empty", "-m", "celeris")
	h.sha = git(first, "rev-parse", "HEAD")
	var shas []string
	for _, a := range arms {
		if d := filepath.Join(h.dir, "celeris-"+a); d != first {
			git(h.dir, "clone", "-q", first, d)
		}
		shas = append(shas, a+"="+h.sha)
	}
	rt := filepath.Join(h.dir, "_temp")
	mem := memlockNow(t)
	h.env = map[string]string{
		"PATH": withPath(h.bin), "FAKE_GO_LOG": h.goLog, "FAKE_GO_TESTBIN": filepath.Join(h.dir, "pkg.test"),
		"RUNNER_TEMP": rt, "STRESS_GO_TMPDIR": filepath.Join(h.dir, "gotmp"),
		"STRESS_ARCH": machineArch(t), "STRESS_HOST": shortHostname(t), "STRESS_MODE": "timing",
		"STRESS_CASE_SHAS": strings.Join(shas, " "), "STRESS_CASE_REFS": "",
		"STRESS_PACKAGES": "./engine/iouring", "STRESS_RUN": "^TestSockaddrString$", "STRESS_COUNT": "1",
		"STRESS_MEMLOCK": "test", "STRESS_MEMLOCK_LIMIT": mem, "STRESS_RACE": "false", "STRESS_TIMEOUT": "1m",
		"STRESS_JOB_TIMEOUT": "30", "STRESS_FLAGS": "", "STRESS_ENV": "", "STRESS_CPUS": "1", "STRESS_PMU": "optional",
		"STRESS_LOG_DIR": filepath.Join(rt, "stress", "logs"), "STRESS_FACTS": filepath.Join(rt, "stress", "host", "facts.txt"),
		"STRESS_BUSY_SECONDS": "0", "STRESS_QUIET_SECONDS": "0", "STRESS_QUIET_POLL": "0",
	}
	// Two single-thread cores the runner has (cpu0 and cpu1): cpu1 is picked.
	h.env["STRESS_SYSFS"] = fakeSysfs(t, cluster(0, 2, 0, "", 0))
	procfs := t.TempDir()
	writeFile(t, filepath.Join(procfs, "loadavg"), "0.05 0.05 0.05 1/100 1\n")
	writeFile(t, filepath.Join(procfs, "stat"), "cpu  100 0 100 1000 0 0 0 0 0 0\n")
	h.env["STRESS_PROCFS"] = procfs
	return h
}

func (h timingHost) goCalls(t *testing.T) []string {
	t.Helper()
	b, err := os.ReadFile(h.goLog)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatal(err)
	}
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

// A timing host job builds every arm's test binary once, before the quiet
// check, so no observation directly follows a compile; every go test of a
// timing builds with -trimpath, so two arms of one commit in two checkouts
// run the same bytes; and every observation records the binary it ran.
func TestTimingHostJobPrebuildsAndTrimsPaths(t *testing.T) {
	h := newTimingHost(t, "A", "B")
	h.env["STRESS_SEQUENCE"] = "A:1:11 B:1:11 B:2:12 A:2:12"
	out, code := runBash(t, h.dir, h.env, `bash "$1"`, script(t, "cluster-host.sh"))
	if code != 0 {
		t.Fatalf("cluster-host.sh: exit %d\n%s", code, out)
	}
	var builds, runs []string
	for _, c := range h.goCalls(t) {
		dir, args, _ := strings.Cut(c, " ")
		switch {
		case strings.HasPrefix(args, "test -c "):
			builds = append(builds, filepath.Base(dir))
		case strings.HasPrefix(args, "test "):
			runs = append(runs, filepath.Base(dir))
		}
		if strings.HasPrefix(args, "test ") && !slices.Contains(strings.Fields(args), "-trimpath") {
			t.Errorf("a timing go test without -trimpath: %s", c)
		}
	}
	if !slices.Equal(builds, []string{"celeris-A", "celeris-B"}) {
		t.Errorf("prebuilds in %v, want one per arm (celeris-A, celeris-B)", builds)
	}
	if !slices.Equal(runs, []string{"celeris-A", "celeris-B", "celeris-B", "celeris-A"}) {
		t.Errorf("observations in %v, want the planned order A B B A", runs)
	}
	facts, err := os.ReadFile(h.env["STRESS_FACTS"])
	if err != nil {
		t.Fatal(err)
	}
	pre, quiet := strings.Index(string(facts), "prebuild "), strings.Index(string(facts), "quiet check: ")
	if pre < 0 || quiet < 0 || pre > quiet {
		t.Errorf("the prebuild (at %d) must be recorded before the quiet check (at %d):\n%s", pre, quiet, facts)
	}
	for _, tok := range strings.Fields(h.env["STRESS_SEQUENCE"]) {
		p := strings.Split(tok, ":")
		log, err := os.ReadFile(filepath.Join(h.env["STRESS_LOG_DIR"], p[0]+"__"+h.env["STRESS_ARCH"]+"__"+p[1]+".log"))
		if err != nil {
			t.Fatal(err)
		}
		sl := parseShard(bytes.NewReader(log))
		if len(sl.obs) != 1 || sl.obs[0]["binary_sha256"] == "" || sl.obs[0]["wall_ns"] == "" {
			t.Errorf("%s: observations %v\n%s", tok, sl.obs, log)
		}
	}
}

// pmu=required on a host where perf cannot count refuses every observation
// before go test runs: the log says why and go test never starts. (Coverage
// for a refusal that existed before; it was only exercised on its passing
// path.) The fake perf fails as perf does at perf_event_paranoid 4, so the
// test runs the same on any runner.
func TestTimingHostJobRefusesPMURequiredWithoutPerf(t *testing.T) {
	h := newTimingHost(t, "A", "B")
	h.env["STRESS_SEQUENCE"], h.env["STRESS_PMU"] = "A:1:11 B:1:11", "required"
	writeExec(t, filepath.Join(h.bin, "perf"), "#!/usr/bin/env bash\necho 'Error: Access to performance monitoring and observability operations is limited.' >&2\nexit 255\n")
	out, code := runBash(t, h.dir, h.env, `bash "$1"`, script(t, "cluster-host.sh"))
	if code != 1 {
		t.Errorf("exit %d, want 1 (every shard refused)\n%s", code, out)
	}
	for _, c := range h.goCalls(t) {
		if _, args, _ := strings.Cut(c, " "); strings.HasPrefix(args, "test ") && !strings.HasPrefix(args, "test -c ") {
			t.Errorf("go test ran on a refused timing: %s", c)
		}
	}
	for _, arm := range []string{"A", "B"} {
		log, err := os.ReadFile(filepath.Join(h.env["STRESS_LOG_DIR"], arm+"__"+h.env["STRESS_ARCH"]+"__1.log"))
		if err != nil {
			t.Fatalf("%v; cluster-host.sh said:\n%s", err, out)
		}
		if !strings.Contains(string(log), "stress-refused: ") || !strings.Contains(string(log), "pmu required but perf is unusable:paranoid=") ||
			!strings.HasSuffix(strings.TrimSpace(string(log)), "stress-trailer: exit=refused elapsed_s=0") {
			t.Errorf("arm %s log does not refuse for want of perf:\n%s", arm, log)
		}
	}
}
