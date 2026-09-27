package remote

import (
	"context"
	"strings"
	"syscall"
	"testing"
)

// TestLocal_StopSweepRunsBeforeTheReap: on Linux the reaper records the exit,
// and sweeps a stopped process's group, while the leader is still a zombie
// (waitid WNOWAIT). The zombie keeps its PID, so the sweep's kill(-pgid)
// reaches only this group: after the reap, an emptied group's ID would be
// free for the kernel to hand out again. The log reads the leader's state as
// each signal is sent.
func TestLocal_StopSweepRunsBeforeTheReap(t *testing.T) {
	l, k := recordingLocal()
	proc, err := l.Start(context.Background(), []string{"-c", "exec sleep 30"})
	if err != nil {
		t.Fatal(err)
	}
	leader := proc.PID()
	k.note = func(int, syscall.Signal) string { return processState(leader) }
	if err := proc.Signal(int(syscall.SIGTERM)); err != nil {
		t.Fatalf("SIGTERM: %v", err)
	}
	res, err := proc.Wait(context.Background())
	if err != nil || !res.Signaled || res.Signal != int(syscall.SIGTERM) {
		t.Fatalf("Wait: got %+v, %v; want killed by SIGTERM", res, err)
	}
	calls := k.snapshot()
	if len(calls) != 2 || calls[1].pid != -leader || calls[1].sig != syscall.SIGKILL {
		t.Fatalf("signals: got %+v, want the SIGTERM and then the sweep's SIGKILL to group %d", calls, leader)
	}
	if !strings.HasPrefix(calls[1].note, "Z") {
		t.Errorf("the sweep ran with leader %d in state %q, not a zombie (Z): it was already reaped, and its group ID with it", leader, calls[1].note)
	}
	t.Logf("leader %d: state %q at the SIGTERM, %q at the sweep", leader, calls[0].note, calls[1].note)
}
