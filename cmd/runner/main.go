// Command runner is the probatorium bench orchestrator.
//
// It walks a (scenario × adapter × run) matrix, exec's each adapter as a
// staged binary on a free loopback port, drives it with loadgen as a
// LIBRARY (not a subprocess), captures FD-leak deltas around every cell,
// and writes a per-cell JSON result plus a top-level manifest. After the
// matrix completes, it folds every result through the report package's
// Aggregate / WriteSchema / WriteMarkdown to produce the v5.0
// results.json + report.md.
//
// The runner does NOT use cmd/loadgen as a subprocess — that binary is
// retained only for ad-hoc CLI use and the future federated multi-host
// loadgen mode (wave 11). Library imports keep the orchestrator
// observable (per-second progress callbacks, single FD-counter scope)
// and avoid one fork+exec per cell.
//
// Lifecycle, mirrored from perfmatrix's runner:
//
//  1. Parse flags into [Config].
//  2. Resolve scenarios + adapters; apply -cells glob filter.
//  3. Start docker-backed services on demand (services.Start).
//  4. Build the cell schedule via interleave.Schedule.
//  5. For each cell:
//     a. Pick a free loopback port.
//     b. servers.StartAdapter(ctx, name, "127.0.0.1:<port>").
//     c. TCP-probe the bind addr until ready (5 s cap). (Remote-target
//     mode replaces a–c with a pre-cell SUT liveness probe: a dead
//     target marks the cell DNF "server-down" in seconds instead of
//     burning the measurement window — see executeCell.)
//     d. Run loadgen.New(...).Run(ctx); classify the outcome
//     (classifyCompletedCell: interrupted / server-died / capability-
//     lie / zero-request / suspect-over-error-budget) and persist it.
//     e. SIGTERM / SIGKILL the adapter; record the FD delta.
//     f. Re-write results.json (atomic temp+rename) so a killed runner
//     still leaves a parseable column current to the last cell.
//  6. Aggregate per-cell samples; write v5.0 JSON + Markdown.
//
// Acceptance for wave 3: this binary is a complete code path. Running it
// without a deploy fails at step 5a with a clean "binary not found"
// error from servers.StartAdapter, which is the contract the bench
// playbook depends on.
//
// Remote-target mode (-target): on the cluster the SUT runs on a
// separate host (bench_target) and the runner runs on the loadgen host,
// so there is no in-process adapter to spawn. When -target is set the
// runner skips freePort / StartAdapter / waitForTCP / the FD-leak scope
// and drives loadgen straight against the already-running remote base
// URL. The schedule collapses to scenarios × one synthetic server (the
// -server-name slug) × runs, so ansible can keep its outer
// (run_index × competitor) interleaving and call the runner once per
// cell with -runs 1 — the runner expands the scenario inner loop. A
// permissive synthetic FeatureSet makes every scenario applicable; a
// SUT that cannot speak a given protocol surfaces as a zero-request
// cell rather than a silently-skipped one.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
	"github.com/goceleris/probatorium/services"
)

// Config is the parsed flag set. Exported so tests can construct it
// directly without going through ParseArgs.
type Config struct {
	Runs     int
	Duration time.Duration
	Warmup   time.Duration
	Cooldown time.Duration
	Cells    string
	Out      string
	Services string
	FailFast bool
	FDTrace  bool
	Seed     int64

	// Timeseries is the path for the gzip time-series sidecar. Empty
	// means <Out>/timeseries.json.gz. The sidecar carries the per-run
	// 1 Hz rps series + a cross-run band, kept out of results.json so
	// the summary stays small and byte-stable.
	Timeseries string

	// Target, when non-empty, switches the runner into remote-target
	// mode: instead of spawning each adapter on a free loopback port,
	// every cell drives loadgen against this already-running base URL
	// (e.g. "http://10.0.0.2:8080"). This is the mode the cluster bench
	// uses — the SUT runs on bench_target and the runner on the loadgen
	// host, so there is no in-process adapter to start.
	Target string
	// TargetArch is the GOARCH of the HOST UNDER TEST, not of this
	// process. The runner executes on the loadgen host (msa2-client,
	// amd64) while the SUT may be msa2-server (amd64) or msr1 (arm64),
	// so runtime.GOARCH tags every arm64 cell as amd64 — which silently
	// mislabels an entire arch's published results. Ansible passes the
	// inventory's `arch` for bench_target. Empty falls back to
	// runtime.GOARCH (correct for the local-adapter path, where the SUT
	// really is this host).
	TargetArch string
	// ServerName is the friendly competitor slug recorded as the cell's
	// server in remote-target mode (e.g. "celeris-iouring-h1-async").
	// Ignored when Target is empty.
	ServerName string

	// DryRun, when true, prints the resolved schedule and exits without
	// starting any adapter. Convenient for CI smoke tests and for
	// validating the -cells glob without requiring a deploy.
	DryRun bool

	// RatedMode enables the Gil-Tene rated (closed-loop, coordinated-
	// omission-corrected) sweep after each cell's saturation pass. OFF by
	// default — rated multiplies per-cell wall-clock by len(RatedFractions)
	// extra passes, so it is opt-in (BENCH_RATED=1 / -rated) and curated by
	// the budget issue (#166). Rated adds PASSES within a cell, never extra
	// schedule cells, so the cell count is identical whether on or off.
	RatedMode bool

	// RatedDuration is the measurement window for each rated pass. Kept
	// short relative to the saturation Duration since rated drives a fixed
	// offered load and converges faster.
	RatedDuration time.Duration

	// RatedFractions are the offered loads for the rated sweep, expressed as
	// fractions of the measured saturation RPS (adapter-relative so the
	// targets stay comparable across servers of wildly different throughput).
	RatedFractions []float64

	// TLSTerminator is the https base URL of the shared TLS terminator that
	// fronts the cleartext adapters for the tls-* scenarios. Empty (default)
	// means no terminator is wired, so TLS-capable adapters do NOT advertise
	// fs.TLS and no tls-* cell is scheduled (it would otherwise trip the
	// executeCell capability-lie guard). See scenarios/tls.go.
	TLSTerminator string

	// SeedServices, when non-empty, switches the runner into a one-shot
	// seed-and-exit mode: it connects to the already-running pg/redis/mc
	// backends (started out-of-band by the distributed bench playbook on the
	// bench target — the runner cannot Start them itself because driver
	// fixtures must live where the SUT can reach them) and loads the same
	// fixture set services.Seed loads, then exits. Value is a comma list of
	// "kind=addr" pairs, e.g.
	//   "postgres=postgres://bench:bench@127.0.0.1:54321/bench?sslmode=disable,redis=127.0.0.1:63791,memcached=127.0.0.1:21211"
	// Any omitted kind is skipped. No bench schedule runs in this mode.
	SeedServices string

	// PrintRequiredServices switches the runner into a one-shot mode that
	// resolves the -cells glob against the scenario catalogue (optionally
	// scoped by -competitors) and prints, one per line, the docker service
	// kinds the bench must provision (postgres / redis / memcached) — or
	// nothing when no driver cell is in scope. The distributed-bench
	// orchestrator (mage Bench) calls this to decide whether to start + seed
	// the fixture containers, so the decision reuses the SAME filterCells +
	// requiredServiceKinds the real run uses and can never drift from it.
	PrintRequiredServices bool

	// Competitors is the comma list of competitor column slugs in scope,
	// consulted ONLY by -print-required-services so the "<scenario>/<slug>"
	// match mirrors the per-cell ids the bench actually schedules. Empty =
	// full local adapter registry.
	Competitors string
}

// defaultRatedFractions is the saturation-relative sweep used when none is
// supplied. 25/50/75/90% brackets the SLO knee without probing past
// saturation (where coordinated-omission correction stops being meaningful).
var defaultRatedFractions = []float64{0.25, 0.5, 0.75, 0.9}

// DefaultConfig returns the fresh-flag defaults. Mirrors perfmatrix's
// runner so muscle-memory carries over.
func DefaultConfig() Config {
	return Config{
		Runs:           5,
		Duration:       45 * time.Second,
		Warmup:         10 * time.Second,
		Cooldown:       5 * time.Second,
		Services:       "local",
		Seed:           0,
		RatedMode:      false,
		RatedDuration:  30 * time.Second,
		RatedFractions: defaultRatedFractions,
	}
}

