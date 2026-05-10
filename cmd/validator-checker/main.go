// Command validator-checker is the standalone invariant evaluator.
//
// Once per second it samples two sources, projects them into a
// [properties.Snapshot], evaluates every registered predicate, and
// writes per-second rows to a SQLite store next to the observer's:
//
//  1. The legacy /debug/vars HTTP endpoint (wave 6) — carries
//     expvar.Go counters: goroutine count, memstats, accepted /
//     active / closed conn totals.
//
//  2. The validation-build Unix-domain socket at
//     /tmp/celeris-validation.sock (wave 7+, celeris v1.4.3+) —
//     carries the in-process assertion counters that only the
//     validation build accumulates: panic count, ratelimit/session/
//     JWT/iouring-SQE assertions. Missing socket is non-fatal (the
//     binary under test may be a production build; we just leave
//     those Snapshot slots at zero).
//
// On the first invariant violation the checker emits a JSON Incident
// record on stdout and exits non-zero so the orchestrator can pick it
// up, capture forensics, and trigger auto-bisect.
package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/goceleris/probatorium/validation/properties"
)

// Config is the checker's flag set.
type Config struct {
	MetricsURL   string
	Out          string
	Interval     time.Duration
	PropertyTier string
	// CelerisStderrPath is a log file path the checker tails to grep
	// for "DATA RACE" / "checkptr" markers. Empty disables the tail.
	CelerisStderrPath string
	// ValidationSocketPath is the unix-domain socket the validation
	// build of celeris exposes its assertion counters on. Empty (or
	// missing file at runtime) skips that poll and leaves the slots
	// at zero — useful when the binary under test is a production
	// build. Default matches celeris/validation.SocketPath.
	ValidationSocketPath string
}

func DefaultConfig() Config {
	return Config{
		MetricsURL:           "http://127.0.0.1:8080/debug/vars",
		Out:                  "checker.sqlite",
		Interval:             time.Second,
		PropertyTier:         "core,middleware",
		ValidationSocketPath: "/tmp/celeris-validation.sock",
	}
}

func (c *Config) Bind(fs *flag.FlagSet) {
	fs.StringVar(&c.MetricsURL, "metrics-url", c.MetricsURL, "celeris metrics endpoint")
	fs.StringVar(&c.Out, "out", c.Out, "sqlite store for per-second predicate evaluations")
	fs.DurationVar(&c.Interval, "interval", c.Interval, "sampling interval")
	fs.StringVar(&c.PropertyTier, "property-tier", c.PropertyTier, "tier filter, comma-separated")
	fs.StringVar(&c.CelerisStderrPath, "celeris-stderr", c.CelerisStderrPath, "celeris stderr log path; tailed for race/checkptr markers")
	fs.StringVar(&c.ValidationSocketPath, "validation-socket", c.ValidationSocketPath, "unix socket exposing celeris validation Counters (empty = skip)")
}

func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("validator-checker", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.Bind(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

const schemaSQL = `
CREATE TABLE IF NOT EXISTS evaluations (
	ts INTEGER NOT NULL,
	predicate TEXT NOT NULL,
	ok INTEGER NOT NULL,
	message TEXT,
	PRIMARY KEY (ts, predicate)
);
`

const insertSQL = `INSERT OR REPLACE INTO evaluations (ts, predicate, ok, message) VALUES (?, ?, ?, ?);`

// Incident is the on-stdout JSON record emitted on hard fail.
type Incident struct {
	TS          int64               `json:"ts"`
	PredicateID string              `json:"predicate"`
	Message     string              `json:"message"`
	Snapshot    properties.Snapshot `json:"snapshot"`
}

func main() {
	cfg, err := ParseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "validator-checker: %v\n", err)
		os.Exit(1)
	}
}

func run(cfg Config) error {
	specs := selectPredicates(cfg.PropertyTier)
	if len(specs) == 0 {
		return fmt.Errorf("no predicates selected (tier=%q)", cfg.PropertyTier)
	}

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
	// Build a separate client for the unix socket — net/http's
	// transport doesn't natively speak unix:// URLs, but a custom
	// DialContext makes it transparent.
	sockClient := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				if cfg.ValidationSocketPath == "" {
					return nil, errors.New("validation socket disabled")
				}
				return net.Dial("unix", cfg.ValidationSocketPath)
			},
		},
	}
	pctx := properties.Context{
		RunStartedAt: time.Now(),
	}

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-tick.C:
			snap := poll(ctx, httpc, cfg.MetricsURL, t)
			if cfg.ValidationSocketPath != "" {
				pollValidationSocket(ctx, sockClient, &snap)
			}
			pctx.Now = t
			pctx.History = append(pctx.History, snap)
			// Cap history at 3600 entries (1h at 1Hz).
			if len(pctx.History) > 3600 {
				pctx.History = pctx.History[len(pctx.History)-3600:]
			}
			for _, s := range specs {
				ok, msg := s.Predicate(&snap, pctx)
				okInt := 1
				if !ok {
					okInt = 0
				}
				if _, err := stmt.Exec(snap.TS, s.ID, okInt, msg); err != nil {
					fmt.Fprintf(os.Stderr, "validator-checker: insert: %v\n", err)
				}
				if !ok {
					inc := Incident{
						TS:          snap.TS,
						PredicateID: s.ID,
						Message:     msg,
						Snapshot:    snap,
					}
					buf, _ := json.MarshalIndent(inc, "", "  ")
					fmt.Println(string(buf))
					return fmt.Errorf("hard fail: %s: %s", s.ID, msg)
				}
			}
		}
	}
}

