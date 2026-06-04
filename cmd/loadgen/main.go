// Command loadgen is a thin shim around the
// [github.com/goceleris/loadgen] library, exposed as a CLI for ad-hoc
// debugging.
//
// The bench runner (cmd/runner) does NOT shell out to this binary --
// it imports loadgen directly so per-cell results stay in-process and
// the FD-leak detector can see the orchestrator's whole footprint. This
// CLI exists for two purposes:
//
//  1. Manual debugging against a freshly-built adapter without going
//     through the full matrix (`./loadgen -target http://127.0.0.1:8080
//     -duration 5s`). With -rated it also runs a Gil-Tene rated
//     (closed-loop, coordinated-omission-corrected) sweep and splices a
//     latency_at_slo block into the emitted JSON (probatorium#156).
//     -rate >0 drives a single direct constant-rate pass instead of
//     saturation.
//  2. Wave 11 / loadgen v2's federated multi-host mode, where each
//     loadgen process IS a remote shard. The CLI surface here is
//     forward-compatible with that future split.
//
// Output: one JSON Result document at -out (or stdout when -out is
// empty). The format matches loadgen.Result so post-processing tools
// already wired up to perfmatrix output read it without changes; the
// optional latency_at_slo key is additive (older readers ignore it).
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/goceleris/loadgen"
	"github.com/goceleris/probatorium/report"
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

	// Rated enables the Gil-Tene rated sweep: one open-loop saturation
	// pass followed by closed-loop (coordinated-omission-corrected) passes
	// at fractions of the measured saturation RPS, emitting a
	// latency_at_slo block alongside the saturation Result. Opt-in (driven
	// by the bench playbook only when BENCH_RATED=1).
	Rated         bool
	RatedDuration time.Duration
}

// defaultRatedFractions is the saturation-relative sweep for the cluster
// binary, mirroring cmd/runner so both producers agree on the rated targets.
var defaultRatedFractions = []float64{0.25, 0.5, 0.75, 0.9}

