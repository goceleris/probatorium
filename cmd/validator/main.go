// Command validator is the validation-tier entry point. It parses
// flags, constructs the orchestrator from [validation], and drives a
// soak run.
//
// Acceptance for wave 6:
//
//	cmd/validator -target=msa2-server -duration=10m -dry-run
//
// must enumerate the cell schedule, property predicates, and faults
// that would run, then exit 0.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"net/http"
	_ "net/http/pprof" // exposes /debug/pprof/* handlers on http.DefaultServeMux
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"syscall"
	"time"

	"github.com/goceleris/probatorium/validation"
)

// Config is the validator's flag set. Exposed for tests.
type Config struct {
	Target             string
	Duration           time.Duration
	Arch               string
	CheckpointInterval time.Duration
	SoakMode           bool
	DryRun             bool
	OutDir             string
	CorpusPath         string
	MarkovPath         string
	OpenAPIPath        string
	CelerisBin         string
	CelerisListenAddr  string
	MetricsURL         string
	PropertyTier       string
	CelerisCommit      string
	ReplayBin          string
	DriverMode         string
	DriverSSHUser      string
	DriverSSHHost      string

	// PprofAddr, when non-empty, binds the standard /debug/pprof/*
	// handlers on this address. Used for live leak / CPU diagnosis
	// during long soaks — `ssh into the host; curl /debug/pprof/heap >
	// heap.pb.gz`. Empty disables (production default).
	//
	// Bind to 127.0.0.1:<port> in any deployment that's reachable
	// from outside the host — pprof handlers expose raw process
	// memory and have no auth.
	PprofAddr string

	// RefappEngine, when non-empty, is appended to the refapp argv
	// as `-engine <value>`. Used by the engine matrix runner (issue
	// #103) to pin celeris engine choice without rebuilding the
	// refapp. Empty leaves the refapp's `auto` default in place.
	RefappEngine string

	// RefappAsyncHandlers, when non-empty ("true"/"false"), is passed to the
	// refapp as `-async-handlers=<v>` (sync/async coverage axis; "false" + a
	// .Async() route reproduces the celeris#309 epoll-h1-sync derivation).
	RefappAsyncHandlers string

	// RefappWorkers, when > 0, caps the refapp's io_uring worker count
	// (celeris.Config.Workers) for the ring-allocating engines. Lets a
	// memory-constrained validation host run the heaviest io_uring refapp
	// without io_uring_setup ENOMEM. 0 = leave the GOMAXPROCS default;
	// celeris requires >= 2 when set.
	RefappWorkers int

	// HeapDumpDir, when non-empty, names a directory the validator
	// writes a heap profile to every HeapDumpInterval (default 1h —
	// relaxed from 60s post-#115 because the validator RSS leak
	// that motivated frequent dumps was fixed in #102). File names:
	// `heap-<unix-ns>.pb.gz`. Diff with
	// `go tool pprof -base heap-EARLY.pb.gz heap-LATE.pb.gz`.
	//
	// Disk-based alternative to the pprof HTTP endpoint for
	// environments where outgoing local network from a separate
	// process is blocked (e.g. macOS dev sandbox). Survives
	// ctx-cancel cleanly so the last samples reflect end-of-run state.
	//
	// Tighten the interval for active leak hunts via
	// PROBATORIUM_HEAP_DUMP_INTERVAL=5m (or any time.ParseDuration
	// value) or by passing -heap-dump-interval=5m explicitly.
	HeapDumpDir      string
	HeapDumpInterval time.Duration

	// Matrix carries the matrix-mode knobs. When Matrix.Enabled is
	// true the validator iterates (refapp × engine) cells and emits
	// a v5.1 validate-results.json with Cells[] populated instead
	// of the single-cell document. Per #103 / #113.
	Matrix MatrixConfig
}

// defaultHeapDumpInterval resolves the default for -heap-dump-interval.
// Relaxed from 60s → 1h after probatorium#102 closed the validator
// RSS leak. Active leak hunts re-tighten via the
// PROBATORIUM_HEAP_DUMP_INTERVAL env var (parsed with time.ParseDuration)
// or by passing -heap-dump-interval explicitly. Invalid env values
// fall back to 1h silently — the flag is diagnostic infrastructure,
// the soak itself comes first.
func defaultHeapDumpInterval() time.Duration {
	if v := os.Getenv("PROBATORIUM_HEAP_DUMP_INTERVAL"); v != "" {
		if d, err := time.ParseDuration(v); err == nil && d > 0 {
			return d
		}
	}
	return time.Hour
}

// DefaultConfig returns the fresh-flag defaults.
func DefaultConfig() Config {
	return Config{
		Duration:           6 * time.Hour,
		CheckpointInterval: 24 * time.Hour,
		Arch:               runtime.GOARCH,
		Target:             "localhost",
		PropertyTier:       "core,middleware",
		CelerisListenAddr:  "127.0.0.1:8080",
		MarkovPath:         "validation/markov/auth_session_ratelimit.yaml",
		OpenAPIPath:        "validation/spec/auth_session_ratelimit.openapi.yaml",
	}
}

