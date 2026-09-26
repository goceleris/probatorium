package validation

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/properties"
)

// TestSeriesWriterRecordsEveryColumn is the guard that the series actually
// carries what it exists for. probatorium#319 was filed because the cell
// shipped a summary line and two heap profiles and neither could attribute a
// rising heap; a series that dropped heap_objects or heap_idle would leave the
// same gap while looking like it had closed it.
func TestSeriesWriterRecordsEveryColumn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.csv")
	w := newSeriesWriter(path)
	if w == nil {
		t.Fatal("newSeriesWriter returned nil for a writable path")
	}
	snap := properties.Snapshot{
		TS: 1700000000, GoroutineCount: 61,
		HeapInuseBytes: 22_471_000, HeapAllocBytes: 19_960_000, HeapObjects: 249_994,
		HeapIdleBytes: 8_090_000, HeapReleasedBytes: 7_659_520, StackInuseBytes: 1_507_328,
		RSSBytes:          65_638_400,
		AcceptedConnTotal: 216_093, ClosedConnTotal: 216_044, ActiveConns: 49,
		AdaptiveSwitches: 1, EngineErrorCount: 1_031,
		EngineWorkers: 2, EngineRequestsTotal: 4_812_004,
		EngineBytesRead: 901_000_000, EngineBytesWritten: 1_402_000_000,
		EngineAcceptCount:                 9_400,
		EngineCloseCount:                  9_371,
		EngineAsyncPromotedConns:          44,
		EngineStandbyActiveConns:          19,
		EngineStandbyCloseCount:           1_208,
		EngineTransplantDetached:          96,
		EngineTransplantAdopted:           97,
		EngineTransplantAdoptSlotOccupied: 3,
		EngineCloseMissingConnState:       5,
		EngineRecvDoubleArmed:             7,
		EngineRecvCQEUnaccounted:          11,
		EngineRecvSQFull:                  13,
		EngineRecvStallEpisodes:           17,
		EngineErrorAcceptFDLimit:          23,
		EngineErrorAcceptCancelled:        29,
		EngineErrorAcceptOther:            31,
		EngineErrorConnTableCap:           37,
		EngineErrorSendPeerGone:           977,
		EngineStandbyErrorCount:           41,
		EngineTransplantHandoffRefused:    43,
		EngineTransplantDrainStopped:      47,
		EngineTransplantStranded:          53,
		EngineTransplantAdoptRefused:      59,
		EngineRecvStallNanos:              67,
		EngineRecvStallMaxNanos:           71,
		EngineRecvLinkedArms:              73,
		EngineRecvLinkedBlockedNanos:      79,
		EngineRecvLinkedBlockedMaxNanos:   83,
		EngineDetachedConns:               89,
		EngineStaleRecvDataTransplanted:   101,
		EngineStaleRecvDataUnattributed:   103,
		EngineTransplantHandoffInFlight:   107,
		EngineTransplantHoldRescued:       109,
		EngineTransplantDoubleClaim:       113,
		EngineTransplantReapFailed:        127,
		EngineTransplantSweepPasses:       131,
		EngineTransplantResidualDetached:  137,
		EngineTransplantResidualH2:        139,
		EngineTransplantResidualPinned:    149,
		EngineTransplantResidualUnstarted: 151,
		EngineTransplantResidualBusy:      157,
	}
	w.Record(snap)
	w.Close()

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(rows) != 2 {
		t.Fatalf("rows = %d, want header + 1 sample", len(rows))
	}
	if strings.Join(rows[0], ",") != strings.Join(seriesColumns, ",") {
		t.Fatalf("header = %v, want %v", rows[0], seriesColumns)
	}
	want := map[string]int64{
		"ts": 1700000000, "goroutines": 61,
		"heap_inuse": 22_471_000, "heap_alloc": 19_960_000, "heap_objects": 249_994,
		"heap_idle": 8_090_000, "heap_released": 7_659_520, "stack_inuse": 1_507_328,
		"rss": 65_638_400, "accepted": 216_093, "closed": 216_044, "active": 49,
		"adaptive_switches": 1, "engine_error_count": 1_031,
		"engine_workers": 2, "engine_requests_total": 4_812_004,
		"engine_bytes_read": 901_000_000, "engine_bytes_written": 1_402_000_000,
		"engine_accept_count":                   9_400,
		"engine_close_count":                    9_371,
		"engine_async_promoted_conns":           44,
		"engine_standby_active_conns":           19,
		"engine_standby_close_count":            1_208,
		"engine_transplant_detached":            96,
		"engine_transplant_adopted":             97,
		"engine_transplant_adopt_slot_occupied": 3,
		"engine_close_missing_conn_state":       5,
		"engine_recv_double_armed":              7,
		"engine_recv_cqe_unaccounted":           11,
		"engine_recv_sq_full":                   13,
		"engine_recv_stall_episodes":            17,
		"engine_error_accept_fd_limit":          23,
		"engine_error_accept_cancelled":         29,
		"engine_error_accept_other":             31,
		"engine_error_conn_table_cap":           37,
		"engine_error_send_peer_gone":           977,
		"engine_standby_error_count":            41,
		"engine_transplant_handoff_refused":     43,
		"engine_transplant_drain_stopped":       47,
		"engine_transplant_stranded":            53,
		"engine_transplant_adopt_refused":       59,
		"engine_recv_stall_nanos":               67,
		"engine_recv_stall_max_nanos":           71,
		"engine_recv_linked_arms":               73,
		"engine_recv_linked_blocked_nanos":      79,
		"engine_recv_linked_blocked_max_nanos":  83,
		"engine_detached_conns":                 89,
		"engine_stale_recv_data_transplanted":   101,
		"engine_stale_recv_data_unattributed":   103,
		"engine_transplant_handoff_in_flight":   107,
		"engine_transplant_hold_rescued":        109,
		"engine_transplant_double_claim":        113,
		"engine_transplant_reap_failed":         127,
		"engine_transplant_sweep_passes":        131,
		"engine_transplant_residual_detached":   137,
		"engine_transplant_residual_h2":         139,
		"engine_transplant_residual_pinned":     149,
		"engine_transplant_residual_unstarted":  151,
		"engine_transplant_residual_busy":       157,
	}
	if len(want) != len(seriesColumns) {
		t.Fatalf("test covers %d columns but the writer has %d — a new column needs a case here",
			len(want), len(seriesColumns))
	}
	// Every expected value is distinct and nonzero, which is what makes the
	// per-column comparison below able to catch a mis-wire at all. A column
	// expected to be 0 passes against a writer that never writes it, and two
	// columns expecting the same number pass against a writer that swaps
	// them -- which was the state of engine_transplant_detached and
	// engine_transplant_adopted, adjacent and both 96, until celeris#646
	// added six more columns and this check with them.
	seen := make(map[int64]string, len(want))
	for col, v := range want {
		if v == 0 {
			t.Errorf("column %q expects 0; a zero fixture cannot tell a wired column from an unwired one", col)
		}
		if other, dup := seen[v]; dup {
			t.Errorf("columns %q and %q both expect %d; swapping them would pass", other, col, v)
		}
		seen[v] = col
	}
	for i, col := range rows[0] {
		got, err := strconv.ParseInt(rows[1][i], 10, 64)
		if err != nil {
			t.Fatalf("column %q: %v", col, err)
		}
		if got != want[col] {
			t.Errorf("column %q = %d, want %d", col, got, want[col])
		}
	}
}

