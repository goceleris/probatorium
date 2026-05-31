//go:build mage

package main

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/goceleris/loadgen"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/servers"
)

// Bench targets. Bench drives the loadgen through the full v5.0
// schema; BenchSince diffs the latest run against a published
// baseline and exits non-zero on regression beyond a threshold.
//
// Both produce a single results dir under results/<ts>-bench-<ver>/
// with per-host JSON merged into one v5.0 manifest, so downstream
// tooling (BenchSince, Publish) only ever reads one path.

// Bench runs the distributed loadgen across the configured bench
// targets. Auto-deploys if no manifest is present on the cluster.
//
// Pipeline:
//
//  1. Read CELERIS_VERSION (env > go.mod > "dev").
//  2. If neither bench target is staged (no manifest), call Deploy.
//  3. ansible-playbook bench.yml with every BENCH_* knob forwarded
//     as an extra-var.
//  4. The playbook fetches per-host JSON results into
//     results/<ts>-bench-<ver>/raw/<host>.json.
//  5. mergeBenchResults walks raw/, validates v5.0 schema, and
//     writes results/<ts>-bench-<ver>/results.json.
//
// Env knobs (with defaults):
//
//	BENCH_TARGET=both              msa2-server | msr1 | both
//	BENCH_COMPETITORS=all          all | <csv>; matches Deploy filter
//	BENCH_DURATION=120s            per-cell active duration
//	BENCH_WARMUP=30s               per-cell warmup
//	BENCH_CONNECTIONS=256          loadgen concurrent conns
//	BENCH_CELLS=*                  cell glob forwarded to the runner's
//	                               -cells over "<scenario>/<competitor>";
//	                               the server half is the competitor slug
//	BENCH_SEED=                    deterministic loadgen seed (empty
//	                               → random)
//	BENCH_RUNS=5                   median over N runs
//	CELERIS_VERSION=               override go.mod auto-detect
//	CLUSTER_USE_LAN=1              LAN fabric instead of Tailscale
func Bench() error {
	if err := requireAnsible(); err != nil {
		return err
	}

	target := envOrDefault("BENCH_TARGET", "both")
	if target != "both" && target != "msa2-server" && target != "msr1" {
		return fmt.Errorf("BENCH_TARGET must be msa2-server, msr1, or both (got %q)", target)
	}
	competitors := envOrDefault("BENCH_COMPETITORS", "all")
	duration := envOrDefault("BENCH_DURATION", "120s")
	warmup := envOrDefault("BENCH_WARMUP", "30s")
	conns := envOrDefault("BENCH_CONNECTIONS", "256")
	cells := envOrDefault("BENCH_CELLS", "*")
	seed := os.Getenv("BENCH_SEED")
	runs := envOrDefault("BENCH_RUNS", "5")
	// Rated mode (probatorium#156) is opt-in and default-OFF: it multiplies
	// per-cell wall-clock by the rated sweep, so the budget issue (#166)
	// curates when it runs. "1"/"true" turns it on; forwarded to the runner
	// via bench_rated so run_bench_cell.yml adds the -rated flag.
	ratedOn := os.Getenv("BENCH_RATED") == "1" || os.Getenv("BENCH_RATED") == "true"
	ratedDuration := envOrDefault("BENCH_RATED_DURATION", "30s")
	version, err := celerisVersion()
	if err != nil {
		return err
	}

	// Auto-deploy when neither bench target has a manifest yet. Cheap
	// pre-flight: SSH to both bench_targets; if neither has the
	// manifest file, kick off Deploy. A partial deploy (one target
	// missing) is treated as already-deployed because re-running
	// Deploy is idempotent and the user's intent is "bench what's
	// there." Note: we can't gate on binaries-present because the
	// manifest schema deliberately doesn't track staged binaries —
	// the playbook's `copy` task is the source of truth for those.
	hasManifest := false
	for _, h := range []string{"msa2-server", "msr1"} {
		if present, _, err := manifestRead(h); err == nil && present {
			hasManifest = true
			break
		}
	}
	if !hasManifest {
		fmt.Println("=== No bench manifest detected; running Deploy first ===")
		// Auto-deploy mirrors the bench scope. Without this, the
		// implicit Deploy installs every toolchain (rust + bun +
		// python) even for a Go-only bench, which pulls in expensive
		// network IO (deadsnakes PPA gpg fetch can fail on flaky
		// hosts) and breaks BENCH_COMPETITORS=axum because it tries
		// to install python too.
		if os.Getenv("DEPLOY_COMPETITORS") == "" {
			_ = os.Setenv("DEPLOY_COMPETITORS", competitors)
			defer func() { _ = os.Unsetenv("DEPLOY_COMPETITORS") }()
		}
		if err := Deploy(); err != nil {
			return fmt.Errorf("auto-deploy: %w", err)
		}
	}

	ts := time.Now().UTC().Format("20060102-150405")
	resultsDir, err := filepath.Abs(filepath.Join("results",
		fmt.Sprintf("%s-bench-%s", ts, version)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Join(resultsDir, "raw"), 0o755); err != nil {
		return err
	}

	fmt.Printf("\n=== Bench ===\n")
	fmt.Printf("  target:       %s\n", target)
	fmt.Printf("  competitors:  %s\n", competitors)
	fmt.Printf("  duration:     %s (warmup %s)\n", duration, warmup)
	fmt.Printf("  connections:  %s\n", conns)
	fmt.Printf("  cells:        %s\n", cells)
	fmt.Printf("  runs:         %s\n", runs)
	fmt.Printf("  rated:        %v\n", ratedOn)
	fmt.Printf("  celeris ver:  %s\n", version)
	fmt.Printf("  results:      %s\n\n", resultsDir)

	// Expand the competitor arg into the matrix COLUMNS (one per registry
	// adapter), each carrying the staged binary dir it runs from and the
	// -engine flag the binary needs. A single binary (e.g. servers/gin)
	// backs multiple columns (gin-h1, gin-h2) that differ only by -engine,
	// so the bench loop must iterate columns — NOT binary dirs — and start
	// each binary with the right mode. competitor_columns is the slug→
	// {bin,engine} map run_bench_cell.yml resolves; competitor_set is the
	// comma-joined column slugs the playbook splits into its schedule.
	columns, err := resolveBenchColumns(competitors)
	if err != nil {
		return err
	}
	colSlugs := make([]string, len(columns))
	colMap := make(map[string]map[string]string, len(columns))
	for i, c := range columns {
		colSlugs[i] = c.Slug
		colMap[c.Slug] = map[string]string{"bin": c.Bin, "engine": c.Engine}
	}
	competitorSetCSV := strings.Join(colSlugs, ",")
	benchVars := map[string]any{"competitor_columns": colMap}
	benchVarsJSON, err := json.MarshalIndent(benchVars, "", "  ")
	if err != nil {
		return err
	}
	benchVarsFile := filepath.Join(resultsDir, "bench-vars.json")
	if err := os.WriteFile(benchVarsFile, benchVarsJSON, 0o600); err != nil {
		return err
	}
	fmt.Printf("  columns:      %d (%s)\n", len(colSlugs), competitorSetCSV)

	// Expand "both" into the two concrete arch hosts and invoke the playbook
	// once per arch. bench.yml resolves hostvars[bench_target].lan_ip, so it
	// needs a REAL host — "both" is not an inventory host (passing it yields
	// hostvars['both'] → undefined). This matches bench.yml's documented
	// contract ("the mage Bench 'both' path invokes this playbook once per
	// target"). Each pass writes its own <TS>-bench-<host>/ dir under the
	// shared resultsDir, which aggregatePerCellResults + mergeBenchResults
	// below fold together (merge already handles target=="both").
	playbookTargets := []string{target}
	if target == "both" {
		playbookTargets = []string{"msa2-server", "msr1"}
	}
	for _, pt := range playbookTargets {
		if len(playbookTargets) > 1 {
			fmt.Printf("\n=== Bench arch pass: %s ===\n", pt)
		}
		args := []string{
			"-i", "inventory.yml",
			benchPlaybook,
			"--extra-vars", "bench_target=" + pt,
			"--extra-vars", "competitor_set=" + competitorSetCSV,
			"--extra-vars", "@" + benchVarsFile,
			"--extra-vars", "bench_duration=" + duration,
			"--extra-vars", "bench_warmup=" + warmup,
			"--extra-vars", "bench_connections=" + conns,
			"--extra-vars", "bench_cells=" + cells,
			"--extra-vars", "bench_runs=" + runs,
			"--extra-vars", "celeris_version=" + version,
			"--extra-vars", "results_local_dir=" + resultsDir,
		}
		if seed != "" {
			args = append(args, "--extra-vars", "bench_seed="+seed)
		}
		if ratedOn {
			args = append(args,
				"--extra-vars", "bench_rated=1",
				"--extra-vars", "bench_rated_duration="+ratedDuration)
		}
		if os.Getenv("CLUSTER_USE_LAN") == "1" {
			args = append(args, "--extra-vars", "use_lan=true")
		}

		cmd := exec.Command("ansible-playbook", args...)
		cmd.Dir = ansibleDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("bench (%s): %w", pt, err)
		}
	}

	// Each ansible cell ran the Go runner once against the remote SUT,
	// emitting per-scenario JSON under
	// resultsDir/<TS>-bench-<bench_target>/<RR>-<comp>/run0/<scenario>/<server>.json.
	// Roll those up into per-host raw payloads under resultsDir/raw/
	// so mergeBenchResults below can assemble the v5.0 results.json.
	if err := aggregatePerCellResults(resultsDir); err != nil {
		return fmt.Errorf("aggregate per-cell results: %w", err)
	}

	merged, err := mergeBenchResults(resultsDir, target, benchParams{
		CelerisVer: version,
		Duration:   duration,
		Warmup:     warmup,
		Conns:      conns,
		Runs:       runs,
		Seed:       seed,
	})
	if err != nil {
		return fmt.Errorf("merge results: %w", err)
	}
	fmt.Printf("\n=== Bench complete: %s ===\n", merged)
	return nil
}

