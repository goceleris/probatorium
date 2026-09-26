// Command observer is the 1Hz process / runtime sampler. It runs
// alongside a celeris (or competitor) bench process and pushes one row
// per second to a SQLite database on disk. Used by the validation tier
// to spot resource trajectory bugs (FD leaks, goroutine leaks, RSS
// drift, GC pause regressions) and optionally by the bench tier when a
// run captures FD/RSS time series.
//
// Sources per tick:
//
//   - /proc/<pid>/status      — RSS, threads.
//   - /proc/<pid>/fd          — open FD count (entry count of the dir).
//   - /proc/<pid>/limits      — soft FD limit (changes are rare; persisted
//     so postmortems can correlate against caps).
//   - <metrics-url>           — celeris's expvar endpoint, for goroutine
//     count, heap_inuse, gc pause p99, accepted /
//     closed conn counters, panic count.
//
// Schema (one table, `observations`):
//
//	ts INTEGER PRIMARY KEY      — UNIX seconds, monotonic per-process.
//	host TEXT                   — os.Hostname() at startup.
//	pid INTEGER                 — observed PID.
//	fd_count INTEGER
//	rss_bytes INTEGER
//	goroutine_count INTEGER
//	heap_inuse_bytes INTEGER
//	gc_pause_p99_ns INTEGER
//	accepted_conn_total INTEGER
//	closed_conn_total INTEGER
//	panic_count INTEGER
//	cpu_utime_ticks INTEGER     — schema v5.9 (celeris#585)
//	cpu_stime_ticks INTEGER
//	engine_zc_sends_submitted INTEGER — schema v5.17 (celeris#585): the
//	engine_zc_notifs INTEGER            SUT's cumulative engine egress
//	engine_inline_bytes INTEGER         counters; -1 = not published by
//	engine_ring_bytes INTEGER           this scrape (never 0, which is a
//	engine_bytes_written INTEGER        reading an OFF arm must be able to make)
//
// Linux is the canonical target; non-linux returns zeroes for every
// /proc-derived field so a dev-host smoke test still exercises the
// metrics-fetch path.
//
// Storage is intentionally SQLite (modernc.org/sqlite, CGO-free pure
// Go) so the observer cross-compiles with the rest of the toolchain and
// the resulting file is readable on any host with sqlite3 in $PATH.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// Config is the parsed flag set.
type Config struct {
	PID        int
	MetricsURL string
	Out        string
	Interval   time.Duration
}

// DefaultConfig returns the fresh-flag defaults.
func DefaultConfig() Config {
	return Config{
		MetricsURL: "http://127.0.0.1:9090/debug/vars",
		Out:        "observations.sqlite",
		Interval:   time.Second,
	}
}

// Bind registers every Config field on fs.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.IntVar(&c.PID, "pid", c.PID, "target PID (0 = no /proc sampling, metrics-only)")
	fs.StringVar(&c.MetricsURL, "metrics-url", c.MetricsURL, "celeris metrics endpoint (expvar JSON)")
	fs.StringVar(&c.Out, "out", c.Out, "SQLite database path")
	fs.DurationVar(&c.Interval, "interval", c.Interval, "sampling interval")
}