// Bind registers every Config field on fs. Separated from ParseArgs so
// unit tests can drive parsing deterministically.
func (c *Config) Bind(fs *flag.FlagSet) {
	fs.IntVar(&c.Runs, "runs", c.Runs, "number of interleaved passes through the matrix")
	fs.DurationVar(&c.Duration, "duration", c.Duration, "measurement window per cell")
	fs.DurationVar(&c.Warmup, "warmup", c.Warmup, "pre-measurement warmup per cell")
	fs.DurationVar(&c.Cooldown, "cooldown", c.Cooldown, "idle gap between cells to let TCP TIME_WAIT drain")
	fs.StringVar(&c.Cells, "cells", c.Cells,
		`glob filter over "<scenario>/<server>" (e.g. "get-simple/*", "*/celeris-*"; supports "!neg" exclusions)`)
	fs.StringVar(&c.Out, "out", c.Out, "output directory; default results/<timestamp>-<git-ref>/")
	fs.StringVar(&c.Timeseries, "timeseries", c.Timeseries,
		"gzip time-series sidecar path; empty = <out>/timeseries.json.gz")
	fs.StringVar(&c.Services, "services", c.Services, `"local" (Docker on same host) | "none" (skip driver services)`)
	fs.BoolVar(&c.FailFast, "fail-fast", c.FailFast, "abort at the first cell error")
	fs.BoolVar(&c.FDTrace, "fd-trace", c.FDTrace, "log per-cell FD counts even when the delta is zero")
	fs.Int64Var(&c.Seed, "seed", c.Seed, "rng seed for reproducibility echo; 0 = time.Now().UnixNano()")
	fs.StringVar(&c.Target, "target", c.Target,
		"remote base URL (http://host:port) to bench against; empty = spawn local loopback adapters")
	fs.StringVar(&c.TargetArch, "target-arch", c.TargetArch,
		"GOARCH of the host under test (amd64|arm64); empty = this process's arch")
	fs.StringVar(&c.ServerName, "server-name", c.ServerName,
		"friendly server slug recorded in per-cell JSON / report when -target is set")
	fs.BoolVar(&c.DryRun, "dry-run", c.DryRun, "print the resolved schedule and exit without starting adapters")
	fs.BoolVar(&c.RatedMode, "rated", c.RatedMode,
		"run a Gil-Tene rated (closed-loop, CO-corrected) sweep after each saturation pass (opt-in; multiplies per-cell time)")
	fs.DurationVar(&c.RatedDuration, "rated-duration", c.RatedDuration, "measurement window for each rated pass")
	fs.StringVar(&c.TLSTerminator, "tls-terminator", c.TLSTerminator,
		"https base URL of the shared TLS terminator fronting the adapters; empty disables all tls-* scenarios")
	fs.StringVar(&c.SeedServices, "seed-services", c.SeedServices,
		`one-shot seed-and-exit: comma list of "kind=addr" (postgres=<dsn>,redis=<host:port>,memcached=<host:port>) to seed already-running backends, then exit`)
	fs.BoolVar(&c.PrintRequiredServices, "print-required-services", c.PrintRequiredServices,
		"resolve -cells (scoped by -competitors) and print the docker service kinds the bench needs, one per line, then exit")
	fs.StringVar(&c.Competitors, "competitors", c.Competitors,
		"comma list of competitor column slugs scoping -print-required-services; empty = full adapter registry")
	fs.Func("rated-fractions",
		"comma-separated saturation fractions for the rated sweep (default 0.25,0.5,0.75,0.9)",
		func(s string) error {
			fr, err := parseRatedFractions(s)
			if err != nil {
				return err
			}
			c.RatedFractions = fr
			return nil
		})
}

// parseRatedFractions parses a comma-separated list of saturation fractions
// (each in (0,1]) for the rated sweep.
func parseRatedFractions(s string) ([]float64, error) {
	var out []float64
	for _, part := range strings.Split(s, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		f, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, fmt.Errorf("rated fraction %q: %w", p, err)
		}
		if f <= 0 || f > 1 {
			return nil, fmt.Errorf("rated fraction %q out of range (0,1]", p)
		}
		out = append(out, f)
	}
	if len(out) == 0 {
		return nil, fmt.Errorf("no valid rated fractions in %q", s)
	}
	return out, nil
}

// ParseArgs parses argv (without the program name). out receives flag
// usage / error output (typically os.Stderr).
func ParseArgs(args []string, out io.Writer) (Config, error) {
	cfg := DefaultConfig()
	fs := flag.NewFlagSet("probatorium-runner", flag.ContinueOnError)
	fs.SetOutput(out)
	cfg.Bind(fs)
	if err := fs.Parse(args); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// cellResultFile is the per-cell on-disk JSON shape.
type cellResultFile struct {
	RunIdx       int       `json:"run_idx"`
	ScenarioName string    `json:"scenario"`
	ServerName   string    `json:"server"`
	Category     string    `json:"category"`
	TargetAddr   string    `json:"target_addr"`
	StartedAt    time.Time `json:"started_at"`
	CompletedAt  time.Time `json:"completed_at"`
	Error        string    `json:"error,omitempty"`

	// Status is the classified per-cell outcome ("ok"/"not_applicable"/
	// "dnf"), schema v5.3. Persisted so the cluster merge path can read it
	// back verbatim and the in-process and cluster paths agree on field
	// width. Derived from Error via report.ClassifyCellError; empty on the
	// rare pre-loadgen mkdir-failure path (treated as OK by readers).
	Status       string          `json:"status,omitempty"`
	Result       *loadgen.Result `json:"result,omitempty"`
	HistogramB64 string          `json:"hdr_histogram_b64,omitempty"`
	FDsBefore    int             `json:"fds_before,omitempty"`
	FDsAfterStop int             `json:"fds_after_stop,omitempty"`
	FDsLeaked    int             `json:"fds_leaked,omitempty"`

	// SaturationModeRPS echoes the open-loop saturation pass's RPS so the
	// rated targets below are interpretable as a fraction of it.
	SaturationModeRPS float64 `json:"saturation_mode_rps,omitempty"`

	// RatedPasses is the per-cell rated sweep: one entry per offered load,
	// each carrying the target RPS and the CO-corrected P99 at that load.
	// Empty unless rated mode ran.
	RatedPasses []ratedPassFile `json:"rated_passes,omitempty"`
}

// ratedPassFile is one rated pass recorded in the per-cell JSON.
type ratedPassFile struct {
	TargetRPS float64       `json:"target_rps"`
	P99       time.Duration `json:"p99"`
}

// runManifest is the top-level summary written next to per-cell files.
type runManifest struct {
	Config      Config    `json:"config"`
	StartedAt   time.Time `json:"started_at"`
	CompletedAt time.Time `json:"completed_at"`
	Host        hostInfo  `json:"host"`
	Seed        int64     `json:"seed"`
	GitSHA      string    `json:"git_sha,omitempty"`
	GoVersion   string    `json:"go_version"`
	Scenarios   []string  `json:"scenarios"`
	Servers     []string  `json:"servers"`
	CellCount   int       `json:"cell_count"`
	Cells       []string  `json:"cells"`
}

type hostInfo struct {
	OS       string `json:"os"`
	Arch     string `json:"arch"`
	Hostname string `json:"hostname,omitempty"`
}

func main() {
	cfg, err := ParseArgs(os.Args[1:], os.Stderr)
	if err != nil {
		os.Exit(2)
	}
	// One-shot seed-and-exit mode (distributed bench driver-backend seeding).
	// Runs before any schedule resolution so it is a pure connect-seed-exit.
	if cfg.SeedServices != "" {
		if err := seedServicesAndExit(cfg.SeedServices); err != nil {
			fmt.Fprintf(os.Stderr, "probatorium-runner: seed-services: %v\n", err)
			os.Exit(1)
		}
		return
	}
	// One-shot capability query: print the docker service kinds the resolved
	// -cells glob needs, then exit. No schedule runs. Lets mage Bench gate the
	// fixture-container start on the runner's own resolution (single source of
	// truth — see Config.PrintRequiredServices).
	if cfg.PrintRequiredServices {
		if err := printRequiredServicesAndExit(cfg); err != nil {
			fmt.Fprintf(os.Stderr, "probatorium-runner: print-required-services: %v\n", err)
			os.Exit(1)
		}
		return
	}
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "probatorium-runner: %v\n", err)
		os.Exit(1)
	}
}