// benchColumn is one matrix column: the report slug (a servers.Registry
// adapter name, e.g. "gin-h2" or "celeris-epoll-h1-sync"), the staged
// binary directory it runs from under competitors/<bin>, and the -engine
// flag value the binary needs. Engine is empty for adapters whose binary
// takes only -bind (gorilla_ws + every NativeBinary), so run_bench_cell.yml
// omits the flag and never trips a "flag provided but not defined" abort.
type benchColumn struct {
	Slug   string
	Bin    string
	Engine string
}

// resolveBenchColumns expands the BENCH_COMPETITORS arg into the matrix
// columns. "all"/"" yields every registered adapter; a CSV yields exactly
// those adapter names (each MUST exist in servers.Registry — an unknown
// name fails loudly rather than silently dropping a column). The crucial
// job this does that a bare CSV split cannot: a single staged binary backs
// several columns (servers/gin → gin-h1 + gin-h2), so it maps each column
// slug to (binary dir, -engine value) using the registry as the source of
// truth. Engine == the registry Adapter.Engine field for Go adapters (the
// two are identical by construction); gorilla_ws is the lone Go binary with
// no -engine flag, and natives are launched with -bind only.
func resolveBenchColumns(arg string) ([]benchColumn, error) {
	all := servers.Names() // sorted, stable
	names := all
	if arg != "" && arg != "all" {
		known := make(map[string]bool, len(all))
		for _, n := range all {
			known[n] = true
		}
		want := make(map[string]bool)
		for _, raw := range strings.Split(arg, ",") {
			n := strings.TrimSpace(raw)
			if n == "" {
				continue
			}
			if !known[n] {
				return nil, fmt.Errorf("BENCH_COMPETITORS: %q is not a registered adapter column (see servers.Registry)", n)
			}
			want[n] = true
		}
		names = make([]string, 0, len(want))
		for _, n := range all {
			if want[n] {
				names = append(names, n)
			}
		}
	}
	cols := make([]benchColumn, 0, len(names))
	for _, n := range names {
		a := servers.Registry[n]
		col := benchColumn{Slug: n}
		if gb, ok := a.Bin.(servers.GoBinary); ok {
			col.Bin = filepath.Base(gb.ModuleDir) // "servers/gin" → "gin"
			// gorilla_ws is the only Go binary with no -engine flag; every
			// other Go binary either consumes -engine (stdhttp/gin/echo/chi/
			// iris/hertz/celeris) or accepts-and-ignores it (gnet/fasthttp/
			// fiber), so passing the registry Engine value is always safe there.
			if col.Bin != "gorilla_ws" {
				col.Engine = a.Engine
			}
		} else {
			// NativeBinary (rust/cpp/dotnet/zig/bun/python) — staged under
			// competitors/<slug>, launched with -bind only.
			col.Bin = n
		}
		cols = append(cols, col)
	}
	return cols, nil
}

