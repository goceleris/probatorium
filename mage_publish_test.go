//go:build mage

package main

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/report"
)

// mkBenchColumn creates a <TS>-bench-<host>/<RR>-<comp>/ column dir,
// optionally with the runner's results.json rollup.
func mkBenchColumn(t *testing.T, resultsDir, benchDir, col string, withRollup bool) {
	t.Helper()
	dir := filepath.Join(resultsDir, benchDir, col)
	if err := os.MkdirAll(filepath.Join(dir, "run0"), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", dir, err)
	}
	if withRollup {
		if err := os.WriteFile(filepath.Join(dir, "results.json"), []byte("{}"), 0o644); err != nil {
			t.Fatalf("write rollup: %v", err)
		}
	}
}

// docWithCells builds a minimal Document whose benchmarks carry the
// given per-(server, scenario) statuses ("ok" entries land in
// SaturationModeRPS; everything else in CellStatuses; suspect in both,
// mirroring BuildDocument).
func docWithCells(cells map[string]map[string]string) *report.Document {
	doc := &report.Document{SchemaVersion: report.SchemaVersion}
	servers := make([]string, 0, len(cells))
	for s := range cells {
		servers = append(servers, s)
	}
	// Sorted for determinism, like BuildDocument.
	sort.Strings(servers)
	for _, s := range servers {
		b := report.ServerResult{
			Name:              s,
			SaturationModeRPS: map[string]float64{},
			CellStatuses:      map[string]string{},
		}
		for sc, st := range cells[s] {
			switch st {
			case "ok":
				b.SaturationModeRPS[sc] = 100000
			case "suspect":
				b.SaturationModeRPS[sc] = 100000
				b.CellStatuses[sc] = st
			default:
				b.CellStatuses[sc] = st
			}
		}
		doc.Benchmarks = append(doc.Benchmarks, b)
	}
	return doc
}

// TestPublishIntegrityHealthy pins the pass-through: a complete run dir
// with intact rollups and an all-OK Document yields no violations and an
// "integrity: ok" summary.
func TestPublishIntegrityHealthy(t *testing.T) {
	resultsDir := t.TempDir()
	mkBenchColumn(t, resultsDir, "20260611T074835-bench-msa2-server", "00-gin-h1", true)
	writeRawHost(t, mustMkRaw(t, resultsDir), "msa2-server", []cellRecord{
		{RunIndex: 0, Competitor: "gin-h1", Scenario: "get-json", Status: "ok"},
	})
	doc := docWithCells(map[string]map[string]string{
		"gin-h1": {"get-json": "ok", "post-4k": "ok"},
	})

	pi := collectPublishIntegrity(resultsDir, doc)
	if v := pi.violations(); len(v) != 0 {
		t.Fatalf("healthy run must have no violations, got %v", v)
	}
	if pi.TotalCells != 2 || pi.StatusCounts[report.CellOK] != 2 {
		t.Errorf("cell census: want 2 ok cells, got total=%d counts=%v", pi.TotalCells, pi.StatusCounts)
	}
	out := renderPublishIntegrity(pi)
	if !strings.Contains(out, "integrity: ok") {
		t.Errorf("summary must say integrity: ok:\n%s", out)
	}
	if err := checkPublishIntegrity(resultsDir, doc); err != nil {
		t.Errorf("healthy run must publish: %v", err)
	}
}

