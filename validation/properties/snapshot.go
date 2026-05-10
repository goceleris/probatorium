// Package properties defines the per-second invariants the validation
// tier evaluates against celeris under load.
//
// Each invariant is a Predicate — a pure function over a [Snapshot] (one
// row of the observer's sqlite time series, or the live /debug/vars
// poll) plus a [Context] carrying rolling-window state. The orchestrator
// runs every Predicate every second; the first one that returns ok=false
// halts the run, captures forensics, and triggers the auto-bisect.
//
// Bug = (seed, commit, arch). Predicates do not mutate state; the
// runtime owns rolling window history in [Context.History].
package properties

import "time"

// Snapshot is one per-second projection of celeris metrics, /proc fields,
// and synthesised counters from the validator-checker. It is intentionally
// a flat value type so the rolling-window slice in [Context] can be
// indexed cheaply and copied without pointer aliasing.
//
// Sources, in priority order:
//
//   - validator-checker's /debug/vars poll (celeris.* + memstats)
//   - /proc/<pid>/{status,fd,limits} (linux only; zero on darwin)
//   - synthetic counters the validator computes itself (e.g. body
//     parsing hooks via -tags=validation; stubbed to zero until wave 7).
type Snapshot struct {
	// TS is the unix-seconds timestamp the snapshot was sampled.
	TS int64
	// PID is the celeris process id (0 if no /proc sampling).
	PID int

	// Engine + runtime
	GoroutineCount   int64
	HeapInuseBytes   int64
	HeapAllocBytes   int64
	GCPauseP99Ns     int64
	NumGoroutineDiff int64 // delta from process baseline

	// Connection lifecycle (celeris.* counters)
	AcceptedConnTotal int64
	ClosedConnTotal   int64
	ActiveConns       int64
	PanicCount        int64

	// Last-byte timestamps the validator-checker maintains per-conn
	// (only stub-populated until wave 7 adds the validation build tag).
	OldestOpenConnLastByteAgeMs int64

	// Process resources
	FDCount     int64
	RSSBytes    int64
	SoftFDLimit int64

	// Race + checkptr signal counters. Populated by the validator-checker
	// itself when it observes the celeris stderr stream (-race / -checkptr
	// builds emit textual reports the checker greps for).
	RaceReports     int64
	CheckptrReports int64

	// Middleware counters (wave 7 surfaces validation-only counters; for
	// now the checker reads what Prometheus exposes on /metrics and
	// reflects subset coverage here).
	RateLimitAllowed     int64
	RateLimitRejected    int64
	SessionsCreatedTotal int64
	SessionsExpiredTotal int64
	JWTValidatedOK       int64
	JWTValidatedFail     int64

	// Driver shadow counters (validator-driven, not celeris-driven).
	// Incremented by the validator's traffic generator after every
	// driver-touching workload step so the checker can diff celeris's
	// observed driver state against the shadow.
	DriverWritesIssued int64
	DriverReadsIssued  int64
	DriverReadHits     int64
	DriverReadMisses   int64

	// Engine-specific counters. Stubbed (zero) until wave 7's
	// -tags=validation build exposes engine-internal queues / SQEs.
	IOUringSQEsSubmitted int64
	IOUringCQEsCompleted int64
	AdaptiveSwitches     int64

	// HTTP wire-format counters. Populated by the validator's response
	// scraper (it MITMs each adapter under test, parsing bytes).
	ResponsesBadFraming      int64 // CL mismatch, double Transfer-Encoding, etc.
	ResponsesHeadWithBody    int64
	Responses204WithBody     int64
	Responses304WithBody     int64
	ResponsesCRLFInHeader    int64
	ResponsesNULInHeader     int64
	ResponsesMissingChunkEnd int64

	// Validation-build counters (celeris v1.4.3+, -tags=validation).
	// Sourced from the Unix-domain socket at /tmp/celeris-validation.sock.
	// Each is an "assertion fired" count — non-zero means the celeris
	// runtime detected a violation it would normally panic on; the
	// validation build accumulates the count so the checker can capture
	// the seed/commit/arch tuple before the run aborts. All five
	// MUST stay at zero through every soak run.
	RatelimitTokenViolations int64
	SessionOwnerMismatches   int64
	JWTLateAdmits            int64
	IouringSQECorruptions    int64
}

// Context carries rolling-window state needed by predicates that look
// further back than one snapshot — heap-slope tracking, goroutine
// baselines, conn-close deadlines, and the validator's own start-of-run
// reference.
type Context struct {
	// RunStartedAt is the wall time the orchestrator began the run. Used
	// by predicates that need an idle-warmup grace period.
	RunStartedAt time.Time

	// Now is the wall time the snapshot was sampled. Predicates use this
	// (not time.Now()) so deterministic replay reproduces the exact same
	// evaluation order.
	Now time.Time

	// IdleMode is true if the orchestrator is in an idle window
	// (post-warmup, pre-load-resume). I-MEM-2 only fires in this mode.
	IdleMode bool

	// BaselineGoroutines is the goroutine count snapshot taken after
	// celeris is up but before the first request lands. Used by
	// I-MEM-2's "goroutines return to baseline+N" assertion.
	BaselineGoroutines int64

	// History is the rolling window of recent snapshots, most recent
	// last. The orchestrator caps len(History) at 3600 entries (1h at
	// 1Hz), which is enough for I-MEM-1's slope window.
	History []Snapshot
}

// Predicate is the signature every invariant implements. It returns
// ok=true on healthy, ok=false plus a human-readable msg on violation.
//
// Predicates must be pure — no I/O, no goroutines, no time.Now() — so
// `validator-replay` is deterministic against a recorded snapshot trace.
type Predicate func(snap *Snapshot, ctx Context) (ok bool, msg string)

// Spec describes one registered predicate.
type Spec struct {
	// ID is the canonical short name, e.g. "I-CONN-1". Used in incident
	// directory names, the per-second log, and the dry-run printout.
	ID string

	// Description is a one-line description, shown to humans in dry-run
	// and incident reports.
	Description string

	// Tier is the property tier this predicate belongs to: "core",
	// "middleware", "engine", "driver". Tier-gated runs (e.g. wave 6 ->
	// "core" only, wave 7 adds the rest) consult this field.
	Tier string

	// Predicate is the evaluator itself. Required.
	Predicate Predicate
}

// Forever returns ctx.Now - ctx.RunStartedAt; convenience for predicates
// that gate on "have we been running long enough".
func Forever(ctx Context) time.Duration {
	if ctx.RunStartedAt.IsZero() {
		return 0
	}
	return ctx.Now.Sub(ctx.RunStartedAt)
}