// seedServicesAndExit parses the -seed-services "kind=addr" list and loads the
// driver fixture set into the named already-running backends via
// services.SeedExternal. Used by the distributed bench: the playbook starts
// pg/redis/mc on the bench target, then invokes the bench-target-side runner
// with -seed-services so the driver-* scenarios hit byte-identical seeded data
// to a local in-process run.
func seedServicesAndExit(spec string) error {
	var pgDSN, redisAddr, mcAddr string
	for _, part := range strings.Split(spec, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kind, addr, ok := strings.Cut(part, "=")
		if !ok {
			return fmt.Errorf("bad -seed-services entry %q (want kind=addr)", part)
		}
		switch strings.TrimSpace(kind) {
		case "postgres", "pg":
			pgDSN = strings.TrimSpace(addr)
		case "redis":
			redisAddr = strings.TrimSpace(addr)
		case "memcached", "mc":
			mcAddr = strings.TrimSpace(addr)
		default:
			return fmt.Errorf("unknown -seed-services kind %q", kind)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	fmt.Fprintf(os.Stderr, "probatorium-runner: seeding services (pg=%t redis=%t mc=%t)...\n",
		pgDSN != "", redisAddr != "", mcAddr != "")
	if err := services.SeedExternal(ctx, pgDSN, redisAddr, mcAddr); err != nil {
		return err
	}
	fmt.Fprintln(os.Stderr, "probatorium-runner: seed complete")
	return nil
}

// printRequiredServicesAndExit resolves the -cells glob against the scenario
// catalogue (scoped by -competitors when given) and prints the docker service
// kinds the bench must provision, one per line, to stdout. It deliberately
// reuses the SAME filterCells + requiredServiceKinds the real schedule uses, so
// the orchestrator's container-start decision can never diverge from what the
// runner would actually execute.
//
// -competitors mirrors the per-cell "<scenario>/<slug>" ids the distributed
// bench schedules: each slug becomes a synthetic adapter so a glob like
// "driver-pg-read/celeris-*" matches exactly as it will at run time. Empty
// falls back to the full local adapter registry (the in-process bench path).
// Capability gating is intentionally NOT applied here — requiredServiceKinds is
// purely scenario-driven, and if a driver scenario is scheduled at all the
// backend must exist (the celeris SUT is always driver-capable).
func printRequiredServicesAndExit(cfg Config) error {
	scs := scenarios.Registry()
	var advs []servers.Adapter
	if strings.TrimSpace(cfg.Competitors) != "" {
		for _, s := range strings.Split(cfg.Competitors, ",") {
			name := strings.TrimSpace(s)
			if name == "" {
				continue
			}
			advs = append(advs, servers.Adapter{Name: name, Category: "remote"})
		}
	} else {
		advs = servers.AdaptersSorted()
	}
	effSc, _, err := filterCells(scs, advs, cfg.Cells)
	if err != nil {
		return err
	}
	for _, k := range requiredServiceKinds(effSc) {
		fmt.Println(k)
	}
	return nil
}

// run is the orchestrator entry point, separated from main so it can be
// driven from tests without exec'ing a subprocess.
func run(cfg Config) error {
	if cfg.Seed == 0 {
		cfg.Seed = time.Now().UnixNano()
	}
	// BENCH_RATED env wins over the flag so the mage Bench path can forward
	// it as an extra-var without rewriting the runner invocation. "1"/"true"
	// turns rated on; any other value (or unset) leaves the flag default.
	if v := os.Getenv("BENCH_RATED"); v == "1" || v == "true" {
		cfg.RatedMode = true
	}
	if cfg.RatedMode {
		fmt.Fprintf(os.Stderr, "probatorium-runner: rated mode ON (fractions=%v duration=%s)\n",
			cfg.RatedFractions, cfg.RatedDuration)
	}
	fmt.Fprintf(os.Stderr, "probatorium-runner: seed=%d\n", cfg.Seed)

	if cfg.Out == "" {
		ts := time.Now().UTC().Format("20060102T150405Z")
		ref := shortGitSHA()
		if ref == "" {
			ref = "local"
		}
		cfg.Out = filepath.Join("results", ts+"-"+ref)
	}
	if !cfg.DryRun {
		if err := os.MkdirAll(cfg.Out, 0o755); err != nil {
			return fmt.Errorf("mkdir %s: %w", cfg.Out, err)
		}
	}

	scs := scenarios.Registry()

	// In remote-target mode the cross-product of adapters is replaced by
	// a single synthetic server named after the competitor slug, so the
	// schedule is scenarios × 1 × runs. The synthetic adapter is still
	// fed through filterCells so the "<scenario>/<slug>" -cells globs
	// match the slug ansible passes.
	advs := servers.AdaptersSorted()
	if cfg.Target != "" {
		advs = []servers.Adapter{{Name: remoteServerName(cfg), Category: "remote"}}
	}

	effSc, effAdv, err := filterCells(scs, advs, cfg.Cells)
	if err != nil {
		return fmt.Errorf("filter cells: %w", err)
	}

	// Wrap each Adapter in an adapterServer so interleave.Schedule (which
	// works against the legacy in-process Server interface) can consume
	// them. The wrapper carries the adapter's FeatureSet and Name through
	// to the scheduler so applicability gating still fires.
	srvs := make([]servers.Server, 0, len(effAdv))
	tlsReady := cfg.TLSTerminator != ""
	for _, a := range effAdv {
		fs := featureSetFor(a, tlsReady)
		if cfg.Target != "" {
			// Remote-target mode: the synthetic adapter `a` carries only the
			// -server-name slug, not a capability manifest. But that slug is a
			// servers.Registry column key (the cluster bench expands the matrix
			// straight from the registry), so we look the REAL adapter up and
			// gate scenarios on its DECLARED capabilities — identical to local
			// mode. Without this, capability-gated scenarios (driver / chain /
			// ws / sse / tls) get scheduled against a static-only competitor
			// that can't serve them, burning a full measurement window on a
			// 404-storm zero-request cell (a static-only adapter would spend
			// ~20s/route × every gated scenario producing nothing but errors).
			// A -server-name that is NOT a registry key (an ad-hoc manual run)
			// falls back to the permissive set so nothing is silently dropped.
			if real, ok := servers.Registry[a.Name]; ok {
				fs = featureSetFor(real, tlsReady)
			} else {
				fs = remoteFeatureSet()
			}
		}
		srvs = append(srvs, &adapterServer{adapter: a, features: fs})
	}

	svcKinds := requiredServiceKinds(effSc)
	switch cfg.Services {
	case "none":
		svcKinds = nil
	case "local", "":
	default:
		return fmt.Errorf("unknown -services value %q (want \"local\" or \"none\")", cfg.Services)
	}

	rootCtx, rootCancel := context.WithCancel(context.Background())
	defer rootCancel()
	sink := newResultsSink(cfg, time.Now().UTC())
	installSignalHandler(rootCancel, sink)

	var handles *services.Handles
	if !cfg.DryRun && len(svcKinds) > 0 {
		fmt.Fprintf(os.Stderr, "probatorium-runner: starting services %v\n", svcKinds)
		handles, err = services.Start(rootCtx, svcKinds...)
		if err != nil {
			return fmt.Errorf("start services: %w", err)
		}
		defer func() {
			stopCtx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			if serr := handles.Stop(stopCtx); serr != nil {
				fmt.Fprintf(os.Stderr, "probatorium-runner: services.Stop: %v\n", serr)
			}
		}()
		if err := handles.Seed(rootCtx); err != nil {
			return fmt.Errorf("seed services: %w", err)
		}
	}

	schedule := interleave.Schedule(cfg.Runs, effSc, srvs)

	if cfg.DryRun {
		_, _ = fmt.Fprintf(os.Stderr, "probatorium-runner: dry-run; %d cells across %d scenarios × %d adapters × %d runs\n",
			len(schedule), len(effSc), len(effAdv), cfg.Runs)
		for _, cell := range schedule {
			_, _ = fmt.Fprintf(os.Stdout, "run%d %s/%s\n",
				cell.RunIdx, cell.Scenario.Name(), cell.Server.Name())
		}
		return nil
	}

	if len(schedule) == 0 {
		fmt.Fprintln(os.Stderr, "probatorium-runner: no cells matched; nothing to do")
		return writeManifest(cfg, effSc, srvs, schedule)
	}

	manifestStart := sink.started
	fmt.Fprintf(os.Stderr, "probatorium-runner: %d cells across %d scenarios × %d adapters × %d runs\n",
		len(schedule), len(effSc), len(effAdv), cfg.Runs)

	preRunFDs := countProcessFDs()

	var firstErr error
	for i, cell := range schedule {
		if rootCtx.Err() != nil {
			// First signal landed between cells: mark everything that
			// never started as interrupted (per-cell JSON + final write)
			// instead of silently truncating the matrix.
			fmt.Fprintln(os.Stderr, "probatorium-runner: cancelled; marking remaining cells interrupted")
			markCellsInterrupted(cfg, sink, schedule[i:])
			break
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] run=%d scenario=%s server=%s\n",
			i+1, len(schedule), cell.RunIdx, cell.Scenario.Name(), cell.Server.Name())

		res, cerr := executeCell(rootCtx, cfg, cell)
		// recordRun keeps per-run statuses (schema v5.4) so a later OK
		// run can never erase this run's evidence; suspect outcomes keep
		// their samples. See resultsSink.recordRun / reduceCellStatus.
		sink.recordRun(cell, res, cfg.RatedMode)
		// Incremental persistence: results.json is complete and parseable
		// after every cell, so even a SIGKILL loses at most the in-flight
		// cell — never the column.
		if ferr := sink.flush(); ferr != nil {
			fmt.Fprintf(os.Stderr, "  flush results.json: %v\n", ferr)
		}
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "  cell error: %v\n", cerr)
			if cfg.FailFast {
				firstErr = fmt.Errorf("fail-fast: %s/%s: %w",
					cell.Scenario.Name(), cell.Server.Name(), cerr)
				break
			}
		}

		// No cooldown after a server-down mark: the cell never drove any
		// load, and the column should finish its probe-and-mark sweep
		// fast (seconds per dead cell, not cooldown × remaining cells).
		if strings.HasPrefix(res.ErrorMsg, "server-down:") {
			continue
		}
		if i+1 < len(schedule) && cfg.Cooldown > 0 {
			select {
			case <-rootCtx.Done():
			case <-time.After(cfg.Cooldown):
			}
		}
	}

	postRunFDs := countProcessFDs()
	if cfg.FDTrace || postRunFDs != preRunFDs {
		fmt.Fprintf(os.Stderr, "probatorium-runner: FD-leak summary — pre=%d post=%d delta=%+d\n",
			preRunFDs, postRunFDs, postRunFDs-preRunFDs)
	}

	cells := sink.cellsSnapshot()
	agg := report.Aggregate(cells)
	ts := report.BuildTimeseries(cells)

	doc := buildDocument(cfg, agg, manifestStart)
	if err := writeReports(cfg, doc, agg, ts, manifestStart); err != nil {
		return fmt.Errorf("write reports: %w", err)
	}

	if err := writeManifest(cfg, effSc, srvs, schedule); err != nil {
		return fmt.Errorf("write manifest: %w", err)
	}

	fmt.Fprintf(os.Stderr, "probatorium-runner: done; started=%s duration=%s out=%s\n",
		manifestStart.Format(time.RFC3339),
		time.Since(manifestStart).Round(time.Millisecond),
		cfg.Out)
	return firstErr
}

// cellOutcome bundles the artefacts of one cell run for the orchestrator.
type cellOutcome struct {
	Result       *loadgen.Result
	HistogramB64 string

	// RatedSamples is the rated sweep for this run (target RPS → P99),
	// empty when rated mode was off. Threaded into report.CellResult so
	// Aggregate can reduce it into LatencyAtSLO.
	RatedSamples []report.RatedSample

	// Status is the classified outcome of this cell (schema v5.3).
	// ErrorMsg is the synthesised per-cell error string it was classified
	// from (empty for an OK cell). Both survive even when Result is nil
	// (a DNF cell never produced a loadgen.Result), so the collection loop
	// can record a not-applicable / did-not-finish cell instead of
	// dropping it or ranking it as a 0-RPS also-ran.
	Status   report.CellStatus
	ErrorMsg string
}