// TestPublishIntegrityV38ShapeRefused rebuilds the three v3.8 failure
// signatures at once — a column that lost its rollup, a grid with >20%
// non-ok cells, and cells measured against a dead SUT — and asserts the
// gate refuses with all three violations unless BENCH_PUBLISH_FORCE=1.
func TestPublishIntegrityV38ShapeRefused(t *testing.T) {
	resultsDir := t.TempDir()
	bench := "20260611T074835-bench-msa2-server"
	mkBenchColumn(t, resultsDir, bench, "00-celeris-epoll-h1-sync", false) // lost rollup
	mkBenchColumn(t, resultsDir, bench, "00-gin-h1", true)

	writeRawHost(t, mustMkRaw(t, resultsDir), "msa2-server", []cellRecord{
		// Reconstructed OK cell from the lost column.
		{RunIndex: 0, Competitor: "celeris-epoll-h1-sync", Scenario: "get-json",
			Status: "ok", Provenance: provenanceReconstructed},
		// Dead-SUT cells: post-cell probe found the port dead, and the
		// next cell's pre-probe refused in seconds.
		{RunIndex: 0, Competitor: "celeris-iouring-h1-async", Scenario: "get-json", Status: "dnf",
			Error: "server-died-mid-cell: post-cell probe: dial tcp 192.168.50.65:8080: connect: connection refused (requests=4029 errors=33100000)"},
		{RunIndex: 0, Competitor: "celeris-iouring-h1-async", Scenario: "post-4k", Status: "dnf",
			Error: "server-down: pre-cell probe: dial tcp 192.168.50.65:8080: connect: connection refused"},
	})

	// 2 of 4 cells non-ok = 50% > 20%.
	doc := docWithCells(map[string]map[string]string{
		"celeris-epoll-h1-sync":    {"get-json": "ok"},
		"celeris-iouring-h1-async": {"get-json": "dnf", "post-4k": "dnf"},
		"gin-h1":                   {"get-json": "ok"},
	})

	pi := collectPublishIntegrity(resultsDir, doc)
	if len(pi.MissingRollups) != 1 || !strings.HasSuffix(pi.MissingRollups[0], "00-celeris-epoll-h1-sync") {
		t.Errorf("MissingRollups: %v", pi.MissingRollups)
	}
	if pi.Reconstructed != 1 {
		t.Errorf("Reconstructed: want 1 got %d", pi.Reconstructed)
	}
	if len(pi.ServerDied) != 2 {
		t.Errorf("ServerDied: want 2 got %v", pi.ServerDied)
	}
	v := pi.violations()
	if len(v) != 3 {
		t.Fatalf("want 3 violations (rollup, non-ok fraction, dead SUT), got %v", v)
	}

	out := renderPublishIntegrity(pi)
	for _, want := range []string{"MISSING ROLLUP", "SERVER DIED", "VIOLATION", "reconstructed cells: 1"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary missing %q:\n%s", want, out)
		}
	}

	t.Setenv("BENCH_PUBLISH_FORCE", "")
	err := checkPublishIntegrity(resultsDir, doc)
	if err == nil || !strings.Contains(err.Error(), "BENCH_PUBLISH_FORCE=1") {
		t.Fatalf("gate must refuse with override hint, got %v", err)
	}

	t.Setenv("BENCH_PUBLISH_FORCE", "1")
	if err := checkPublishIntegrity(resultsDir, doc); err != nil {
		t.Fatalf("BENCH_PUBLISH_FORCE=1 must override: %v", err)
	}
}

// TestPublishIntegrityThresholdBoundary pins the >20% rule as strictly
// greater-than: exactly 1 non-ok cell in 5 (20%) passes, 2 in 5 fails.
// A single genuine capability gap must not block a small-grid publish.
func TestPublishIntegrityThresholdBoundary(t *testing.T) {
	resultsDir := t.TempDir() // no column dirs, no raw — Document census only

	pass := docWithCells(map[string]map[string]string{
		"gin-h1": {"a": "ok", "b": "ok", "c": "ok", "d": "ok", "e": "not_applicable"},
	})
	if v := collectPublishIntegrity(resultsDir, pass).violations(); len(v) != 0 {
		t.Errorf("20%% exactly must pass, got %v", v)
	}

	fail := docWithCells(map[string]map[string]string{
		"gin-h1": {"a": "ok", "b": "ok", "c": "ok", "d": "dnf", "e": "not_applicable"},
	})
	v := collectPublishIntegrity(resultsDir, fail).violations()
	if len(v) != 1 || !strings.Contains(v[0], "40% of cells are non-ok") {
		t.Errorf("40%% must fail the fraction rule, got %v", v)
	}
}

// TestPublishIntegritySuspectCountsOnce pins the census rule for suspect
// cells: present in both the headline map and CellStatuses, they count
// once, under the suspect bucket, and toward the non-ok fraction.
func TestPublishIntegritySuspectCountsOnce(t *testing.T) {
	doc := docWithCells(map[string]map[string]string{
		"celeris-epoll-h1-sync": {"churn-close": "suspect", "get-json": "ok"},
	})
	pi := collectPublishIntegrity(t.TempDir(), doc)
	if pi.TotalCells != 2 || pi.StatusCounts[report.CellSuspect] != 1 || pi.StatusCounts[report.CellOK] != 1 {
		t.Errorf("census: total=%d counts=%v", pi.TotalCells, pi.StatusCounts)
	}
}

// mustMkRaw creates resultsDir/raw and returns it.
func mustMkRaw(t *testing.T, resultsDir string) string {
	t.Helper()
	rawDir := filepath.Join(resultsDir, "raw")
	if err := os.MkdirAll(rawDir, 0o755); err != nil {
		t.Fatalf("mkdir raw: %v", err)
	}
	return rawDir
}
