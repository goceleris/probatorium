package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeCell drops a runner-shaped per-cell JSON under
// <root>/00-<server>/run0/<scenario>/<server>.json — the layout
// scanResultsDir walks.
func writeCell(t *testing.T, root, server, scenario, status, errMsg string) {
	t.Helper()
	dir := filepath.Join(root, "00-"+server, "run0", scenario)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	data, err := json.Marshal(cellResult{
		Status:   status,
		Scenario: scenario,
		Server:   server,
		Error:    errMsg,
	})
	if err != nil {
		t.Fatalf("marshal cell: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, server+".json"), data, 0o644); err != nil {
		t.Fatalf("write cell: %v", err)
	}
}

// TestScanOnlyCapabilityGapsEnterSkipList pins the v3.9 hygiene rule
// with the real v3.8 failure strings: only a genuine capability gap
// (not_applicable) may feed the auto-skip machinery. dnf (dead SUT),
// suspect (over error budget) and interrupted cells never enter the
// skip list, and a pre-v5.4 "zero-request cell" record that was
// misclassified not_applicable (in v3.8 every such cell was a dead SUT)
// is re-classified to dnf by the same report.ClassifyCellError the
// runner uses — and therefore excluded too.
func TestScanOnlyCapabilityGapsEnterSkipList(t *testing.T) {
	root := t.TempDir()

	writeCell(t, root, "gin-h1", "get-json", "ok", "")
	// Genuine capability gap: zero successes from a LIVE server.
	writeCell(t, root, "stdhttp-h1", "auto-mix-111", "not_applicable",
		"capability-lie: declared HTTP2C but got zero successes from live server")
	// Dead SUT, pre-cell probe (v3.9 liveness gate).
	writeCell(t, root, "celeris-iouring-h1-async", "get-json", "dnf",
		"server-down: pre-cell probe: dial tcp 192.168.50.65:8080: connect: connection refused")
	// Over the error budget — data exists, integrity flagged.
	writeCell(t, root, "celeris-epoll-h1-sync", "churn-close", "suspect",
		"suspect: error ratio 0.960 exceeds budget 0.5 (errors=290204598 requests=12081484)")
	// Hang-guard interrupt.
	writeCell(t, root, "gin-h1", "post-4k", "dnf",
		"interrupted: cell cancelled mid-run (requests=312 errors=0)")
	// Pre-v5.4 record: status says not_applicable, but the error string
	// re-classifies to dnf under today's rules.
	writeCell(t, root, "celeris-std-h1", "sse-fanout-1024", "not_applicable",
		"zero-request cell: 0 requests after 90s (errors=34700000)")

	skip, scanned, byStatus := scanResultsDir(root)
	if scanned != 6 {
		t.Fatalf("scanned: want 6 got %d", scanned)
	}
	if len(skip) != 1 {
		t.Fatalf("skip list: want exactly the capability gap, got %d entries: %+v", len(skip), skip)
	}
	if skip[0].Server != "stdhttp-h1" || skip[0].Scenario != "auto-mix-111" || skip[0].Status != "not_applicable" {
		t.Errorf("skip entry: got %+v", skip[0])
	}
	// The histogram still reports every status so the operator sees what
	// the scan saw, even though only not_applicable feeds the skip list.
	if byStatus["dnf"] != 2 || byStatus["suspect"] != 1 || byStatus["ok"] != 1 || byStatus["not_applicable"] != 2 {
		t.Errorf("byStatus histogram: got %v", byStatus)
	}
}