// resultsSink owns the cross-cell collection map and writes results.json
// incrementally — atomic temp+rename after every recorded run — so a
// SIGKILLed runner (the ansible hang-guard escalates) still leaves a
// parseable, current-to-the-last-cell column file. v3.8 lost the entire
// celeris-epoll-h1-sync column this way: the second signal force-exited
// before the single end-of-run write, stranding 27 good cells in
// per-cell JSONs the ingest never reads. Methods are safe for concurrent
// use; the second-signal handler calls flushBestEffort from its own
// goroutine.
type resultsSink struct {
	mu        sync.Mutex
	cfg       Config
	started   time.Time
	collected map[string]*report.CellResult

	// demoted marks cells where at least one run produced a
	// SUT-behaviour failure (anything but a harness-side "interrupted:"
	// mark). Such a cell may keep its data but can never publish as
	// plain OK again — an OK rerun must not erase the evidence (the
	// v3.8 OK-promotion bug, main.go pre-v3.9 collection loop).
	demoted map[string]bool

	// firstErr remembers the first non-OK error string per cell so the
	// surviving ErrorMsg points at the ORIGINAL failure, not the latest.
	firstErr map[string]string

	// dirty is set once anything was recorded, so the best-effort signal
	// flush never creates an empty results.json for dry-run / no-cell
	// invocations.
	dirty bool
}

func newResultsSink(cfg Config, started time.Time) *resultsSink {
	return &resultsSink{
		cfg:       cfg,
		started:   started,
		collected: map[string]*report.CellResult{},
		demoted:   map[string]bool{},
		firstErr:  map[string]string{},
	}
}

// recordRun folds one run outcome into the per-cell result. The per-run
// status is ALWAYS appended (schema v5.4 RunStatuses) and the cell-level
// status re-reduced via reduceCellStatus, so the final results.json
// carries every run's evidence regardless of outcome order.
func (s *resultsSink) recordRun(cell interleave.Cell, out cellOutcome, rated bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	status := out.Status
	if status == "" {
		status = report.ClassifyCellError(out.ErrorMsg)
	}
	key := report.CellID(cell.Scenario.Name(), cell.Server.Name())
	cr := s.collected[key]
	if cr == nil {
		cr = &report.CellResult{
			ScenarioName: cell.Scenario.Name(),
			ServerName:   cell.Server.Name(),
			Category:     cell.Scenario.Category(),
		}
		s.collected[key] = cr
	}
	cr.RunStatuses = append(cr.RunStatuses, status)
	if status != report.CellOK {
		if _, ok := s.firstErr[key]; !ok {
			s.firstErr[key] = out.ErrorMsg
		}
		// A harness-side interruption says nothing about the SUT — it
		// must not turn a cell with otherwise-clean data suspect.
		if !strings.HasPrefix(out.ErrorMsg, "interrupted:") {
			s.demoted[key] = true
		}
	}
	// Suspect runs carry a real measurement (that is the point of the
	// status); OK runs always do. DNF / N/A runs never append a bogus
	// 0-RPS sample.
	if status.HasData() && out.Result != nil {
		cr.Samples = append(cr.Samples, *out.Result)
		cr.HistogramsB64 = append(cr.HistogramsB64, out.HistogramB64)
		if rated {
			cr.RatedSamples = append(cr.RatedSamples, out.RatedSamples)
		}
	}
	cr.Status = reduceCellStatus(cr.RunStatuses, len(cr.Samples) > 0, s.demoted[key])
	if cr.Status == report.CellOK {
		cr.ErrorMsg = ""
	} else {
		cr.ErrorMsg = s.firstErr[key]
	}
	s.dirty = true
}

// cellsSnapshot returns the collected cells as a sorted value slice —
// the shape Aggregate / BuildTimeseries consume. Sorting keeps the
// rendered output byte-stable across runs.
func (s *resultsSink) cellsSnapshot() []report.CellResult {
	s.mu.Lock()
	defer s.mu.Unlock()
	keys := make([]string, 0, len(s.collected))
	for k := range s.collected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	cells := make([]report.CellResult, 0, len(keys))
	for _, k := range keys {
		cells = append(cells, *s.collected[k])
	}
	return cells
}

// flush writes the current aggregate to <out>/results.json (atomic
// temp+rename via writeJSON). Called after every cell and on shutdown.
func (s *resultsSink) flush() error {
	cells := s.cellsSnapshot()
	agg := report.Aggregate(cells)
	doc := buildDocument(s.cfg, agg, s.started)
	return writeJSON(filepath.Join(s.cfg.Out, "results.json"), doc)
}

// flushBestEffort is the second-signal path: it never panics and never
// blocks the exit on an error — the per-cell incremental flush already
// left a parseable results.json current to the last finished cell, so
// losing this final write costs at most the interrupted-cell marks.
func (s *resultsSink) flushBestEffort() {
	defer func() {
		if r := recover(); r != nil {
			fmt.Fprintf(os.Stderr, "probatorium-runner: final flush panic: %v\n", r)
		}
	}()
	s.mu.Lock()
	dirty := s.dirty
	s.mu.Unlock()
	if !dirty {
		return
	}
	if err := s.flush(); err != nil {
		fmt.Fprintf(os.Stderr, "probatorium-runner: final flush: %v\n", err)
	}
}

// reduceCellStatus folds per-run statuses into the cell-level status.
// All-OK stays OK. A cell with data whose only blemishes are harness
// interruptions (demoted=false) also stays OK — RunStatuses still
// carries the evidence. A cell with data plus any SUT-behaviour failure
// is suspect: the data exists, but a sibling run crashed / lied /
// stormed, so an OK rerun no longer erases the record into a clean
// "ok". With no data at all, any DNF run wins (loud) over
// not-applicable.
func reduceCellStatus(runs []report.CellStatus, hasData, demoted bool) report.CellStatus {
	allOK := true
	anyDNF := false
	for _, st := range runs {
		switch st {
		case report.CellOK:
		case report.CellDNF:
			allOK = false
			anyDNF = true
		default:
			allOK = false
		}
	}
	switch {
	case allOK:
		return report.CellOK
	case hasData && demoted:
		return report.CellSuspect
	case hasData:
		return report.CellOK
	case anyDNF:
		return report.CellDNF
	default:
		return report.CellNotApplicable
	}
}

// markCellsInterrupted records every not-yet-started cell as DNF
// "interrupted" after the first signal, writing the per-cell JSON for
// each so the cluster ingest sees an explicit outcome instead of a
// missing file, then flushes once. v3.8's hang-guard SIGTERM simply
// stopped the loop here, and the in-flight truncation surfaced later as
// bogus 354µs "zero-request cells" classified not_applicable.
func markCellsInterrupted(cfg Config, sink *resultsSink, cells []interleave.Cell) {
	const reason = "interrupted: run cancelled before cell start"
	for _, cell := range cells {
		writeSkippedCellFile(cfg, cell, reason)
		sink.recordRun(cell, cellOutcome{Status: report.CellDNF, ErrorMsg: reason}, cfg.RatedMode)
	}
	if err := sink.flush(); err != nil {
		fmt.Fprintf(os.Stderr, "probatorium-runner: flush after interrupt: %v\n", err)
	}
}

// writeSkippedCellFile persists a per-cell JSON for a cell that never
// drove load (interrupted before start). Same shape executeCell writes,
// so the cluster merge path needs no special case.
func writeSkippedCellFile(cfg Config, cell interleave.Cell, errMsg string) {
	now := time.Now().UTC()
	cellRes := cellResultFile{
		RunIdx:       cell.RunIdx,
		ScenarioName: cell.Scenario.Name(),
		ServerName:   cell.Server.Name(),
		Category:     cell.Scenario.Category(),
		StartedAt:    now,
		CompletedAt:  now,
		Error:        errMsg,
		Status:       string(report.ClassifyCellError(errMsg)),
	}
	outFile := filepath.Join(cfg.Out, fmt.Sprintf("run%d", cell.RunIdx),
		cell.Scenario.Name(), cell.Server.Name()+".json")
	if err := writeJSON(outFile, &cellRes); err != nil {
		fmt.Fprintf(os.Stderr, "  write %s: %v\n", outFile, err)
	}
}

// buildCellConfig maps a scenario's Workload onto baseURL and overlays
// the run-wide duration / warmup / worker defaults. Pure (no I/O, no
// live server), so the scenario→loadgen.Config mapping is unit-testable
// without booting an adapter. Both the local and remote executeCell
// paths funnel through here.
func buildCellConfig(cell interleave.Cell, baseURL string, cfg Config) loadgen.Config {
	lgCfg := cell.Scenario.Workload(baseURL)
	if lgCfg.URL == "" {
		lgCfg.URL = baseURL + "/"
	}
	lgCfg.Duration = cfg.Duration
	lgCfg.Warmup = cfg.Warmup
	if lgCfg.Connections > 0 {
		// loadgen sizes EVERY driver's concurrency from Workers, never
		// from Config.Connections: Mode drivers (ws-*/sse-fanout) hold one
		// stream per worker, and the keep-alive H1 pool dials
		// Workers×connsPerWorker conns (h1client numConns). Under the old
		// 64-worker default that made the concurrency axis fictional —
		// fanout-128 vs -1024 opened 64 streams each, and get-json (128),
		// get-simple-1c (1) and get-simple-1024c (1024) all ran 64 conns.
		// Map the scenario's declared Connections onto Workers so each
		// cell runs the concurrency its row label claims. (Connections
		// stays set too: documentation, and correct if a later loadgen
		// honours it directly.) No scenario sets Workers explicitly, so
		// this mapping is total; the 64 default below only covers
		// workloads that declare no Connections at all.
		lgCfg.Workers = lgCfg.Connections
	}
	if lgCfg.Workers == 0 {
		lgCfg.Workers = 64
	}
	// Enable the loadgen self-CPU sampler (1Hz, P95 on Result.CPUPctP95).
	// buildCellConfig starts from Scenario.Workload(), NOT loadgen.DefaultConfig
	// (which sets CPUMonitor=true), so without this the sampler stays off and
	// every published loadgen_cpu_p95 is empty — which also disables the
	// network-bound classifier that reads it. The whole-system ClientCPUPercent
	// monitor is always on; this is the per-process P95 the report consumes.
	lgCfg.CPUMonitor = true
	return lgCfg
}

