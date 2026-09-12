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

import (
	"strings"
	"time"
)

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
	GoroutineCount int64
	HeapInuseBytes int64
	HeapAllocBytes int64
	// HeapObjects, HeapIdleBytes, HeapReleasedBytes and StackInuseBytes are
	// recorded but judged by no predicate. They exist so the per-cell series
	// (probatorium#319) can separate the three things a rising HeapInuse can
	// mean: live objects accumulating (HeapObjects rises with HeapAlloc),
	// size-class fragmentation (HeapInuse rises while HeapAlloc does not), and
	// the runtime holding spans back from the OS (HeapIdle / HeapReleased).
	// Reading the 24h soak's I-MEM-1 failure needed exactly this split and the
	// artifact did not carry it.
	HeapObjects       int64
	HeapIdleBytes     int64
	HeapReleasedBytes int64
	StackInuseBytes   int64
	GCPauseP99Ns      int64
	NumGoroutineDiff  int64 // delta from process baseline

	// Connection lifecycle (celeris.* counters)
	AcceptedConnTotal int64
	ClosedConnTotal   int64
	ActiveConns       int64
	PanicCount        int64
	// ExpectedPanics is the number of panics the workload DESIGNED so far
	// (corpus states marked `expect: panic`, counted by the Tier 1 walker
	// when their 5xx arrives). Zero when no accounting is wired (e.g. the
	// standalone checker CLI). I-PANIC judges PanicCount - ExpectedPanics.
	ExpectedPanics int64

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

	// Middleware counters. Published by the refapps that install the
	// matching middleware (validation/refapp/internal/debugvars) and read
	// off /debug/vars in every build; the validation socket below feeds
	// the same fields when celeris is built with -tags=validation.
	RateLimitAllowed     int64
	RateLimitRejected    int64
	SessionsCreatedTotal int64
	SessionsExpiredTotal int64
	JWTValidatedOK       int64
	JWTValidatedFail     int64
	// SessionCookieDrops is celeris middleware/session.DroppedCookies():
	// requests whose id-CHANGING session cookie could not be emitted
	// because the handler had already written the body. The client keeps
	// (or never learns) an id that does not name the session it just
	// authenticated into -- celeris#507. Counted in every build, so unlike
	// SessionOwnerMismatches this one needs no validation tag.
	SessionCookieDrops int64

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

	// InstrumentedProperties is the sorted, comma-joined list of predicate
	// IDs the refapp declares it actually feeds (its
	// celeris.instrumented_properties key). Only the refapps that install a
	// given middleware can judge that middleware's invariant, so a
	// predicate absent from this list is reported as not-instrumented for
	// the cell rather than passing on a structurally-zero counter --
	// probatorium#297, where nine predicates read as clean because nothing
	// ever wrote to them.
	//
	// A string and not a []string on purpose: Snapshot must stay a
	// comparable value type (the rolling History is copied around and the
	// checker's tests compare whole snapshots).
	// OpenConnsTracked is how many connections the refapp's last-byte
	// table currently holds. It is the liveness signal for I-CONN-1's
	// instrumentation: an age of 0 with a non-empty table means every open
	// conn was just active (clean), while an age of 0 with an EMPTY table
	// is indistinguishable from a refapp that never installed the hook.
	// The property loop declares I-CONN-1 only once this goes positive.
	OpenConnsTracked int64
	// CheckptrBuild is celeris.checkptr_build: true only when the refapp was
	// compiled with -tags=checkptr (paired with -d=checkptr). I-CHECKPTR is
	// declared only when it is set.
	CheckptrBuild bool
	// ValidationBuild is true when the refapp was compiled with
	// -tags=validation, which compiles celeris's own assertion counters
	// in. The property loop declares I-ENG-IOURING on it for an io_uring
	// cell; without it IouringSQECorruptions is the stub's zero.
	ValidationBuild bool
	// EngineName is celeris.engine from /debug/vars ("io_uring", "epoll",
	// "std"). Published by every refapp since the document was written and
	// parsed by nothing until I-CONN-1 needed it: the legitimate idle
	// ceiling differs fourfold between the native engines and std, so a
	// single threshold cannot be correct for both.
	EngineName             string
	InstrumentedProperties string

	// IdleWindow is the orchestrator's idle window this sample was taken
	// in: 0 under load, n inside the n-th window (tier1Config.IdleWindows).
	// I-MEM-2 takes the first window as its baseline and judges the rest.
	IdleWindow int
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

	// IdleMode is true while the orchestrator holds an idle window
	// (IdleWindow > 0). Kept alongside IdleWindow for the predicates that
	// only ask whether load is off.
	IdleMode bool

	// IdleWindow is the current orchestrator idle window (0 under load).
	IdleWindow int

	// IdleBaselineGoroutines is the goroutine count at the END of the
	// first idle window: the refapp's own settled idle level, pools and
	// per-listener ladders included, measured after it has served load
	// once. I-MEM-2 judges every later idle window against it. Zero
	// until the first window has been left.
	IdleBaselineGoroutines int64

	// LoadStartedAt is when sustained load began after the first idle
	// window; zero when the cell never idled. The slope predicates anchor
	// their warm-up here when set, so the burst/idle prelude neither
	// eats into the warm-up nor lands its resume transient in a fit.
	LoadStartedAt time.Time

	// BaselineGoroutines is the goroutine count of the first sample after
	// the refapp announced ready. Kept for the soak summary; it is NOT
	// I-MEM-2's reference, because every refapp grows past it under its
	// first load (driver pools, the std engine's per-conn goroutines) and
	// legitimately never comes back down.
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

