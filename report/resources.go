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
	"time"

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

	// CPUTicks is the SUT process's cumulative utime+stime from
	// /proc/<pid>/stat in USER_HZ ticks (100/s on every Linux arch the
	// bench runs), written by observers that carry the cpu_utime_ticks /
	// cpu_stime_ticks columns (schema v5.9, celeris#585). CPUTicksOK is
	// false when the DB predates those columns, so a legitimate 0 tick
	// reading is never confused with "absent".
	CPUTicks   int64
	CPUTicksOK bool
}

// userHZ is the Linux USER_HZ the /proc/<pid>/stat utime/stime fields
// are expressed in. It is a fixed kernel ABI constant (100) on x86_64 and
// arm64 regardless of the scheduler HZ, which is why the observer stores
// raw ticks and the parser divides here.
const userHZ = 100

// CPUPoint is one per-second aggregate-CPU reading parsed from an mpstat
// `all` row. Ordinal is the row's position in the log (mpstat emits
// wall-clock timestamps, so the series is joined positionally with the
// observer rows rather than on absolute time).
type CPUPoint struct {
	Ordinal int
	CPUPct  float64

	// TSUnix is the row's wall-clock instant (unix seconds) reconstructed
	// from the log's banner date plus the row's HH:MM:SS[ AM|PM] prefix,
	// midnight-rollover aware. mpstat prints the SUT's LOCAL time with no
	// zone; the bench launches it under TZ=UTC S_TIME_FORMAT=ISO
	// (run_bench_cell.yml) so this is UTC and joins the runner's UTC
	// started_at/completed_at. 0 when no banner date or row time could be
	// parsed, in which case per-scenario windowing is impossible.
	TSUnix int64

	// SoftPct is the row's mpstat %soft (softirq) column; -1 when the log
	// carries no %soft column.
	SoftPct float64
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

	// The cpu tick columns (schema v5.9) are optional: a DB written by an
	// older observer has no such column and SELECTing it would fail the
	// whole parse, so probe the table layout first and only read the
	// ticks when both columns exist.
	hasTicks, err := observerHasCPUTicks(db)
	if err != nil {
		return nil, fmt.Errorf("table_info %s: %w", path, err)
	}
	q := `SELECT ts, fd_count, rss_bytes, goroutine_count, heap_inuse_bytes, gc_pause_p99_ns FROM observations ORDER BY ts`
	if hasTicks {
		q = `SELECT ts, fd_count, rss_bytes, goroutine_count, heap_inuse_bytes, gc_pause_p99_ns, cpu_utime_ticks, cpu_stime_ticks FROM observations ORDER BY ts`
	}
	rows, err := db.Query(q)
	if err != nil {
		return nil, fmt.Errorf("query %s: %w", path, err)
	}
	defer func() { _ = rows.Close() }()

	var out []ObserverSample
	for rows.Next() {
		var s ObserverSample
		if hasTicks {
			var ut, st sql.NullInt64
			if err := rows.Scan(&s.TSUnix, &s.FDCount, &s.RSSBytes, &s.Goroutines, &s.HeapInuseBytes, &s.GCPauseP99Ns, &ut, &st); err != nil {
				return nil, fmt.Errorf("scan %s: %w", path, err)
			}
			if ut.Valid && st.Valid {
				s.CPUTicks = ut.Int64 + st.Int64
				s.CPUTicksOK = true
			}
		} else if err := rows.Scan(&s.TSUnix, &s.FDCount, &s.RSSBytes, &s.Goroutines, &s.HeapInuseBytes, &s.GCPauseP99Ns); err != nil {
			return nil, fmt.Errorf("scan %s: %w", path, err)
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows %s: %w", path, err)
	}
	return out, nil
}

// observerHasCPUTicks reports whether the observations table carries the
// cpu_utime_ticks AND cpu_stime_ticks columns (observer >= schema v5.9).
func observerHasCPUTicks(db *sql.DB) (bool, error) {
	rows, err := db.Query(`PRAGMA table_info(observations)`)
	if err != nil {
		return false, err
	}
	defer func() { _ = rows.Close() }()
	var haveU, haveS bool
	for rows.Next() {
		var cid, notnull, pk int
		var name, ctype string
		var dflt sql.NullString
		if err := rows.Scan(&cid, &name, &ctype, &notnull, &dflt, &pk); err != nil {
			return false, err
		}
		switch name {
		case "cpu_utime_ticks":
			haveU = true
		case "cpu_stime_ticks":
			haveS = true
		}
	}
	return haveU && haveS, rows.Err()
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

	cpuIdx, idleIdx, softIdx := -1, -1, -1
	var sum float64
	var n int
	ordinal := 0
	// Wall-clock reconstruction: the banner's date anchors the day, each
	// `all` row's leading HH:MM:SS[ AM|PM] gives the second-of-day, and a
	// row whose second-of-day is below the previous one crossed midnight.
	var day time.Time
	haveDay := false
	prevSec := -1
	dayOffset := int64(0)
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
		if !haveDay && fields[0] == "Linux" {
			if d, ok := mpstatBannerDate(fields); ok {
				day, haveDay = d, true
			}
			continue
		}
		if ci, ii, si, isHeader := mpstatHeader(fields); isHeader {
			cpuIdx, idleIdx, softIdx = ci, ii, si
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
		pt := CPUPoint{Ordinal: ordinal, CPUPct: busy, SoftPct: -1}
		if softIdx >= 0 && softIdx < len(fields) {
			if soft, serr := strconv.ParseFloat(fields[softIdx], 64); serr == nil {
				pt.SoftPct = soft
			}
		}
		if haveDay {
			if sec, ok := mpstatRowSecond(fields, cpuIdx); ok {
				if prevSec >= 0 && sec < prevSec {
					dayOffset += 24 * 3600
				}
				prevSec = sec
				pt.TSUnix = day.Unix() + dayOffset + int64(sec)
			}
		}
		series = append(series, pt)
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
// indices of the CPU, %idle and %soft columns (softIdx is -1 when the
// layout has no %soft). A header is any row carrying both a "CPU" field
// and a "%idle" field.
func mpstatHeader(fields []string) (cpuIdx, idleIdx, softIdx int, ok bool) {
	cpuIdx, idleIdx, softIdx = -1, -1, -1
	for i, f := range fields {
		switch f {
		case "CPU":
			cpuIdx = i
		case "%idle":
			idleIdx = i
		case "%soft":
			softIdx = i
		}
	}
	return cpuIdx, idleIdx, softIdx, cpuIdx >= 0 && idleIdx >= 0
}

// mpstatBannerDate extracts the run date from mpstat's first line
// ("Linux 6.8.0 (host) \t2026-09-13 \t_x86_64_ \t(32 CPU)"). sysstat
// prints the date with the locale's %x unless S_TIME_FORMAT=ISO, so three
// layouts are accepted: ISO (what the bench forces), en_US MM/DD/YYYY and
// the C locale's MM/DD/YY. The date is read as UTC — see CPUPoint.TSUnix.
func mpstatBannerDate(fields []string) (time.Time, bool) {
	for _, f := range fields {
		for _, layout := range []string{"2006-01-02", "01/02/2006", "01/02/06"} {
			if d, err := time.ParseInLocation(layout, f, time.UTC); err == nil {
				return d, true
			}
		}
	}
	return time.Time{}, false
}

// mpstatRowSecond parses a data row's leading timestamp into a
// second-of-day. The timestamp is fields[0] ("HH:MM:SS", 24 h under
// S_TIME_FORMAT=ISO or the C locale) optionally followed by an AM/PM
// field (12 h locales); cpuIdx tells how many prefix fields there are
// (1 or 2), which is the robust way to know whether an AM/PM token is
// present.
func mpstatRowSecond(fields []string, cpuIdx int) (int, bool) {
	var t time.Time
	var err error
	switch cpuIdx {
	case 1:
		t, err = time.Parse("15:04:05", fields[0])
	case 2:
		t, err = time.Parse("03:04:05 PM", fields[0]+" "+strings.ToUpper(fields[1]))
	default:
		return 0, false
	}
	if err != nil {
		return 0, false
	}
	return t.Hour()*3600 + t.Minute()*60 + t.Second(), true
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

// ReduceResources folds a cell's per-run [ResourceStats] into one
// representative (#154): each summary scalar is the median across the runs
// that reported it (so a single GC spike or RSS blip does not skew the
// headline), and the last reporting run's series is kept verbatim as the
// illustrative trajectory. A metric stays null in the result iff it was
// null in EVERY run, so a non-Go competitor keeps goroutine/GC null while
// RSS/CPU/FD survive. nil run entries are skipped; the result is nil when
// no run carried resources.
//
// This is the single source of truth for the per-run reduction shared by
// the report-side [Aggregate] (the typed Document path) and the cluster
// per-host summary (mage_bench.go summarizeCells).
func ReduceResources(runs []*ResourceStats) *ResourceStats {
	present := make([]*ResourceStats, 0, len(runs))
	for _, r := range runs {
		if r != nil {
			present = append(present, r)
		}
	}
	if len(present) == 0 {
		return nil
	}
	out := &ResourceStats{Series: present[len(present)-1].Series}
	out.Summary.PeakRSSBytes = medianI64Ptr(collectResI64(present, func(s ResourceSummary) *int64 { return s.PeakRSSBytes }))
	out.Summary.SteadyRSSBytes = medianI64Ptr(collectResI64(present, func(s ResourceSummary) *int64 { return s.SteadyRSSBytes }))
	out.Summary.GCPauseP99Ns = medianI64Ptr(collectResI64(present, func(s ResourceSummary) *int64 { return s.GCPauseP99Ns }))
	out.Summary.GoroutineHWM = medianI64Ptr(collectResI64(present, func(s ResourceSummary) *int64 { return s.GoroutineHWM }))
	out.Summary.FDHWM = medianI64Ptr(collectResI64(present, func(s ResourceSummary) *int64 { return s.FDHWM }))
	out.Summary.MeanCPUPct = medianF64Ptr(collectResF64(present, func(s ResourceSummary) *float64 { return s.MeanCPUPct }))
	// Per-scenario window fields (schema v5.9): nil on every column-wide
	// aggregate, so the column-wide reduction is byte-identical to before.
	out.Summary.MeanSoftPct = medianF64Ptr(collectResF64(present, func(s ResourceSummary) *float64 { return s.MeanSoftPct }))
	out.Summary.SUTProcessCPUPct = medianF64Ptr(collectResF64(present, func(s ResourceSummary) *float64 { return s.SUTProcessCPUPct }))
	out.Window = present[len(present)-1].Window
	return out
}

// collectResI64 gathers the non-nil int64 values a selector pulls from
// each run's summary.
func collectResI64(runs []*ResourceStats, sel func(ResourceSummary) *int64) []int64 {
	var out []int64
	for _, r := range runs {
		if v := sel(r.Summary); v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// collectResF64 is collectResI64 for float metrics.
func collectResF64(runs []*ResourceStats, sel func(ResourceSummary) *float64) []float64 {
	var out []float64
	for _, r := range runs {
		if v := sel(r.Summary); v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// medianI64Ptr returns the median of xs as a fresh pointer, or nil when xs
// is empty (every run had the metric null).
func medianI64Ptr(xs []int64) *int64 {
	if len(xs) == 0 {
		return nil
	}
	v := medianInt64(xs)
	return &v
}

// medianF64Ptr is medianI64Ptr for floats (uses the p50 of the percentile
// helper so the tie-break matches the RPS/CPU aggregation in this package).
func medianF64Ptr(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	v := percentile(xs, 50)
	return &v
}