// aggregatePerCellResults walks the per-cell output the bench playbook's
// runner invocation produced and folds it into one raw/<host>.json per
// bench_target host. Since issue #152 each ansible cell runs the Go
// runner once (-runs 1) against the remote SUT, and the runner expands
// the full scenario catalogue itself, so the directory layout is now:
//
//	resultsDir/
//	  <TS>-bench-<bench_target>/    ← one dir per `mage Bench` (or two
//	                                   when BENCH_TARGET=both, one per
//	                                   target)
//	    <RR>-<competitor>/          ← one ansible cell = (run, competitor)
//	      run0/<scenario>/<server>.json  ← runner per-cell JSON (what we
//	                                        ingest; cellResultFile shape)
//	      results.json, report.md        ← runner's own rollup (ignored)
//	      server.log, cpu.*.log          ← side-channel artefacts
//
// The runner always writes run0/ because ansible drives it with -runs 1;
// the cross-run interleaving lives in ansible's outer loop, so the
// <RR>- prefix carries the real run index.
//
// Output shape (one file per bench_target). Each cell row now also
// carries the scenario the runner expanded, and `summary` is keyed by
// "<competitor>/<scenario>" so the scenario dimension survives the
// rollup instead of collapsing to one number per competitor:
//
//	{
//	  "host": "msa2-server",
//	  "celeris_version": "...",
//	  "summary": {
//	    "<competitor>/<scenario>": {
//	      "runs":            <int>,
//	      "median_rps":      <float>,
//	      "median_p99_ns":   <int>,
//	      "total_requests":  <int>,
//	      "total_errors":    <int>
//	    }, ...
//	  },
//	  "cells": [
//	    {"run_index": 0, "competitor": "gin", "scenario": "get-json", "loadgen": <raw>},
//	    ...
//	  ]
//	}
//
// `summary` is the headline view a human (or a regression-diff tool)
// reads first. Median across runs (rather than mean) hardens the
// numbers against single-cell outliers — a transient GC pause or a
// neighbour-noise blip on the cluster won't shift the median if it's
// only one of three+ runs.
//
// `cells` is preserved verbatim so downstream tooling can still
// compute richer aggregations (HdrHistogram merging, LatencyAtSLO
// rated-mode sweeps, time-series stitching) without re-running bench.
func aggregatePerCellResults(resultsDir string) error {
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return err
	}
	hostCells := make(map[string][]cellRecord)
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		// Top-level dirs we created (`raw/`) are skipped; bench dirs
		// match the timestamp-bench-host pattern from the playbook.
		const sep = "-bench-"
		idx := strings.Index(name, sep)
		if idx < 0 {
			continue
		}
		host := name[idx+len(sep):]
		cellEntries, err := os.ReadDir(filepath.Join(resultsDir, name))
		if err != nil {
			return err
		}
		for _, c := range cellEntries {
			if !c.IsDir() {
				continue
			}
			// Cell dirs look like "00-gin", "01-stdhttp", ...
			parts := strings.SplitN(c.Name(), "-", 2)
			if len(parts) != 2 {
				continue
			}
			runIdx, err := parseRunIndex(parts[0])
			if err != nil {
				continue
			}
			cellDir := filepath.Join(resultsDir, name, c.Name())
			recs, err := readRunnerCellResults(cellDir, runIdx, parts[1])
			if err != nil {
				return fmt.Errorf("read runner output %s: %w", cellDir, err)
			}
			// Server-side resource sampling (#154) lands directly in the
			// cell dir (observer.sqlite + cpu.log) next to the runner's
			// nested run<N>/ output. Best-effort: a cell that ran without
			// an observer simply carries no resources. The same aggregate
			// applies to every scenario the runner expanded in this cell,
			// since the observer scopes to the whole cell process.
			res := readCellResources(cellDir)
			for i := range recs {
				recs[i].Resources = res
			}
			hostCells[host] = append(hostCells[host], recs...)
		}
	}

	celerisVer, _ := celerisVersion()
	for host, cells := range hostCells {
		summary, err := summarizeCells(cells)
		if err != nil {
			return fmt.Errorf("summarize %s: %w", host, err)
		}
		payload := map[string]any{
			"host":            host,
			"celeris_version": celerisVer,
			"summary":         summary,
			"cells":           cells,
		}
		buf, err := json.MarshalIndent(payload, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(
			filepath.Join(rawDir, host+".json"),
			buf, 0o644,
		); err != nil {
			return err
		}
	}

	if err := writeClusterTimeseries(resultsDir, hostCells); err != nil {
		return fmt.Errorf("write timeseries sidecar: %w", err)
	}
	return nil
}

