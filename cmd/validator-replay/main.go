// Command validator-replay is the single-seed deterministic replay
// harness. Given a seed and a celeris commit, it expands the seed
// into the (workload, fault_schedule) tuple via [validation.ReplayPlan]
// and either prints it (-dry-run) or executes it against the named
// target host.
//
// Acceptance for wave 6:
//
//	cmd/validator-replay --seed=0x1 --commit=$(git rev-parse HEAD) --target=msa2-server -dry-run
//
// must deterministically print the workload + fault schedule for that
// seed.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goceleris/probatorium/validation"
	"github.com/goceleris/probatorium/validation/fault"
)

// Config is the replay flag set.
type Config struct {
	Seed              string
	Commit            string
	Target            string
	Duration          time.Duration
	DryRun            bool
	CelerisPID        int
	CelerisListenPort int
}

func DefaultConfig() Config {
	return Config{
		Target:            "localhost",
		Duration:          time.Hour,
		CelerisListenPort: 8080,
	}
}

func (c *Config) Bind(fs *flag.FlagSet) {
	fs.StringVar(&c.Seed, "seed", c.Seed, "seed value (hex with 0x prefix, or decimal)")
	fs.StringVar(&c.Commit, "commit", c.Commit, "celeris commit SHA (recorded in any incident)")
	fs.StringVar(&c.Target, "target", c.Target, "target host")
	fs.DurationVar(&c.Duration, "duration", c.Duration, "replay window")
	fs.BoolVar(&c.DryRun, "dry-run", c.DryRun, "print plan and exit")
	fs.IntVar(&c.CelerisPID, "celeris-pid", c.CelerisPID, "celeris PID (required for PID-dependent faults)")
	fs.IntVar(&c.CelerisListenPort, "celeris-port", c.CelerisListenPort, "celeris bind port")
}

func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("validator-replay", flag.ContinueOnError)
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
		fmt.Fprintf(os.Stderr, "validator-replay: %v\n", err)
		os.Exit(1)
	}
}

// parseSeed accepts "0x1", "1", "0X10", or decimal "16". Returns
// uint64 with no implicit endianness conversion.
func parseSeed(s string) (uint64, error) {
	if s == "" {
		return 0, fmt.Errorf("seed required")
	}
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "0x") || strings.HasPrefix(s, "0X") {
		return strconv.ParseUint(s[2:], 16, 64)
	}
	return strconv.ParseUint(s, 10, 64)
}

func run(cfg Config) error {
	seed, err := parseSeed(cfg.Seed)
	if err != nil {
		return fmt.Errorf("bad seed %q: %w", cfg.Seed, err)
	}
	rs := validation.ReplayPlan(seed, cfg.Duration, cfg.CelerisPID, cfg.CelerisListenPort)

	fmt.Printf("validator-replay\n")
	fmt.Printf("  target=%s commit=%s duration=%s\n", cfg.Target, cfg.Commit, cfg.Duration)
	validation.PrintReplayPlan(os.Stdout, rs)

	if cfg.DryRun {
		return nil
	}

	// Live replay: drive the schedule against the configured run
	// duration. The wave 6 binary does NOT spawn celeris itself —
	// the orchestrator (or the operator) does that, and feeds the
	// PID in via -celeris-pid. The replay just applies the fault
	// schedule and exits.
	ctx, cancel := context.WithTimeout(context.Background(), cfg.Duration)
	defer cancel()
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() { <-sigCh; cancel() }()

	started := time.Now()
	return fault.Run(ctx, started, rs.Schedule)
}
