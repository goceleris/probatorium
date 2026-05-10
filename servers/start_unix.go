//go:build unix

package servers

import "syscall"

// newProcAttr requests its own process group so the runner can
// SIGTERM/SIGKILL the whole subtree (negative PID) without taking out
// the runner itself.
func newProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setpgid: true}
}

// signalGroup signals the process group whose leader has pid. ESRCH —
// "no such process" — is treated as success so a child that exited on
// its own between the SIGTERM and the wait does not turn into a stop
// error.
func signalGroup(pid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pid, sig); err != nil && err != syscall.ESRCH {
		return err
	}
	return nil
}
