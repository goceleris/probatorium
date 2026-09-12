package validation

import (
	"context"
	"io"
	"net/http"
	neturl "net/url"
	"os"
	"strings"
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
	// CrashReports, when non-nil, returns how many crash signatures named the
	// pointer checker (validation/liveness.go). A -d=checkptr violation is a
	// runtime throw, so by the time it could be polled the process is gone
	// and every subsequent poll fails -- no tick will ever carry it. On
	// shutdown the loop therefore makes ONE final Observe using a copy of
	// the last good snapshot with CheckptrReports filled in from here. The
	// copy keeps every other field as it was, so the slope predicates see a
	// repeated sample rather than a synthetic zero.
	CrashReports func() int64
	// IdleWindow reports the orchestrator's current idle window (0 under
	// load, n inside the n-th; see tier1Config.IdleWindows). nil when the
	// tier never idles. Stamped on every sample; the loop declares
	// I-MEM-2 once the second window begins.
	IdleWindow func() int
	// ResponseConformance, when non-nil, returns the wire scraper's running
	// counts (validation/rfc_scrape.go). The loop copies them into every
	// Snapshot so I-RFC-1 and I-RFC-2 judge what celeris actually wrote on
	// the wire, rather than what celeris says it wrote -- the independence
	// those predicates were specified with and never had.
	ResponseConformance func() ResponseCounters
	// SeriesPath, when non-empty, receives one CSV row per sample: the
	// inputs the slope oracles judge, plus the columns that say which kind
	// of growth a rising heap is. See seriesWriter (probatorium#319).
	SeriesPath string
	// BaselineHeapPath, when non-empty, receives ONE heap profile fetched
	// the first time a sample lands at or after properties.SlopeWarmup()
	// past run start -- i.e. the instant the slope oracles begin judging.
	//
	// Without it an I-MEM incident dossier holds a single heap profile and
	// the growing allocation site can only be INFERRED from composition.
	// With it, triage is a measurement:
	//
	//	go tool pprof -inuse_space -base heap-warm.pprof heap.pprof
	//
	// which names the site directly. The v1.5.11 soak's I-MEM-1 failure
	// could not be attributed for exactly this reason.
	BaselineHeapPath string
}