// writeClusterTimeseries is the control-side time-series merge for the
// cluster path (#153). Cluster nodes stay pristine: each node only emits
// per-cell loadgen.json carrying .timeseries; this folds them here.
//
// The cluster pipeline never builds report.CellResult, so we go through
// report.BuildScenarioSeries on a []loadgen.Result assembled per
// (host, competitor, scenario), in RunIndex order. The result is one
// resultsDir/timeseries.json.gz alongside the per-host raw payloads.
//
// Per-cell unmarshal errors are skipped (mirroring the missing-result
// skip in readRunnerCellResults) rather than failing the whole bench.
func writeClusterTimeseries(resultsDir string, hostCells map[string][]cellRecord) error {
	type seriesKey struct {
		Host, Competitor, Scenario string
	}
	grouped := map[seriesKey][]cellRecord{}
	for host, cells := range hostCells {
		for _, c := range cells {
			k := seriesKey{Host: host, Competitor: c.Competitor, Scenario: c.Scenario}
			grouped[k] = append(grouped[k], c)
		}
	}

	keys := make([]seriesKey, 0, len(grouped))
	for k := range grouped {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i].Scenario != keys[j].Scenario {
			return keys[i].Scenario < keys[j].Scenario
		}
		if keys[i].Competitor != keys[j].Competitor {
			return keys[i].Competitor < keys[j].Competitor
		}
		return keys[i].Host < keys[j].Host
	})

	doc := &report.TimeseriesDoc{
		GeneratedAt:   time.Now().UTC(),
		SchemaVersion: report.TimeseriesSchemaVersion,
	}
	for _, k := range keys {
		recs := grouped[k]
		sort.Slice(recs, func(i, j int) bool { return recs[i].RunIndex < recs[j].RunIndex })
		results := make([]loadgen.Result, 0, len(recs))
		for _, r := range recs {
			var res loadgen.Result
			if err := json.Unmarshal(r.Loadgen, &res); err != nil {
				continue
			}
			results = append(results, res)
		}
		// Server keys on competitor; the host dimension is folded into the
		// Category slot so a competitor benched on both targets stays
		// attributable without bloating the sidecar shape.
		doc.Scenarios = append(doc.Scenarios,
			report.BuildScenarioSeries(k.Scenario, k.Competitor, k.Host, results))
	}

	data, err := doc.MarshalGzip()
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(resultsDir, "timeseries.json.gz"), data, 0o644)
}

// runnerCellFile mirrors cmd/runner's cellResultFile JSON shape — the
// per-cell artefact the runner writes under run<N>/<scenario>/<server>.json.
// Only the fields aggregation needs are decoded; `result` is kept as a
// raw message so it round-trips into cellRecord.Loadgen unchanged for
// downstream loadgen.Result parsing.
type runnerCellFile struct {
	RunIdx       int             `json:"run_idx"`
	ScenarioName string          `json:"scenario"`
	ServerName   string          `json:"server"`
	Result       json.RawMessage `json:"result"`

	// RatedPasses is the rated sweep emitted by the runner only when rated
	// mode ran (probatorium#156); the BenchSince latency_at_slo gate is
	// derived from it. SaturationModeRPS echoes the saturation scale anchor.
	SaturationModeRPS float64         `json:"saturation_mode_rps,omitempty"`
	RatedPasses       []ratedPassWire `json:"rated_passes,omitempty"`
}

// ratedPassWire mirrors cmd/runner's ratedPassFile: one rated pass's offered
// load and CO-corrected P99.
type ratedPassWire struct {
	TargetRPS float64       `json:"target_rps"`
	P99       time.Duration `json:"p99"`
}

// readRunnerCellResults walks one ansible cell dir's runner output
// (run*/<scenario>/<server>.json) and lifts each per-scenario result
// into a cellRecord. runIdx and competitor come from the <RR>-<comp>
// ansible dir name (the source of truth for the interleaved run index);
// the runner's own internal run_idx is always 0 because ansible drives
// it with -runs 1, so we deliberately ignore it.
//
// Cells whose runner JSON carries no `result` (the SUT failed to answer,
// e.g. an H2-only scenario against an H1-only server) are skipped: a
// missing row is louder in the merged report than a zero-valued guess.
func readRunnerCellResults(cellDir string, runIdx int, competitor string) ([]cellRecord, error) {
	runEntries, err := os.ReadDir(cellDir)
	if err != nil {
		return nil, err
	}
	var out []cellRecord
	for _, runE := range runEntries {
		// The runner nests results under run<N>/; siblings (results.json,
		// report.md, logs) are not directories and are skipped.
		if !runE.IsDir() || !strings.HasPrefix(runE.Name(), "run") {
			continue
		}
		scenarioDir := filepath.Join(cellDir, runE.Name())
		scEntries, err := os.ReadDir(scenarioDir)
		if err != nil {
			return nil, err
		}
		for _, scE := range scEntries {
			if !scE.IsDir() {
				continue
			}
			jsonDir := filepath.Join(scenarioDir, scE.Name())
			jsonEntries, err := os.ReadDir(jsonDir)
			if err != nil {
				return nil, err
			}
			for _, jf := range jsonEntries {
				if jf.IsDir() || !strings.HasSuffix(jf.Name(), ".json") {
					continue
				}
				data, err := os.ReadFile(filepath.Join(jsonDir, jf.Name()))
				if err != nil {
					return nil, err
				}
				var cf runnerCellFile
				if err := json.Unmarshal(data, &cf); err != nil {
					return nil, fmt.Errorf("parse %s: %w", jf.Name(), err)
				}
				if len(cf.Result) == 0 || string(cf.Result) == "null" {
					continue
				}
				scenario := cf.ScenarioName
				if scenario == "" {
					scenario = scE.Name()
				}
				out = append(out, cellRecord{
					RunIndex:   runIdx,
					Competitor: competitor,
					Scenario:   scenario,
					Loadgen:    cf.Result,
				})
			}
		}
	}
	return out, nil
}

// competitorStats is the per-competitor headline view emitted in
// `summary` for each host. Field names use snake_case for JSON
// readability; the totals fields make it easy to grep for the
// classic "any errors?" question without scanning every cell.
type competitorStats struct {
	Runs          int     `json:"runs"`
	MedianRPS     float64 `json:"median_rps"`
	MedianP99Ns   int64   `json:"median_p99_ns"`
	TotalRequests int64   `json:"total_requests"`
	TotalErrors   int64   `json:"total_errors"`

	// Resources is the per-(competitor, scenario) representative server
	// resource aggregate (#154): median-of-runs for each summary scalar,
	// with the last run's series kept verbatim. Nil when no run in the
	// bucket captured observer data.
	Resources *report.ResourceStats `json:"resources,omitempty"`

	// LatencyAtSLO maps an SLO budget (ms) to the max sustained target RPS
	// whose median (across runs) rated P99 stayed under budget. Bigger is
	// better. Omitted unless rated mode ran. Human-readable mirror of the
	// typed Document's latency_at_slo; the gate itself reads the Document.
	LatencyAtSLO map[int]int `json:"latency_at_slo,omitempty"`
}

