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
//     c. TCP-probe the bind addr until ready (5 s cap).
//     d. Run loadgen.New(...).Run(ctx) and persist the result.
//     e. SIGTERM / SIGKILL the adapter; record the FD delta.
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
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
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
		Duration:       120 * time.Second,
		Warmup:         30 * time.Second,
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
	fs.StringVar(&c.ServerName, "server-name", c.ServerName,
		"friendly server slug recorded in per-cell JSON / report when -target is set")
	fs.BoolVar(&c.DryRun, "dry-run", c.DryRun, "print the resolved schedule and exit without starting adapters")
	fs.BoolVar(&c.RatedMode, "rated", c.RatedMode,
		"run a Gil-Tene rated (closed-loop, CO-corrected) sweep after each saturation pass (opt-in; multiplies per-cell time)")
	fs.DurationVar(&c.RatedDuration, "rated-duration", c.RatedDuration, "measurement window for each rated pass")
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
	RunIdx       int             `json:"run_idx"`
	ScenarioName string          `json:"scenario"`
	ServerName   string          `json:"server"`
	Category     string          `json:"category"`
	TargetAddr   string          `json:"target_addr"`
	StartedAt    time.Time       `json:"started_at"`
	CompletedAt  time.Time       `json:"completed_at"`
	Error        string          `json:"error,omitempty"`
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
	if err := run(cfg); err != nil {
		fmt.Fprintf(os.Stderr, "probatorium-runner: %v\n", err)
		os.Exit(1)
	}
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
	for _, a := range effAdv {
		fs := featureSetFor(a)
		if cfg.Target != "" {
			// The runner cannot probe the remote SUT's real capabilities,
			// so advertise everything and let unsupported protocols surface
			// as zero-request cells instead of being silently skipped.
			fs = remoteFeatureSet()
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
	installSignalHandler(rootCancel)

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

	manifestStart := time.Now().UTC()
	fmt.Fprintf(os.Stderr, "probatorium-runner: %d cells across %d scenarios × %d adapters × %d runs\n",
		len(schedule), len(effSc), len(effAdv), cfg.Runs)

	collected := map[string]*report.CellResult{}
	preRunFDs := countProcessFDs()

	var firstErr error
	for i, cell := range schedule {
		if rootCtx.Err() != nil {
			fmt.Fprintln(os.Stderr, "probatorium-runner: cancelled")
			break
		}
		fmt.Fprintf(os.Stderr, "[%d/%d] run=%d scenario=%s server=%s\n",
			i+1, len(schedule), cell.RunIdx, cell.Scenario.Name(), cell.Server.Name())

		res, cerr := executeCell(rootCtx, cfg, cell)
		if res.Result != nil {
			key := report.CellID(cell.Scenario.Name(), cell.Server.Name())
			cr := collected[key]
			if cr == nil {
				cr = &report.CellResult{
					ScenarioName: cell.Scenario.Name(),
					ServerName:   cell.Server.Name(),
					Category:     cell.Scenario.Category(),
				}
				collected[key] = cr
			}
			cr.Samples = append(cr.Samples, *res.Result)
			cr.HistogramsB64 = append(cr.HistogramsB64, res.HistogramB64)
			if cfg.RatedMode {
				cr.RatedSamples = append(cr.RatedSamples, res.RatedSamples)
			}
		}
		if cerr != nil {
			fmt.Fprintf(os.Stderr, "  cell error: %v\n", cerr)
			if cfg.FailFast {
				firstErr = fmt.Errorf("fail-fast: %s/%s: %w",
					cell.Scenario.Name(), cell.Server.Name(), cerr)
				break
			}
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

	// Convert collected map to slice for Aggregate. Sorting keeps the
	// resulting markdown / JSON byte-stable across runs.
	cells := make([]report.CellResult, 0, len(collected))
	keys := make([]string, 0, len(collected))
	for k := range collected {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		cells = append(cells, *collected[k])
	}
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
	if lgCfg.Workers == 0 {
		lgCfg.Workers = 64
	}
	return lgCfg
}

// executeCell drives loadgen for one cell and writes the per-cell JSON
// file. In local mode it boots the adapter on a free loopback port and
// records FD-leak deltas around it; in remote-target mode (cfg.Target
// set) it skips the adapter lifecycle entirely and drives loadgen at the
// already-running remote base URL. Returns the loadgen result on success
// and a synthesised error otherwise; the per-cell JSON is written either
// way so a partial matrix still produces inspectable artefacts.
func executeCell(parent context.Context, cfg Config, cell interleave.Cell) (cellOutcome, error) {
	outDir := filepath.Join(cfg.Out, fmt.Sprintf("run%d", cell.RunIdx), cell.Scenario.Name())
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return cellOutcome{}, fmt.Errorf("mkdir %s: %w", outDir, err)
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
		if werr := writeJSON(outFile, &cellRes); werr != nil {
			fmt.Fprintf(os.Stderr, "  write %s: %v\n", outFile, werr)
		}
	}()

	var lgURL string
	if cfg.Target != "" {
		// Remote-target mode: the SUT is already running on another host.
		// Skip freePort / StartAdapter / waitForTCP and the FD-leak scope —
		// the runner's /proc/self/fd is unrelated to the remote SUT, whose
		// FD/RSS is covered by the cluster-side CPU/observer sidecar.
		cellRes.TargetAddr = cfg.Target
		lgURL = cfg.Target
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
		cellRes.Error = "loadgen.Run: " + err.Error()
		return cellOutcome{}, err
	}
	cellRes.Result = res
	out := cellOutcome{Result: res}

	if res != nil && res.Requests == 0 {
		cellRes.Error = fmt.Sprintf("zero-request cell: errors=%d duration=%s", res.Errors, res.Duration)
		return out, errors.New(cellRes.Error)
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
		out.RatedSamples = samples
		cellRes.RatedPasses = passes
	}
	return out, nil
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
// gate (scenario, server) pairs. The mapping is conservative: H1 is on
// for every adapter that does not declare h2c-only, H2C is on for every
// adapter whose Engine name signals an H2C path, and Drivers /
// Middleware track Category.
func featureSetFor(a servers.Adapter) servers.FeatureSet {
	fs := servers.FeatureSet{HTTP1: true}
	switch {
	case strings.Contains(a.Engine, "h2c"):
		fs.HTTP2C = true
		// h2c-noupg is the only mode that refuses H1; everything else
		// (h2c+upg, hybrid) accepts both.
		if !strings.Contains(a.Engine, "noupg") {
			fs.H2CUpgrade = true
		} else {
			fs.HTTP1 = false
		}
	case strings.Contains(a.Engine, "auto"):
		fs.HTTP2C = true
		fs.Auto = true
		fs.H2CUpgrade = true
	case strings.Contains(a.Engine, "hybrid"):
		fs.HTTP2C = true
		fs.H2CUpgrade = true
	}
	if strings.Contains(a.Engine, "async") {
		fs.AsyncHandlers = true
	}
	// Driver and middleware features are gated on Category — every adapter
	// in the registry currently carries the contract endpoints, but the
	// driver / chain scenarios stub-out via 501 in this wave.
	switch a.Category {
	case "celeris":
		fs.Drivers = true
		fs.Middleware = true
	case "go-net-http", "go-fasthttp", "go-netpoll":
		fs.Drivers = true
		fs.Middleware = true
	}
	return fs
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

// installSignalHandler routes SIGINT/SIGTERM to cancel the root context.
// A second signal forces immediate exit so a hung adapter cannot wedge
// shutdown.
func installSignalHandler(cancel context.CancelFunc) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
	go func() {
		<-ch
		fmt.Fprintln(os.Stderr, "probatorium-runner: interrupted, shutting down")
		cancel()
		<-ch
		fmt.Fprintln(os.Stderr, "probatorium-runner: second signal, forcing exit")
		os.Exit(130)
	}()
}

// writeJSON marshals v to path with 2-space indent. Creates the parent
// directory if missing.
func writeJSON(p string, v any) error {
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(p, buf, 0o644)
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

	return report.BuildDocument(report.BuildInput{
		HostArchPair:    runtime.GOOS + "/" + runtime.GOARCH,
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
		m := report.ServerMeta{
			Category:       a.Category,
			Language:       a.Language,
			Framework:      a.Framework,
			Engine:         a.Engine,
			CompileOptions: report.CompileOptionsFor(a.Language, runtime.GOARCH),
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
