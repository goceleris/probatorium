package validation

import (
	"encoding/csv"
	"os"
	"strconv"

	"github.com/goceleris/probatorium/validation/properties"
)

// seriesFlushEvery is how many rows the writer buffers before flushing. At the
// property loop's 1 Hz cadence that is half a minute of samples, so a cell
// killed mid-run still leaves an almost-complete file rather than an empty one.
const seriesFlushEvery = 30

// seriesColumns is the header, and the order every row is written in.
//
// Deliberately narrow. This is not a dump of Snapshot: it is the inputs the
// slope oracles judge, plus the three columns that say WHICH KIND of growth a
// rising heap is, plus the adaptive switch counter (only the adaptive engine
// moves it; the series shows WHEN a cell promoted, celeris#580), plus the
// engine's own error counter (a step in it at the second of a walker's
// slow-read record is the deaf-listener signature, celeris#588). See
// seriesWriter for why that distinction is the whole point.
//
// celeris#646 split that error counter into eleven cause buckets, and only
// SIX of the twelve new counters are here. The rest are end-of-cell totals on
// Tier1Summary.EngineErrorClasses and nothing else. A bucket earns a column
// when the question asked of it is "when" and the artifact carries something
// timestamped to join that against -- the promotion instant, the transplant
// hand-off, a walker's slow-read fire, or the per-second denominators a rate
// needs. report.ErrorClasses records the call for each of the eleven, one
// bucket at a time, and TestTheSeriesCarriesExactlyTheBucketsDeclaredForIt
// fails if the two ever disagree.
//
// probatorium#391 (schema 5.15) applied the same bar to the rest of
// engine.EngineMetrics and admitted TEN of its twenty keys: the four
// celeris#647 hand-off outcomes, the five recv-stall and linked-recv duration
// columns celeris#607 turned on, and engine_detached_conns, the only gauge.
// report.EngineCounters records the call for each, and
// TestTheSeriesCarriesExactlyTheEngineCountersDeclaredForIt enforces it.
// TestEveryPublishedEngineKeyHasAColumnOrADeclaration fails on a published key
// that has neither a column nor a declaration saying it is tally-only.
//
// New columns are APPENDED, never inserted: the header names every column and
// a reader should key off it, but an appended column cannot invalidate a
// column index somebody already wrote down against a shipped artifact.
var seriesColumns = []string{
	"ts", "goroutines", "heap_inuse", "heap_alloc", "heap_objects",
	"heap_idle", "heap_released", "stack_inuse", "rss",
	"accepted", "closed", "active", "adaptive_switches", "engine_error_count",
	"engine_workers", "engine_requests_total", "engine_bytes_read",
	"engine_bytes_written",
	"engine_accept_count",
	"engine_close_count",
	"engine_async_promoted_conns",
	"engine_standby_active_conns",
	"engine_standby_close_count",
	"engine_transplant_detached",
	"engine_transplant_adopted",
	"engine_transplant_adopt_slot_occupied",
	"engine_close_missing_conn_state",
	"engine_recv_double_armed",
	"engine_recv_cqe_unaccounted",
	"engine_recv_sq_full",
	"engine_recv_stall_episodes",
	"engine_error_accept_fd_limit",
	"engine_error_accept_cancelled",
	"engine_error_accept_other",
	"engine_error_conn_table_cap",
	"engine_error_send_peer_gone",
	"engine_standby_error_count",
	"engine_transplant_handoff_refused",
	"engine_transplant_drain_stopped",
	"engine_transplant_stranded",
	"engine_transplant_adopt_refused",
	"engine_recv_stall_nanos",
	"engine_recv_stall_max_nanos",
	"engine_recv_linked_arms",
	"engine_recv_linked_blocked_nanos",
	"engine_recv_linked_blocked_max_nanos",
	"engine_detached_conns",
}

