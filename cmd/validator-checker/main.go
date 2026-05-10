// Command validator-checker is the standalone invariant evaluator.
// It polls celeris's /debug/vars (wave 6) — wave 7 swaps the URL for
// the validation-build /metrics endpoint — once per second, projects
// each poll into a [properties.Snapshot], evaluates every registered
// predicate, and writes per-second rows to a SQLite store next to the
// observer's. On the first invariant violation it emits a JSON
// incident record on stdout and exits non-zero so the orchestrator
// can pick it up.
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
}

func DefaultConfig() Config {
	return Config{
		MetricsURL:   "http://127.0.0.1:8080/debug/vars",
		Out:          "checker.sqlite",
		Interval:     time.Second,
		PropertyTier: "core,middleware",
	}
}

func (c *Config) Bind(fs *flag.FlagSet) {
	fs.StringVar(&c.MetricsURL, "metrics-url", c.MetricsURL, "celeris metrics endpoint")
	fs.StringVar(&c.Out, "out", c.Out, "sqlite store for per-second predicate evaluations")
	fs.DurationVar(&c.Interval, "interval", c.Interval, "sampling interval")
	fs.StringVar(&c.PropertyTier, "property-tier", c.PropertyTier, "tier filter, comma-separated")
	fs.StringVar(&c.CelerisStderrPath, "celeris-stderr", c.CelerisStderrPath, "celeris stderr log path; tailed for race/checkptr markers")
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
	TS          int64                `json:"ts"`
	PredicateID string               `json:"predicate"`
	Message     string               `json:"message"`
	Snapshot    properties.Snapshot  `json:"snapshot"`
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
	defer db.Close()
	if _, err := db.Exec(schemaSQL); err != nil {
		return fmt.Errorf("create schema: %w", err)
	}
	stmt, err := db.Prepare(insertSQL)
	if err != nil {
		return fmt.Errorf("prepare insert: %w", err)
	}
	defer stmt.Close()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	httpc := &http.Client{Timeout: 500 * time.Millisecond}
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
	defer resp.Body.Close()
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
