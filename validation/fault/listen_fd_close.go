package fault

import (
	"context"
	"fmt"
	"strconv"
)

// ListenFDClose closes celeris's listen FD via the /proc/<pid>/fd/<n>
// interface. The fault is intentionally severe — it forces the engine
// to observe an EBADF on the next accept, exercising the
// listen-fd-loss recovery path the adaptive standby refactor (PR #49)
// introduced.
//
// Reversibility: closing a listen FD is irreversible at the kernel
// level. Undo is a no-op; the test is expected to either tolerate the
// disruption (engine reopens its listener via SO_REUSEPORT
// re-registration) or be flagged as a one-shot terminal fault by the
// scheduler so the run wraps cleanly afterward.
//
// Requires root on the celeris host — gdb attach is the mechanism: we
// drive `gdb -batch -p <pid> -ex 'call close(<fd>)' -ex detach`. The
// validator's ansible role provisions the gdb binary; targets without
// gdb fail Apply with a clean error.
type ListenFDClose struct {
	Host Host
	PID  int
	// FD is the file-descriptor number to close. The validator's
	// celeris adapter prints "listen_fd=<n>" on startup so the
	// scheduler can capture it.
	FD int
}

func (f *ListenFDClose) String() string {
	return fmt.Sprintf("listen-fd-close(host=%s pid=%d fd=%d)", f.Host, f.PID, f.FD)
}

func (f *ListenFDClose) Apply(ctx context.Context) error {
	if f.PID <= 0 || f.FD <= 0 {
		return fmt.Errorf("listen-fd-close: bad config %+v", f)
	}
	argv := []string{
		"gdb", "-batch",
		"-p", strconv.Itoa(f.PID),
		"-ex", "call (int)close(" + strconv.Itoa(f.FD) + ")",
		"-ex", "detach",
	}
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("listen-fd-close.apply", argv, out, err)
	}
	return nil
}

// Undo is a no-op — once a FD is closed there is no reopen at the
// kernel level. Recovery (if any) is the engine's responsibility.
func (f *ListenFDClose) Undo(ctx context.Context) error { return nil }