// DefaultConfig returns the fresh-flag defaults. Connections defaults
// to loadgen's own DefaultConfig.Connections rather than 256 verbatim
// so the CLI tracks library defaults if they shift.
func DefaultConfig() Config {
	d := loadgen.DefaultConfig()
	return Config{
		Target:        "http://127.0.0.1:8080",
		Method:        "GET",
		Duration:      30 * time.Second,
		Warmup:        5 * time.Second,
		Connections:   d.Connections,
		Workers:       d.Workers,
		Rate:          0,
		RatedDuration: 30 * time.Second,
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
	fs.IntVar(&c.Rate, "rate", c.Rate, "0 = saturation; >0 drives a single constant-rate (CO-corrected) pass")
	fs.BoolVar(&c.HTTP2, "http2", c.HTTP2, "drive HTTP/2 cleartext (prior knowledge)")
	fs.BoolVar(&c.H2CUpgrade, "h2c-upgrade", c.H2CUpgrade, "drive HTTP/1->H2C upgrade handshake")
	fs.StringVar(&c.BodyFile, "body-file", c.BodyFile, "request body, read from file (POST/PUT/PATCH)")
	fs.StringVar(&c.Out, "out", c.Out, "result JSON output path; empty = stdout")
	fs.BoolVar(&c.Rated, "rated", c.Rated,
		"run a Gil-Tene rated (closed-loop, CO-corrected) sweep after saturation and emit latency_at_slo")
	fs.DurationVar(&c.RatedDuration, "rated-duration", c.RatedDuration, "measurement window for each rated pass")
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
	lg := loadgen.DefaultConfig()
	lg.URL = cfg.Target
	lg.Method = cfg.Method
	lg.Duration = cfg.Duration
	lg.Warmup = cfg.Warmup
	lg.Connections = cfg.Connections
	lg.Workers = cfg.Workers
	lg.HTTP2 = cfg.HTTP2
	lg.H2CUpgrade = cfg.H2CUpgrade
	if cfg.Rate > 0 {
		// Direct constant-rate run (no sweep): drive a single CO-corrected
		// pass at the requested rate. loadgen.Config.Rate is float64.
		lg.Rate = float64(cfg.Rate)
	}

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

	// Rated sweep: the saturation pass above anchors the scale; drive rated
	// (closed-loop, CO-corrected) passes at fractions of it and derive the
	// latency_at_slo block emitted alongside the saturation Result.
	var sloByMs map[int]int
	if cfg.Rated && res != nil && res.RequestsPerSec > 0 {
		sloByMs = ratedSweep(ctx, lg, res.RequestsPerSec, cfg.RatedDuration)
	}
	return emit(res, sloByMs, cfg.Out)
}

// ratedSweep drives one rated pass per defaultRatedFractions of saturationRPS
// and returns latency_at_slo[ms] = max target RPS whose P99 <= ms, for each
// report.SLOThresholds budget. latency_at_slo is throughput-at-SLO (bigger is
// better) -- the metric the regression gate keys on. A pass that errors or
// returns zero requests is skipped. The sweep always drives rated load via
// loadgen.Config.Rate so coordinated-omission correction is applied by loadgen
// itself; never hand-roll a pacer, which would defeat the correction.
func ratedSweep(ctx context.Context, base loadgen.Config, saturationRPS float64, ratedDuration time.Duration) map[int]int {
	p99ByTarget := map[int]time.Duration{}
	for _, frac := range defaultRatedFractions {
		target := saturationRPS * frac
		if target <= 0 {
			continue
		}
		lgCfg := base
		lgCfg.Rate = target
		lgCfg.Duration = ratedDuration
		bm, err := loadgen.New(lgCfg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "probatorium-loadgen: rated target=%.0f: loadgen.New: %v\n", target, err)
			continue
		}
		rres, err := bm.Run(ctx)
		if err != nil || rres == nil || rres.Requests == 0 {
			fmt.Fprintf(os.Stderr, "probatorium-loadgen: rated target=%.0f: skipped (err=%v)\n", target, err)
			continue
		}
		p99ByTarget[int(target+0.5)] = rres.Latency.P99
	}
	if len(p99ByTarget) == 0 {
		return nil
	}
	slo := map[int]int{}
	for _, ms := range report.SLOThresholds {
		budget := time.Duration(ms) * time.Millisecond
		best := 0
		for t, p99 := range p99ByTarget {
			if p99 <= budget && t > best {
				best = t
			}
		}
		if best > 0 {
			slo[ms] = best
		}
	}
	if len(slo) == 0 {
		return nil
	}
	return slo
}

// emit writes the saturation Result as 2-space-indented JSON to outPath (or
// stdout). When a rated sweep produced a latency_at_slo block it is spliced in
// as a top-level key ADDITIVELY: the cluster aggregator (summarizeCells) parses
// the saturation Result fields verbatim and reads latency_at_slo when present,
// so adding the key never breaks the existing loadgen.Result decode.
func emit(res *loadgen.Result, sloByMs map[int]int, outPath string) error {
	buf, err := json.MarshalIndent(res, "", "  ")
	if err != nil {
		return err
	}
	if len(sloByMs) > 0 {
		buf, err = spliceLatencyAtSLO(buf, sloByMs)
		if err != nil {
			return err
		}
	}
	if outPath == "" {
		_, err := os.Stdout.Write(append(buf, '\n'))
		return err
	}
	return os.WriteFile(outPath, append(buf, '\n'), 0o644)
}

// spliceLatencyAtSLO re-encodes the loadgen.Result JSON with a top-level
// latency_at_slo map appended. We round-trip through a generic map (preserving
// every saturation field) rather than string-splicing so the output stays valid
// JSON regardless of the Result shape. Keys are stringified ms so the object
// satisfies JSON's string-key requirement; summarizeCells reads them back as ints.
func spliceLatencyAtSLO(resJSON []byte, sloByMs map[int]int) ([]byte, error) {
	var obj map[string]any
	if err := json.Unmarshal(resJSON, &obj); err != nil {
		return nil, fmt.Errorf("re-decode result for latency_at_slo splice: %w", err)
	}
	slo := make(map[string]int, len(sloByMs))
	for ms, rps := range sloByMs {
		slo[strconv.Itoa(ms)] = rps
	}
	obj["latency_at_slo"] = slo
	return json.MarshalIndent(obj, "", "  ")
}
