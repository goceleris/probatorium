// Command validator-checker is the standalone invariant evaluator: a
// thin CLI over validation/checker, the same poll + predicate loop the
// validator orchestrator runs in-process for every Tier 1 cell.
//
// Once per second it samples two sources, projects them into a
// [properties.Snapshot], evaluates every selected predicate, and
// writes per-second rows to a SQLite store next to the observer's:
//
//  1. The refapp's /debug/vars HTTP endpoint (see checker.DebugVarsKeys)
//     — goroutine count, memstats, accepted / active / closed conn
//     totals, panic count.
//
//  2. The validation-build Unix-domain socket at
//     /tmp/celeris-validation.sock (celeris v1.4.3+, -tags=validation
//     only) — the in-process assertion counters. Missing socket is
//     non-fatal (the binary under test may be a production build; the
//     Snapshot slots stay at zero).
//
// On the first invariant violation the checker emits a JSON Incident
// record on stdout and exits non-zero. The orchestrator no longer
// launches this binary (it runs the loop in-process); it remains for
// ad-hoc use against a running refapp:
//
//	validator-checker -metrics-url=http://127.0.0.1:8080/debug/vars -pid=$(pgrep refapp)
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
	"syscall"
	"time"

	_ "modernc.org/sqlite"

	"github.com/goceleris/probatorium/validation/checker"
	"github.com/goceleris/probatorium/validation/properties"
)

// Config is the checker's flag set.
type Config struct {
	MetricsURL   string
	Out          string
	Interval     time.Duration
	PropertyTier string
	// PID, when > 0, is the refapp process whose /proc/<pid>/status
	// VmRSS feeds Snapshot.RSSBytes (I-MEM-4). 0 leaves RSS unsampled.
	PID int
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
	fs.IntVar(&c.PID, "pid", c.PID, "refapp pid; samples /proc/<pid>/status VmRSS for I-MEM-4 (0 = skip)")
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
	specs := checker.SelectPredicates(cfg.PropertyTier)
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
	sockClient := checker.NewSocketClient(cfg.ValidationSocketPath, 500*time.Millisecond)
	ev := checker.NewEvaluator(specs)

	tick := time.NewTicker(cfg.Interval)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case t := <-tick.C:
			snap, perr := checker.Poll(ctx, httpc, cfg.MetricsURL, t)
			if perr != nil {
				ev.RecordPollError()
				fmt.Fprintf(os.Stderr, "validator-checker: poll: %v\n", perr)
				continue
			}
			if cfg.ValidationSocketPath != "" {
				checker.PollValidationSocket(ctx, sockClient, &snap)
			}
			snap.PID = cfg.PID
			snap.RSSBytes = checker.ReadRSS(cfg.PID)
			violations := ev.Observe(snap, t)
			failed := map[string]string{}
			for _, v := range violations {
				failed[v.ID] = v.Message
			}
			for _, s := range specs {
				okInt := 1
				msg, bad := failed[s.ID]
				if bad {
					okInt = 0
				}
				if _, err := stmt.Exec(snap.TS, s.ID, okInt, msg); err != nil {
					fmt.Fprintf(os.Stderr, "validator-checker: insert: %v\n", err)
				}
			}
			if len(violations) > 0 {
				v := violations[0]
				inc := Incident{TS: snap.TS, PredicateID: v.ID, Message: v.Message, Snapshot: snap}
				buf, _ := json.MarshalIndent(inc, "", "  ")
				fmt.Println(string(buf))
				return fmt.Errorf("hard fail: %s: %s", v.ID, v.Message)
			}
		}
	}
}