// appendDeclared adds ids to a comma-separated instrumented-properties list,
// which is how a cell tells the checker which predicates have a live data
// source (see checker.DeclaredOnly). Refapps publish their own on
// /debug/vars; these two are declared by the validator instead, because the
// data source is the validator's wire scraper rather than the refapp.
func appendDeclared(list string, ids ...string) string {
	for _, id := range ids {
		if list == "" {
			list = id
			continue
		}
		if !strings.Contains(list, id) {
			list += "," + id
		}
	}
	return list
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
	series := newSeriesWriter(cfg.SeriesPath)
	defer series.Close()

	// lastGood is the most recent snapshot the predicates judged; see
	// propertyLoopConfig.CrashReports for the one place it is reused.
	var lastGood properties.Snapshot
	haveLast := false

	// firstSampleAt mirrors the evaluator's RunStartedAt: both are set from
	// the first SUCCESSFUL poll, so the baseline lands on the same clock the
	// slope predicates use.
	var firstSampleAt time.Time
	// loadStartedAt is when the first idle window was left: the slope
	// oracles' warm-up runs from here (properties.Context.LoadStartedAt),
	// so the warm heap profile must too.
	var loadStartedAt time.Time
	prevIdle := 0
	baselineDone := false
	captureBaseline := func(elapsed time.Duration) {
		if baselineDone || cfg.BaselineHeapPath == "" || cfg.MetricsURL == "" {
			return
		}
		if elapsed < properties.SlopeWarmup() {
			return
		}
		baselineDone = true // one attempt only; a retry loop would perturb the heap it measures
		u, err := neturl.Parse(cfg.MetricsURL)
		if err != nil {
			return
		}
		u.Path = "/debug/pprof/heap"
		u.RawQuery = ""
		reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, u.String(), nil)
		if err != nil {
			return
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			return
		}
		defer func() { _ = resp.Body.Close() }()
		if resp.StatusCode != http.StatusOK {
			return
		}
		f, err := os.Create(cfg.BaselineHeapPath)
		if err != nil {
			return
		}
		defer func() { _ = f.Close() }()
		_, _ = io.Copy(f, resp.Body)
	}
	poll := func(t time.Time) {
		snap, err := checker.Poll(ctx, hc, cfg.MetricsURL, t)
		if err != nil {
			if ctx.Err() == nil {
				ev.RecordPollError(t)
			}
			return
		}
		snap.PID = cfg.PID
		snap.RSSBytes = checker.ReadRSS(cfg.PID)
		if firstSampleAt.IsZero() {
			firstSampleAt = t
		}
		if cfg.IdleWindow != nil {
			snap.IdleWindow = cfg.IdleWindow()
		}
		if prevIdle == 1 && snap.IdleWindow == 0 {
			loadStartedAt = t
		}
		prevIdle = snap.IdleWindow
		if snap.IdleWindow == 0 {
			anchor := firstSampleAt
			if !loadStartedAt.IsZero() {
				anchor = loadStartedAt
			}
			captureBaseline(t.Sub(anchor))
		}
		// I-MEM-2 judges idle window 2 and later against window 1; declare
		// it only once that window exists, so a cell that never idled (or
		// idled once and died) reports it as not instrumented rather than
		// passing on a predicate that only ever skipped.
		if snap.IdleWindow >= 2 {
			snap.InstrumentedProperties = appendDeclared(snap.InstrumentedProperties, "I-MEM-2")
		}
		if cfg.ExpectedPanics != nil {
			snap.ExpectedPanics = cfg.ExpectedPanics()
		}
		// I-CONN-1's table is installed by debugvars.NewServer, which every
		// refapp uses -- but "installed" is an assumption about source we
		// do not re-read each run, and a zero age from a missing table is
		// indistinguishable from a zero age with nothing open. Declaring on
		// the first positive tracked count turns that assumption into an
		// observation.
		if snap.OpenConnsTracked > 0 {
			snap.InstrumentedProperties = appendDeclared(snap.InstrumentedProperties, "I-CONN-1")
		}
		if snap.CheckptrBuild {
			snap.InstrumentedProperties = appendDeclared(snap.InstrumentedProperties, "I-CHECKPTR")
		}
		lastGood, haveLast = snap, true
		if cfg.ResponseConformance != nil {
			rc := cfg.ResponseConformance()
			// Declare the two predicates only once the scraper has actually
			// parsed a response. Removing them from the waiver list outright
			// would have made them PASS in every cell where the slice never
			// ran -- structurally-zero counters reported as clean, which is
			// the exact vacuity the waiver list exists to prevent. The
			// checker's DeclaredOnly path already models "instrumented here
			// but not there"; this reuses it.
			if rc.Exchanges > 0 {
				snap.InstrumentedProperties = appendDeclared(snap.InstrumentedProperties, "I-RFC-1", "I-RFC-2")
			}
			snap.ResponsesBadFraming = rc.BadFraming
			snap.ResponsesHeadWithBody = rc.HeadWithBody
			snap.Responses204WithBody = rc.Body204
			snap.Responses304WithBody = rc.Body304
			snap.ResponsesMissingChunkEnd = rc.MissingChunkEnd
			snap.ResponsesCRLFInHeader = rc.CRLFInHeader
			snap.ResponsesNULInHeader = rc.NULInHeader
		}
		series.Record(snap)
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
			// A checkptr throw is only observable after the process has
			// died, which is exactly when polling stops. Give the evaluator
			// one last look at the final good sample, carrying the count
			// the liveness scan collected, so I-CHECKPTR can fire.
			if cfg.CrashReports != nil && haveLast {
				if n := cfg.CrashReports(); n > 0 {
					final := lastGood
					final.CheckptrReports = n
					final.InstrumentedProperties = appendDeclared(final.InstrumentedProperties, "I-CHECKPTR")
					for _, v := range ev.Observe(final, time.Now()) {
						if v.First && cfg.Violations != nil {
							select {
							case cfg.Violations <- Incident{
								Tier: TierProperty, PredicateID: v.ID, Message: v.Message,
								Snapshot: v.Snapshot, ObservedAt: time.Now().UTC(),
								RefappPID: cfg.PID, RecordOnly: !cfg.HardFail,
							}:
							default:
							}
						}
					}
				}
			}
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
