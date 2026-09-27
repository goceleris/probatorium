//go:build linux

package remote

import (
	"errors"

	"golang.org/x/sys/unix"
)

// awaitExit blocks until process pid has exited, without reaping it
// (WNOWAIT), and reports whether it could. Until the reap the exited process
// is a zombie that keeps its PID, so its group ID cannot be handed out again
// either. os.Process does the same before its own reap (os/wait_waitid.go).
func awaitExit(pid int) bool {
	var info unix.Siginfo
	for {
		err := unix.Waitid(unix.P_PID, pid, &info, unix.WEXITED|unix.WNOWAIT, nil)
		if !errors.Is(err, unix.EINTR) {
			return err == nil
		}
	}
}