// cellRecord is one row in `cells` — the per-cell view written to
// raw/<host>.json. Carried as a value type so summarizeCells can
// operate on the same slice without an extra parse pass.
//
// Scenario carries the per-scenario dimension the runner now expands
// (issue #152): one cellRecord per (run_index, competitor, scenario)
// rather than the old one-per-(run_index, competitor). Loadgen is the
// raw loadgen.Result lifted out of the runner's per-cell cellResultFile.
type cellRecord struct {
	RunIndex   int             `json:"run_index"`
	Competitor string          `json:"competitor"`
	Scenario   string          `json:"scenario"`
	Loadgen    json.RawMessage `json:"loadgen"`

	// Resources is the server-side resource aggregate (#154) parsed from
	// the cell's observer.sqlite + cpu.log. Nil when the cell ran without
	// an observer. One aggregate per cell — shared across the scenarios
	// the runner expanded — since the observer scopes the whole cell.
	Resources *report.ResourceStats `json:"resources,omitempty"`

	// RatedPasses carries the rated sweep lifted from the runner's per-cell
	// JSON (probatorium#156). Empty unless rated mode ran. Folded into the
	// merged Document's latency_at_slo by mergeBenchResults so the gate has
	// a live signal.
	RatedPasses []ratedPassWire `json:"rated_passes,omitempty"`
}

// readCellResources parses the per-cell observer.sqlite + cpu.log into a
// resource aggregate. Best-effort: any missing/unreadable input yields
// the partial aggregate it can build (or nil when neither input exists),
// so a cluster cell that ran without the sampler never fails the merge.
func readCellResources(cellDir string) *report.ResourceStats {
	samples, dbErr := report.ParseObserverDB(filepath.Join(cellDir, "observer.sqlite"))
	cpuMean, cpuSeries, cpuOK, _ := report.ParseMPStat(filepath.Join(cellDir, "cpu.log"))
	if dbErr != nil && !cpuOK {
		return nil
	}
	stats := report.SummarizeResources(samples, cpuMean, cpuOK, cpuSeries)
	return &stats
}

// summarizeCells folds the per-cell loadgen.Result blobs into one stats
// record per (competitor, scenario). Buckets are keyed by
// "<competitor>/<scenario>" so the scenario dimension the runner now
// expands (issue #152) survives the rollup — collapsing back to one
// number per competitor is exactly the bug #152 fixes.
//
// Median is chosen over mean because a single GC pause / scheduler
// blip on the cluster can shift the mean by 5% while the median
// shrugs off any one outlier — and our smallest bench profile is
// 3 runs (BENCH_RUNS default), where the median is well-defined and
// robust.
func summarizeCells(cells []cellRecord) (map[string]competitorStats, error) {
	type loadgenLite struct {
		Requests       int64   `json:"requests"`
		Errors         int64   `json:"errors"`
		RequestsPerSec float64 `json:"requests_per_sec"`
		Latency        struct {
			P99 int64 `json:"p99"`
		} `json:"latency"`
	}
	// First pass: bucket cells by (competitor, scenario).
	rps := map[string][]float64{}
	p99 := map[string][]int64{}
	reqs := map[string]int64{}
	errs := map[string]int64{}
	runs := map[string]int{}
	res := map[string][]*report.ResourceStats{}
	rated := map[string][][]ratedPassWire{}
	for _, c := range cells {
		var lg loadgenLite
		if err := json.Unmarshal(c.Loadgen, &lg); err != nil {
			return nil, fmt.Errorf("parse cell %s/%s run=%d: %w",
				c.Competitor, c.Scenario, c.RunIndex, err)
		}
		key := summaryKey(c.Competitor, c.Scenario)
		rps[key] = append(rps[key], lg.RequestsPerSec)
		p99[key] = append(p99[key], lg.Latency.P99)
		reqs[key] += lg.Requests
		errs[key] += lg.Errors
		runs[key]++
		if c.Resources != nil {
			res[key] = append(res[key], c.Resources)
		}
		if len(c.RatedPasses) > 0 {
			rated[key] = append(rated[key], c.RatedPasses)
		}
	}
	// Second pass: compute median per bucket.
	out := make(map[string]competitorStats, len(runs))
	for key, n := range runs {
		out[key] = competitorStats{
			Runs:          n,
			MedianRPS:     medianFloat(rps[key]),
			MedianP99Ns:   medianInt(p99[key]),
			TotalRequests: reqs[key],
			TotalErrors:   errs[key],
			Resources:     reduceResources(res[key]),
			LatencyAtSLO:  reduceLatencyAtSLO(rated[key]),
		}
	}
	return out, nil
}

