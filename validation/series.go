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
// rising heap is. See seriesWriter for why that distinction is the whole point.
var seriesColumns = []string{
	"ts", "goroutines", "heap_inuse", "heap_alloc", "heap_objects",
	"heap_idle", "heap_released", "stack_inuse", "rss",
	"accepted", "closed", "active",
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
