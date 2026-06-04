package report

import (
	"bufio"
	"database/sql"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"

	_ "modernc.org/sqlite"
)

// resources.go is the driver-isolated parse+aggregate layer for the
// server-side resource sampling (probatorium#154). It reads ONE per-cell
// observer.sqlite (the `observations` table cmd/observer writes) and ONE
// per-cell mpstat cpu.log, then folds them into a nullable ResourceStats.
//
// The whole layer depends only on the stdlib + modernc.org/sqlite (pure
// Go, CGO-free, already a dep), so it builds and tests identically on the
// dev Mac and the Linux cluster, and under CGO_ENABLED=0.

// maxSeriesPoints caps the downsampled time-series so a 2-minute (or
// longer) 1 Hz capture stays small in the report JSON.
const maxSeriesPoints = 60

// steadyTrailingFraction is the trailing window over which "steady" RSS
// is taken: the median of the last 80% of samples, dropping the warmup
// ramp. Pinned by the unit test.
const steadyTrailingFraction = 0.8

// ObserverSample is one row of the observer's `observations` table. Only
// the columns the resource aggregate needs are read. The observer always
// writes a concrete int64 (never SQL NULL), writing 0 on an absent or
// failed scrape, so a zero in goroutine/heap/gc is the "treat as absent"
// signal for non-Go competitors.
type ObserverSample struct {
	TSUnix         int64
	FDCount        int64
	RSSBytes       int64
	Goroutines     int64
	HeapInuseBytes int64
	GCPauseP99Ns   int64
}

// CPUPoint is one per-second aggregate-CPU reading parsed from an mpstat
// `all` row. Ordinal is the row's position in the log (mpstat emits
// wall-clock timestamps, so the series is joined positionally with the
// observer rows rather than on absolute time).
type CPUPoint struct {
	Ordinal int
	CPUPct  float64
}

// ParseObserverDB opens a per-cell observer.sqlite read-only and returns
// its `observations` rows ordered by timestamp. A missing file returns
// an error so the caller can treat resource capture as best-effort
// (cluster cells that ran without an observer simply have no DB).
func ParseObserverDB(path string) ([]ObserverSample, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		return nil, fmt.Errorf("open %s: %w", path, err)
	}
	defer func() { _ = db.Close() }()

	rows, err := db.Query(`SELECT ts, fd_count, rss_bytes, goroutine_count, heap_inuse_bytes, gc_pause_p99_ns FROM observations ORDER BY ts`)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", path, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ObserverSample
	for rows.Next() {
		var s ObserverSample
		if err := rows.Scan(&s.TSUnix, &s.FDCount, &s.RSSBytes, &s.Goroutines, &s.HeapInuseBytes, &s.GCPauseP99Ns); err != nil {
			return nil, fmt.Errorf("scan %s: %w", path, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows %s: %w", path, err)
	}
	return out, nil
}

// ParseMPStat parses an `mpstat -P ALL 1 <N>` text log and returns the
// mean aggregate-CPU-busy percentage (100 − %idle over `all` rows) plus
// the per-second series. ok is false (and the caller leaves CPU null)
// when no `all` rows were found.
//
// The %idle and CPU column indices are detected from the header row
// rather than assumed, so the parser is robust to mpstat format drift
// across sysstat versions (AM/PM timestamp prefix, %nice/%gnice
// variants). The trailing `Average:` block is skipped.
func ParseMPStat(path string) (mean float64, series []CPUPoint, ok bool, err error) {
	f, err := os.Open(path)
	if err != nil {
		return 0, nil, false, err
	}
	defer func() { _ = f.Close() }()

	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)

	cpuIdx, idleIdx := -1, -1
	var sum float64
	var n int
	ordinal := 0
	for sc.Scan() {
		fields := strings.Fields(sc.Text())
		if len(fields) == 0 {
			continue
		}
		// The Average: block at the end repeats the per-CPU rows with a
		// running mean; skip it so it does not double-count.
		if strings.HasPrefix(fields[0], "Average") {
			break
		}
		if ci, ii, isHeader := mpstatHeader(fields); isHeader {
			cpuIdx, idleIdx = ci, ii
			continue
		}
		if cpuIdx < 0 || idleIdx < 0 {
			continue
		}
		if cpuIdx >= len(fields) || idleIdx >= len(fields) {
			continue
		}
		if fields[cpuIdx] != "all" {
			continue
		}
		idle, perr := strconv.ParseFloat(fields[idleIdx], 64)
		if perr != nil {
			continue
		}
		busy := 100 - idle
		series = append(series, CPUPoint{Ordinal: ordinal, CPUPct: busy})
		sum += busy
		n++
		ordinal++
	}
	if err := sc.Err(); err != nil {
		return 0, nil, false, err
	}
	if n == 0 {
		return 0, nil, false, nil
	}
	return sum / float64(n), series, true, nil
}

// mpstatHeader detects an mpstat column header row and returns the
// indices of the CPU and %idle columns. A header is any row carrying
// both a "CPU" field and a "%idle" field.
func mpstatHeader(fields []string) (cpuIdx, idleIdx int, ok bool) {
	cpuIdx, idleIdx = -1, -1
	for i, f := range fields {
		switch f {
		case "CPU":
			cpuIdx = i
		case "%idle":
			idleIdx = i
		}
	}
	return cpuIdx, idleIdx, cpuIdx >= 0 && idleIdx >= 0
}

