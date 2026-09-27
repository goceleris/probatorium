package remote

import (
	"bufio"
	"bytes"
	"context"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// TestLocal_SignalReachesForkedChildren: a signal from the driver must reach
// what the process forked, not only the process. A shell that forks its last
// command (bash does, for `echo ready; sleep 30`) dies on a SIGTERM sent to it
// alone, and the forked child lives on with PPID 1 where no Wait can reach it.
// In `go test ./validation/...` that left hundreds of live sleeps behind on
// top of the zombies (probatorium#415). Each script prints the PID of the
// child it forked; after SIGTERM and the reap, that child must be dead too,
// including a child that ignores SIGTERM while its parent exits on it.
func TestLocal_SignalReachesForkedChildren(t *testing.T) {
	for _, tc := range []struct {
		name   string
		script string
	}{
		// The outer shell runs the inner one in the foreground and waits for
		// it; `; true` keeps it from exec'ing its last command. The inner
		// shell prints its own PID and becomes the sleeper.
		{"foreground child", `sh -c 'echo "child=$$"; exec sleep 30'; true`},
		{"background child", `sleep 30 & echo "child=$!"; wait`},
		// The child ignores SIGTERM and the leader does not: the leader dies,
		// and the child must still go, killed with its group when the
		// leader is reaped.
		{"child that ignores SIGTERM", `sh -c 'trap "" TERM; echo "child=$$"; exec sleep 30' & wait`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			proc, err := NewLocal("/bin/sh").Start(ctx, []string{"-c", tc.script})
			if err != nil {
				t.Fatal(err)
			}
			group := proc.PID()
			child := readChildPID(t, proc)
			t.Cleanup(func() {
				// Never leave the sleeper behind if the assertion fails, and
				// never signal a PID that has left the group (reused).
				if pg, err := syscall.Getpgid(child); err == nil && pg == group {
					_ = syscall.Kill(child, syscall.SIGKILL)
				}
			})
			if state := processState(child); !isRunning(state) {
				t.Fatalf("child %d is not running before the signal (state %q)", child, state)
			}
			if err := proc.Signal(int(syscall.SIGTERM)); err != nil {
				t.Fatalf("SIGTERM: %v", err)
			}
			if _, err := proc.Wait(ctx); err != nil {
				t.Fatalf("wait: %v", err)
			}
			deadline := time.Now().Add(5 * time.Second)
			for {
				state := processState(child)
				if !isRunning(state) {
					return
				}
				if time.Now().After(deadline) {
					t.Fatalf("child %d that pid %d forked is still running (state %q) 5s after the driver's SIGTERM: the signal reached only the process, not its group", child, group, state)
				}
				time.Sleep(20 * time.Millisecond)
			}
		})
	}
}

// readChildPID reads the "child=<pid>" line the script prints.
func readChildPID(t *testing.T, proc Process) int {
	t.Helper()
	got := make(chan int, 1)
	go func() {
		sc := bufio.NewScanner(proc.Stderr())
		for sc.Scan() {
			if v, ok := strings.CutPrefix(sc.Text(), "child="); ok {
				if n, err := strconv.Atoi(v); err == nil {
					got <- n
					break
				}
			}
		}
		// Keep draining so the script never blocks on a full pipe.
		for sc.Scan() {
		}
	}()
	select {
	case n := <-got:
		return n
	case <-time.After(10 * time.Second):
		_ = proc.Signal(int(syscall.SIGKILL))
		t.Fatal("the script never printed child=<pid>")
		return 0
	}
}

// processState is pid's state letter(s), or "" once no such process exists.
// Linux reads /proc/<pid>/stat (its state is the first field after the
// command name, which may itself contain ')'), so the test does not need ps
// there; elsewhere it asks ps.
func processState(pid int) string {
	if runtime.GOOS == "linux" {
		b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
		if err != nil {
			return ""
		}
		if i := bytes.LastIndexByte(b, ')'); i >= 0 {
			if f := strings.Fields(string(b[i+1:])); len(f) > 0 {
				return f[0]
			}
		}
		return "?"
	}
	out, err := exec.Command("ps", "-o", "stat=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}

// isRunning: the process exists and has not exited (a zombie has exited; only
// its reaping is left, which is its new parent's business).
func isRunning(state string) bool {
	return state != "" && !strings.HasPrefix(state, "Z")
}