// executeCell drives loadgen for one cell and writes the per-cell JSON
// file. In local mode it boots the adapter on a free loopback port and
// records FD-leak deltas around it; in remote-target mode (cfg.Target
// set) it skips the adapter lifecycle entirely and drives loadgen at the
// already-running remote base URL. Returns the loadgen result on success
// and a synthesised error otherwise; the per-cell JSON is written either
// way so a partial matrix still produces inspectable artefacts.
func executeCell(parent context.Context, cfg Config, cell interleave.Cell) (out cellOutcome, _ error) {
	outDir := filepath.Join(cfg.Out, fmt.Sprintf("run%d", cell.RunIdx), cell.Scenario.Name())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		err = fmt.Errorf("mkdir %s: %w", outDir, err)
		// Classified explicitly because the persisting defer below is not
		// registered yet — without this the cell would surface as a
		// status-less (treated-OK) zero row in the collection.
		return cellOutcome{Status: report.CellDNF, ErrorMsg: err.Error()}, err
	}
	outFile := filepath.Join(outDir, cell.Server.Name()+".json")

	cellRes := cellResultFile{
		RunIdx:       cell.RunIdx,
		ScenarioName: cell.Scenario.Name(),
		ServerName:   cell.Server.Name(),
		Category:     cell.Scenario.Category(),
		StartedAt:    time.Now().UTC(),
	}
	defer func() {
		cellRes.CompletedAt = time.Now().UTC()
		// Classify the outcome from the single error string the body
		// recorded, so the in-process collection loop and the cluster
		// merge path (which re-reads this JSON's `error` field) agree on
		// the same CellStatus. The classification is persisted on the
		// per-cell JSON so the cluster path can read it back verbatim
		// rather than re-deriving and risking a field-width disagreement.
		cellRes.Status = string(report.ClassifyCellError(cellRes.Error))
		out.Status = report.CellStatus(cellRes.Status)
		out.ErrorMsg = cellRes.Error
		if werr := writeJSON(outFile, &cellRes); werr != nil {
			fmt.Fprintf(os.Stderr, "  write %s: %v\n", outFile, werr)
		}
	}()

	var lgURL string
	var probeAddr string // dialable SUT addr for the pre/post liveness probes
	if cfg.Target != "" {
		// Remote-target mode: the SUT is already running on another host.
		// Skip freePort / StartAdapter / waitForTCP and the FD-leak scope —
		// the runner's /proc/self/fd is unrelated to the remote SUT, whose
		// FD/RSS is covered by the cluster-side CPU/observer sidecar.
		cellRes.TargetAddr = cfg.Target
		lgURL = cfg.Target
		// SUT liveness gate (v3.9): when the SUT has already crashed
		// (celeris v1.4.15's io_uring heap corruption took it down
		// mid-column in v3.8) every remaining cell used to burn its full
		// measurement window against a dead port and come back as a
		// 34.7M-error "zero-request" N/A. Probe first: a dead target
		// marks the cell DNF in seconds, and the column finishes its
		// probe-and-mark sweep fast (the run loop skips the cooldown
		// after a server-down cell). The probe re-runs per cell, so a SUT
		// brought back by systemd resumes real measurements automatically.
		if addr, ok := hostPortFromURL(cfg.Target); ok {
			probeAddr = addr
			if perr := probeSUT(parent, addr); perr != nil {
				cellRes.Error = "server-down: pre-cell probe: " + perr.Error()
				return cellOutcome{}, errors.New(cellRes.Error)
			}
		}
	} else {
		cellRes.FDsBefore = countProcessFDs()

		port, err := freePort()
		if err != nil {
			cellRes.Error = "free port: " + err.Error()
			return cellOutcome{}, err
		}
		bindAddr := fmt.Sprintf("127.0.0.1:%d", port)
		cellRes.TargetAddr = bindAddr
		lgURL = "http://" + bindAddr

		startCtx, startCancel := context.WithTimeout(parent, 10*time.Second)
		stop, err := servers.StartAdapter(startCtx, cell.Server.Name(), bindAddr)
		startCancel()
		if err != nil {
			cellRes.Error = "adapter start: " + err.Error()
			return cellOutcome{}, err
		}
		defer func() {
			if serr := stop(); serr != nil {
				fmt.Fprintf(os.Stderr, "  adapter stop: %v\n", serr)
			}
			cellRes.FDsAfterStop = countProcessFDs()
			cellRes.FDsLeaked = cellRes.FDsAfterStop - cellRes.FDsBefore
			if cellRes.FDsLeaked != 0 || cfg.FDTrace {
				fmt.Fprintf(os.Stderr, "  cell-fd: scenario=%s server=%s before=%d after=%d diff=%+d\n",
					cell.Scenario.Name(), cell.Server.Name(),
					cellRes.FDsBefore, cellRes.FDsAfterStop, cellRes.FDsLeaked)
			}
		}()

		if err := waitForTCP(parent, bindAddr, 10*time.Second); err != nil {
			cellRes.Error = "ready-check: " + err.Error()
			return cellOutcome{}, err
		}
		// Local mode needs no pre-cell probe — waitForTCP above IS the
		// gate — but the post-cell probe below still wants the addr (the
		// adapter is alive until the deferred stop(), so a dead probe
		// after an anomalous result means it crashed mid-cell).
		probeAddr = bindAddr
	}

	lgCfg := buildCellConfig(cell, lgURL, cfg)

	timeout := 5*cfg.Warmup + 2*cfg.Duration + 60*time.Second
	cellCtx, cellCancel := context.WithTimeout(parent, timeout)
	defer cellCancel()

	bm, err := loadgen.New(lgCfg)
	if err != nil {
		cellRes.Error = "loadgen.New: " + err.Error()
		return cellOutcome{}, err
	}
	res, err := bm.Run(cellCtx)
	if err != nil {
		cellRes.Error = loadgenRunCellError(parent, err, probeAddr)
		return cellOutcome{}, errors.New(cellRes.Error)
	}
	cellRes.Result = res
	oc := cellOutcome{Result: res}
	if res == nil {
		cellRes.Error = "loadgen.Run: returned nil result"
		return oc, errors.New(cellRes.Error)
	}
	// res.Histogram is ALREADY the hdr-encoded base64 wire form (loadgen's
	// EncodeHistogram -> hdrhistogram-go Encode base64-encodes its output), which
	// is exactly what report.mergeHistograms' hdr.Decode expects. Carry it
	// VERBATIM onto the in-process outcome (resultsSink.recordRun appends
	// oc.HistogramB64 to CellResult.HistogramsB64) and the on-disk per-cell file.
	// Without this the in-process / single-node aggregation dropped the HDR
	// distribution entirely; re-base64'ing it instead would double-encode and
	// make Decode fail silently (the bug the cluster path had). The headline
	// blob is the saturation pass's, which is exactly the res captured here.
	if len(res.Histogram) > 0 {
		oc.HistogramB64 = string(res.Histogram)
		cellRes.HistogramB64 = string(res.Histogram)
	}

	in := completedCell{
		ScenarioName:  cell.Scenario.Name(),
		ServerName:    cell.Server.Name(),
		Category:      cell.Scenario.Category(),
		Requests:      res.Requests,
		Errors:        res.Errors,
		ConnectErrors: res.ConnectErrors,
		Duration:      res.Duration,
		Interrupted:   parent.Err() != nil,
		ServerAlive:   true,
		ErrorBudget:   scenarios.ErrorBudgetFor(cell.Scenario),
	}
	// Post-cell liveness probe, lazily: only an anomalous cell (zero
	// requests, or error ratio past 50%) needs the dead-or-alive fact to
	// classify; an interrupted cell is classified as such before the
	// probe result would matter.
	if !in.Interrupted && probeAddr != "" &&
		(in.Requests == 0 || errorRatio(in.Requests, in.Errors) > 0.5) {
		if perr := probeSUT(parent, probeAddr); perr != nil {
			in.ServerAlive = false
			in.ProbeErr = perr.Error()
		}
	}
	if v := classifyCompletedCell(in); v.ErrMsg != "" {
		cellRes.Error = v.ErrMsg
		if v.Hard {
			// With -fail-fast this aborts the matrix; otherwise the cell
			// records a non-nil error and the run exits non-zero.
			return oc, errors.New(v.ErrMsg)
		}
		// Suspect: keep the measurement — surfacing the number next to
		// the flag is the point of the status — but skip the rated sweep
		// below, whose targets would anchor on a saturation figure we
		// just flagged.
		fmt.Fprintf(os.Stderr, "  cell flagged: %s\n", v.ErrMsg)
		return oc, nil
	}

	// Rated sweep: after the open-loop saturation pass, drive loadgen at
	// fractions of the measured saturation RPS in closed-loop (CO-corrected)
	// mode. Each pass sets loadgen.Config.Rate so coordinated-omission
	// correction is applied by loadgen itself — never hand-roll a pacer here,
	// which would defeat the correction. The saturation pass is reused as the
	// scale anchor, so the targets stay adapter-relative.
	if cfg.RatedMode && res != nil && res.RequestsPerSec > 0 {
		cellRes.SaturationModeRPS = res.RequestsPerSec
		samples, passes := runRatedSweep(parent, cfg, lgCfg, res.RequestsPerSec)
		oc.RatedSamples = samples
		cellRes.RatedPasses = passes
	}
	return oc, nil
}

