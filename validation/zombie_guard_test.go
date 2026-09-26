package validation

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

// The zombie guard (probatorium#415). A process that is Start()ed and never
// Wait()ed stays in the process table as a zombie until its parent reaps it
// or exits. This package's tests fork thousands of short-lived processes, and
// Tier 3's per-seed loop used to SIGTERM every refapp without reaping it:
// `go test ./validation/...` held ~1,100 zombies at once, which on a laptop
// with a 2,666 per-user process limit made every other fork on the host fail.
//
// The guard runs after m.Run() in TestMain and fails the package when this
// test binary still has unreaped children. It counts ONLY children of this
// process (PPID == os.Getpid()) in state Z: the host-wide zombie count belongs
// to whatever else runs on the box and is never asserted on.

// zombieGuardBound is how many unreaped children may survive the settle
// window. Zero: a child whose owner does Wait is reaped within microseconds of
// exiting, so one that is still a zombie after zombieGuardSettle has no
// reaper at all. The settle window, not the bound, absorbs a reap that is
// merely late.
const zombieGuardBound = 0

// zombieGuardSettle bounds how long the guard re-samples before it decides.
// It returns at the first sample at or under the bound, so a clean package
// pays one sample.
const zombieGuardSettle = 10 * time.Second

// errZombieCountUnsupported means this host offers no way to list this
// process's children: /proc is unreadable (or absent) and there is no ps.
// The guard then FAILS the package. It runs from TestMain, where a skip would
// print nothing under a plain `go test` and read as a pass.
var errZombieCountUnsupported = errors.New("cannot list child processes on this platform")

// zombieChildren returns the PIDs of this process's children that have exited
// and have not been reaped.
func zombieChildren() ([]int, error) {
	kids, err := childProcesses()
	if err != nil {
		return nil, err
	}
	var pids []int
	for pid, state := range kids {
		if strings.HasPrefix(state, "Z") {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids, nil
}

// childProcesses maps every child of this process, live or zombie, to its
// state letter(s).
func childProcesses() (map[int]string, error) {
	self := os.Getpid()
	if runtime.GOOS == "linux" {
		if kids, err := childProcessesProc(self); err == nil {
			return kids, nil
		}
	}
	return childProcessesPS(self)
}

// childProcessesProc reads /proc/<pid>/stat for every process. The fields
// after the command name, which is parenthesised and may itself contain
// spaces and parentheses, start with the state and then the PPID.
func childProcessesProc(self int) (map[int]string, error) {
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return nil, err
	}
	kids := map[int]string{}
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile("/proc/" + e.Name() + "/stat")
		if err != nil {
			continue // exited and reaped between ReadDir and here
		}
		state, ppid, ok := parseProcStat(b)
		if ok && ppid == self {
			kids[pid] = state
		}
	}
	return kids, nil
}

func parseProcStat(b []byte) (state string, ppid int, ok bool) {
	i := bytes.LastIndexByte(b, ')')
	if i < 0 {
		return "", 0, false
	}
	f := strings.Fields(string(b[i+1:]))
	if len(f) < 2 {
		return "", 0, false
	}
	ppid, err := strconv.Atoi(f[1])
	if err != nil {
		return "", 0, false
	}
	return f[0], ppid, true
}

// childProcessesPS asks ps(1) for every process's PID, PPID and state, and
// keeps this process's children. The ps child it forks is reaped by Output
// before the list is parsed, so it never counts itself.
func childProcessesPS(self int) (map[int]string, error) {
	out, err := exec.Command("ps", "-A", "-o", "pid=,ppid=,stat=").Output()
	if err != nil {
		if errors.Is(err, exec.ErrNotFound) {
			return nil, fmt.Errorf("%w: %v", errZombieCountUnsupported, err)
		}
		return nil, fmt.Errorf("ps: %w", err)
	}
	return parsePSChildren(out, self), nil
}

func parsePSChildren(out []byte, self int) map[int]string {
	kids := map[int]string{}
	for _, line := range strings.Split(string(out), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, err1 := strconv.Atoi(f[0])
		ppid, err2 := strconv.Atoi(f[1])
		if err1 != nil || err2 != nil {
			continue
		}
		if ppid == self {
			kids[pid] = f[2]
		}
	}
	return kids
}

