package checker

import (
	"sort"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// HistoryCap is the rolling-window length of [properties.Context.History]:
// 3600 samples = 1h at 1 Hz, the longest window any predicate uses.
const HistoryCap = 3600

// Uninstrumented lists the registered predicates that have NO data
// source in the current deployment, keyed by ID with the reason. They
// are still evaluated (and pass vacuously) so plan.json and the tally
// keep listing them, but the tally reports them separately and does
// not count them as passed: a predicate whose input is structurally
// zero has not verified anything.
//
// Instrumented today (refapp /debug/vars + orchestrator /proc):
// I-CONN-2, I-MEM-1, I-MEM-3, I-MEM-4 (linux local driver only -- the
// Tally also lists it as not instrumented when RSS was never sampled),
// I-PANIC, I-ENG-ADAPTIVE.
var Uninstrumented = map[string]string{
	"I-CONN-1":       "needs a per-connection last-byte table (OldestOpenConnLastByteAgeMs is never populated)",
	"I-RFC-1":        "needs the response-scraping MITM (Responses* counters are never populated)",
	"I-RFC-2":        "needs the response-scraping MITM (Responses* counters are never populated)",
	"I-RACE":         "refapps are not built with -race and no stderr marker counter exists",
	"I-CHECKPTR":     "refapps are not built with -d=checkptr and no stderr marker counter exists",
	"I-MEM-2":        "needs an orchestrator-driven idle window (Context.IdleMode is never set)",
	"I-MW-RATELIMIT": "counters live only in -tags=validation builds served over the unix socket; refapps are plain builds",
	"I-MW-SESSION":   "counters live only in -tags=validation builds served over the unix socket; refapps are plain builds",
	"I-MW-JWT":       "counters live only in -tags=validation builds served over the unix socket; refapps are plain builds",
	"I-ENG-IOURING":  "SQE/CQE counters and the sqe_corruptions assertion exist only in -tags=validation builds",
	"I-DRV":          "needs the driver shadow map (Driver* counters are never populated)",
}

// Violation is one failed predicate evaluation.
type Violation struct {
	ID       string
	Message  string
	Snapshot properties.Snapshot
	// First is true for the first violation of this ID in the run; the
	// orchestrator emits one Incident per ID and counts the rest.
	First bool
}

// Tally is the per-run summary of an Evaluator. JSON tags match the
// tier1_tally.json sidecar.
type Tally struct {
	// Samples is the number of snapshots observed (successful polls).
	Samples int64 `json:"samples"`
	// PollErrors is the number of polls that returned no snapshot
	// (transport error, non-200, unparseable body).
	PollErrors int64 `json:"poll_errors"`
	// Evaluations is the total predicate evaluations (Samples x specs).
	Evaluations int64 `json:"evaluations"`
	// Violations is the total number of failed evaluations across all
	// predicates (one per sample per predicate while the violation
	// persists), NOT the number of distinct predicates.
	Violations int64 `json:"violations"`
	// ViolationIDs are the distinct predicate IDs that failed at least
	// once, sorted.
	ViolationIDs []string `json:"violation_ids,omitempty"`
	// Predicates are the IDs that were evaluated, sorted.
	Predicates []string `json:"predicates,omitempty"`
	// NotInstrumented are the evaluated IDs listed in Uninstrumented:
	// they passed vacuously and are excluded from Passed.
	NotInstrumented []string `json:"not_instrumented,omitempty"`
	// FailureSummaries maps a failed predicate ID to its FIRST violation
	// message.
	FailureSummaries map[string]string `json:"failure_summaries,omitempty"`
	// PerPredicate maps every evaluated ID to its violation count.
	PerPredicate map[string]int64 `json:"per_predicate,omitempty"`

	// Baseline / end-of-run resource points for the soak summary.
	BaselineGoroutines int64 `json:"baseline_goroutines"`
	LastGoroutines     int64 `json:"last_goroutines"`
	FirstHeapInuse     int64 `json:"first_heap_inuse_bytes"`
	LastHeapInuse      int64 `json:"last_heap_inuse_bytes"`
	FirstRSS           int64 `json:"first_rss_bytes"`
	LastRSS            int64 `json:"last_rss_bytes"`
}

// Passed is the number of instrumented predicates that were evaluated
// at least once and never failed.
func (t Tally) Passed() int {
	if t.Samples == 0 {
		return 0
	}
	failed := map[string]bool{}
	for _, id := range t.ViolationIDs {
		failed[id] = true
	}
	not := map[string]bool{}
	for _, id := range t.NotInstrumented {
		not[id] = true
	}
	n := 0
	for _, id := range t.Predicates {
		if !failed[id] && !not[id] {
			n++
		}
	}
	return n
}

// Failed is the number of distinct predicates that failed at least once.
func (t Tally) Failed() int { return len(t.ViolationIDs) }

// Evaluator drives the selected predicates over a stream of snapshots.
// Not safe for concurrent use; the property loop owns one per cell.
type Evaluator struct {
	specs []properties.Spec
	ctx   properties.Context
	tally Tally
	per   map[string]int64
	first map[string]string
}

// NewEvaluator returns an Evaluator over specs (typically
// SelectPredicates(tier)).
func NewEvaluator(specs []properties.Spec) *Evaluator {
	e := &Evaluator{
		specs: specs,
		per:   map[string]int64{},
		first: map[string]string{},
	}
	for _, s := range specs {
		e.per[s.ID] = 0
	}
	return e
}

// RecordPollError counts a poll that produced no snapshot.
func (e *Evaluator) RecordPollError() { e.tally.PollErrors++ }

// Observe appends snap to the rolling History, updates the Context
// (RunStartedAt and BaselineGoroutines come from the FIRST observed
// sample -- the first successful poll after the refapp announced ready)
// and evaluates every spec. Returns the violations for this sample.
func (e *Evaluator) Observe(snap properties.Snapshot, now time.Time) []Violation {
	if e.tally.Samples == 0 {
		e.ctx.RunStartedAt = now
		e.ctx.BaselineGoroutines = snap.GoroutineCount
		e.tally.BaselineGoroutines = snap.GoroutineCount
		e.tally.FirstHeapInuse = snap.HeapInuseBytes
		e.tally.FirstRSS = snap.RSSBytes
	}
	e.tally.Samples++
	e.tally.LastGoroutines = snap.GoroutineCount
	e.tally.LastHeapInuse = snap.HeapInuseBytes
	if snap.RSSBytes > 0 {
		if e.tally.FirstRSS == 0 {
			e.tally.FirstRSS = snap.RSSBytes
		}
		e.tally.LastRSS = snap.RSSBytes
	}
	e.ctx.Now = now
	e.ctx.History = append(e.ctx.History, snap)
	if len(e.ctx.History) > HistoryCap {
		e.ctx.History = e.ctx.History[len(e.ctx.History)-HistoryCap:]
	}

	var out []Violation
	for _, s := range e.specs {
		e.tally.Evaluations++
		ok, msg := s.Predicate(&snap, e.ctx)
		if ok {
			continue
		}
		e.tally.Violations++
		first := e.per[s.ID] == 0
		e.per[s.ID]++
		if first {
			e.first[s.ID] = msg
		}
		out = append(out, Violation{ID: s.ID, Message: msg, Snapshot: snap, First: first})
	}
	return out
}

// Context returns a copy of the current [properties.Context] (History
// shares the backing array; callers must not mutate it).
func (e *Evaluator) Context() properties.Context { return e.ctx }

// Tally returns the current summary.
func (e *Evaluator) Tally() Tally {
	t := e.tally
	t.Predicates = make([]string, 0, len(e.specs))
	t.PerPredicate = make(map[string]int64, len(e.specs))
	for _, s := range e.specs {
		t.Predicates = append(t.Predicates, s.ID)
		t.PerPredicate[s.ID] = e.per[s.ID]
		if _, ok := Uninstrumented[s.ID]; ok {
			t.NotInstrumented = append(t.NotInstrumented, s.ID)
		} else if s.ID == properties.IMEM4.ID && e.tally.LastRSS == 0 {
			// RSS was never sampled (no pid, non-linux, remote refapp):
			// I-MEM-4 skipped on every tick and verified nothing.
			t.NotInstrumented = append(t.NotInstrumented, s.ID)
		}
		if n := e.per[s.ID]; n > 0 {
			t.ViolationIDs = append(t.ViolationIDs, s.ID)
			if t.FailureSummaries == nil {
				t.FailureSummaries = map[string]string{}
			}
			t.FailureSummaries[s.ID] = e.first[s.ID]
		}
	}
	sort.Strings(t.Predicates)
	sort.Strings(t.NotInstrumented)
	sort.Strings(t.ViolationIDs)
	return t
}