// completedCell carries the facts classifyCompletedCell folds into a
// verdict for a cell whose loadgen pass RETURNED. (The pre-loadgen
// failure paths — adapter start, ready-check, server-down pre-probe —
// keep their own error strings.)
type completedCell struct {
	ScenarioName string
	ServerName   string
	Category     string
	Requests     int64
	Errors       int64

	// ConnectErrors is loadgen's dial/handshake-failure subset of Errors
	// (Result.ConnectErrors, loadgen >= the c902b92 pin; zero from older
	// builds). Errors ≈ ConnectErrors means the server was unreachable,
	// not misbehaving — additive evidence the rules below use to sharpen
	// dead-SUT detection beyond the post-probe.
	ConnectErrors uint64

	Duration time.Duration

	// Interrupted is true when the run context was cancelled while the
	// cell was in flight: the measurement window was truncated, so
	// whatever came back is not a sample.
	Interrupted bool

	// ServerAlive is the post-cell probe verdict; ProbeErr is the dial
	// error when dead. Only consulted for anomalous cells — the caller
	// probes lazily.
	ServerAlive bool
	ProbeErr    string

	// ErrorBudget is the scenario's error-ratio ceiling
	// (scenarios.ErrorBudgetFor).
	ErrorBudget float64
}

// cellVerdict is classifyCompletedCell's outcome: the synthesised
// machine-readable cell error ("" = clean) and whether it is a hard
// failure (DNF / N/A — counts toward -fail-fast and a non-zero exit) or
// a soft flag (suspect — the measurement is kept).
type cellVerdict struct {
	ErrMsg string
	Hard   bool
}

// errorRatio is errors/(errors+requests) — the fraction of attempted
// operations that failed. 0 when nothing happened at all.
func errorRatio(requests, errors int64) float64 {
	total := requests + errors
	if total <= 0 {
		return 0
	}
	return float64(errors) / float64(total)
}

// classifyCompletedCell decides what a returned loadgen pass actually
// was. Decision order (first match wins), each rule pinned to the v3.8
// cell that motivated it:
//
//  1. Run context cancelled → "interrupted:" (DNF). The ansible
//     hang-guard SIGTERM left 354µs/549µs zero-request stubs that v3.8
//     published as not_applicable.
//  2. Anomalous (zero requests, or error ratio > 50%) + post-probe dead
//     → "server-died-mid-cell:" (DNF). The io_uring heap corruption
//     crashed the SUT 4029 requests into chain-api-post-4k; the 33.1M
//     post-crash dial errors then tripped the old ratio-based
//     capability-lie guard (N/A), and the following dead-port streaming
//     cells (0 req / 34.7M err) published as zero-request N/A.
//  3. Zero requests + errors overwhelmingly connect-class →
//     "server-down:" (DNF) EVEN when the post-probe passed: a SUT
//     flapping under systemd restart can answer the probe's three
//     spaced dials while refusing every loadgen connect for the whole
//     window. Connect-class evidence outranks the probe — ConnectErrors
//     counts dial/handshake failures only, which a live server serving
//     wrong answers can never produce in bulk. (Caveat: loadgen counts
//     WS/SSE upgrade rejections as connect-class too, so a streaming
//     capability lie with the new counters lands here as DNF instead
//     of rule 4's N/A — ambiguity resolves loud, never skip-eligible,
//     matching report.ClassifyCellError's default.)
//  4. Zero successes + errors > 0 + server alive + capability-gated
//     class → "capability-lie:" (N/A). The ONLY runtime path to N/A
//     left: the adapter declared the capability, is demonstrably up,
//     yet served nothing. A cell with even one success can never be a
//     lie.
//  5. Zero requests otherwise → "zero-request cell:" (DNF — loud; see
//     report.ClassifyCellError for why this stopped being N/A).
//  6. Error ratio over the scenario budget → "suspect:" (soft; data
//     kept). churn-close's 0.96+ ratios published as status=ok in every
//     v3.8/history run. When connect-class failures cover the whole
//     overage past the budget, the reason says so: the server was
//     unreachable for part of the cell, not misbehaving.
func classifyCompletedCell(in completedCell) cellVerdict {
	ratio := errorRatio(in.Requests, in.Errors)
	switch {
	case in.Interrupted:
		return cellVerdict{
			ErrMsg: fmt.Sprintf("interrupted: cell cancelled mid-run (requests=%d errors=%d duration=%s)",
				in.Requests, in.Errors, in.Duration),
			Hard: true,
		}
	case (in.Requests == 0 || ratio > 0.5) && !in.ServerAlive:
		msg := fmt.Sprintf("server-died-mid-cell: post-cell probe: %s (requests=%d errors=%d",
			in.ProbeErr, in.Requests, in.Errors)
		if in.ConnectErrors > 0 {
			msg += fmt.Sprintf(" connect_errors=%d", in.ConnectErrors)
		}
		return cellVerdict{ErrMsg: msg + ")", Hard: true}
	case in.Requests == 0 && connectClassDominated(in.Errors, in.ConnectErrors):
		return cellVerdict{
			ErrMsg: fmt.Sprintf(
				"server-down: zero requests and the errors are connect-class (connect_errors=%d errors=%d) — no stream ever served; a passing post-probe means a flapping listener, not a healthy server",
				in.ConnectErrors, in.Errors),
			Hard: true,
		}
	case in.Requests == 0 && in.Errors > 0 && capabilityGatedClass(in.Category):
		return cellVerdict{
			ErrMsg: fmt.Sprintf(
				"capability-lie: scheduled %s scenario %q got zero successes from live server %s (errors=%d) — adapter declared the capability but did not serve the route",
				in.Category, in.ScenarioName, in.ServerName, in.Errors),
			Hard: true,
		}
	case in.Requests == 0:
		return cellVerdict{
			ErrMsg: fmt.Sprintf("zero-request cell: errors=%d duration=%s", in.Errors, in.Duration),
			Hard:   true,
		}
	case ratio > in.ErrorBudget:
		msg := fmt.Sprintf("suspect: error ratio %.3f exceeds budget %.2f (errors=%d requests=%d)",
			ratio, in.ErrorBudget, in.Errors, in.Requests)
		// Overage attribution: the budget allows ErrorBudget×total failed
		// operations; when the connect-class subset covers everything past
		// that allowance, the blow-up is reachability, not server
		// misbehaviour — say so next to the flag.
		allowed := in.ErrorBudget * float64(in.Requests+in.Errors)
		if in.ConnectErrors > 0 && float64(in.ConnectErrors) >= float64(in.Errors)-allowed {
			msg += fmt.Sprintf("; the overage is connect-class (connect_errors=%d) — server unreachable for part of the cell", in.ConnectErrors)
		}
		return cellVerdict{ErrMsg: msg, Hard: false}
	}
	return cellVerdict{}
}

// connectClassDominated reports whether a cell's error total is
// overwhelmingly dial/handshake failures. The two counters are kept by
// different layers (Errors per failed request, ConnectErrors at the
// driver) and can differ by a few attempts cut off at phase boundaries,
// so "all of them" is a 99% band rather than equality. Always false for
// pre-ConnectErrors loadgen builds (counter zero), keeping legacy
// artefacts on the old rules.
func connectClassDominated(errs int64, connectErrs uint64) bool {
	if errs <= 0 || connectErrs == 0 {
		return false
	}
	return float64(connectErrs) >= 0.99*float64(errs)
}

// loadgenRunCellError synthesises the per-cell error string for a failed
// loadgen.Run. A fail-fast abort — loadgen returns a nil Result and an
// ErrNeverConnected-wrapped "loadgen: dial: …" error when not a single
// stream was established within its fail-fast window — means the SUT was
// dead or flapping from the cell's first dial, so it wears the same
// "server-down:" prefix as the pre-cell probe mark: DNF, the run loop
// skips the cooldown, and the publish integrity gate counts it as a
// dead-SUT measurement. The post-probe is attached as evidence only; a
// flapping listener can pass it, which must not soften the verdict.
// Every other Run error keeps the legacy "loadgen.Run:" prefix (DNF via
// report.ClassifyCellError's default).
func loadgenRunCellError(parent context.Context, err error, probeAddr string) string {
	if !errors.Is(err, loadgen.ErrNeverConnected) {
		return "loadgen.Run: " + err.Error()
	}
	probe := "no post-probe addr"
	if probeAddr != "" {
		if perr := probeSUT(parent, probeAddr); perr != nil {
			probe = "post-probe: " + perr.Error()
		} else {
			probe = "post-probe passed — flapping listener, not a healthy server"
		}
	}
	return fmt.Sprintf("server-down: loadgen fail-fast: %s (%s)", err.Error(), probe)
}

// probeSUT answers "is the SUT accepting TCP connections right now?"
// with a plain dial — the right primitive for a remote target (the
// readiness gate from commit 81a5661 verifies the argv of the LOCAL
// listening process; from the loadgen host all that is observable is the
// socket). Up to three quick attempts so one accept-queue blip does not
// read as a dead server. The dials run on a context detached from the
// run's cancellation so an interrupt mid-probe can never masquerade as a
// server death.
func probeSUT(parent context.Context, addr string) error {
	ctx := context.WithoutCancel(parent)
	var lastErr error
	for attempt := 0; attempt < 3; attempt++ {
		if attempt > 0 {
			time.Sleep(250 * time.Millisecond)
		}
		d := net.Dialer{Timeout: time.Second}
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		lastErr = err
	}
	return lastErr
}

// hostPortFromURL extracts the dialable host:port from a -target base
// URL, defaulting the port from the scheme. ok=false for unparseable
// input — the caller skips probing rather than failing the cell on a
// probe bug.
func hostPortFromURL(raw string) (string, bool) {
	u, err := url.Parse(raw)
	if err != nil || u.Hostname() == "" {
		return "", false
	}
	port := u.Port()
	if port == "" {
		switch u.Scheme {
		case "https":
			port = "443"
		default:
			port = "80"
		}
	}
	return net.JoinHostPort(u.Hostname(), port), true
}

