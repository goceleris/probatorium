//go:build mage

package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/loadgen"
)

// probatorium#441 on the cluster path: the runner's per-cell JSON reaches
// raw/<host>.json (aggregatePerCellResults) and from there the merged
// results.json (mergeBenchResults). These tests feed it per-cell JSON as
// generic maps, because the keys are the contract with the runner, and read
// the merged document back the same way, because the keys are the contract
// with mage Publish and the readers.

// writeRatedCell writes one runner per-cell JSON with the given rated arrays.
func writeRatedCell(t *testing.T, cellDir, scenario, server, status string, sat float64, extra map[string]any) {
	t.Helper()
	dir := filepath.Join(cellDir, "run0", scenario)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cell := map[string]any{
		"run_idx": 0, "scenario": scenario, "server": server, "status": status,
		"saturation_mode_rps": sat,
		"result": &loadgen.Result{Requests: 1000, Duration: 5 * time.Second, RequestsPerSec: sat,
			Latency: loadgen.Percentiles{P50: time.Millisecond, P99: 2 * time.Millisecond}},
	}
	for k, v := range extra {
		cell[k] = v
	}
	data, err := json.Marshal(cell)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, server+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cellDir, "results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// mergedRated runs the cluster merge over resultsDir and returns the single
// benchmark row of the merged results.json, decoded generically.
func mergedRated(t *testing.T, resultsDir string) map[string]any {
	t.Helper()
	if err := aggregatePerCellResults(resultsDir, 0); err != nil {
		t.Fatalf("aggregatePerCellResults: %v", err)
	}
	merged, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v1.6.0", Duration: "40s", Warmup: "10s", Conns: "256", Runs: "1"})
	if err != nil {
		t.Fatalf("merge: %v", err)
	}
	data, err := os.ReadFile(merged)
	if err != nil {
		t.Fatal(err)
	}
	var doc struct {
		SchemaVersion string           `json:"schema_version"`
		Benchmarks    []map[string]any `json:"benchmarks"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("%d benchmark rows, want 1", len(doc.Benchmarks))
	}
	return doc.Benchmarks[0]
}

func subMap(t *testing.T, m map[string]any, key string) map[string]any {
	t.Helper()
	v, _ := m[key].(map[string]any)
	return v
}

func passList(t *testing.T, row map[string]any, scenario string) []map[string]any {
	t.Helper()
	arr, _ := subMap(t, row, "rated_passes")[scenario].([]any)
	out := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		out = append(out, e.(map[string]any))
	}
	return out
}

// TestMergeCarriesEveryRatedPassWithItsStatus: a cell with two kept passes, one
// pass loadgen could not start and one that sent nothing. All four reach the
// merged document, each with its status, reason, target and achieved rate;
// latency_at_slo and the headline are still built from the two kept passes
// only, and the headline names the target it came from.
func TestMergeCarriesEveryRatedPassWithItsStatus(t *testing.T) {
	resultsDir := t.TempDir()
	cellDir := filepath.Join(resultsDir, "20261024T120000-bench-msa2-server", "00-celeris-epoll-h1-sync")
	writeRatedCell(t, cellDir, "get-json", "celeris-epoll-h1-sync", "ok", 400000, map[string]any{
		"rated_passes": []map[string]any{
			{"fraction": 0.25, "target_rps": 100000, "p99": int64(8 * time.Millisecond), "status": "ok",
				"achieved_rps": 99000, "achieved_ratio": 0.99, "requests": 2970000, "errors": 5, "connect_errors": 1},
			{"fraction": 0.5, "target_rps": 200000, "p99": int64(30 * time.Millisecond), "status": "ok",
				"achieved_rps": 190000, "achieved_ratio": 0.95, "requests": 5700000, "errors": 0},
		},
		"rated_passes_not_kept": []map[string]any{
			{"fraction": 0.75, "target_rps": 300000, "status": "not_run", "reason": "loadgen_new", "detail": "connection refused", "p99": 0},
			{"fraction": 0.9, "target_rps": 360000, "status": "skipped", "reason": "zero_requests", "p99": 0},
		},
	})
	row := mergedRated(t, resultsDir)

	got := passList(t, row, "get-json")
	if len(got) != 4 {
		t.Fatalf("merged rated_passes has %d entries, want 4 (2 kept + 2 not kept): %v", len(got), got)
	}
	wantStatus := []string{"ok", "ok", "not_run", "skipped"}
	wantReason := []string{"", "", "loadgen_new", "zero_requests"}
	wantFrac := []float64{0.25, 0.5, 0.75, 0.9}
	for i, e := range got {
		if e["status"] != wantStatus[i] || (e["reason"] == nil && wantReason[i] != "") || (e["reason"] != nil && e["reason"] != wantReason[i]) {
			t.Errorf("pass %d = status %v reason %v, want %s %q", i, e["status"], e["reason"], wantStatus[i], wantReason[i])
		}
		if e["fraction"] != wantFrac[i] {
			t.Errorf("pass %d fraction = %v, want %v", i, e["fraction"], wantFrac[i])
		}
		if e["run"] != float64(0) {
			t.Errorf("pass %d run = %v, want 0", i, e["run"])
		}
	}
	if got[0]["achieved_rps"] != float64(99000) || got[0]["achieved_ratio"] != 0.99 || got[0]["errors"] != float64(5) || got[0]["connect_errors"] != float64(1) {
		t.Errorf("the kept pass lost its achieved rate / error counts: %v", got[0])
	}
	if got[2]["detail"] != "connection refused" {
		t.Errorf("the not-run pass lost its detail: %v", got[2])
	}

	// The headline and the SLO map are what they were: kept passes only.
	if p99 := subMap(t, row, "rated_mode_p99_at_target_rps")["get-json"]; p99 != float64(30*time.Millisecond) {
		t.Errorf("rated_mode_p99_at_target_rps = %v, want 30ms (the highest KEPT pass, not a skipped 0.9)", p99)
	}
	slo := subMap(t, subMap(t, row, "latency_at_slo"), "get-json")
	if slo["50"] != float64(200000) || slo["10"] != float64(100000) {
		t.Errorf("latency_at_slo = %v, want 10ms->100000 and 50ms->200000", slo)
	}
	if got := subMap(t, row, "rated_headline_target_rps")["get-json"]; got != float64(200000) {
		t.Errorf("rated_headline_target_rps = %v, want 200000: the headline p99 came from the 0.5 pass, and the document must say so", got)
	}
}

// TestMergeKeepsACellWhoseRatedPassesAllFailed: every pass of an ok cell was
// skipped. The cell still carries its four records, and publishes no rated
// numbers (no latency_at_slo, no headline), instead of vanishing from the
// rated record.
func TestMergeKeepsACellWhoseRatedPassesAllFailed(t *testing.T) {
	resultsDir := t.TempDir()
	cellDir := filepath.Join(resultsDir, "20261024T120000-bench-msa2-server", "00-gin-h1")
	var skipped []map[string]any
	for i, f := range []float64{0.25, 0.5, 0.75, 0.9} {
		skipped = append(skipped, map[string]any{"fraction": f, "target_rps": 100000 * float64(i+1),
			"status": "skipped", "reason": "zero_requests", "p99": 0})
	}
	writeRatedCell(t, cellDir, "driver-pg-write", "gin-h1", "ok", 400000, map[string]any{"rated_passes_not_kept": skipped})
	row := mergedRated(t, resultsDir)
	if got := passList(t, row, "driver-pg-write"); len(got) != 4 {
		t.Fatalf("%d records for a cell whose four passes were skipped, want 4: %v", len(got), got)
	}
	if _, ok := subMap(t, row, "latency_at_slo")["driver-pg-write"]; ok {
		t.Error("latency_at_slo has a row for a cell with no kept pass")
	}
	if _, ok := subMap(t, row, "rated_mode_p99_at_target_rps")["driver-pg-write"]; ok {
		t.Error("rated_mode_p99_at_target_rps has a headline for a cell with no kept pass")
	}
	if _, ok := subMap(t, row, "rated_headline_target_rps")["driver-pg-write"]; ok {
		t.Error("rated_headline_target_rps names a target for a cell with no kept pass")
	}
}

// TestMergeAcceptsARunnerThatPredatesTheRecord: a per-cell JSON from a runner
// older than 5.24 has rated_passes entries of {target_rps, p99} and nothing
// else. They merge exactly as before (latency_at_slo, headline) and show up in
// the pass list as kept passes with no measured rate; the list never invents an
// achieved rate for them.
func TestMergeAcceptsARunnerThatPredatesTheRecord(t *testing.T) {
	resultsDir := t.TempDir()
	cellDir := filepath.Join(resultsDir, "20261024T120000-bench-msa2-server", "00-gin-h1")
	writeRatedCell(t, cellDir, "get-json", "gin-h1", "ok", 400000, map[string]any{
		"rated_passes": []map[string]any{
			{"target_rps": 100000, "p99": int64(8 * time.Millisecond)},
			{"target_rps": 200000, "p99": int64(30 * time.Millisecond)},
		},
	})
	row := mergedRated(t, resultsDir)
	if p99 := subMap(t, row, "rated_mode_p99_at_target_rps")["get-json"]; p99 != float64(30*time.Millisecond) {
		t.Errorf("headline = %v, want 30ms", p99)
	}
	if slo := subMap(t, subMap(t, row, "latency_at_slo"), "get-json"); slo["50"] != float64(200000) {
		t.Errorf("latency_at_slo = %v, want 50ms->200000", slo)
	}
	got := passList(t, row, "get-json")
	if len(got) != 2 {
		t.Fatalf("%d records, want the 2 legacy passes", len(got))
	}
	for i, e := range got {
		if e["status"] != "ok" {
			t.Errorf("legacy pass %d status = %v, want ok (it was kept)", i, e["status"])
		}
		if v, present := e["achieved_rps"]; present && v != float64(0) {
			t.Errorf("legacy pass %d carries an achieved_rps %v nobody measured", i, v)
		}
	}
}

// TestMergeRecordsAFlaggedCellsRatedPassesAsNotRun: a suspect cell keeps its
// measurement but runs no sweep; the not-run records travel with it and no
// rated number is published for it.
func TestMergeRecordsAFlaggedCellsRatedPassesAsNotRun(t *testing.T) {
	resultsDir := t.TempDir()
	cellDir := filepath.Join(resultsDir, "20261024T120000-bench-msa2-server", "00-gin-h1")
	writeRatedCell(t, cellDir, "get-json", "gin-h1", "suspect", 400000, map[string]any{
		"error": "suspect: error ratio 0.504 exceeds budget 0.05",
		"rated_passes_not_kept": []map[string]any{
			{"fraction": 0.5, "status": "not_run", "reason": "cell_flagged", "target_rps": 0, "p99": 0},
			{"fraction": 0.9, "status": "not_run", "reason": "cell_flagged", "target_rps": 0, "p99": 0},
		},
	})
	row := mergedRated(t, resultsDir)
	got := passList(t, row, "get-json")
	if len(got) != 2 || got[0]["status"] != "not_run" || got[1]["reason"] != "cell_flagged" {
		t.Fatalf("records = %v, want two not_run/cell_flagged", got)
	}
	if _, ok := subMap(t, row, "latency_at_slo")["get-json"]; ok {
		t.Error("a flagged cell published latency_at_slo")
	}
}