// TestSeriesWriterSurvivesAnUnflushedKill covers the reason the writer flushes
// periodically rather than only at Close: a cell that is cancelled, wedges, or
// has its process killed must still leave a readable series behind. Writing
// past one flush boundary and then abandoning the writer without closing it
// stands in for that.
func TestSeriesWriterSurvivesAnUnflushedKill(t *testing.T) {
	path := filepath.Join(t.TempDir(), "series.csv")
	w := newSeriesWriter(path)
	if w == nil {
		t.Fatal("newSeriesWriter returned nil")
	}
	const wrote = seriesFlushEvery + 5
	for i := range wrote {
		w.Record(properties.Snapshot{TS: int64(i), HeapInuseBytes: int64(i) * 1024})
	}
	// Deliberately no Close.

	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer func() { _ = f.Close() }()
	rows, err := csv.NewReader(f).ReadAll()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	// Everything up to the last flush boundary must be on disk. The tail
	// between that boundary and the kill is expected to be missing.
	if got := len(rows) - 1; got < seriesFlushEvery {
		t.Fatalf("only %d samples survived an unflushed kill, want at least %d "+
			"(the writer is not flushing periodically)", got, seriesFlushEvery)
	}
}

// TestSeriesWriterIsOptionalAndNilSafe: an empty path disables the series, and
// every method must tolerate the nil that produces. Diagnostics never outrank
// the run, so an unwritable path must not take a cell down either.
func TestSeriesWriterIsOptionalAndNilSafe(t *testing.T) {
	if w := newSeriesWriter(""); w != nil {
		t.Fatal("empty path should disable the series")
	}
	if w := newSeriesWriter(filepath.Join(t.TempDir(), "no-such-dir", "series.csv")); w != nil {
		t.Fatal("an unopenable path should disable the series, not return a writer")
	}
	var nilw *seriesWriter
	nilw.Record(properties.Snapshot{})
	nilw.Close()
	nilw.Close()
}

// The celeris#646 error-class split is a judgement about which buckets are
// worth a column written once a second for a whole cell, and that judgement
// is written down in report.ErrorClasses. It is enforced here rather than
// trusted: a bucket promoted to Series there and forgotten in seriesColumns
// publishes a decision the artifact does not carry, and a bucket added to
// seriesColumns without the declaration carries a column nothing explains.
func TestTheSeriesCarriesExactlyTheBucketsDeclaredForIt(t *testing.T) {
	inSeries := make(map[string]bool, len(seriesColumns))
	for _, c := range seriesColumns {
		inSeries[c] = true
	}
	var carried int
	for name, class := range report.ErrorClasses {
		switch {
		case class.Series && !inSeries[name]:
			t.Errorf("report.ErrorClasses marks %q as a series bucket and seriesColumns does not carry it", name)
		case !class.Series && inSeries[name]:
			t.Errorf("seriesColumns carries %q but report.ErrorClasses declares it tally-only", name)
		case class.Series:
			carried++
		}
		if class.Counts == "" {
			t.Errorf("error class %q says nothing about what it counts", name)
		}
		if class.Why == "" {
			t.Errorf("error class %q does not justify Series=%v; the next bucket has to make the same call", name, class.Series)
		}
	}
	if want := len(report.SeriesErrorClasses()); carried != want {
		t.Errorf("series carries %d declared buckets, want %d", carried, want)
	}
	// The adaptive engine's share-by-sub-engine split is not one of the
	// eleven cause buckets (it cuts the same total along the other axis),
	// so it is not in report.ErrorClasses and the loop above cannot see
	// it. It is in the series for the same reason its two siblings
	// engine_standby_active_conns and engine_standby_close_count are:
	// celeris#645 needs to know which sub-engine an error landed on AT
	// THE SECOND the accept-side buckets moved.
	if !inSeries["engine_standby_error_count"] {
		t.Error("seriesColumns is missing engine_standby_error_count")
	}
}