// runRatedSweep drives one rated (closed-loop, CO-corrected) pass per
// configured fraction of saturationRPS, returning the (target, P99) samples
// for aggregation plus the on-disk pass records. A pass that errors or returns
// zero requests is skipped rather than failing the cell — a partial sweep is
// still useful, and the saturation pass already succeeded.
func runRatedSweep(parent context.Context, cfg Config, base loadgen.Config, saturationRPS float64) ([]report.RatedSample, []ratedPassFile) {
	var samples []report.RatedSample
	var passes []ratedPassFile
	for _, frac := range cfg.RatedFractions {
		target := saturationRPS * frac
		if target <= 0 {
			continue
		}
		lgCfg := base
		lgCfg.Rate = target
		lgCfg.Duration = cfg.RatedDuration

		timeout := 5*cfg.Warmup + 2*cfg.RatedDuration + 60*time.Second
		ctx, cancel := context.WithTimeout(parent, timeout)
		bm, err := loadgen.New(lgCfg)
		if err != nil {
			cancel()
			fmt.Fprintf(os.Stderr, "  rated pass target=%.0f: loadgen.New: %v\n", target, err)
			continue
		}
		rres, err := bm.Run(ctx)
		cancel()
		if err != nil || rres == nil || rres.Requests == 0 {
			fmt.Fprintf(os.Stderr, "  rated pass target=%.0f: skipped (err=%v)\n", target, err)
			continue
		}
		samples = append(samples, report.RatedSample{TargetRPS: target, P99: rres.Latency.P99})
		passes = append(passes, ratedPassFile{TargetRPS: target, P99: rres.Latency.P99})
	}
	return samples, passes
}

// adapterServer adapts an [servers.Adapter] to the in-process
// [servers.Server] interface that interleave.Schedule consumes. The
// wrapper carries Name and FeatureSet through; Kind() is left as the
// adapter Category for visibility in the manifest.
type adapterServer struct {
	adapter  servers.Adapter
	features servers.FeatureSet
}

func (s *adapterServer) Name() string                 { return s.adapter.Name }
func (s *adapterServer) Kind() string                 { return s.adapter.Category }
func (s *adapterServer) Features() servers.FeatureSet { return s.features }

// featureSetFor maps an Adapter to the FeatureSet the scheduler uses to
// gate (scenario, server) pairs.
//
// Wire-protocol facets (HTTP1 / HTTP2C / Auto / H2CUpgrade / AsyncHandlers)
// are derived from the Engine name — that is legitimate, the engine literally
// determines which protocols the listener speaks. The scenario-class facets
// (Drivers / Middleware / WS / SSE / TLS) are read from the adapter's DECLARED
// [servers.Capabilities] manifest, NOT guessed from the Category name. The old
// by-name guess was a lie: it granted Drivers / Middleware to every Go adapter
// whether or not it actually mounted those routes. Trusting the manifest means
// a scenario is only scheduled against an adapter that claims the matching
// class; a claim the adapter then fails to honour at run time becomes a hard
// error in executeCell (the capability-lie guard) instead of a silent
// 0-RPS / all-404 cell.
func featureSetFor(a servers.Adapter, tlsReady bool) servers.FeatureSet {
	fs := servers.FeatureSet{HTTP1: true}
	switch {
	case strings.Contains(a.Engine, "h2c"):
		// "h2c" without any upgrade suffix is the Go-net/http h2c
		// mode (chi-h2, gin-h2, echo-h2, hertz-h2, iris-h2,
		// stdhttp-h2). All use http.Protocols.SetUnencryptedHTTP2()
		// or h2c.NewHandler, which only accept PRIOR-KNOWLEDGE h2c
		// — they do NOT speak the h1→h2c upgrade handshake (the
		// 101 Switching Protocols response the loadgen's
		// -h2c-upgrade mode looks for). Flagging H2CUpgrade=true
		// for these engines produces a DNF cell on every
		// auto-mix-111 / mixed-protocol run, not a real signal.
		// Only the celeris "h2c+upg" mode (today: "iouring-auto+upg-async"
		// — caught by the "auto" branch below) actually implements
		// the upgrade path, so H2CUpgrade is intentionally left false
		// for plain "h2c" engines. (Regression seen in v3.7:
		// chi-h2 / auto-mix-111 DNF'd with "h2c upgrade: server
		// returned status 200 (expected 101)" — fixed here.)
		fs.HTTP2C = true
		if strings.Contains(a.Engine, "noupg") {
			fs.HTTP1 = false
		}
	case strings.Contains(a.Engine, "auto"):
		// Celeris "iouring-auto+upg-async" — the only engine that
		// implements the full h1+h2c+upgrade protocol triple.
		fs.HTTP2C = true
		fs.Auto = true
		fs.H2CUpgrade = true
	case strings.Contains(a.Engine, "hybrid"):
		// Go net/http "hybrid" — SetHTTP1(true) + SetUnencryptedHTTP2(true)
		// accepts plain HTTP/1.1 OR prior-knowledge h2c. It does NOT
		// speak the h1→h2c upgrade handshake (101 Switching Protocols)
		// because the Go stdlib's http2.Transport requires a custom
		// handler that the stdlib doesn't wire up. Leave H2CUpgrade=false
		// so loadgen's -h2c-upgrade mode is not scheduled against
		// stdhttp-hybrid (it would DNF with "h2c upgrade: server
		// returned status 200 (expected 101)" — same root cause as the
		// v3.7 chi-h2 regression; stdhttp-hybrid is just a different
		// adapter making the same false H2CUpgrade claim).
		fs.HTTP2C = true
	}
	if strings.Contains(a.Engine, "async") {
		fs.AsyncHandlers = true
	}
	// Scenario-class facets come from the declared manifest — the single
	// source of truth the scheduler trusts.
	fs.Drivers = a.Capabilities.Drivers
	fs.Middleware = a.Capabilities.Middleware
	fs.WS = a.Capabilities.WS
	fs.SSE = a.Capabilities.SSE
	// TLS is only advertised when a shared terminator is actually wired
	// (-tls-terminator). Adapters declare Capabilities.TLS=true for "could
	// be fronted by a terminator", but without one the cleartext loopback
	// can't serve https, so a scheduled tls-* cell would trip the
	// capability-lie guard. Gating here keeps tls-* unscheduled until the
	// terminator infra lands (scenarios/tls.go).
	fs.TLS = a.Capabilities.TLS && tlsReady
	return fs
}

// capabilityGatedClass reports whether a scenario category is gated on a
// declared adapter capability (and therefore subject to the executeCell
// capability-lie guard). Static and concurrency scenarios drive the universal
// contract endpoints and are NOT capability-gated, so a non-2xx there is a real
// performance / correctness signal handled by the normal result path, not a lie.
func capabilityGatedClass(category string) bool {
	switch category {
	case scenarios.CategoryDriver,
		scenarios.CategoryChain,
		scenarios.CategoryWS,
		scenarios.CategorySSE,
		scenarios.CategoryTLS:
		return true
	default:
		return false
	}
}

// remoteServerName resolves the synthetic server name used in
// remote-target mode: the -server-name slug when set, else the bare
// target URL so the cell is still attributable.
func remoteServerName(cfg Config) string {
	if cfg.ServerName != "" {
		return cfg.ServerName
	}
	return cfg.Target
}

// remoteFeatureSet is the permissive capability set advertised for the
// synthetic remote server. Every facet is on so no scenario is gated
// out before reaching the SUT; protocols the SUT cannot speak surface as
// zero-request cells (the executeCell guard) rather than missing rows.
func remoteFeatureSet() servers.FeatureSet {
	return servers.FeatureSet{
		HTTP1:         true,
		HTTP2C:        true,
		H2CUpgrade:    true,
		Auto:          true,
		Drivers:       true,
		Middleware:    true,
		WS:            true,
		SSE:           true,
		TLS:           true,
		AsyncHandlers: true,
	}
}

// filterCells applies the comma-separated -cells glob (with optional "!"
// negations) against the "<scenario>/<server>" cell ids. Behaviour
// mirrors perfmatrix's runner so muscle-memory carries over.
func filterCells(scs []scenarios.Scenario, advs []servers.Adapter, glob string) ([]scenarios.Scenario, []servers.Adapter, error) {
	if glob == "" || glob == "*" {
		return scs, advs, nil
	}
	var include, exclude []string
	for _, part := range strings.Split(glob, ",") {
		p := strings.TrimSpace(part)
		if p == "" {
			continue
		}
		if strings.HasPrefix(p, "!") {
			exclude = append(exclude, p[1:])
		} else {
			include = append(include, p)
		}
	}
	if len(include) == 0 {
		include = []string{"*"}
	}
	for _, g := range append(include, exclude...) {
		if _, err := path.Match(g, "probe/probe"); err != nil {
			return nil, nil, fmt.Errorf("invalid glob %q: %w", g, err)
		}
	}
	matchAny := func(patterns []string, id string) bool {
		for _, g := range patterns {
			if g == "*" {
				return true
			}
			if ok, _ := path.Match(g, id); ok {
				return true
			}
		}
		return false
	}
	scKeep := map[string]bool{}
	advKeep := map[string]bool{}
	for _, s := range scs {
		for _, a := range advs {
			id := s.Name() + "/" + a.Name
			if !matchAny(include, id) {
				continue
			}
			if matchAny(exclude, id) {
				continue
			}
			scKeep[s.Name()] = true
			advKeep[a.Name] = true
		}
	}
	outS := make([]scenarios.Scenario, 0, len(scs))
	for _, s := range scs {
		if scKeep[s.Name()] {
			outS = append(outS, s)
		}
	}
	outA := make([]servers.Adapter, 0, len(advs))
	for _, a := range advs {
		if advKeep[a.Name] {
			outA = append(outA, a)
		}
	}
	return outS, outA, nil
}