// reduceLatencyAtSLO folds a bucket's per-run rated sweeps into the headline
// latency_at_slo map: for each report.SLOThresholds budget (ms), the max
// target RPS whose median (across runs) P99 stayed under budget. Bigger is
// better. Returns nil when the bucket carries no rated passes.
func reduceLatencyAtSLO(runs [][]ratedPassWire) map[int]int {
	byTarget := map[int][]int64{}
	for _, run := range runs {
		for _, rp := range run {
			t := int(rp.TargetRPS + 0.5)
			byTarget[t] = append(byTarget[t], int64(rp.P99))
		}
	}
	if len(byTarget) == 0 {
		return nil
	}
	medByTarget := make(map[int]time.Duration, len(byTarget))
	for t, ps := range byTarget {
		medByTarget[t] = time.Duration(medianInt(ps))
	}
	slo := map[int]int{}
	for _, ms := range report.SLOThresholds {
		budget := time.Duration(ms) * time.Millisecond
		best := 0
		for t, d := range medByTarget {
			if d <= budget && t > best {
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

// reduceResources folds a bucket's per-run ResourceStats into one
// representative (#154): each summary scalar is the median across runs
// (so a single GC spike or RSS blip does not skew the headline), and the
// last run's series is kept verbatim as the illustrative trajectory. A
// metric stays null in the result iff it was null in EVERY run, so a
// non-Go competitor keeps goroutine/GC null while RSS/CPU/FD survive.
func reduceResources(runs []*report.ResourceStats) *report.ResourceStats {
	if len(runs) == 0 {
		return nil
	}
	out := &report.ResourceStats{Series: runs[len(runs)-1].Series}
	out.Summary.PeakRSSBytes = medianIntPtr(collectI64(runs, func(s report.ResourceSummary) *int64 { return s.PeakRSSBytes }))
	out.Summary.SteadyRSSBytes = medianIntPtr(collectI64(runs, func(s report.ResourceSummary) *int64 { return s.SteadyRSSBytes }))
	out.Summary.GCPauseP99Ns = medianIntPtr(collectI64(runs, func(s report.ResourceSummary) *int64 { return s.GCPauseP99Ns }))
	out.Summary.GoroutineHWM = medianIntPtr(collectI64(runs, func(s report.ResourceSummary) *int64 { return s.GoroutineHWM }))
	out.Summary.FDHWM = medianIntPtr(collectI64(runs, func(s report.ResourceSummary) *int64 { return s.FDHWM }))
	out.Summary.MeanCPUPct = medianFloatPtr(collectF64(runs, func(s report.ResourceSummary) *float64 { return s.MeanCPUPct }))
	return out
}

// collectI64 gathers the non-nil values a selector pulls from each run's
// summary.
func collectI64(runs []*report.ResourceStats, sel func(report.ResourceSummary) *int64) []int64 {
	var out []int64
	for _, r := range runs {
		if v := sel(r.Summary); v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// collectF64 is collectI64 for float metrics.
func collectF64(runs []*report.ResourceStats, sel func(report.ResourceSummary) *float64) []float64 {
	var out []float64
	for _, r := range runs {
		if v := sel(r.Summary); v != nil {
			out = append(out, *v)
		}
	}
	return out
}

// medianIntPtr returns the median of xs as a fresh pointer, or nil when
// xs is empty (every run had the metric null).
func medianIntPtr(xs []int64) *int64 {
	if len(xs) == 0 {
		return nil
	}
	v := medianInt(xs)
	return &v
}

// medianFloatPtr is medianIntPtr for floats.
func medianFloatPtr(xs []float64) *float64 {
	if len(xs) == 0 {
		return nil
	}
	v := medianFloat(xs)
	return &v
}

// summaryKey joins a competitor and scenario into the per-bucket key for
// the `summary` map. Scenario is tolerated empty (pre-#152 cells) so the
// key degrades to the bare competitor rather than a dangling "comp/".
func summaryKey(competitor, scenario string) string {
	if scenario == "" {
		return competitor
	}
	return competitor + "/" + scenario
}

// medianFloat returns the median of xs. xs is mutated (sorted in
// place) — callers should pass a fresh slice, which every call site
// in this file does.
func medianFloat(xs []float64) float64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Float64s(xs)
	mid := len(xs) / 2
	if len(xs)%2 == 1 {
		return xs[mid]
	}
	return (xs[mid-1] + xs[mid]) / 2
}

// medianInt is medianFloat for int64.
func medianInt(xs []int64) int64 {
	if len(xs) == 0 {
		return 0
	}
	sort.Slice(xs, func(i, j int) bool { return xs[i] < xs[j] })
	mid := len(xs) / 2
	if len(xs)%2 == 1 {
		return xs[mid]
	}
	return (xs[mid-1] + xs[mid]) / 2
}

// parseRunIndex extracts the integer run-index from a cell-dir prefix
// like "00", "01", "12". Two-digit zero-padded by the playbook so it
// sorts lexically; we just need the number for the JSON payload.
func parseRunIndex(s string) (int, error) {
	// Strip any leading zeros without using strconv.Atoi's leading-
	// zero behaviour (which is fine; the playbook emits ascii decimal).
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil {
		return 0, fmt.Errorf("parse run-index %q: %w", s, err)
	}
	return n, nil
}

// BenchSince runs Bench then diffs the produced results against a
// pinned baseline version. Exits non-zero if any cell's
// latency_at_slo regresses by more than REGRESSION_THRESHOLD (default
// 5%). Used by the CI gate that protects releases.
//
// Env knobs (in addition to every BENCH_* knob from Bench):
//
//	BASELINE_VERSION=v1.4.2          baseline tag to compare against
//	REGRESSION_THRESHOLD=0.05        max allowed relative regression
//
// Baseline data is read from results/<...>-bench-<BASELINE_VERSION>/
// — the most recent run for that version. If no such run exists the
// target errors out instead of silently passing.
func BenchSince() error {
	if err := Bench(); err != nil {
		return err
	}
	baseline := envOrDefault("BASELINE_VERSION", "v1.4.2")
	threshold := envOrDefault("REGRESSION_THRESHOLD", "0.05")

	current, err := latestBenchResults("")
	if err != nil {
		return fmt.Errorf("locate current results: %w", err)
	}
	base, err := latestBenchResults(baseline)
	if err != nil {
		return fmt.Errorf("locate baseline %s results: %w", baseline, err)
	}
	regressed, report, err := diffBenchResults(base, current, threshold)
	if err != nil {
		return fmt.Errorf("diff: %w", err)
	}
	fmt.Print(report)
	if regressed {
		return fmt.Errorf("regression detected vs %s (threshold %s)", baseline, threshold)
	}
	fmt.Printf("\n=== No regression vs %s (threshold %s) ===\n", baseline, threshold)
	return nil
}

// benchParams threads the already-parsed BENCH_* knobs from Bench()
// into mergeBenchResults so it can populate the v5.1 BenchmarkConfig
// (durations are re-parsed with time.ParseDuration; ints with
// strconv.Atoi). Values are the raw env strings; merge tolerates
// malformed entries (zero-valued field) rather than failing the run.
type benchParams struct {
	CelerisVer string
	Duration   string
	Warmup     string
	Conns      string
	Runs       string
	Seed       string
}

// clusterScenarioName is the legacy single-scenario fallback. Since
// issue #152 the bench playbook drives the Go runner per cell and the
// runner expands the full scenario catalogue, so cells now carry a real
// scenario name and the Document's per-scenario maps are populated
// per-scenario. This constant only backfills pre-#152 raw payloads (or
// the degenerate case of a runner cell with no scenario recorded) so old
// runs still merge into a well-formed single-column Document.
const clusterScenarioName = "bench"

// mergeBenchResults walks resultsDir/raw/*.json (one file per host
// produced by the bench playbook), folds every cell's loadgen.Result
// into report.CellResult records, and writes a single canonical v5.1
// report.Document to results.json at the resultsDir root. Returns the
// path to the merged file.
//
// One Document per run: the bench_target arch keys HostArchPair and the
// target is echoed into BenchmarkConfig.ScenariosFilter. BENCH_TARGET=
// both collapses both hosts' cells into the one Document keyed by
// competitor — per-host fidelity that the retired loose "hosts" map
// carried is intentionally dropped here in favour of one schema-typed
// shape every downstream tool can read.
func mergeBenchResults(resultsDir, target string, p benchParams) (string, error) {
	rawDir := filepath.Join(resultsDir, "raw")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return "", err
	}

	// Bucket samples by competitor across every host's raw payload. A
	// competitor that ran on both bench targets contributes its runs
	// from both files into the same CellResult.
	collected := map[string]*report.CellResult{}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(rawDir, e.Name()))
		if err != nil {
			return "", err
		}
		var payload struct {
			Host           string       `json:"host"`
			CelerisVersion string       `json:"celeris_version"`
			Cells          []cellRecord `json:"cells"`
		}
		if err := json.Unmarshal(data, &payload); err != nil {
			return "", fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		for _, cell := range payload.Cells {
			var res loadgen.Result
			if err := json.Unmarshal(cell.Loadgen, &res); err != nil {
				return "", fmt.Errorf("parse cell %s run=%d in %s: %w",
					cell.Competitor, cell.RunIndex, e.Name(), err)
			}
			cr := collected[cell.Competitor]
			if cr == nil {
				cr = &report.CellResult{
					ScenarioName: clusterScenarioName,
					ServerName:   cell.Competitor,
				}
				collected[cell.Competitor] = cr
			}
			cr.Samples = append(cr.Samples, res)
			// loadgen.Result.Histogram is the V2-compressed HdrHistogram
			// payload as raw bytes; CellResult wants base64 strings so
			// report.Aggregate can decode + merge them across runs.
			b64 := ""
			if len(res.Histogram) > 0 {
				b64 = base64.StdEncoding.EncodeToString(res.Histogram)
			}
			cr.HistogramsB64 = append(cr.HistogramsB64, b64)
			// Thread the rated sweep so Aggregate reduces it into the typed
			// Document's benchmarks[].latency_at_slo — this is what makes the
			// BenchSince gate live on the cluster path (probatorium#156). One
			// inner slice per run.
			if len(cell.RatedPasses) > 0 {
				rs := make([]report.RatedSample, 0, len(cell.RatedPasses))
				for _, rp := range cell.RatedPasses {
					rs = append(rs, report.RatedSample{TargetRPS: rp.TargetRPS, P99: rp.P99})
				}
				cr.RatedSamples = append(cr.RatedSamples, rs)
			}
		}
	}

	cells := make([]report.CellResult, 0, len(collected))
	names := make([]string, 0, len(collected))
	for k := range collected {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		cells = append(cells, *collected[n])
	}
	agg := report.Aggregate(cells)

	now := time.Now().UTC()
	bench := report.BenchmarkConfig{
		FinishedAt:      now,
		Runs:            atoiOr(p.Runs, 0),
		Duration:        parseDurationOr(p.Duration),
		Warmup:          parseDurationOr(p.Warmup),
		GitRef:          gitRefOr(),
		LoadgenVer:      goModRequireVersion("github.com/goceleris/loadgen"),
		CelerisVer:      p.CelerisVer,
		ScenariosFilter: "target=" + target,
	}

	env := report.Environment{
		// The bench playbook does not yet fetch a kernel-sysctl sidecar
		// (ansible/bench.yml collects only per-cell loadgen.json + logs),
		// so v1 of #155 synthesises Environment from the known fabric
		// constants and leaves the sysctl list empty. Capturing the live
		// sysctls is a cluster-side ansible follow-up.
		KernelSysctlsApplied: []string{},
		LoadgenHost:          "msa2-client",
		Fabric:               benchFabric(),
	}

	doc := report.BuildDocument(report.BuildInput{
		HostArchPair:    "linux/" + benchTargetArch(target),
		Environment:     env,
		BenchmarkConfig: bench,
		Servers:         clusterServerMeta(collected),
		Agg:             agg,
	})

	out := filepath.Join(resultsDir, "results.json")
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return "", err
	}
	return out, nil
}

// clusterServerMeta projects the competitors seen in the raw payloads
// into report.ServerMeta. Cluster cells encode only the competitor
// slug, so we look each up in servers.Registry for the (category,
// language, framework, engine) facets and synthesise CompileOptions
// from the language. Competitors absent from the registry still get a
// zero-valued meta so the Document carries them.
func clusterServerMeta(cells map[string]*report.CellResult) map[string]report.ServerMeta {
	out := make(map[string]report.ServerMeta, len(cells))
	for name := range cells {
		a, ok := servers.Registry[name]
		m := report.ServerMeta{}
		if ok {
			m.Category = a.Category
			m.Language = a.Language
			m.Framework = a.Framework
			m.Engine = a.Engine
			m.CompileOptions = report.CompileOptionsFor(a.Language, benchTargetGOARCH())
			if a.Language == "go" {
				m.LanguageVersion = runtime.Version()
			}
		}
		out[name] = m
	}
	return out
}

// atoiOr parses s as an int, returning def on any error.
func atoiOr(s string, def int) int {
	n, err := strconv.Atoi(strings.TrimSpace(s))
	if err != nil {
		return def
	}
	return n
}

// parseDurationOr parses s as a time.Duration, returning 0 on any error.
func parseDurationOr(s string) time.Duration {
	d, err := time.ParseDuration(strings.TrimSpace(s))
	if err != nil {
		return 0
	}
	return d
}

// gitRefOr returns the celeris git ref label for the run. It reuses
// celerisVersion (env > go.mod > "dev") since the cluster bench is
// always labelled by the celeris tag under test.
func gitRefOr() string {
	v, _ := celerisVersion()
	return v
}

// benchFabric describes the wire fabric for the cluster run: the 20G
// LACP LAN when CLUSTER_USE_LAN=1, otherwise the Tailscale overlay.
func benchFabric() string {
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		return "3-host LACP 20G"
	}
	return "3-host Tailscale overlay"
}

// benchTargetArch maps a bench_target host to its CPU arch for the
// HostArchPair tag. msa2-server is amd64; msr1 is arm64. BENCH_TARGET=
// both has no single arch, so it reports "multi".
func benchTargetArch(target string) string {
	switch target {
	case "msa2-server":
		return "amd64"
	case "msr1":
		return "arm64"
	default:
		return "multi"
	}
}

// benchTargetGOARCH returns the GOARCH used to synthesise Go
// CompileOptions for cluster competitors. The cross-compile produces
// one binary per arch; for the metadata we report the env override
// when set, else the dev host's arch.
func benchTargetGOARCH() string {
	if a := os.Getenv("BENCH_GOARCH"); a != "" {
		return a
	}
	return runtime.GOARCH
}

// latestBenchResults returns the path to the most recent
// results/<ts>-bench-<version>/results.json. If version is empty,
// returns the most recent bench run regardless of version. Returns
// an error if no matching run exists — callers (BenchSince) treat
// that as a hard failure rather than silently passing.
//
// Bench results sit DIRECTLY inside the per-run dir (the bench mage
// target writes the aggregated JSON itself before tarballing); the
// nested path mode below (used for validate) doesn't apply here.
func latestBenchResults(version string) (string, error) {
	return latestResultsByPattern("-bench-", "results.json", version, false)
}

// latestValidateResults returns the path to the most recent
// validate-results.json under results/<ts>-validate-<version>/.
//
// The ansible playbook's fetch+extract dance creates one extra level
// of nesting (the remote-side validate_run_dir name lands inside
// resultsDir), so this walks one level deeper than latestBenchResults.
// Same version fallback semantics.
func latestValidateResults(version string) (string, error) {
	return latestResultsByPattern("-validate-", "validate-results.json", version, true)
}

// latestResultsByPattern is the shared scan-results-dir helper. dirInfix
// matches a substring of the per-run dirname ("-bench-" / "-validate-");
// fileName names the canonical results file. version, if non-empty,
// requires the per-run dirname to end with <infix><version>.
//
// When nested is true, the helper also recurses one level into each
// matching per-run dir — needed for validate runs where the ansible
// playbook drops a `<ts>-validate-<host>-<refapp>/` directory INSIDE
// resultsDir as part of fetch+extract. Bench results sit flat, so
// pass nested=false there.
func latestResultsByPattern(dirInfix, fileName, version string, nested bool) (string, error) {
	entries, err := os.ReadDir("results")
	if err != nil {
		return "", err
	}
	var best string
	var bestTime time.Time
	consider := func(path string) {
		st, err := os.Stat(path)
		if err != nil {
			return
		}
		if st.ModTime().After(bestTime) {
			bestTime = st.ModTime()
			best = path
		}
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.Contains(name, dirInfix) {
			continue
		}
		if version != "" && !strings.HasSuffix(name, dirInfix+version) {
			continue
		}
		runDir := filepath.Join("results", name)
		// Flat layout: <runDir>/<fileName>.
		consider(filepath.Join(runDir, fileName))
		if !nested {
			continue
		}
		// Nested layout: <runDir>/<subdir>/<fileName>. The ansible
		// playbook can drop one subdir per fetched host, so scan all.
		subs, err := os.ReadDir(runDir)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			consider(filepath.Join(runDir, sub.Name(), fileName))
		}
	}
	if best == "" {
		flavor := strings.TrimSuffix(strings.TrimPrefix(dirInfix, "-"), "-")
		if version == "" {
			return "", fmt.Errorf("no %s results under results/", flavor)
		}
		return "", fmt.Errorf("no %s results for version %s under results/", flavor, version)
	}
	return best, nil
}

