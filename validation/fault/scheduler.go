package fault

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// ScheduledFault is one element of a Schedule.
type ScheduledFault struct {
	// Offset is the duration after the run start at which Apply fires.
	Offset time.Duration
	// Duration is how long the fault is left in place before Undo.
	// Zero means "one-shot, Undo immediately after Apply".
	Duration time.Duration
	// Fault is the perturbation itself.
	Fault Fault
}

// Schedule is an ordered list of faults. Generate produces deterministic
// schedules from a seed; the run loop applies and undoes faults in
// timestamp order.
type Schedule []ScheduledFault

// Sort puts the schedule in ascending Offset order. Generate already
// returns sorted schedules; Sort is exposed for callers who construct
// a Schedule by hand.
func (s Schedule) Sort() {
	sort.SliceStable(s, func(i, j int) bool { return s[i].Offset < s[j].Offset })
}

// String renders the schedule one entry per line; convenience for
// dry-run output.
func (s Schedule) String() string {
	if len(s) == 0 {
		return "(empty schedule)"
	}
	out := ""
	for _, f := range s {
		out += fmt.Sprintf("  t=%-10s dur=%-10s %s\n", f.Offset, f.Duration, f.Fault.String())
	}
	return out
}

// GenerateConfig parameterises Generate.
type GenerateConfig struct {
	// Seed is the master seed; PCG-derived RNG decides which faults
	// land where. The same seed always yields the same schedule.
	Seed uint64
	// RunDuration is the budget — faults are scheduled within this
	// window. Generate refuses to emit a fault whose Offset+Duration
	// would extend past RunDuration.
	RunDuration time.Duration
	// MinSpacing is the minimum gap between consecutive Apply calls.
	// Defaults to 30s if zero.
	MinSpacing time.Duration
	// CelerisPID is the PID of the celeris-under-test process. Faults
	// that need a PID (signals, fd-pressure, listen-fd-close) read it
	// from here.
	CelerisPID int
	// CelerisListenFD is the FD number of celeris's listen socket.
	// Used by ListenFDClose only.
	CelerisListenFD int
	// CelerisListenPort is the TCP port celeris is bound to. Used by
	// IPTablesDrop.
	CelerisListenPort int
	// LoadgenIface is the iface name on the load-gen host. Used by
	// tc-netem faults.
	LoadgenIface string
}