// Bind registers every Config field on fs.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.StringVar(&c.Target, "target", c.Target, "target host (msa2-server | msr1 | localhost)")
	fs.DurationVar(&c.Duration, "duration", c.Duration, "total run budget")
	fs.StringVar(&c.Arch, "arch", c.Arch, "arch stamp recorded in incident reports (default runtime.GOARCH)")
	fs.DurationVar(&c.CheckpointInterval, "checkpoint-interval", c.CheckpointInterval, "checkpoint cadence")
	fs.BoolVar(&c.SoakMode, "soak-mode", c.SoakMode, "enable extended invariant set + 1h checkpoint cadence")
	fs.BoolVar(&c.DryRun, "dry-run", c.DryRun, "enumerate the plan + exit")
	fs.StringVar(&c.OutDir, "out", c.OutDir, "results directory; default results/<timestamp>-validate-<arch>/")
	fs.StringVar(&c.CorpusPath, "corpus", c.CorpusPath, "seed corpus path; empty falls back to corpus.InitialSeeds")
	fs.StringVar(&c.MarkovPath, "markov", c.MarkovPath, "Markov transition YAML path")
	fs.StringVar(&c.OpenAPIPath, "openapi", c.OpenAPIPath, "OpenAPI 3.1 spec path")
	fs.StringVar(&c.CelerisBin, "celeris-bin", c.CelerisBin, "celeris executable; empty disables auto-launch")
	fs.StringVar(&c.CelerisListenAddr, "celeris-addr", c.CelerisListenAddr, "celeris bind addr")
	fs.StringVar(&c.MetricsURL, "metrics-url", c.MetricsURL, "override the refapp /debug/vars URL the property loop polls; default derives it from the refapp's ready banner")
	fs.StringVar(&c.PropertyTier, "property-tier", c.PropertyTier, "comma-separated tier filter (core,middleware,engine,driver); empty = all")
	fs.StringVar(&c.CelerisCommit, "celeris-commit", c.CelerisCommit, "celeris commit SHA; recorded in incidents")
	fs.StringVar(&c.ReplayBin, "replay-bin", c.ReplayBin, "cmd/validator-replay binary; empty disables Tier 3")
	fs.StringVar(&c.DriverMode, "driver", c.DriverMode, "process driver (local|ssh); default local")
	fs.StringVar(&c.DriverSSHUser, "ssh-user", c.DriverSSHUser, "SSH login user (only with -driver=ssh)")
	fs.StringVar(&c.DriverSSHHost, "ssh-host", c.DriverSSHHost, "SSH host:port (only with -driver=ssh)")
	fs.StringVar(&c.RefappEngine, "refapp-engine", c.RefappEngine, "celeris engine to pin for the refapp (iouring|epoll|std|adaptive); empty leaves refapp default")
	fs.StringVar(&c.RefappAsyncHandlers, "refapp-async-handlers", c.RefappAsyncHandlers, "pass -async-handlers=<v> to the refapp (true|false); empty leaves refapp default")
	fs.IntVar(&c.RefappWorkers, "refapp-workers", c.RefappWorkers, "cap io_uring refapp worker count (celeris Workers); 0 leaves GOMAXPROCS default, must be >=2 if set")
	fs.StringVar(&c.HeapDumpDir, "heap-dump-dir", c.HeapDumpDir, "directory to write periodic heap profiles to; empty disables")
	// Default cadence relaxed to 1h after the validator RSS leak that
	// motivated this diagnostic (probatorium#102) was fixed. Active
	// leak chases re-tighten via PROBATORIUM_HEAP_DUMP_INTERVAL or
	// the explicit flag. Per #115.
	fs.DurationVar(&c.HeapDumpInterval, "heap-dump-interval", defaultHeapDumpInterval(), "interval between heap dumps when -heap-dump-dir is set; env: PROBATORIUM_HEAP_DUMP_INTERVAL")
	fs.StringVar(&c.PprofAddr, "pprof-addr", c.PprofAddr, "expose /debug/pprof/* on this addr (e.g. 127.0.0.1:6060); empty disables")
	c.Matrix.Bind(fs)
}

// ParseArgs parses argv (without the program name).
func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("validator", flag.ContinueOnError)
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
		fmt.Fprintf(os.Stderr, "validator: %v\n", err)
		os.Exit(1)
	}
}

