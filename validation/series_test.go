package validation

import (
	"encoding/csv"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

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
	}
	if len(want) != len(seriesColumns) {
		t.Fatalf("test covers %d columns but the writer has %d — a new column needs a case here",
			len(want), len(seriesColumns))
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
