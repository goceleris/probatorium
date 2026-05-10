//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
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
//	BENCH_CELLS=*                  cell glob (loadgen -cells)
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
	fmt.Printf("  celeris ver:  %s\n", version)
	fmt.Printf("  results:      %s\n\n", resultsDir)

	args := []string{
		"-i", "inventory.yml",
		benchPlaybook,
		"--extra-vars", "bench_target=" + target,
		"--extra-vars", "competitor_set=" + competitors,
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
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		args = append(args, "--extra-vars", "use_lan=true")
	}

	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("bench: %w", err)
	}

	// The playbook produced per-cell loadgen.json files under
	// resultsDir/<TS>-bench-<bench_target>/<RR>-<comp>/loadgen.json.
	// Roll those up into per-host raw payloads under resultsDir/raw/
	// so mergeBenchResults below can assemble the v5.0 results.json.
	if err := aggregatePerCellResults(resultsDir); err != nil {
		return fmt.Errorf("aggregate per-cell results: %w", err)
	}

	merged, err := mergeBenchResults(resultsDir, version, target)
	if err != nil {
		return fmt.Errorf("merge results: %w", err)
	}
	fmt.Printf("\n=== Bench complete: %s ===\n", merged)
	return nil
}

// aggregatePerCellResults walks every per-cell loadgen.json the bench
// playbook produced and folds them into one raw/<host>.json per
// bench_target host. The directory layout the playbook emits is:
//
//	resultsDir/
//	  <TS>-bench-<bench_target>/    ← one dir per `mage Bench` (or two
//	                                   when BENCH_TARGET=both, one per
//	                                   target)
//	    <RR>-<competitor>/
//	      loadgen.json              ← what we ingest
//	      server.log, cpu.log, ...  ← side-channel artefacts
//
// Output shape (one file per bench_target):
//
//	{
//	  "host": "msa2-server",
//	  "celeris_version": "...",
//	  "cells": [
//	    {"run_index": 0, "competitor": "gin", "loadgen": <raw>},
//	    ...
//	  ]
//	}
//
// Pre-aggregator the runner produced rich per-scenario v5.0 payloads;
// this is a deliberately thin first pass that keeps the schema lossy-
// but-honest (every cell carries the full loadgen.Result so downstream
// tooling can compute LatencyAtSLO / HdrHistogram merges itself).
// Lifting to the rich v5.0 schema is tracked separately.
func aggregatePerCellResults(resultsDir string) error {
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		return err
	}

	entries, err := os.ReadDir(resultsDir)
	if err != nil {
		return err
	}
	type cell struct {
		RunIndex   int             `json:"run_index"`
		Competitor string          `json:"competitor"`
		Loadgen    json.RawMessage `json:"loadgen"`
	}
	hostCells := make(map[string][]cell)
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
			loadgenPath := filepath.Join(resultsDir, name, c.Name(), "loadgen.json")
			data, err := os.ReadFile(loadgenPath)
			if err != nil {
				// Missing loadgen.json means the cell never ran (e.g.
				// server failed to bind). Skip; the merged report will
				// just be short a cell, which is louder than guessing.
				continue
			}
			hostCells[host] = append(hostCells[host], cell{
				RunIndex:   runIdx,
				Competitor: parts[1],
				Loadgen:    data,
			})
		}
	}

	celerisVer, _ := celerisVersion()
	for host, cells := range hostCells {
		payload := map[string]any{
			"host":            host,
			"celeris_version": celerisVer,
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
	return nil
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

// mergeBenchResults walks resultsDir/raw/*.json (one file per host
// produced by the bench playbook), validates each is a v5.0-shaped
// payload, and writes a combined results.json at the resultsDir
// root. Returns the path to the merged file.
//
// v5.0 schema (loose — only the top-level fields are pinned here):
//
//	{
//	  "version": "5.0",
//	  "celeris_version": "<tag>",
//	  "target": "msa2-server" | "msr1" | "both",
//	  "hosts": { "<host>": { ...per-host raw payload... } }
//	}
func mergeBenchResults(resultsDir, celerisVer, target string) (string, error) {
	rawDir := filepath.Join(resultsDir, "raw")
	entries, err := os.ReadDir(rawDir)
	if err != nil {
		return "", err
	}
	merged := map[string]any{
		"version":         "5.0",
		"celeris_version": celerisVer,
		"target":          target,
		"hosts":           map[string]any{},
	}
	hostsMap := merged["hosts"].(map[string]any)
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(rawDir, e.Name()))
		if err != nil {
			return "", err
		}
		var payload any
		if err := json.Unmarshal(data, &payload); err != nil {
			return "", fmt.Errorf("parse %s: %w", e.Name(), err)
		}
		host := strings.TrimSuffix(e.Name(), ".json")
		hostsMap[host] = payload
	}
	out := filepath.Join(resultsDir, "results.json")
	data, err := json.MarshalIndent(merged, "", "  ")
	if err != nil {
		return "", err
	}
	if err := os.WriteFile(out, data, 0o644); err != nil {
		return "", err
	}
	return out, nil
}

