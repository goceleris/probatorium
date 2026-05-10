// Command loadgen is a thin shim around the
// [github.com/goceleris/loadgen] library, exposed as a CLI for ad-hoc
// debugging.
//
// The bench runner (cmd/runner) does NOT shell out to this binary —
// it imports loadgen directly so per-cell results stay in-process and
// the FD-leak detector can see the orchestrator's whole footprint. This
// CLI exists for two purposes:
//
//  1. Manual debugging against a freshly-built adapter without going
//     through the full matrix (`./loadgen -target http://127.0.0.1:8080
//     -duration 5s`).
//  2. Wave 11 / loadgen v2's federated multi-host mode, where each
//     loadgen process IS a remote shard. The CLI surface here is
//     forward-compatible with that future split (-rate is reserved for
//     rated-mode, currently TBD pending loadgen v2).
//
// Output: one JSON Result document at -out (or stdout when -out is
// empty). The format matches loadgen.Result so post-processing tools
// already wired up to perfmatrix output read it without changes.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goceleris/loadgen"
)

// Config is the parsed flag set for the CLI shim. Mirrors the fields the
// runner sets on loadgen.Config but exposes only the knobs a manual
// debugging session would reach for.
type Config struct {
	Target      string
	Method      string
	Duration    time.Duration
	Warmup      time.Duration
	Connections int
	Workers     int
	Rate        int
	HTTP2       bool
	H2CUpgrade  bool
	BodyFile    string
	Out         string
}

// DefaultConfig returns the fresh-flag defaults. Connections defaults
// to loadgen's own DefaultConfig.Connections rather than 256 verbatim
// so the CLI tracks library defaults if they shift.
func DefaultConfig() Config {
	d := loadgen.DefaultConfig()
	return Config{
		Target:      "http://127.0.0.1:8080",
		Method:      "GET",
		Duration:    30 * time.Second,
		Warmup:      5 * time.Second,
		Connections: d.Connections,
		Workers:     d.Workers,
		Rate:        0,
	}
}

// Bind registers every Config field on fs.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.StringVar(&c.Target, "target", c.Target, "target URL (e.g. http://127.0.0.1:8080)")
	fs.StringVar(&c.Method, "method", c.Method, "HTTP method")
	fs.DurationVar(&c.Duration, "duration", c.Duration, "measurement window")
	fs.DurationVar(&c.Warmup, "warmup", c.Warmup, "warmup window before measurement")
	fs.IntVar(&c.Connections, "connections", c.Connections, "TCP connection count (HTTP/1.1) or H2 conn pool size")
	fs.IntVar(&c.Workers, "workers", c.Workers, "concurrent worker goroutines")
	fs.IntVar(&c.Rate, "rate", c.Rate, "0 = saturation; >0 reserves the rated-mode slot (TBD; loadgen v2)")
	fs.BoolVar(&c.HTTP2, "http2", c.HTTP2, "drive HTTP/2 cleartext (prior knowledge)")
	fs.BoolVar(&c.H2CUpgrade, "h2c-upgrade", c.H2CUpgrade, "drive HTTP/1→H2C upgrade handshake")
	fs.StringVar(&c.BodyFile, "body-file", c.BodyFile, "request body, read from file (POST/PUT/PATCH)")
	fs.StringVar(&c.Out, "out", c.Out, "result JSON output path; empty = stdout")
}

// ParseArgs parses argv (without the program name).
func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("probatorium-loadgen", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.Bind(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func main() {
	cfg, err := ParseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "probatorium-loadgen: %v\n", err)
		os.Exit(1)
	}
}

// run wires the CLI flags into a loadgen.Config and drives loadgen.Run.
// SIGINT/SIGTERM cancel the active context so a long debug run can be
// interrupted without orphaning workers.
func run(cfg Config) error {
	if cfg.Rate > 0 {
		return fmt.Errorf("-rate >0 (rated mode) is reserved for loadgen v2; current loadgen exposes saturation only")
	}

	lg := loadgen.DefaultConfig()
	lg.URL = cfg.Target
	lg.Method = cfg.Method
	lg.Duration = cfg.Duration
	lg.Warmup = cfg.Warmup
	lg.Connections = cfg.Connections
	lg.Workers = cfg.Workers
	lg.HTTP2 = cfg.HTTP2
	lg.H2CUpgrade = cfg.H2CUpgrade

	if cfg.BodyFile != "" {
		body, err := os.ReadFile(cfg.BodyFile)
		if err != nil {
			return fmt.Errorf("read body file: %w", err)
		}
		lg.Body = body
	}

	bm, err := loadgen.New(lg)
	if err != nil {
		return fmt.Errorf("loadgen.New: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "probatorium-loadgen: interrupted")
		cancel()
	}()

	res, err := bm.Run(ctx)
	if err != nil {
		return fmt.Errorf("loadgen.Run: %w", err)
	}
	return emit(res, cfg.Out)
}

// emit writes the loadgen.Result as 2-space-indented JSON. Empty path
// means stdout — convenient for piping into jq.
func emit(res *loadgen.Result, outPath string) error {
	buf, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if outPath == "" {
		_, err := os.Stdout.Write(append(buf, '\n'))
		return err
	}
	return os.WriteFile(outPath, append(buf, '\n'), 0o644)
}
