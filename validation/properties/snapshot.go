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

	// Connection lifecycle (celeris.* counters)
	AcceptedConnTotal int64
	ClosedConnTotal   int64
	ActiveConns       int64
	PanicCount        int64
	// EngineErrorCount is celeris EngineMetrics.ErrorCount. Judged by no
	// predicate; recorded in the per-cell series so a step in it can be
	// joined against a walker's slow-read record (celeris#588).
	//
	// Since celeris#646 it is DERIVED: the exact sum of the eleven
	// EngineError* buckets below, with no separate running total that
	// could drift from its parts. Read it for how much, and the buckets
	// for what.
	EngineErrorCount int64
	// The eleven cause buckets ErrorCount is the sum of (celeris#646).
	// celeris#645 is why they exist: the adaptive engine recorded 421
	// engine errors in a 112-second cell against io_uring's 63 and
	// epoll's 0, and a single counter could bound the answer but never
	// name it.
	//
	// Judged by NO predicate and gated by nothing. Unlike the
	// must-stay-zero witnesses further down, every one of these counts
	// something that legitimately happens — an abandoned response, an
	// accept cancelled by the PauseAccept a promotion performs, a
	// handler that returned an error — so a threshold before a run has
	// said what normal looks like would be a number nobody measured.
	// report.ErrorClasses carries what each one means and which of them
	// the per-cell series samples at 1 Hz.
	EngineErrorAcceptFDLimit    int64
	EngineErrorAcceptCancelled  int64
	EngineErrorAcceptOther      int64
	EngineErrorConnTableCap     int64
	EngineErrorConnRegister     int64
	EngineErrorListenerRecreate int64
	EngineErrorTransplantAdopt  int64
	EngineErrorSendPeerGone     int64
	EngineErrorSend             int64
	EngineErrorRequestBody      int64
	EngineErrorHandler          int64
	// EngineStandbyErrorCount is the share of EngineErrorCount the
	// adaptive engine's STANDBY sub-engine contributed — the same split
	// EngineStandbyActiveConns and EngineStandbyCloseCount apply to the
	// live gauge and the close count, and the other half of celeris#645's
	// question. The buckets say WHAT went wrong; this says which
	// sub-engine it went wrong on. Zero on every non-adaptive engine.
	//
	// Deliberately NOT a member of the eleven above: it cuts the same
	// total along a different axis, so summing it with them would double
	// count.
	EngineStandbyErrorCount int64
	// EngineWorkers, EngineRequestsTotal, EngineBytesRead and
	// EngineBytesWritten are celeris EngineMetrics.Workers, RequestCount,
	// BytesRead and BytesWritten: the four inputs the adaptive controller
	// turns into its two promotion signals (conns/worker = ActiveConns /
	// Workers, and bytes/req = delta(BytesRead+BytesWritten) /
	// delta(RequestCount)). Judged by no predicate; recorded so an
	// adaptive cell that never promoted can be told apart from one that
	// was never offered enough load, and from one the controller
	// deliberately suppressed as link-bound.
	EngineWorkers       int64
	EngineRequestsTotal int64
	EngineBytesRead     int64
	EngineBytesWritten  int64
	// Engine-side connection accounting and the transplant hand-off.
	// EngineAcceptCount / EngineCloseCount are the engine's own view of
	// what the refapp counts through OnConnect / OnDisconnect, so the
	// two together attribute an I-CONN-2 drift instead of merely
	// reporting it (celeris#624): the engine running ahead means a
	// close skipped its hook, the two agreeing while `active` is short
	// means a connection was detached and never re-adopted. The standby
	// pair splits the adaptive engine's two halves, which its Metrics()
	// otherwise sums (celeris#627).
	EngineAcceptCount        int64
	EngineCloseCount         int64
	EngineAsyncPromotedConns int64
	EngineStandbyActiveConns int64
	EngineStandbyCloseCount  int64
	EngineTransplantDetached int64
	EngineTransplantAdopted  int64
	// Must-stay-zero defect witnesses, each naming one specific defect:
	// the adoption path finding its slot occupied, a close decrementing
	// the live gauge with no connection state so the hook is skipped
	// (both celeris#624), a second recv armed while one is in flight and
	// a recv completion accounting to nothing (both celeris#484), and
	// the two celeris#607 guards. Judged by ZeroWitness in the gate: a
	// nonzero value is the defect firing, not a note.
	EngineTransplantAdoptSlotOccupied int64
	EngineCloseMissingConnState       int64
	EngineRecvDoubleArmed             int64
	EngineRecvCQEUnaccounted          int64
	EngineRecvSQFull                  int64
	EngineRecvStallEpisodes           int64
	// The rest of engine.EngineMetrics (probatorium#391). probatorium#386
	// published all fifty-two fields, and twenty of them stopped HERE: this
	// struct did not have them, so ParseDebugVars had nowhere to put them
	// and neither the series nor the tally could carry them. Judged by no
	// predicate and gated by nothing; report.EngineCounters says what each
	// counts, how its readings combine, and whether the per-cell series
	// samples it. Named "Engine" + the EngineMetrics field, except
	// EngineDetachedConns, which mirrors its debugvars key. The one
	// published key with no field is celeris.engine_throughput, and
	// checker.EngineKeysNotParsed says why.
	//
	// The four hand-off outcomes celeris#647 added so its celeris#624 fix is
	// falsifiable. Stranded must stay zero; the other three are recoveries.
	EngineTransplantHandoffRefused int64
	EngineTransplantDrainStopped   int64
	EngineTransplantStranded       int64
	EngineTransplantAdoptRefused   int64
	// The celeris#484 resume window -- the exposure witnesses for
	// EngineRecvDoubleArmed -- and the #560 guard standing in it.
	EngineRecvResumeWhileCancelPending int64
	EngineRecvResumeWhileRecvInFlight  int64
	EngineRecvArmDeclined              int64
	// The duration half of the celeris#607 recv stall, and the linked-recv
	// chain #607 was solved on. The two *MaxNanos are engine-side RUNNING
	// MAXIMA: never sum them, never difference them into a rate. The
	// nanosecond totals decode through a JSON float64, so they are exact
	// only below 2^53 ns (about 104 days of summed stall), which no cell
	// approaches.
	EngineRecvStallNanos            int64
	EngineRecvStallMaxNanos         int64
	EngineRecvLinkedArms            int64
	EngineRecvLinkedBlockedNanos    int64
	EngineRecvLinkedBlockedMaxNanos int64
	// Detach accounting (celeris#549, celeris#584). EngineDetachedConns is
	// a GAUGE, the only one in this block.
	EngineDetachedConns      int64
	EngineDetachWindowCloses int64
	// The io_uring egress split: zero-copy exposure, and the bytes that
	// went out inline versus through the ring.
	EngineZCSendsSubmitted int64
	EngineZCNotifs         int64
	EngineInlineBytes      int64
	EngineRingBytes        int64
	// EngineAsyncRoutes is static after Listen: the denominator for
	// EngineAsyncPromotedConns.
	EngineAsyncRoutes int64
	// ExpectedPanics is the number of panics the workload DESIGNED so far
	// (corpus states marked `expect: panic`, counted by the Tier 1 walker
	// when their 5xx arrives). Zero when no accounting is wired (e.g. the
	// standalone checker CLI). I-PANIC judges PanicCount - ExpectedPanics.
	ExpectedPanics int64

	// Last-byte timestamps the validator-checker maintains per-conn
	// (only stub-populated until wave 7 adds the validation build tag).
	OldestOpenConnLastByteAgeMs int64

	// Process resources. RSSBytes is the refapp's VmRSS, read from
	// /proc/<pid>/status by checker.ReadRSS (validation/propertyloop.go and
	// cmd/validator-checker) and judged by I-MEM-4. The fd and GC-pause series
	// live on report.ResourceSample, filled by cmd/observer. FDCount,
	// SoftFDLimit, GCPauseP99Ns and NumGoroutineDiff used to be declared here
	// as well, and nothing wrote or read these copies; they only made a name
	// search look as if a predicate could judge them (probatorium#395). A field
	// belongs here once something feeds it:
	// TestEveryFieldReadFromASnapshotHasAWriter, in the nested module
	// validation/properties/fieldguard, fails on one that is read and never
	// written.
	RSSBytes int64

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

	// Engine-specific counters.
	//
	// IOUringSQEsSubmitted and IOUringCQEsCompleted used to sit here,
	// declared "stubbed (zero) until wave 7's -tags=validation build
	// exposes engine-internal queues / SQEs". That build shipped and never
	// fed them, so I-ENG-IOURING's two accounting branches compared 0 with
	// 0 on every evaluation and could not fail (probatorium#395). They are
	// gone rather than left reading zero: celeris publishes no SQE/CQE totals
	// today, and wiring them needs new celeris counters first (celeris#688).
	AdaptiveSwitches int64

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
	// RaceBuild is true when the refapp was compiled with -race. The
	// property loop declares I-RACE on it and feeds RaceReports from the
	// liveness scan; without it the count is structurally zero.
	RaceBuild bool
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
