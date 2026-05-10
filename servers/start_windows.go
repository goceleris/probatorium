//go:build windows

package servers

import "syscall"

// newProcAttr is a no-op on Windows; there is no portable equivalent of
// setpgid for the orchestrator's purposes (the conformance / runner
// flows that need this primitive run on linux bench hosts).
func newProcAttr() *syscall.SysProcAttr { return &syscall.SysProcAttr{} }

// signalGroup is best-effort on Windows: it sends sig to the named PID
// only. The runner is not officially supported on Windows; this
// implementation keeps `go build ./...` cross-platform.
func signalGroup(pid int, sig syscall.Signal) error {
	p, err := syscall.OpenProcess(syscall.PROCESS_TERMINATE, false, uint32(pid))
	if err != nil {
		return err
	}
	defer syscall.CloseHandle(p)
	return syscall.TerminateProcess(p, 1)
}
