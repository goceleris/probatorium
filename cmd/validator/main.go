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
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
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
	fs.StringVar(&c.MetricsURL, "metrics-url", c.MetricsURL, "celeris metrics endpoint; default http://<celeris-addr>/debug/vars")
	fs.StringVar(&c.PropertyTier, "property-tier", c.PropertyTier, "comma-separated tier filter (core,middleware,engine,driver); empty = all")
	fs.StringVar(&c.CelerisCommit, "celeris-commit", c.CelerisCommit, "celeris commit SHA; recorded in incidents")
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
	if cfg.OutDir == "" {
		ts := time.Now().UTC().Format("20060102-150405")
		cfg.OutDir = filepath.Join("results", ts+"-validate-"+cfg.Arch)
	}
	if cfg.MetricsURL == "" {
		cfg.MetricsURL = "http://" + cfg.CelerisListenAddr + "/debug/vars"
	}

	o, err := validation.New(validation.Config{
		Target:             cfg.Target,
		Arch:               cfg.Arch,
		CelerisCommit:      cfg.CelerisCommit,
		Duration:           cfg.Duration,
		CheckpointInterval: cfg.CheckpointInterval,
		SoakMode:           cfg.SoakMode,
		DryRun:             cfg.DryRun,
		OutDir:             cfg.OutDir,
		CorpusPath:         cfg.CorpusPath,
		MarkovPath:         cfg.MarkovPath,
		OpenAPIPath:        cfg.OpenAPIPath,
		CelerisBin:         cfg.CelerisBin,
		CelerisListenAddr:  cfg.CelerisListenAddr,
		MetricsURL:         cfg.MetricsURL,
		PropertyTier:       cfg.PropertyTier,
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

	return o.Run(ctx)
}