// requiredServiceKinds inspects the effective scenario set and returns
// the docker service kinds the orchestrator must provision.
func requiredServiceKinds(scs []scenarios.Scenario) []string {
	need := map[string]bool{}
	for _, s := range scs {
		if s.Category() != scenarios.CategoryDriver {
			continue
		}
		name := s.Name()
		switch {
		case strings.Contains(name, "pg") || strings.Contains(name, "postgres"):
			need[services.KindPostgres] = true
		case strings.Contains(name, "redis"):
			need[services.KindRedis] = true
		case strings.Contains(name, "memcached") || strings.Contains(name, "-mc-"):
			need[services.KindMemcached] = true
		case strings.Contains(name, "session"):
			need[services.KindRedis] = true
		}
	}
	out := make([]string, 0, len(need))
	for k := range need {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// freePort asks the kernel for a loopback port that is free RIGHT NOW
// and returns it; the caller passes the result to the adapter via -bind.
// There is an inherent TOCTOU window between the port being released
// here and the adapter binding, but the runner serialises cells so the
// race is degenerate in practice.
func freePort() (int, error) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return 0, err
	}
	defer func() { _ = l.Close() }()
	return l.Addr().(*net.TCPAddr).Port, nil
}

// waitForTCP dials addr with a short backoff until it accepts or the
// timeout elapses. Used as the post-spawn ready check before handing the
// address to loadgen.
func waitForTCP(ctx context.Context, addr string, timeout time.Duration) error {
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	var d net.Dialer
	for {
		conn, err := d.DialContext(wctx, "tcp", addr)
		if err == nil {
			_ = conn.Close()
			return nil
		}
		select {
		case <-wctx.Done():
			return fmt.Errorf("tcp probe %s: %w", addr, wctx.Err())
		case <-time.After(50 * time.Millisecond):
		}
	}
}

// countProcessFDs returns the count of open FDs in the runner process.
// /proc/self/fd is a linuxism; non-linux returns 0 so the leak detector
// degrades to a no-op rather than failing the run on macOS dev hosts.
func countProcessFDs() int {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0
	}
	return len(entries)
}

// installSignalHandler routes SIGINT/SIGTERM to cancel the root context;
// the run loop then marks the in-flight cell + everything unstarted as
// "interrupted" and writes final results. A second signal forces
// immediate exit so a hung adapter cannot wedge shutdown — but flushes
// results.json best-effort first, because v3.8's second SIGTERM landed
// before the (then end-of-run-only) write and lost a whole column.
func installSignalHandler(cancel context.CancelFunc, sink *resultsSink) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "probatorium-runner: interrupted, shutting down")
		cancel()
		<-ch
		fmt.Fprintln(os.Stderr, "probatorium-runner: second signal, flushing results and forcing exit")
		sink.flushBestEffort()
		os.Exit(130)
	}()
}

// writeJSON marshals v to path with 2-space indent, atomically: the
// bytes land in a same-directory temp file that is renamed over path, so
// a runner killed mid-write (second SIGTERM, OOM, SIGKILL) never leaves
// a torn half-file — the previous complete version survives until the
// rename lands. Creates the parent directory if missing.
func writeJSON(p string, v any) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(p), "."+filepath.Base(p)+".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(buf); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	// Match os.WriteFile's historical 0644 (CreateTemp gives 0600).
	if err := tmp.Chmod(0o644); err != nil {
		_ = tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, p); err != nil {
		cleanup()
		return err
	}
	return nil
}

// writeManifest writes the top-level manifest summarising the run.
func writeManifest(cfg Config, scs []scenarios.Scenario, srvs []servers.Server, sched []interleave.Cell) error {
	host := hostInfo{OS: runtime.GOOS, Arch: runtime.GOARCH}
	if hn, err := os.Hostname(); err == nil {
		host.Hostname = hn
	}
	m := runManifest{
		Config:      cfg,
		StartedAt:   time.Now().UTC(),
		CompletedAt: time.Now().UTC(),
		Host:        host,
		Seed:        cfg.Seed,
		GitSHA:      shortGitSHA(),
		GoVersion:   runtime.Version(),
		CellCount:   len(sched),
	}
	for _, s := range scs {
		m.Scenarios = append(m.Scenarios, s.Name())
	}
	for _, s := range srvs {
		m.Servers = append(m.Servers, s.Name())
	}
	for _, c := range sched {
		m.Cells = append(m.Cells, fmt.Sprintf("run%d/%s/%s",
			c.RunIdx, c.Scenario.Name(), c.Server.Name()))
	}
	return writeJSON(filepath.Join(cfg.Out, "manifest.json"), &m)
}

// buildDocument folds the per-cell aggregate map into the canonical v5.1
// Document via report.BuildDocument. Per-server metadata + the run's
// Environment / BenchmarkConfig are projected here so report/ stays a
// leaf node that never imports servers/ or scenarios/.
func buildDocument(cfg Config, agg map[string]report.CellAggregate, started time.Time) *report.Document {
	env := report.Environment{
		// In-process runs drive loopback against the local host; the
		// kernel-sysctl capture is a cluster-only concern, so the
		// runner emits an empty (non-nil) slice to satisfy the schema's
		// required environment block.
		KernelSysctlsApplied: []string{},
		Fabric:               "loopback",
	}
	if hn, err := os.Hostname(); err == nil {
		env.LoadgenHost = hn
	}

	bench := report.BenchmarkConfig{
		StartedAt:       started,
		FinishedAt:      time.Now().UTC(),
		Runs:            cfg.Runs,
		Duration:        cfg.Duration,
		Warmup:          cfg.Warmup,
		GitRef:          shortGitSHA(),
		LoadgenVer:      modRequireVersion("github.com/goceleris/loadgen"),
		CelerisVer:      modRequireVersion("github.com/goceleris/celeris"),
		ScenariosFilter: "",
		AdaptersFilter:  cfg.Cells,
	}

	targetArch := cfg.TargetArch
	if targetArch == "" {
		targetArch = runtime.GOARCH
	}
	return report.BuildDocument(report.BuildInput{
		HostArchPair:    runtime.GOOS + "/" + targetArch,
		Environment:     env,
		BenchmarkConfig: bench,
		Servers:         serverMetaFromRegistry(),
		Agg:             agg,
	})
}

// serverMetaFromRegistry projects servers.Registry into the report-side
// ServerMeta map BuildDocument consumes. LanguageVersion is the runner's
// own toolchain for Go adapters; CompileOptions mirror the canonical
// build path (crossCompileGoBinary for Go, the native role flags for
// rust/python/bun).
func serverMetaFromRegistry() map[string]report.ServerMeta {
	out := make(map[string]report.ServerMeta, len(servers.Registry))
	for name, a := range servers.Registry {
		fwVer := a.FrameworkVersion
		if a.Framework == "celeris" {
			// celeris' version is the runner's pinned build, not a registry
			// constant; modRequireVersion returns "" if absent (acceptable).
			fwVer = modRequireVersion("github.com/goceleris/celeris")
		}
		m := report.ServerMeta{
			Category:         a.Category,
			Language:         a.Language,
			Framework:        a.Framework,
			FrameworkVersion: fwVer,
			Engine:           a.Engine,
			CompileOptions:   report.CompileOptionsFor(a.Language, runtime.GOARCH),
		}
		if a.Language == "go" {
			m.LanguageVersion = runtime.Version()
		}
		out[name] = m
	}
	return out
}

// modRequireVersion returns the version pinned for modPath in the
// caller's go.mod require block, or "" when absent. Mirrors
// celerisVersion's go.mod parse without the env-override / "dev"
// fallback so the field stays empty (rather than misleading) when the
// dependency isn't present.
func modRequireVersion(modPath string) string {
	data, err := os.ReadFile("go.mod")
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(strings.TrimSpace(line))
		if len(fields) >= 2 && fields[0] == modPath {
			return fields[1]
		}
	}
	return ""
}

// writeReports persists the v5.0 results.json + report.md in the run's
// output directory, plus the gzip time-series sidecar. The summary
// renderers tolerate empty inputs so a run with zero successful cells
// still produces files for the CI gate to read.
func writeReports(cfg Config, doc *report.Document, agg map[string]report.CellAggregate, ts *report.TimeseriesDoc, started time.Time) error {
	jsonPath := filepath.Join(cfg.Out, "results.json")
	if err := writeJSON(jsonPath, doc); err != nil {
		return fmt.Errorf("write %s: %w", jsonPath, err)
	}

	tsPath := cfg.Timeseries
	if tsPath == "" {
		tsPath = filepath.Join(cfg.Out, "timeseries.json.gz")
	}
	if err := writeTimeseries(tsPath, ts); err != nil {
		return fmt.Errorf("write %s: %w", tsPath, err)
	}

	mdPath := filepath.Join(cfg.Out, "report.md")
	f, err := os.Create(mdPath)
	if err != nil {
		return fmt.Errorf("create %s: %w", mdPath, err)
	}
	defer func() { _ = f.Close() }()
	meta := report.Meta{
		GitRef:     shortGitSHA(),
		StartedAt:  started,
		FinishedAt: time.Now().UTC(),
		Runs:       cfg.Runs,
		Duration:   cfg.Duration,
		TotalCells: len(agg),
	}
	if hn, herr := os.Hostname(); herr == nil {
		meta.Host = hn
	}
	if err := report.WriteMarkdown(f, doc, agg, meta); err != nil {
		return fmt.Errorf("write %s: %w", mdPath, err)
	}
	if section := report.MarkdownTimeseries(ts); section != "" {
		if _, err := io.WriteString(f, section); err != nil {
			return fmt.Errorf("write timeseries section %s: %w", mdPath, err)
		}
	}
	return nil
}

// writeTimeseries persists the gzip time-series sidecar. A nil or
// empty-scenario doc is tolerated (still written) so downstream tooling
// can rely on the file existing after any run.
func writeTimeseries(p string, ts *report.TimeseriesDoc) error {
	if ts == nil {
		ts = report.BuildTimeseries(nil)
	}
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	data, err := ts.MarshalGzip()
	if err != nil {
		return err
	}
	return os.WriteFile(p, data, 0o644)
}

// shortGitSHA returns the abbreviated HEAD SHA, or "" if git is not
// available or the runner is not inside a repo.
func shortGitSHA() string {
	cmd := exec.Command("git", "rev-parse", "--short", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(out))
}
