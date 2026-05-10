package fault

import (
	"context"
	"fmt"
	"strconv"
)

// SignalPause delivers SIGSTOP to the target PID on Apply and SIGCONT
// on Undo. Used to simulate scheduler pauses, GC-style world-stops,
// and brief process-level outages.
//
// Sending SIGSTOP on a process that no longer exists surfaces as an
// error; the scheduler treats that as a fail-noisy event (the celeris
// PID must exist for the fault to be meaningful).
type SignalPause struct {
	Host Host
	PID  int
}

func (f *SignalPause) String() string {
	return fmt.Sprintf("signal-pause(host=%s pid=%d)", f.Host, f.PID)
}

func (f *SignalPause) Apply(ctx context.Context) error {
	if f.PID <= 0 {
		return fmt.Errorf("signal-pause: bad pid %d", f.PID)
	}
	argv := []string{"kill", "-STOP", strconv.Itoa(f.PID)}
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("signal-pause.apply", argv, out, err)
	}
	return nil
}

func (f *SignalPause) Undo(ctx context.Context) error {
	if f.PID <= 0 {
		return fmt.Errorf("signal-pause: bad pid %d", f.PID)
	}
	argv := []string{"kill", "-CONT", strconv.Itoa(f.PID)}
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("signal-pause.undo", argv, out, err)
	}
	return nil
}
