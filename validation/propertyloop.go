package validation

import (
	"context"
	"net/http"
	"time"

	"github.com/goceleris/probatorium/validation/checker"
	"github.com/goceleris/probatorium/validation/properties"
)

// propertyLoopConfig parameterises runPropertyLoop.
type propertyLoopConfig struct {
	// MetricsURL is the refapp's /debug/vars URL, built from the REAL
	// bound address announced on the ready banner.
	MetricsURL string
	// PID is the refapp's pid; drives the /proc/<pid>/status VmRSS sample
	// (I-MEM-4). 0 leaves RSS unsampled and I-MEM-4 skips.
	PID int
	// Interval is the poll cadence; zero means 1 s (the predicates'
	// windows assume 1 Hz).
	Interval time.Duration
	// Specs are the predicates to evaluate (checker.SelectPredicates).
	Specs []properties.Spec
	// HardFail marks the emitted Incidents as cell-cancelling
	// (Config.PropertyHardFail); false marks them RecordOnly.
	HardFail bool
	// ExpectedPanics, when non-nil, returns the workload's running count
	// of designed panics; copied into every Snapshot so I-PANIC nets them
	// out.
	ExpectedPanics func() int64
	// Violations receives one Incident per predicate ID on its FIRST
	// violation (non-blocking send; the orchestrator's channel has
	// capacity 1 and a dropped send is still counted in the tally).
	Violations chan<- Incident
	// SnapshotPath, when non-empty, receives the running tally every
	// propertyLoopSnapshotEvery ticks and once more on exit, so a long
	// soak shows mid-run property progress.
	SnapshotPath string
}

// propertyLoopSnapshotEvery is the tick cadence of the SnapshotPath
// write (30 s at 1 Hz).
const propertyLoopSnapshotEvery = 30

// propertyLoopSkippedSSH is the Tally.SkippedReason recorded when the
// orchestrator drives the refapp over ssh.
const propertyLoopSkippedSSH = "ssh driver: the refapp serves /debug/vars to loopback peers on the remote host only; the property loop was not run"

// propertyPollTimeout caps one /debug/vars GET. The document is a few
// KB served from memory; anything slower than this is the refapp
// wedging, which I-HANG owns.
const propertyPollTimeout = 500 * time.Millisecond

// runPropertyLoop is the in-process replacement for the never-launched
// cmd/validator-checker: once per Interval it polls the refapp's
// /debug/vars, samples its RSS, and evaluates every selected predicate
// against the rolling History (checker.Evaluator). RunStartedAt and
// BaselineGoroutines come from the first successful sample -- i.e. the
// first poll after the refapp announced ready -- so every warm-up
// window is refapp-relative, not orchestrator-relative.
//
// The first violation of each predicate is surfaced as an Incident
// (TierProperty, PredicateID = the I-* ID, Snapshot attached) exactly
// like the tier-1-walker oracles in runTierProperty's TallyCallback.
// With cfg.HardFail the orchestrator's Run loop cancels the cell after
// writing the dossier and capturing forensics; without it the Incident
// is RecordOnly -- dossier + forensics, and the cell keeps running.
// Every violation, first or not, is counted in the returned Tally so
// the cell document (and the absolute gate) sees it even when the
// incident channel was already occupied.
//
// Returns when ctx is done; never earlier.
func runPropertyLoop(ctx context.Context, cfg propertyLoopConfig) checker.Tally {
	interval := cfg.Interval
	if interval <= 0 {
		interval = time.Second
	}
	hc := &http.Client{Timeout: propertyPollTimeout}
	ev := checker.NewEvaluator(cfg.Specs)

	poll := func(t time.Time) {
		snap, err := checker.Poll(ctx, hc, cfg.MetricsURL, t)
		if err != nil {
			if ctx.Err() == nil {
				ev.RecordPollError()
			}
			return
		}
		snap.PID = cfg.PID
		snap.RSSBytes = checker.ReadRSS(cfg.PID)
		if cfg.ExpectedPanics != nil {
			snap.ExpectedPanics = cfg.ExpectedPanics()
		}
		for _, v := range ev.Observe(snap, t) {
			if !v.First || cfg.Violations == nil {
				continue
			}
			select {
			case cfg.Violations <- Incident{
				Tier:        TierProperty,
				PredicateID: v.ID,
				Message:     v.Message,
				Snapshot:    v.Snapshot,
				ObservedAt:  t.UTC(),
				RefappPID:   cfg.PID,
				RecordOnly:  !cfg.HardFail,
			}:
			default:
				// Channel full: the orchestrator is already handling a
				// hard fail. The tally still records this violation.
			}
		}
	}
	snapshot := func() {
		if cfg.SnapshotPath != "" {
			_ = writeJSON(cfg.SnapshotPath, ev.Tally())
		}
	}

	// Sample immediately so the baseline is the refapp's state at
	// readiness, not one interval later.
	poll(time.Now())
	tick := time.NewTicker(interval)
	defer tick.Stop()
	n := 0
	for {
		select {
		case <-ctx.Done():
			snapshot()
			return ev.Tally()
		case t := <-tick.C:
			poll(t)
			n++
			if n%propertyLoopSnapshotEvery == 0 {
				snapshot()
			}
		}
	}
}