// seriesWriter appends one row per property-loop sample to a CSV in the cell
// directory (probatorium#319).
//
// The loop already polls every field here once a second and feeds them to the
// predicates, then discards them. That made the 24h soak's only failure
// undiagnosable from its own artifact: I-MEM-1 fits a line to per-150s bucket
// MINIMA of heap_inuse after a 5 minute warm-up, so the quantity that decides
// the verdict is a trough trajectory, and the cell shipped a one-line summary
// and two heap profiles instead. The profiles could not close the gap either --
// at one sample per 512 KB, the 1.88 MB rise the predicate fired on is smaller
// than the profiler's own sampling error.
//
// With the series, a verdict can be re-derived offline and, more usefully,
// attributed: live objects climbing alongside HeapAlloc is a leak, HeapInuse
// climbing without them is size-class fragmentation, and HeapIdle climbing is
// the runtime holding spans back from the OS. Those are different bugs.
//
// An hour at 1 Hz is 3,600 rows, a few hundred KB, against the multi-MB pprof
// files the same directory already carries -- and GitHub compresses artifacts
// on upload, so plain CSV costs nothing over gzip and cannot be truncated into
// unreadability the way a half-written gzip stream can.
//
// Every method tolerates a nil receiver, so the caller needs no branches.
type seriesWriter struct {
	f     *os.File
	w     *csv.Writer
	n     int
	row   []string
	badly bool // a write failed; stop trying, the cell matters more
}

// newSeriesWriter opens path and writes the header. A failure to open is not
// an error the cell should die on -- diagnostics never outrank the run -- so
// it returns nil and the caller's Record calls become no-ops.
func newSeriesWriter(path string) *seriesWriter {
	if path == "" {
		return nil
	}
	f, err := os.Create(path)
	if err != nil {
		return nil
	}
	s := &seriesWriter{f: f, w: csv.NewWriter(f), row: make([]string, len(seriesColumns))}
	if err := s.w.Write(seriesColumns); err != nil {
		_ = f.Close()
		return nil
	}
	return s
}

// Record appends one sample.
func (s *seriesWriter) Record(snap properties.Snapshot) {
	if s == nil || s.badly {
		return
	}
	v := []int64{
		snap.TS, snap.GoroutineCount, snap.HeapInuseBytes, snap.HeapAllocBytes,
		snap.HeapObjects, snap.HeapIdleBytes, snap.HeapReleasedBytes,
		snap.StackInuseBytes, snap.RSSBytes,
		snap.AcceptedConnTotal, snap.ClosedConnTotal, snap.ActiveConns,
		snap.AdaptiveSwitches, snap.EngineErrorCount,
		snap.EngineWorkers, snap.EngineRequestsTotal,
		snap.EngineBytesRead, snap.EngineBytesWritten,
		snap.EngineAcceptCount,
		snap.EngineCloseCount,
		snap.EngineAsyncPromotedConns,
		snap.EngineStandbyActiveConns,
		snap.EngineStandbyCloseCount,
		snap.EngineTransplantDetached,
		snap.EngineTransplantAdopted,
		snap.EngineTransplantAdoptSlotOccupied,
		snap.EngineCloseMissingConnState,
		snap.EngineRecvDoubleArmed,
		snap.EngineRecvCQEUnaccounted,
		snap.EngineRecvSQFull,
		snap.EngineRecvStallEpisodes,
		snap.EngineErrorAcceptFDLimit,
		snap.EngineErrorAcceptCancelled,
		snap.EngineErrorAcceptOther,
		snap.EngineErrorConnTableCap,
		snap.EngineErrorSendPeerGone,
		snap.EngineStandbyErrorCount,
		snap.EngineTransplantHandoffRefused,
		snap.EngineTransplantDrainStopped,
		snap.EngineTransplantStranded,
		snap.EngineTransplantAdoptRefused,
		snap.EngineRecvStallNanos,
		snap.EngineRecvStallMaxNanos,
		snap.EngineRecvLinkedArms,
		snap.EngineRecvLinkedBlockedNanos,
		snap.EngineRecvLinkedBlockedMaxNanos,
		snap.EngineDetachedConns,
	}
	for i, x := range v {
		s.row[i] = strconv.FormatInt(x, 10)
	}
	if err := s.w.Write(s.row); err != nil {
		s.badly = true
		return
	}
	s.n++
	if s.n%seriesFlushEvery == 0 {
		s.w.Flush()
	}
}

// Close flushes and closes. Safe on a nil receiver and safe to call twice.
func (s *seriesWriter) Close() {
	if s == nil || s.f == nil {
		return
	}
	s.w.Flush()
	_ = s.f.Close()
	s.f = nil
}