// run is the orchestrator entry point.
func run(cfg Config) error {
	// Matrix mode: defer to the matrix runner. It manages OutDir +
	// per-cell sub-orchestrators itself, so we don't fall through to
	// the single-cell setup below.
	if cfg.Matrix.Enabled {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		sigCh := make(chan os.Signal, 1)
		signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
		go func() {
			<-sigCh
			fmt.Fprintln(os.Stderr, "validator: interrupted (matrix)")
			cancel()
		}()
		return runMatrix(ctx, cfg, cfg.Matrix)
	}

	if cfg.OutDir == "" {
		ts := time.Now().UTC().Format("20060102-150405")
		cfg.OutDir = filepath.Join("results", ts+"-validate-"+cfg.Arch)
	}
	// MetricsURL stays empty unless -metrics-url was given: the
	// orchestrator derives the /debug/vars URL from the refapp's ready
	// banner, which is also correct for a "-celeris-addr 127.0.0.1:0"
	// launch.

	o, err := validation.New(validation.Config{
		Target:              cfg.Target,
		Arch:                cfg.Arch,
		CelerisCommit:       cfg.CelerisCommit,
		Duration:            cfg.Duration,
		CheckpointInterval:  cfg.CheckpointInterval,
		SoakMode:            cfg.SoakMode,
		DryRun:              cfg.DryRun,
		OutDir:              cfg.OutDir,
		CorpusPath:          cfg.CorpusPath,
		MarkovPath:          cfg.MarkovPath,
		OpenAPIPath:         cfg.OpenAPIPath,
		CelerisBin:          cfg.CelerisBin,
		CelerisListenAddr:   cfg.CelerisListenAddr,
		MetricsURL:          cfg.MetricsURL,
		PropertyTier:        cfg.PropertyTier,
		ReplayBin:           cfg.ReplayBin,
		RefappEngine:        cfg.RefappEngine,
		RefappAsyncHandlers: cfg.RefappAsyncHandlers,
		RefappWorkers:       cfg.RefappWorkers,
		DriverMode:          cfg.DriverMode,
		DriverSSHUser:       cfg.DriverSSHUser,
		DriverSSHHost:       cfg.DriverSSHHost,
	})
	if err != nil {
		return err
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		fmt.Fprintln(os.Stderr, "validator: interrupted")
		cancel()
	}()

	// pprof server, when requested. Runs on a separate goroutine so it
	// doesn't compete with the orchestrator for the main event loop.
	// Best-effort: bind failure logs but doesn't fail the run — pprof
	// is diagnostic infrastructure, the soak itself comes first.
	if cfg.PprofAddr != "" {
		go func() {
			srv := &http.Server{
				Addr:              cfg.PprofAddr,
				Handler:           http.DefaultServeMux,
				ReadHeaderTimeout: 10 * time.Second,
			}
			fmt.Fprintf(os.Stderr, "validator: pprof listening on %s/debug/pprof/\n", cfg.PprofAddr)
			if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
				fmt.Fprintf(os.Stderr, "validator: pprof server error: %v\n", err)
			}
		}()
	}

	// Periodic heap dumps to disk, when requested. Same diagnostic
	// purpose as pprof HTTP but file-based — works in environments
	// where outgoing local network from another process is blocked.
	if cfg.HeapDumpDir != "" {
		if err := os.MkdirAll(cfg.HeapDumpDir, 0o755); err != nil {
			fmt.Fprintf(os.Stderr, "validator: heap-dump-dir mkdir: %v\n", err)
		} else {
			interval := cfg.HeapDumpInterval
			if interval <= 0 {
				interval = 60 * time.Second
			}
			go runHeapDumper(ctx, cfg.HeapDumpDir, interval)
			fmt.Fprintf(os.Stderr, "validator: heap dumps every %s → %s\n", interval, cfg.HeapDumpDir)
		}
	}

	return o.Run(ctx)
}

// runHeapDumper writes a runtime/pprof.WriteHeapProfile snapshot to
// dir every interval. File name is `heap-<unix-ns>.pb.gz`. Exits when
// ctx is cancelled. Best-effort: any write failure is logged to
// stderr but doesn't halt the run.
func runHeapDumper(ctx context.Context, dir string, interval time.Duration) {
	tick := time.NewTicker(interval)
	defer tick.Stop()
	dump := func() {
		runtime.GC() // capture a clean post-GC heap snapshot
		path := filepath.Join(dir, fmt.Sprintf("heap-%d.pb.gz", time.Now().UnixNano()))
		f, err := os.Create(path)
		if err != nil {
			fmt.Fprintf(os.Stderr, "validator: heap dump create: %v\n", err)
			return
		}
		defer func() { _ = f.Close() }()
		if err := pprof.WriteHeapProfile(f); err != nil {
			fmt.Fprintf(os.Stderr, "validator: heap dump write: %v\n", err)
		}
	}
	dump() // initial sample at t=0
	for {
		select {
		case <-ctx.Done():
			dump() // final sample on shutdown
			return
		case <-tick.C:
			dump()
		}
	}
}
