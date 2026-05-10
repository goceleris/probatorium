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

	// DryRun, when true, prints the resolved schedule and exits without
	// starting any adapter. Convenient for CI smoke tests and for
	// validating the -cells glob without requiring a deploy.
	DryRun bool
}

// DefaultConfig returns the fresh-flag defaults. Mirrors perfmatrix's
// runner so muscle-memory carries over.
func DefaultConfig() Config {
	return Config{
		Runs:     5,
		Duration: 120 * time.Second,
		Warmup:   30 * time.Second,
		Cooldown: 5 * time.Second,
		Services: "local",
		Seed:     0,
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
	fs.StringVar(&c.Services, "services", c.Services, `"local" (Docker on same host) | "none" (skip driver services)`)
	fs.BoolVar(&c.FailFast, "fail-fast", c.FailFast, "abort at the first cell error")
	fs.BoolVar(&c.FDTrace, "fd-trace", c.FDTrace, "log per-cell FD counts even when the delta is zero")
	fs.Int64Var(&c.Seed, "seed", c.Seed, "rng seed for reproducibility echo; 0 = time.Now().UnixNano()")
	fs.BoolVar(&c.DryRun, "dry-run", c.DryRun, "print the resolved schedule and exit without starting adapters")
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
	advs := servers.AdaptersSorted()

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
		srvs = append(srvs, &adapterServer{adapter: a, features: featureSetFor(a)})
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

	doc := buildDocument(cfg, agg, manifestStart)
	if err := writeReports(cfg, doc, agg, manifestStart); err != nil {
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
}

// executeCell boots the adapter, drives loadgen, and writes the per-cell
// JSON file. Returns the loadgen result on success and a synthesised
// error otherwise; the per-cell JSON is written either way so a partial
// matrix still produces inspectable artefacts.
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

	cellRes.FDsBefore = countProcessFDs()

	port, err := freePort()
	if err != nil {
		cellRes.Error = "free port: " + err.Error()
		return cellOutcome{}, err
	}
	bindAddr := fmt.Sprintf("127.0.0.1:%d", port)
	cellRes.TargetAddr = bindAddr

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

	lgURL := "http://" + bindAddr
	lgCfg := cell.Scenario.Workload(lgURL)
	if lgCfg.URL == "" {
		lgCfg.URL = lgURL + "/"
	}
	lgCfg.Duration = cfg.Duration
	lgCfg.Warmup = cfg.Warmup
	if lgCfg.Workers == 0 {
		lgCfg.Workers = 64
	}

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
	return out, nil
}

// adapterServer adapts an [servers.Adapter] to the in-process
// [servers.Server] interface that interleave.Schedule consumes. The
// wrapper carries Name and FeatureSet through; Kind() is left as the
// adapter Category for visibility in the manifest.
type adapterServer struct {
	adapter  servers.Adapter
	features servers.FeatureSet
}

func (s *adapterServer) Name() string                { return s.adapter.Name }
func (s *adapterServer) Kind() string                { return s.adapter.Category }
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

// buildDocument folds the per-cell aggregate map into a v5.0 Document.
// Per-server fields (Category / Language / Framework / Engine) are read
// from the registry so the output schema is grounded against the
// adapter table rather than the on-disk JSON.
func buildDocument(cfg Config, agg map[string]report.CellAggregate, started time.Time) *report.Document {
	bench := report.BenchmarkConfig{
		StartedAt:       started,
		FinishedAt:      time.Now().UTC(),
		Runs:            cfg.Runs,
		Duration:        cfg.Duration,
		Warmup:          cfg.Warmup,
		GitRef:          shortGitSHA(),
		LoadgenVer:      "",
		CelerisVer:      "",
		ScenariosFilter: "",
		AdaptersFilter:  cfg.Cells,
	}

	// Bucket aggregates by adapter so each entry in Benchmarks covers
	// every scenario for one server.
	byAdapter := map[string]*report.ServerResult{}
	for _, c := range agg {
		sr := byAdapter[c.ServerName]
		if sr == nil {
			a, ok := servers.Registry[c.ServerName]
			sr = &report.ServerResult{
				Name:                    c.ServerName,
				SaturationModeRPS:       map[string]float64{},
				RatedModeP99AtTargetRPS: map[string]time.Duration{},
				LatencyAtSLO:            map[string]map[int]int{},
				HdrHistogramB64:         map[string]string{},
				LoadgenCPUP95:           map[string]float64{},
				SentVsHandledDeltaPct:   map[string]float64{},
			}
			if ok {
				sr.Category = a.Category
				sr.Language = a.Language
				sr.Framework = a.Framework
				sr.Engine = a.Engine
			}
			byAdapter[c.ServerName] = sr
		}
		sr.SaturationModeRPS[c.ScenarioName] = c.RPSMedian
		// LatencyMerged is exact when present; fall back to the median
		// snapshot otherwise.
		lat := c.LatencyMerged
		if lat == (report.Percentiles{}) {
			lat = c.LatencyMedian
		}
		sr.RatedModeP99AtTargetRPS[c.ScenarioName] = lat.P99
		// LatencyAtSLO synthesised by sliding the merged P99 against the
		// canonical thresholds in report.SLOThresholds: if P99 ≤ N ms, the
		// adapter sustained the median RPS at N. Wave 4 / 5 will replace
		// this with proper rated-load probing per threshold.
		if _, ok := sr.LatencyAtSLO[c.ScenarioName]; !ok {
			sr.LatencyAtSLO[c.ScenarioName] = map[int]int{}
		}
		for _, ms := range report.SLOThresholds {
			if lat.P99 <= time.Duration(ms)*time.Millisecond {
				sr.LatencyAtSLO[c.ScenarioName][ms] = int(c.RPSMedian)
			}
		}
		if c.MergedHistogramB64 != "" {
			sr.HdrHistogramB64[c.ScenarioName] = c.MergedHistogramB64
		}
	}

	out := &report.Document{
		SchemaVersion:   report.SchemaVersion,
		HostArchPair:    runtime.GOOS + "/" + runtime.GOARCH,
		BenchmarkConfig: bench,
	}
	names := make([]string, 0, len(byAdapter))
	for k := range byAdapter {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		out.Benchmarks = append(out.Benchmarks, *byAdapter[n])
	}
	return out
}

// writeReports persists the v5.0 results.json + report.md in the run's
// output directory. Both renderers tolerate empty inputs so a run with
// zero successful cells still produces files for the CI gate to read.
func writeReports(cfg Config, doc *report.Document, agg map[string]report.CellAggregate, started time.Time) error {
	jsonPath := filepath.Join(cfg.Out, "results.json")
	if err := writeJSON(jsonPath, doc); err != nil {
		return fmt.Errorf("write %s: %w", jsonPath, err)
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
	return nil
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