// zombieGuard re-samples this process's zombie children until the count is at
// or under zombieGuardBound or zombieGuardSettle has passed, writes a verdict
// to w, and returns the exit code the package should get: 0 to pass, 1 to
// fail.
func zombieGuard(w io.Writer) int {
	deadline := time.Now().Add(zombieGuardSettle)
	for {
		pids, err := zombieChildren()
		if err != nil {
			_, _ = fmt.Fprintf(w, "zombie guard (probatorium#415): FAIL, could not count this binary's child processes, so it cannot show that none was left unreaped: %v\n", err)
			return 1
		}
		if len(pids) <= zombieGuardBound {
			return 0
		}
		if time.Now().After(deadline) {
			shown := pids
			if len(shown) > 20 {
				shown = shown[:20]
			}
			_, _ = fmt.Fprintf(w, "zombie guard (probatorium#415): FAIL, %d exited children of this test binary (pid %d) were never reaped after %s (bound %d); first pids: %v. Something Start()ed a process and never Wait()ed it.\n",
				len(pids), os.Getpid(), zombieGuardSettle, zombieGuardBound, shown)
			return 1
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// TestZombieGuard_ParsesProcStat: the Linux path reads state and PPID from
// the fields after the LAST ')', because the command name may contain both
// spaces and parentheses.
func TestZombieGuard_ParsesProcStat(t *testing.T) {
	for _, tc := range []struct {
		in    string
		state string
		ppid  int
		ok    bool
	}{
		{"4242 (sh) Z 100 4242 4242 0 -1", "Z", 100, true},
		{"4243 (validation.test) S 1 4243 4243 0 -1", "S", 1, true},
		{"4244 (a) b (c)) Z 77 1 1 0", "Z", 77, true},
		{"4245 (x y) R", "", 0, false},
		{"garbage", "", 0, false},
	} {
		state, ppid, ok := parseProcStat([]byte(tc.in))
		if state != tc.state || ppid != tc.ppid || ok != tc.ok {
			t.Errorf("parseProcStat(%q) = %q, %d, %v; want %q, %d, %v", tc.in, state, ppid, ok, tc.state, tc.ppid, tc.ok)
		}
	}
}

// TestZombieGuard_ParsesPS: the ps path keeps only this process's children,
// whatever their state, and the header-free output may be space-padded.
func TestZombieGuard_ParsesPS(t *testing.T) {
	out := []byte("    1     0 Ss\n  900   500 Z\n  901   500 S+\n  902   777 Z\n  bad   500 Z\n")
	got := parsePSChildren(out, 500)
	want := map[int]string{900: "Z", 901: "S+"}
	if len(got) != len(want) || got[900] != want[900] || got[901] != want[901] {
		t.Fatalf("parsePSChildren = %v, want %v", got, want)
	}
}

// TestZombieGuard_SeesAnUnreapedChild proves the guard is not vacuous on this
// platform: a child that has exited but was not waited for must show up as a
// zombie of this process, and must be gone once it is reaped.
func TestZombieGuard_SeesAnUnreapedChild(t *testing.T) {
	if _, err := zombieChildren(); errors.Is(err, errZombieCountUnsupported) {
		t.Skipf("the guard cannot count children here: %v", err)
	}
	cmd := exec.Command("/bin/sh", "-c", "exit 0")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	pid := cmd.Process.Pid
	reaped := false
	reap := func() {
		if !reaped {
			reaped = true
			_ = cmd.Wait()
		}
	}
	t.Cleanup(reap)
	deadline := time.Now().Add(tier1TestBudget)
	for {
		pids, err := zombieChildren()
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(pids, pid) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("pid %d exited unreaped but was never counted as a zombie of pid %d (saw %v)", pid, os.Getpid(), pids)
		}
		time.Sleep(10 * time.Millisecond)
	}
	reap()
	kids, err := childProcesses()
	if err != nil {
		t.Fatal(err)
	}
	if state, ok := kids[pid]; ok {
		t.Fatalf("pid %d still listed as a child (state %s) after it was reaped", pid, state)
	}
}
