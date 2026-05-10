package fault

import (
	"context"
	"fmt"
	"strconv"
)

// FDPressure ramps the soft FD limit on the celeris process via
// prlimit(1). Apply tightens the limit to SoftLimit; Undo restores it
// to RestoreLimit (which the scheduler captures via prlimit --output
// before applying).
//
// Tightening RLIMIT_NOFILE does NOT close already-open FDs, but it
// surfaces accept() / open() failures for any new ones — which is
// exactly the pressure profile that catches "engine assumes accept
// always succeeds" bugs.
type FDPressure struct {
	Host Host
	PID  int
	// SoftLimit is the new RLIMIT_NOFILE soft cap. Must be positive;
	// typical values for stress are 100-1000.
	SoftLimit int
	// RestoreLimit is the soft cap to set on Undo. The scheduler
	// captures the original soft cap at apply time and stashes it here.
	RestoreLimit int
}

func (f *FDPressure) String() string {
	return fmt.Sprintf("fd-pressure(host=%s pid=%d soft=%d restore=%d)",
		f.Host, f.PID, f.SoftLimit, f.RestoreLimit)
}

func (f *FDPressure) Apply(ctx context.Context) error {
	if f.PID <= 0 || f.SoftLimit <= 0 {
		return fmt.Errorf("fd-pressure: bad config %+v", f)
	}
	argv := []string{"prlimit", "--nofile=" + strconv.Itoa(f.SoftLimit), "--pid", strconv.Itoa(f.PID)}
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("fd-pressure.apply", argv, out, err)
	}
	return nil
}

func (f *FDPressure) Undo(ctx context.Context) error {
	if f.PID <= 0 || f.RestoreLimit <= 0 {
		return nil
	}
	argv := []string{"prlimit", "--nofile=" + strconv.Itoa(f.RestoreLimit), "--pid", strconv.Itoa(f.PID)}
	out, err := run(ctx, f.Host, true, argv...)
	if err != nil {
		return formatErr("fd-pressure.undo", argv, out, err)
	}
	return nil
}