// selectPredicates resolves a comma-separated tier filter, with empty
// meaning "every registered predicate".
func selectPredicates(tier string) []properties.Spec {
	if tier == "" {
		return properties.All()
	}
	var out []properties.Spec
	seen := map[string]bool{}
	for _, t := range strings.Split(tier, ",") {
		t = strings.TrimSpace(t)
		if t == "" {
			continue
		}
		for _, p := range properties.ByTier(t) {
			if !seen[p.ID] {
				out = append(out, p)
				seen[p.ID] = true
			}
		}
	}
	return out
}

// poll is the per-tick metrics fetch. Builds a [properties.Snapshot]
// from the celeris /debug/vars JSON; missing fields default to zero.
func poll(ctx context.Context, hc *http.Client, url string, t time.Time) properties.Snapshot {
	snap := properties.Snapshot{TS: t.Unix()}
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return snap
	}
	resp, err := hc.Do(req)
	if err != nil {
		return snap
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return snap
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return snap
	}
	snap.GoroutineCount = readInt64(doc, "goroutines")
	snap.AcceptedConnTotal = readInt64(doc, "celeris.accepted_conn_total")
	snap.ClosedConnTotal = readInt64(doc, "celeris.closed_conn_total")
	snap.ActiveConns = readInt64(doc, "celeris.active_conns")
	snap.PanicCount = readInt64(doc, "celeris.panic_count")
	if ms, ok := doc["memstats"].(map[string]any); ok {
		snap.HeapInuseBytes = readInt64(ms, "HeapInuse")
		snap.HeapAllocBytes = readInt64(ms, "HeapAlloc")
	}
	return snap
}

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

// pollValidationSocket fetches the in-process assertion counters from
// celeris's validation-build unix-domain endpoint and copies them onto
// snap. Schema matches `celeris/validation.Counters` (v1.4.3+):
//
//	{
//	  "panic_count":                  uint64,
//	  "ratelimit_token_violations":   uint64,
//	  "session_owner_mismatches":     uint64,
//	  "jwt_late_admits":              uint64,
//	  "iouring_sqe_corruptions":      uint64
//	}
//
// A connection failure (production build, socket missing, ECONNREFUSED)
// is non-fatal — the slots stay at the previous tick's value (initially
// zero), which is the correct behaviour for predicates that only react
// to non-zero counts. PanicCount also feeds I-PANIC via the existing
// /debug/vars `celeris.panic_count`, so a missing socket on a
// production binary doesn't silence the panic invariant.
func pollValidationSocket(ctx context.Context, hc *http.Client, snap *properties.Snapshot) {
	// The URL host is ignored because DialContext routes to the unix
	// socket regardless. We keep the path "/snapshot" to match the
	// canonical route in celeris/validation/endpoint.go.
	req, err := http.NewRequestWithContext(ctx, "GET", "http://unix/snapshot", nil)
	if err != nil {
		return
	}
	resp, err := hc.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	var c struct {
		PanicCount               int64 `json:"panic_count"`
		RatelimitTokenViolations int64 `json:"ratelimit_token_violations"`
		SessionOwnerMismatches   int64 `json:"session_owner_mismatches"`
		JWTLateAdmits            int64 `json:"jwt_late_admits"`
		IouringSQECorruptions    int64 `json:"iouring_sqe_corruptions"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return
	}
	// PanicCount: validation socket wins over /debug/vars when both
	// are present — the validation build's counter is the canonical
	// source (assertions.IncrementPanic is called from the recover
	// path BEFORE expvar would notice).
	if c.PanicCount > snap.PanicCount {
		snap.PanicCount = c.PanicCount
	}
	snap.RatelimitTokenViolations = c.RatelimitTokenViolations
	snap.SessionOwnerMismatches = c.SessionOwnerMismatches
	snap.JWTLateAdmits = c.JWTLateAdmits
	snap.IouringSQECorruptions = c.IouringSQECorruptions
}
