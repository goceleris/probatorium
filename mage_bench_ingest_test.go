//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/loadgen"
)

// writeRunnerCell drops a runner-shaped per-cell JSON under
// cellDir/run0/<scenario>/<server>.json — the exact artefact
// readRunnerCellResults ingests. result==nil writes a status-only cell
// (no measurement).
func writeRunnerCell(t *testing.T, cellDir, scenario, server, status, errMsg string, result *loadgen.Result) {
	t.Helper()
	dir := filepath.Join(cellDir, "run0", scenario)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	cell := map[string]any{
		"run_idx":  0,
		"scenario": scenario,
		"server":   server,
		"status":   status,
	}
	if errMsg != "" {
		cell["error"] = errMsg
	}
	if result != nil {
		cell["result"] = result
	}
	data, err := json.MarshalIndent(cell, "", "  ")
	if err != nil {
		t.Fatalf("marshal cell: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, server+".json"), data, 0o644); err != nil {
		t.Fatalf("write cell: %v", err)
	}
}

// TestAggregateReconstructsColumnMissingRollup pins the v3.8 lost-column
// recovery: the celeris-epoll-h1-sync runner took a second hang-guard
// signal and force-exited before writing its results.json rollup,
// stranding 27 finished cells in run0/<scenario>/<server>.json. The
// per-cell JSONs are the ingest source either way, so
// aggregatePerCellResults must recover the finished cells normally AND
// mark their provenance so the merge log / Publish integrity gate can
// tell a clean column from a reconstructed one. A sibling column whose
// rollup survived must carry no provenance mark.
func TestAggregateReconstructsColumnMissingRollup(t *testing.T) {
	resultsDir := t.TempDir()
	benchDir := filepath.Join(resultsDir, "20260611T074835-bench-msa2-server")

	mkResult := func(rps float64) *loadgen.Result {
		return &loadgen.Result{
			Requests:       52_500_000,
			Errors:         701,
			Duration:       90 * time.Second,
			RequestsPerSec: rps,
			Latency:        loadgen.Percentiles{P50: 70 * time.Microsecond, P99: 700 * time.Microsecond},
		}
	}

	// Lost column: per-cell JSONs only, NO results.json (the runner
	// force-exited mid-column). One finished cell plus the in-flight
	// cell the interrupt marked DNF.
	lost := filepath.Join(benchDir, "00-celeris-epoll-h1-sync")
	writeRunnerCell(t, lost, "get-json", "celeris-epoll-h1-sync", "ok", "", mkResult(655138))
	writeRunnerCell(t, lost, "post-4k", "celeris-epoll-h1-sync", "dnf",
		"interrupted: cell cancelled mid-run (requests=312 errors=0)", nil)

	// Intact column: same per-cell layout plus the runner's own rollup.
	intact := filepath.Join(benchDir, "00-gin-h1")
	writeRunnerCell(t, intact, "get-json", "gin-h1", "ok", "", mkResult(120000))
	if err := os.WriteFile(filepath.Join(intact, "results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatalf("write intact rollup: %v", err)
	}

	if err := aggregatePerCellResults(resultsDir); err != nil {
		t.Fatalf("aggregatePerCellResults: %v", err)
	}

	data, err := os.ReadFile(filepath.Join(resultsDir, "raw", "msa2-server.json"))
	if err != nil {
		t.Fatalf("read raw payload: %v", err)
	}
	var payload struct {
		Summary map[string]json.RawMessage `json:"summary"`
		Cells   []cellRecord               `json:"cells"`
	}
	if err := json.Unmarshal(data, &payload); err != nil {
		t.Fatalf("parse raw payload: %v", err)
	}
	if len(payload.Cells) != 3 {
		t.Fatalf("cells: want 3 got %d", len(payload.Cells))
	}
	for _, c := range payload.Cells {
		key := fmt.Sprintf("%s/%s", c.Competitor, c.Scenario)
		switch c.Competitor {
		case "celeris-epoll-h1-sync":
			if c.Provenance != provenanceReconstructed {
				t.Errorf("%s: want provenance %q got %q", key, provenanceReconstructed, c.Provenance)
			}
		case "gin-h1":
			if c.Provenance != "" {
				t.Errorf("%s: intact column must carry no provenance, got %q", key, c.Provenance)
			}
		default:
			t.Errorf("unexpected cell %s", key)
		}
	}
	// The recovered OK cell's numbers must survive into the summary —
	// reconstruction is provenance marking, never data loss.
	if _, ok := payload.Summary["celeris-epoll-h1-sync/get-json"]; !ok {
		t.Errorf("summary missing recovered cell celeris-epoll-h1-sync/get-json (keys: %v)", keysOf(payload.Summary))
	}
	// The interrupted in-flight cell travels as a DNF record, not data.
	// (A no-data cell's loadgen field is the JSON literal null — the
	// shape every consumer already gates on.)
	for _, c := range payload.Cells {
		if c.Scenario == "post-4k" {
			if c.Status != "dnf" || (len(c.Loadgen) != 0 && string(c.Loadgen) != "null") {
				t.Errorf("post-4k: want dnf record with no payload, got status=%q payload=%q", c.Status, c.Loadgen)
			}
		}
	}
}

func keysOf[V any](m map[string]V) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}