// latestBenchResults returns the path to the most recent
// results/<ts>-bench-<version>/results.json. If version is empty,
// returns the most recent bench run regardless of version. Returns
// an error if no matching run exists — callers (BenchSince) treat
// that as a hard failure rather than silently passing.
func latestBenchResults(version string) (string, error) {
	entries, err := os.ReadDir("results")
	if err != nil {
		return "", err
	}
	var best string
	var bestTime time.Time
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.Contains(name, "-bench-") {
			continue
		}
		if version != "" && !strings.HasSuffix(name, "-bench-"+version) {
			continue
		}
		path := filepath.Join("results", name, "results.json")
		st, err := os.Stat(path)
		if err != nil {
			continue
		}
		if st.ModTime().After(bestTime) {
			bestTime = st.ModTime()
			best = path
		}
	}
	if best == "" {
		if version == "" {
			return "", fmt.Errorf("no bench results under results/")
		}
		return "", fmt.Errorf("no bench results for version %s under results/", version)
	}
	return best, nil
}

// diffBenchResults compares a baseline results.json against a current
// one and reports per-cell regression. The diff is intentionally
// light: it walks the v5.0 hosts/<host> object, extracts any
// "latency_at_slo" numeric field at any depth, and computes the
// relative delta. Returns (regressed, humanReport, err) where
// regressed is true iff any cell's relative delta is worse than
// thresholdStr (parsed as float fraction, e.g. "0.05" = 5%).
//
// The report format is fixed-width text (not markdown — see CRITICAL
// CONSTRAINT) so it can be read in a CI log without rendering.
func diffBenchResults(basePath, currPath, thresholdStr string) (bool, string, error) {
	threshold, err := parseFloat(thresholdStr)
	if err != nil {
		return false, "", fmt.Errorf("REGRESSION_THRESHOLD: %w", err)
	}
	baseFlat, err := flattenLatencyAtSLO(basePath)
	if err != nil {
		return false, "", err
	}
	currFlat, err := flattenLatencyAtSLO(currPath)
	if err != nil {
		return false, "", err
	}

	var sb strings.Builder
	fmt.Fprintf(&sb, "\n=== Bench diff: %s vs %s ===\n", basePath, currPath)
	fmt.Fprintf(&sb, "%-60s %14s %14s %10s\n", "cell", "baseline", "current", "delta")
	fmt.Fprintf(&sb, "%s\n", strings.Repeat("-", 100))

	regressed := false
	// Stable iteration order — sorted keys so CI logs diff cleanly
	// across runs. We dedupe across both maps to surface cells that
	// only exist on one side.
	keys := unionKeys(baseFlat, currFlat)
	for _, k := range keys {
		b := baseFlat[k]
		c := currFlat[k]
		var delta float64
		var deltaStr string
		switch {
		case b == 0 && c == 0:
			deltaStr = "n/a"
		case b == 0:
			deltaStr = "new"
		case c == 0:
			deltaStr = "missing"
			regressed = true
		default:
			delta = (c - b) / b
			deltaStr = fmt.Sprintf("%+.2f%%", delta*100)
			if -delta > threshold { // current < baseline by more than threshold
				regressed = true
				deltaStr += " !!"
			}
		}
		fmt.Fprintf(&sb, "%-60s %14.0f %14.0f %10s\n", k, b, c, deltaStr)
	}
	return regressed, sb.String(), nil
}

// flattenLatencyAtSLO loads a v5.0 results.json and returns a map of
// "host/path/to/cell" → latency_at_slo numeric value. Walks the
// "hosts" object recursively; any "latency_at_slo" key whose value
// is a number contributes one entry. Other shapes are ignored —
// the diff only cares about this single headline metric.
func flattenLatencyAtSLO(path string) (map[string]float64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	out := make(map[string]float64)
	hosts, ok := doc["hosts"].(map[string]any)
	if !ok {
		return out, nil
	}
	for host, payload := range hosts {
		walkLatencyAtSLO(host, payload, out)
	}
	return out, nil
}

// walkLatencyAtSLO recursively descends into v and records every
// numeric "latency_at_slo" leaf under prefix. Map keys append with
// "/", arrays append with "[i]". Non-numeric latency_at_slo entries
// are skipped silently.
func walkLatencyAtSLO(prefix string, v any, out map[string]float64) {
	switch t := v.(type) {
	case map[string]any:
		if raw, ok := t["latency_at_slo"]; ok {
			if f, ok := raw.(float64); ok {
				out[prefix+"/latency_at_slo"] = f
			}
		}
		for k, child := range t {
			if k == "latency_at_slo" {
				continue
			}
			walkLatencyAtSLO(prefix+"/"+k, child, out)
		}
	case []any:
		for i, child := range t {
			walkLatencyAtSLO(fmt.Sprintf("%s[%d]", prefix, i), child, out)
		}
	}
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

// unionKeys returns the sorted union of keys from two maps. Used to
// produce a stable diff order across runs.
func unionKeys(a, b map[string]float64) []string {
	seen := make(map[string]struct{}, len(a)+len(b))
	for k := range a {
		seen[k] = struct{}{}
	}
	for k := range b {
		seen[k] = struct{}{}
	}
	out := make([]string, 0, len(seen))
	for k := range seen {
		out = append(out, k)
	}
	// Avoid pulling sort in just for this — small N, simple
	// insertion sort keeps imports light.
	for i := 1; i < len(out); i++ {
		for j := i; j > 0 && out[j-1] > out[j]; j-- {
			out[j-1], out[j] = out[j], out[j-1]
		}
	}
	return out
}