// diffBenchResults compares a baseline results.json against a current
// one and reports per-cell regression. It flattens each document's
// typed Benchmarks[].LatencyAtSLO into "name/scenario/slo" → max-
// sustained-RPS and computes the relative delta. Returns (regressed,
// humanReport, err) where regressed is true iff any cell's relative
// delta is worse than thresholdStr (parsed as float fraction, e.g.
// "0.05" = 5%).
//
// The report format is fixed-width text (not markdown — see CRITICAL
// CONSTRAINT) so it can be read in a CI log without rendering.
func diffBenchResults(basePath, currPath, thresholdStr string) (bool, string, error) {
	threshold, err := parseFloat(thresholdStr)
	if err != nil {
		return false, "", fmt.Errorf("REGRESSION_THRESHOLD: %w", err)
	}
	// Delegate the flatten + compare to the report package (probatorium#156):
	// the gate logic lives there so plain `go test ./report/` reaches it
	// without the mage build tag, and both producers share one definition of
	// "latency_at_slo regressed". The report walk finds the numeric
	// latency_at_slo leaves anywhere under the typed Document (here:
	// benchmarks[].latency_at_slo, populated by the rated sweep), so the gate
	// is live the moment any cell carries a measured latency_at_slo.
	// latency_at_slo is throughput-at-SLO (bigger is better); a drop past the
	// threshold, or a baseline cell vanishing, regresses.
	base, err := report.LoadResultsTree(basePath)
	if err != nil {
		return false, "", err
	}
	curr, err := report.LoadResultsTree(currPath)
	if err != nil {
		return false, "", err
	}
	regs, regressed := report.DiffLatencyAtSLO(base, curr, threshold)

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n=== Bench diff: %s vs %s ===\n", basePath, currPath)
	sb.WriteString(report.RenderRegressionReport(regs, threshold))
	return regressed, sb.String(), nil
}

// parseFloat is a thin wrapper around strconv that gives a friendlier
// error message for the threshold env var. Kept private to this file
// — the helpers package already pulls in strconv via celerisVersion's
// lineage so we avoid the import here by deferring to fmt.Sscanf.
func parseFloat(s string) (float64, error) {
	var f float64
	if _, err := fmt.Sscanf(s, "%f", &f); err != nil {
		return 0, fmt.Errorf("not a number: %q", s)
	}
	return f, nil
}
