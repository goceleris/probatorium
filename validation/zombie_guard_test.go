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
// to whatever else runs on the box and is never asserted on. It always prints
// one verdict line naming the lister it used, so a `go test -v` log shows
// that it ran and how (a plain `go test` shows a package's output only when
// the package fails).

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

// childLister lists a process's children, live or zombie, as PID -> state
// letter(s).
type childLister struct {
	name string
	list func(self int) (map[int]string, error)
}

// childListers are the ways this platform can list a process's children, in
// the order childProcesses tries them: /proc on Linux, then ps.
func childListers() []childLister {
	var ls []childLister
	if runtime.GOOS == "linux" {
		ls = append(ls, childLister{"/proc", childProcessesProc})
	}
	return append(ls, childLister{"ps", childProcessesPS})
}

// childProcesses maps every child of this process, live or zombie, to its
// state letter(s), and names the lister that answered. It fails only when no
// lister could answer at all.
func childProcesses() (map[int]string, string, error) {
	self := os.Getpid()
	var errs []error
	for _, l := range childListers() {
		kids, err := l.list(self)
		if err == nil {
			return kids, l.name, nil
		}
		errs = append(errs, fmt.Errorf("%s: %w", l.name, err))
	}
	return nil, "", errors.Join(errs...)
}

// zombiesIn returns the PIDs in kids whose state is Z, sorted.
func zombiesIn(kids map[int]string) []int {
	var pids []int
	for pid, state := range kids {
		if strings.HasPrefix(state, "Z") {
			pids = append(pids, pid)
		}
	}
	slices.Sort(pids)
	return pids
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
// keeps this process's children. ps is running while it lists, so its own row
// is in the output as a live child of this process; it is dropped here, and
// being live it never counted as a zombie anyway.
func childProcessesPS(self int) (map[int]string, error) {
	cmd := exec.Command("ps", "-A", "-o", "pid=,ppid=,stat=")
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("ps: %w", err)
	}
	kids := parsePSChildren(out, self)
	delete(kids, cmd.Process.Pid)
	return kids, nil
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

// zombieGuard is the guard TestMain runs over this test binary's children.
func zombieGuard(w io.Writer) int {
	return runZombieGuard(w, os.Getpid(), childProcesses, zombieGuardSettle)
}

// runZombieGuard re-samples list until the zombie count is at or under
// zombieGuardBound or settle has passed, writes one verdict line to w, and
// returns the exit code the package should get: 0 to pass, 1 to fail. It
// fails closed: a list that cannot be taken is a FAIL, because a guard that
// cannot count cannot show that nothing was left unreaped, and a skip inside
// TestMain would print nothing under a plain `go test` and read as a pass.
func runZombieGuard(w io.Writer, self int, list func() (map[int]string, string, error), settle time.Duration) int {
	deadline := time.Now().Add(settle)
	for samples := 1; ; samples++ {
		kids, source, err := list()
		if err != nil {
			_, _ = fmt.Fprintf(w, "zombie guard (probatorium#415): FAIL, could not count this binary's child processes, so it cannot show that none was left unreaped: %v\n", err)
			return 1
		}
		pids := zombiesIn(kids)
		if len(pids) <= zombieGuardBound {
			_, _ = fmt.Fprintf(w, "zombie guard (probatorium#415): PASS, %d unreaped children of this test binary (pid %d), listed via %s after %d sample(s) (bound %d)\n",
				len(pids), self, source, samples, zombieGuardBound)
			return 0
		}
		if time.Now().After(deadline) {
			shown := pids
			if len(shown) > 20 {
				shown = shown[:20]
			}
			_, _ = fmt.Fprintf(w, "zombie guard (probatorium#415): FAIL, %d exited children of this test binary (pid %d) were never reaped after %s, listed via %s (bound %d); first pids: %v. Something Start()ed a process and never Wait()ed it.\n",
				len(pids), self, settle, source, zombieGuardBound, shown)
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

// TestZombieGuard_Verdicts: the guard's decision on its own, with the
// children list stubbed. A list it cannot take FAILS (fail closed); a zombie
// that outlasts the settle window FAILS; one that is reaped inside it PASSES;
// every verdict is printed with the lister that produced it.
func TestZombieGuard_Verdicts(t *testing.T) {
	const self = 500
	listed := func(steps ...map[int]string) func() (map[int]string, string, error) {
		i := 0
		return func() (map[int]string, string, error) {
			kids := steps[min(i, len(steps)-1)]
			i++
			return kids, "stub", nil
		}
	}
	for _, tc := range []struct {
		name     string
		list     func() (map[int]string, string, error)
		wantCode int
		wantLine string
	}{
		{
			name:     "children cannot be listed",
			list:     func() (map[int]string, string, error) { return nil, "", errors.New("no /proc, no ps") },
			wantCode: 1,
			wantLine: "FAIL, could not count this binary's child processes, so it cannot show that none was left unreaped: no /proc, no ps",
		},
		{
			name:     "a zombie outlasts the settle window",
			list:     listed(map[int]string{900: "Z", 901: "S"}),
			wantCode: 1,
			wantLine: "FAIL, 1 exited children of this test binary (pid 500) were never reaped after 300ms, listed via stub (bound 0); first pids: [900]",
		},
		{
			name:     "a zombie is reaped inside the settle window",
			list:     listed(map[int]string{900: "Z"}, map[int]string{900: "Z"}, map[int]string{901: "S"}),
			wantCode: 0,
			wantLine: "PASS, 0 unreaped children of this test binary (pid 500), listed via stub after 3 sample(s) (bound 0)",
		},
		{
			name:     "no children at all",
			list:     listed(map[int]string{}),
			wantCode: 0,
			wantLine: "PASS, 0 unreaped children of this test binary (pid 500), listed via stub after 1 sample(s) (bound 0)",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer
			code := runZombieGuard(&buf, self, tc.list, 300*time.Millisecond)
			if code != tc.wantCode {
				t.Errorf("exit code %d, want %d; verdict %q", code, tc.wantCode, buf.String())
			}
			if !strings.Contains(buf.String(), "zombie guard (probatorium#415): "+tc.wantLine) {
				t.Errorf("verdict %q, want it to contain %q", buf.String(), tc.wantLine)
			}
		})
	}
}

// TestZombieGuard_SeesAnUnreapedChild proves the guard is not vacuous on this
// platform, for every way it can list children here (on Linux both /proc,
// which it uses first, and ps when installed; elsewhere ps): a child that has
// exited but was not waited for must show up as a zombie of this process, and
// must be gone once it is reaped. It never skips: a platform where children
// cannot be listed fails the guard itself.
func TestZombieGuard_SeesAnUnreapedChild(t *testing.T) {
	listers := childListers()
	if runtime.GOOS == "linux" {
		if _, err := exec.LookPath("ps"); err != nil {
			listers = listers[:1] // /proc only
		}
	}
	for _, l := range listers {
		// "proc", not "/proc": a slash in a subtest name splits it for -run.
		t.Run(strings.TrimPrefix(l.name, "/"), func(t *testing.T) {
			self := os.Getpid()
			if _, err := l.list(self); err != nil {
				t.Fatalf("%s cannot list this process's children: %v", l.name, err)
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
				kids, err := l.list(self)
				if err != nil {
					t.Fatal(err)
				}
				if slices.Contains(zombiesIn(kids), pid) {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("pid %d exited unreaped but %s never listed it as a zombie of pid %d (saw %v)", pid, l.name, self, kids)
				}
				time.Sleep(10 * time.Millisecond)
			}
			reap()
			kids, err := l.list(self)
			if err != nil {
				t.Fatal(err)
			}
			if state, ok := kids[pid]; ok {
				t.Fatalf("pid %d still listed by %s as a child (state %s) after it was reaped", pid, l.name, state)
			}
		})
	}
}