// ParseArgs parses argv (without the program name).
func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("probatorium-observer", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.Bind(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// Observation is the per-tick record.
type Observation struct {
	TS                int64
	Host              string
	PID               int
	FDCount           int64
	RSSBytes          int64
	GoroutineCount    int64
	HeapInuseBytes    int64
	GCPauseP99Ns      int64
	AcceptedConnTotal int64
	ClosedConnTotal   int64
	PanicCount        int64

	// CPUUtimeTicks / CPUStimeTicks are the target process's cumulative
	// user / system CPU time from /proc/<pid>/stat (fields 14 and 15) in
	// USER_HZ ticks (100/s), schema v5.9 (celeris#585). Cumulative, so
	// the reader takes deltas: report.WindowResources turns the first and
	// last sample of a scenario window into the SUT's own CPU percent,
	// separable from the host-wide mpstat number. Zero when /proc is
	// unreadable (non-linux, process gone).
	CPUUtimeTicks int64
	CPUStimeTicks int64

	// Engine egress counters (schema v5.17, celeris#585), cumulative, read
	// off the SUT's /debug/vars: SEND_ZC SQEs submitted and notifications
	// completed, and BytesWritten split into the bytes a detached stream
	// wrote with a raw write(2) (inline, never zero-copy) and the bytes the
	// io_uring ring sent. -1 when the scrape carried no such counter (no
	// metrics URL, scrape failed, a document from another PID, or a celeris
	// that predates the field), so that 0 stays a measurement: the OFF arm
	// of a SEND_ZC A/B must read exactly 0 submits, and "absent" must not
	// look like that.
	EngineZCSendsSubmitted int64
	EngineZCNotifs         int64
	EngineInlineBytes      int64
	EngineRingBytes        int64
	EngineBytesWritten     int64
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS observations (
	ts INTEGER PRIMARY KEY,
	host TEXT,
	pid INTEGER,
	fd_count INTEGER,
	rss_bytes INTEGER,
	goroutine_count INTEGER,
	heap_inuse_bytes INTEGER,
	gc_pause_p99_ns INTEGER,
	accepted_conn_total INTEGER,
	closed_conn_total INTEGER,
	panic_count INTEGER,
	cpu_utime_ticks INTEGER,
	cpu_stime_ticks INTEGER,
	engine_zc_sends_submitted INTEGER,
	engine_zc_notifs INTEGER,
	engine_inline_bytes INTEGER,
	engine_ring_bytes INTEGER,
	engine_bytes_written INTEGER
);
`

const insertSQL = `
INSERT OR REPLACE INTO observations (
	ts, host, pid, fd_count, rss_bytes, goroutine_count,
	heap_inuse_bytes, gc_pause_p99_ns,
	accepted_conn_total, closed_conn_total, panic_count,
	cpu_utime_ticks, cpu_stime_ticks,
	engine_zc_sends_submitted, engine_zc_notifs,
	engine_inline_bytes, engine_ring_bytes, engine_bytes_written
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?);
`

func main() {
	cfg, err := ParseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "probatorium-observer: %v\n", err)
		os.Exit(1)
	}
}

// run opens the SQLite store, ensures the schema, and ticks the sampler
// until SIGINT/SIGTERM. The sampler tolerates per-source errors — a
// stuck metrics endpoint or a vanished /proc path produces zero values
// for that field rather than aborting the run.
func run(cfg Config) error {
	host, _ := os.Hostname()

	db, err := sql.Open("sqlite", cfg.Out)
	if err != nil {
		return fmt.Errorf("open %s: %w", cfg.Out, err)
	}
	defer func() { _ = db.Close() }()
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	stmt, err := db.Prepare(insertSQL)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer func() { _ = stmt.Close() }()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	httpc := &http.Client{Timeout: 500 * time.Millisecond}

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-tick.C:
			obs := sample(ctx, httpc, cfg, host, t)
			if _, err := stmt.Exec(
				obs.TS, obs.Host, obs.PID,
				obs.FDCount, obs.RSSBytes, obs.GoroutineCount,
				obs.HeapInuseBytes, obs.GCPauseP99Ns,
				obs.AcceptedConnTotal, obs.ClosedConnTotal, obs.PanicCount,
				obs.CPUUtimeTicks, obs.CPUStimeTicks,
				obs.EngineZCSendsSubmitted, obs.EngineZCNotifs,
				obs.EngineInlineBytes, obs.EngineRingBytes, obs.EngineBytesWritten,
			); err != nil {
				fmt.Fprintf(os.Stderr, "probatorium-observer: insert: %v\n", err)
			}
		}
	}
}

// sample collects one observation. /proc-backed fields are zero on
// non-linux; metrics-backed fields are zero when the metrics endpoint
// is unreachable.
func sample(ctx context.Context, httpc *http.Client, cfg Config, host string, t time.Time) Observation {
	obs := Observation{TS: t.Unix(), Host: host, PID: cfg.PID,
		EngineZCSendsSubmitted: -1, EngineZCNotifs: -1,
		EngineInlineBytes: -1, EngineRingBytes: -1, EngineBytesWritten: -1}

	if cfg.PID > 0 {
		obs.FDCount = countFDs(cfg.PID)
		obs.RSSBytes = readRSS(cfg.PID)
		obs.CPUUtimeTicks, obs.CPUStimeTicks = readCPUTicks(cfg.PID)
	}

	if cfg.MetricsURL != "" {
		mv := fetchMetrics(ctx, httpc, cfg.MetricsURL, cfg.PID)
		obs.GoroutineCount = mv.Goroutines
		obs.HeapInuseBytes = mv.HeapInuse
		obs.GCPauseP99Ns = mv.GCPauseP99
		obs.AcceptedConnTotal = mv.AcceptedConn
		obs.ClosedConnTotal = mv.ClosedConn
		obs.PanicCount = mv.Panics
		obs.EngineZCSendsSubmitted = mv.Engine[0]
		obs.EngineZCNotifs = mv.Engine[1]
		obs.EngineInlineBytes = mv.Engine[2]
		obs.EngineRingBytes = mv.Engine[3]
		obs.EngineBytesWritten = mv.Engine[4]
	}
	return obs
}

// countFDs returns the open-FD count for pid via /proc/<pid>/fd. Zero
// on non-linux or when the path is unreadable (process gone, permission
// denied).
func countFDs(pid int) int64 {
	entries, err := os.ReadDir(fmt.Sprintf("/proc/%d/fd", pid))
	if err != nil {
		return 0
	}
	return int64(len(entries))
}

// readCPUTicks returns the process's cumulative user and system CPU time
// from /proc/<pid>/stat (fields 14 utime and 15 stime) in USER_HZ ticks.
// Zero pair when the file is unreadable or malformed.
func readCPUTicks(pid int) (utime, stime int64) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, 0
	}
	u, s, ok := parseProcStatCPU(string(data))
	if !ok {
		return 0, 0
	}
	return u, s
}

// parseProcStatCPU extracts utime and stime from a /proc/<pid>/stat line.
// Field 2 (comm) is parenthesised and may itself contain spaces or ')',
// so the split starts after the LAST ')': from there field 3 (state) is
// index 0, which puts utime (field 14) at index 11 and stime (15) at 12.
func parseProcStatCPU(stat string) (utime, stime int64, ok bool) {
	i := strings.LastIndex(stat, ")")
	if i < 0 {
		return 0, 0, false
	}
	fields := strings.Fields(stat[i+1:])
	if len(fields) < 13 {
		return 0, 0, false
	}
	u, err := strconv.ParseInt(fields[11], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	s, err := strconv.ParseInt(fields[12], 10, 64)
	if err != nil {
		return 0, 0, false
	}
	return u, s, true
}

// readRSS returns RSS in bytes from /proc/<pid>/status's "VmRSS" line.
// The field reports kilobytes; we multiply to bytes for consistency
// with runtime.MemStats fields elsewhere in the schema.
func readRSS(pid int) int64 {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/status", pid))
	if err != nil {
		return 0
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "VmRSS:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			return 0
		}
		kb, err := strconv.ParseInt(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return kb * 1024
	}
	return 0
}

// metricsValues bundles the fields we read off the celeris expvar JSON.
type metricsValues struct {
	Goroutines   int64
	HeapInuse    int64
	GCPauseP99   int64
	AcceptedConn int64
	ClosedConn   int64
	Panics       int64

	// Engine holds the engineKeys counters in order; -1 = absent.
	Engine [len(engineKeys)]int64
}

// engineKeys are the engine egress counters the observer records, each
// under the two spellings a celeris SUT publishes: the validation refapps'
// flat debugvars key, and the bench server's "celeris.engine_metrics"
// object, which is the engine.EngineMetrics struct under its Go field
// names (servers/celeris/debugvars.go).
var engineKeys = [...]struct{ flat, field string }{
	{"celeris.engine_zc_sends_submitted", "ZCSendsSubmitted"},
	{"celeris.engine_zc_notifs", "ZCNotifs"},
	{"celeris.engine_inline_bytes", "InlineBytes"},
	{"celeris.engine_ring_bytes", "RingBytes"},
	{"celeris.engine_bytes_written", "BytesWritten"},
}

// absentMetrics is the zero reading: runtime fields 0 (their historical
// "absent" convention) and every engine counter -1.
func absentMetrics() metricsValues {
	var v metricsValues
	for i := range v.Engine {
		v.Engine[i] = -1
	}
	return v
}

// fetchMetrics GETs metricsURL, parses the expvar JSON shape, and
// extracts the fields the observer cares about. Unknown / missing
// fields default to zero.
//
// expvar emits a flat JSON object; the celeris runtime publishes both
// runtime.MemStats (under "memstats") and a flat "celeris.<counter>"
// space for engine-specific counters. The reader here is forgiving:
// every field is optional, and any parse error short-circuits to a
// zero-value snapshot.
//
// wantPID > 0 arms the process guard: a document that names its own "pid"
// (the bench server's does) and names a different one is refused whole,
// because it was served by some other process than the one whose /proc
// fields this row carries -- a leftover SUT still holding the sidecar
// port, or a respawned SUT after the observer pinned the original.
func fetchMetrics(ctx context.Context, httpc *http.Client, url string, wantPID int) metricsValues {
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return absentMetrics()
	}
	resp, err := httpc.Do(req)
	if err != nil {
		return absentMetrics()
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return absentMetrics()
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return absentMetrics()
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return absentMetrics()
	}
	if p, ok := doc["pid"].(float64); ok && wantPID > 0 && int(p) != wantPID {
		return absentMetrics()
	}
	v := absentMetrics()
	em, _ := doc["celeris.engine_metrics"].(map[string]any)
	for i, k := range engineKeys {
		if n, ok := readPresentInt64(doc, k.flat); ok {
			v.Engine[i] = n
		} else if n, ok := readPresentInt64(em, k.field); ok {
			v.Engine[i] = n
		}
	}
	v.Goroutines = readInt64(doc, "goroutines")
	v.AcceptedConn = readInt64(doc, "celeris.accepted_conn_total")
	v.ClosedConn = readInt64(doc, "celeris.closed_conn_total")
	v.Panics = readInt64(doc, "celeris.panic_count")
	if ms, ok := doc["memstats"].(map[string]any); ok {
		v.HeapInuse = readInt64(ms, "HeapInuse")
		// PauseNs is a circular buffer in MemStats; we surface the most
		// recent slot as a coarse "what was the last GC pause" signal.
		// Wave-4 wires a richer GC histogram source if this proves too
		// noisy.
		if pauses, ok := ms["PauseNs"].([]any); ok && len(pauses) > 0 {
			if numPause, ok := ms["NumGC"].(float64); ok && int(numPause) > 0 {
				idx := (int(numPause) - 1) % len(pauses)
				v.GCPauseP99 = readInt64Slice(pauses, idx)
			}
		}
	}
	return v
}

// readInt64 pulls a numeric value out of an expvar JSON map. JSON
// numbers always decode as float64 in encoding/json; we round-trip
// through int64 because the schema expects integer counters.
func readInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}

// readPresentInt64 is readInt64 that also says whether the key held a
// number at all, for counters whose 0 is a reading.
func readPresentInt64(m map[string]any, key string) (int64, bool) {
	if m == nil {
		return 0, false
	}
	if f, ok := m[key].(float64); ok && f >= 0 {
		return int64(f), true
	}
	return 0, false
}

// readInt64Slice indexes a JSON array of numbers; mirrors readInt64 for
// numeric coercion.
func readInt64Slice(s []any, i int) int64 {
	if i < 0 || i >= len(s) {
		return 0
	}
	switch v := s[i].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	default:
		return 0
	}
}