// skipPrefix marks the msg of an evaluation that reached NO verdict.
const skipPrefix = "skip: "

// Skip is what a predicate returns when it cannot judge this sample:
// the slope window is not full yet, the input was never sampled (RSS
// without a pid), the idle window was never entered. It is ok=true --
// nothing was violated -- but the evaluator counts it separately and a
// predicate that only ever skipped is NOT reported as passed. Before
// this distinction existed a 150 s nightly cell reported I-MEM-1/3/4 as
// passed although none of them had judged a single sample.
func Skip(reason string) (bool, string) { return true, skipPrefix + reason }

// IsSkip reports whether a predicate result is a [Skip].
func IsSkip(ok bool, msg string) bool { return ok && strings.HasPrefix(msg, skipPrefix) }

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

	// Persist is how many CONSECUTIVE failing evaluations (skips do not
	// count either way) the evaluator requires before it declares a
	// violation. 0 or 1 = the first failing sample is a violation. The
	// slope predicates set it to one trough-bucket width so a verdict
	// must survive a complete re-bucketing of the window before it
	// hard-fails a cell.
	Persist int

	// MinObservation is the shortest observation window in which this
	// predicate can reach a verdict AT ALL: below it the predicate
	// skips no matter how healthy or how broken the refapp is. Zero
	// (the default) means it judges from a single snapshot.
	//
	// It exists to tell two silences apart. A predicate that reached no
	// verdict in a cell SHORTER than this could not have: a 150 s
	// nightly cell can never fit I-MEM-1's 5 min warm-up plus its
	// 10 min window, and failing the nightly for that would be failing
	// it for its own tier definition. A predicate that reached no
	// verdict with the time to do so is a coverage failure -- the
	// oracle was silent when it should have spoken, which on an
	// absolute zero-signal gate is not the same as zero signal
	// (probatorium#299).
	MinObservation time.Duration
}

// Forever returns ctx.Now - ctx.RunStartedAt; convenience for predicates
// that gate on "have we been running long enough".
func Forever(ctx Context) time.Duration {
	if ctx.RunStartedAt.IsZero() {
		return 0
	}
	return ctx.Now.Sub(ctx.RunStartedAt)
}