// SummarizeResources folds observer samples + parsed CPU into the
// nullable ResourceStats. zero-or-absent => null is applied to the
// runtime-derived metrics (goroutine/GC/heap): a competitor with no Go
// runtime samples 0 for all of them, so those summary/series fields stay
// nil while RSS/CPU/FD remain populated.
func SummarizeResources(samples []ObserverSample, cpuMean float64, cpuOK bool, cpuSeries []CPUPoint) ResourceStats {
	var stats ResourceStats
	if len(samples) == 0 && !cpuOK {
		return stats
	}

	rss := make([]int64, 0, len(samples))
	var peakRSS, fdHWM, goroutineHWM int64
	var anyGoroutine, anyGC, anyHeap bool
	gc := make([]int64, 0, len(samples))
	for _, s := range samples {
		if s.RSSBytes > peakRSS {
			peakRSS = s.RSSBytes
		}
		if s.RSSBytes > 0 {
			rss = append(rss, s.RSSBytes)
		}
		if s.FDCount > fdHWM {
			fdHWM = s.FDCount
		}
		if s.Goroutines > 0 {
			anyGoroutine = true
			if s.Goroutines > goroutineHWM {
				goroutineHWM = s.Goroutines
			}
		}
		if s.GCPauseP99Ns > 0 {
			anyGC = true
			gc = append(gc, s.GCPauseP99Ns)
		}
		if s.HeapInuseBytes > 0 {
			anyHeap = true
		}
	}

	if peakRSS > 0 {
		stats.Summary.PeakRSSBytes = ptrI64(peakRSS)
	}
	if steady, ok := steadyRSS(rss); ok {
		stats.Summary.SteadyRSSBytes = ptrI64(steady)
	}
	if len(samples) > 0 {
		// FD is always present when any observer row exists (it is a
		// /proc count, zero only when /proc is unreadable).
		stats.Summary.FDHWM = ptrI64(fdHWM)
	}
	if cpuOK {
		stats.Summary.MeanCPUPct = ptrF64(cpuMean)
	}
	if anyGoroutine {
		stats.Summary.GoroutineHWM = ptrI64(goroutineHWM)
	}
	if anyGC {
		stats.Summary.GCPauseP99Ns = ptrI64(percentileI64(gc, 0.99))
	}

	stats.Series = buildSeries(samples, cpuSeries, anyGoroutine, anyHeap)
	return stats
}

// steadyRSS returns the median of the trailing steadyTrailingFraction of
// the RSS samples, dropping the warmup ramp. ok is false when there are
// no non-zero RSS samples.
func steadyRSS(rss []int64) (int64, bool) {
	if len(rss) == 0 {
		return 0, false
	}
	start := int(math.Floor(float64(len(rss)) * (1 - steadyTrailingFraction)))
	if start >= len(rss) {
		start = len(rss) - 1
	}
	tail := append([]int64(nil), rss[start:]...)
	sort.Slice(tail, func(i, j int) bool { return tail[i] < tail[j] })
	mid := len(tail) / 2
	if len(tail)%2 == 1 {
		return tail[mid], true
	}
	return (tail[mid-1] + tail[mid]) / 2, true
}

// buildSeries downsamples the observer rows to ≤maxSeriesPoints and zips
// the positional CPU reading onto each kept point. Runtime-derived
// fields stay nil when the cell carried no goroutine/heap data.
func buildSeries(samples []ObserverSample, cpuSeries []CPUPoint, anyGoroutine, anyHeap bool) []ResourcePoint {
	if len(samples) == 0 {
		return nil
	}
	stride := 1
	if len(samples) > maxSeriesPoints {
		stride = (len(samples) + maxSeriesPoints - 1) / maxSeriesPoints
	}
	out := make([]ResourcePoint, 0, maxSeriesPoints)
	for i := 0; i < len(samples); i += stride {
		s := samples[i]
		p := ResourcePoint{TSUnix: s.TSUnix}
		if s.RSSBytes > 0 {
			p.RSSBytes = ptrI64(s.RSSBytes)
		}
		p.FDCount = ptrI64(s.FDCount)
		if anyGoroutine && s.Goroutines > 0 {
			p.Goroutines = ptrI64(s.Goroutines)
		}
		if anyHeap && s.HeapInuseBytes > 0 {
			p.HeapInuseBytes = ptrI64(s.HeapInuseBytes)
		}
		if i < len(cpuSeries) {
			p.CPUPct = ptrF64(cpuSeries[i].CPUPct)
		}
		out = append(out, p)
	}
	return out
}

// percentileI64 returns the p-quantile (0..1) of xs using
// nearest-rank. xs is not mutated.
func percentileI64(xs []int64, p float64) int64 {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int64(nil), xs...)
	sort.Slice(s, func(i, j int) bool { return s[i] < s[j] })
	idx := int(math.Ceil(p*float64(len(s)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(s) {
		idx = len(s) - 1
	}
	return s[idx]
}

func ptrI64(v int64) *int64     { return &v }
func ptrF64(v float64) *float64 { return &v }