// Generate deterministically expands cfg into a Schedule. Useful as
// the spine of the validator-replay harness: given (seed, commit,
// arch), the schedule reconstructs.
//
// Generation strategy: roll for a small number of faults (target
// average one fault every 5 minutes, capped at 12 total), pick fault
// kind uniformly from the available catalogue, pick parameters from
// short per-kind tables, place Offset uniformly in [MinSpacing,
// RunDuration - Duration - MinSpacing].
func Generate(cfg GenerateConfig) Schedule {
	if cfg.RunDuration <= 0 {
		return nil
	}
	spacing := cfg.MinSpacing
	if spacing <= 0 {
		spacing = 30 * time.Second
	}
	if cfg.LoadgenIface == "" {
		cfg.LoadgenIface = "eth0"
	}
	rng := rand.New(rand.NewPCG(cfg.Seed, ^cfg.Seed^0x9e3779b97f4a7c15))

	target := int(cfg.RunDuration / (5 * time.Minute))
	if target < 1 {
		target = 1
	}
	if target > 12 {
		target = 12
	}

	var sched Schedule
	used := map[int64]bool{} // slot dedup at 1s granularity

	for len(sched) < target {
		kind := rng.IntN(5)
		var (
			f   Fault
			dur time.Duration
		)
		switch kind {
		case 0: // tc-netem delay/jitter
			f = &TCNetem{
				Host:     HostClient,
				Iface:    cfg.LoadgenIface,
				DelayMs:  10 + rng.IntN(150),
				JitterMs: rng.IntN(50),
			}
			dur = 30*time.Second + time.Duration(rng.IntN(120))*time.Second
		case 1: // tc-netem loss
			f = &TCNetem{
				Host:    HostClient,
				Iface:   cfg.LoadgenIface,
				LossPct: 1 + float64(rng.IntN(20)),
			}
			dur = 30*time.Second + time.Duration(rng.IntN(60))*time.Second
		case 2: // signal pause
			if cfg.CelerisPID == 0 {
				continue
			}
			f = &SignalPause{Host: HostServer, PID: cfg.CelerisPID}
			dur = time.Duration(200+rng.IntN(2000)) * time.Millisecond
		case 3: // fd pressure
			if cfg.CelerisPID == 0 {
				continue
			}
			f = &FDPressure{
				Host:         HostServer,
				PID:          cfg.CelerisPID,
				SoftLimit:    100 + rng.IntN(900),
				RestoreLimit: 1048576,
			}
			dur = 30*time.Second + time.Duration(rng.IntN(120))*time.Second
		case 4: // iptables drop
			if cfg.CelerisListenPort == 0 {
				continue
			}
			chain := "INPUT"
			if rng.IntN(2) == 0 {
				chain = "OUTPUT"
			}
			f = &IPTablesDrop{
				Host:  HostServer,
				Chain: chain,
				Port:  cfg.CelerisListenPort,
			}
			dur = 5*time.Second + time.Duration(rng.IntN(20))*time.Second
		}
		if f == nil {
			continue
		}
		// Place Offset uniformly in [spacing, RunDuration-dur-spacing].
		latest := cfg.RunDuration - dur - spacing
		if latest <= spacing {
			break
		}
		off := spacing + time.Duration(rng.Int64N(int64(latest-spacing)))
		// Quantize to seconds for human-readable schedules.
		slot := int64(off / time.Second)
		if used[slot] {
			continue
		}
		used[slot] = true
		sched = append(sched, ScheduledFault{Offset: off, Duration: dur, Fault: f})
	}
	sched.Sort()
	return sched
}

// Run applies the schedule against ctx-bound clock. Returns the first
// Apply error encountered; on shutdown ALL still-active faults have
// their Undo called regardless of prior errors.
//
// The runner is safe to call from a single goroutine; concurrent Run
// calls on the same schedule undefined.
func Run(ctx context.Context, started time.Time, sched Schedule) error {
	var (
		mu     sync.Mutex
		active []Fault
	)
	defer func() {
		// Best-effort Undo of every fault still in flight; uses a
		// fresh context so a parent cancellation does not abort the
		// cleanup.
		undoCtx, cancel := context.WithTimeout(context.Background(), UndoTimeout)
		defer cancel()
		mu.Lock()
		toUndo := append([]Fault(nil), active...)
		active = nil
		mu.Unlock()
		for _, f := range toUndo {
			_ = f.Undo(undoCtx)
		}
	}()

	for _, sf := range sched {
		// Wait until Offset elapses.
		until := started.Add(sf.Offset)
		wait := time.Until(until)
		if wait > 0 {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(wait):
			}
		}
		applyCtx, applyCancel := context.WithTimeout(ctx, ApplyTimeout)
		err := sf.Fault.Apply(applyCtx)
		applyCancel()
		if err != nil {
			return fmt.Errorf("apply %s: %w", sf.Fault, err)
		}
		mu.Lock()
		active = append(active, sf.Fault)
		mu.Unlock()

		// Schedule Undo after Duration. Done in a child goroutine so
		// overlapping faults stay overlapping.
		go func(f Fault, after time.Duration) {
			t := time.NewTimer(after)
			defer t.Stop()
			select {
			case <-ctx.Done():
				return
			case <-t.C:
			}
			undoCtx, cancel := context.WithTimeout(context.Background(), UndoTimeout)
			defer cancel()
			_ = f.Undo(undoCtx)
			mu.Lock()
			for i, a := range active {
				if a == f {
					active = append(active[:i], active[i+1:]...)
					break
				}
			}
			mu.Unlock()
		}(sf.Fault, sf.Duration)
	}
	// Wait for ctx.Done so the deferred Undo can sweep up.
	<-ctx.Done()
	return nil
}
