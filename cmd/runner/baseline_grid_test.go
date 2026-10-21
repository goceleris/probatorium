package main

import (
	"io"
	"slices"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/servers"
)

// baselineTwin maps a baseline grid id to its main-celeris twin's id.
func baselineTwin(id string) (string, bool) {
	sc, srv, _ := strings.Cut(id, "/")
	e, ok := strings.CutPrefix(srv, servers.BaselinePrefix)
	if !ok {
		return "", false
	}
	return sc + "/celeris-" + e, true
}

// TestBaselineIsOffByDefaultInTheGrid: with BENCH_BASELINE unset the '*/*'
// dry-run grid has no baseline column and is exactly the pinned 813 cells
// (the routine runs' cost and published set do not change).
func TestBaselineIsOffByDefaultInTheGrid(t *testing.T) {
	t.Setenv(servers.BaselineEnvVar, "")
	grid := realizedGrid(t)
	if len(grid) != budget.FullRealizedCells {
		t.Fatalf("default grid = %d cells, want the pinned %d", len(grid), budget.FullRealizedCells)
	}
	for _, id := range grid {
		if strings.Contains(id, servers.BaselinePrefix) {
			t.Fatalf("baseline cell %s in the default grid", id)
		}
	}
}

// TestBaselineGridPins: with BENCH_BASELINE=1 the dry-run grid lists the
// baseline columns (eight, never celeris-std-h1), adds exactly the pinned
// budget.BaselineRealizedCells cells / BaselineRatedRealizedCells rated
// cells, and every baseline column realizes the same scenarios as its main
// twin (same Engine and Capabilities, so featureSetFor gates the same set).
func TestBaselineGridPins(t *testing.T) {
	t.Setenv(servers.BaselineEnvVar, "1")
	grid := realizedGrid(t)

	var base []string
	cols := map[string]bool{}
	for _, id := range grid {
		if _, ok := baselineTwin(id); ok {
			base = append(base, id)
			_, srv, _ := strings.Cut(id, "/")
			cols[srv] = true
		}
	}
	if want := servers.BaselineNames(); len(cols) != len(want) {
		t.Errorf("baseline columns in the grid = %d (%v), want the %d registered (%v)", len(cols), cols, len(want), want)
	}
	if len(base) != budget.BaselineRealizedCells {
		t.Errorf("baseline cells = %d, budget.BaselineRealizedCells pins %d: re-pin the budget (and the benchmark-tier.yml baseline budgets)", len(base), budget.BaselineRealizedCells)
	}
	if len(grid) != budget.FullRealizedCells+budget.BaselineRealizedCells {
		t.Errorf("grid with the baseline = %d, want %d + %d", len(grid), budget.FullRealizedCells, budget.BaselineRealizedCells)
	}
	inGrid := map[string]bool{}
	for _, id := range grid {
		inGrid[id] = true
	}
	for _, id := range base {
		tw, _ := baselineTwin(id)
		if !inGrid[tw] {
			t.Errorf("%s has no main twin cell %s: the baseline must not run a scenario its twin does not", id, tw)
		}
	}
	for _, id := range grid {
		if sc, srv, _ := strings.Cut(id, "/"); strings.HasPrefix(srv, "celeris-") && !strings.HasPrefix(srv, servers.BaselinePrefix) && srv != "celeris-std-h1" {
			if !inGrid[sc+"/"+servers.BaselinePrefix+strings.TrimPrefix(srv, "celeris-")] {
				t.Errorf("main cell %s has no baseline twin", id)
			}
		}
	}

	p := budget.Full()
	cfg, err := ParseArgs(ratedArgs(p), io.Discard)
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	rated, ratedBase := 0, 0
	for _, id := range grid {
		if !cfg.ratesCell(id) {
			continue
		}
		rated++
		if _, ok := baselineTwin(id); ok {
			ratedBase++
		}
	}
	if ratedBase != budget.BaselineRatedRealizedCells {
		t.Errorf("rated baseline cells = %d, budget.BaselineRatedRealizedCells pins %d", ratedBase, budget.BaselineRatedRealizedCells)
	}
	if rated != budget.FullRatedRealizedCells+budget.BaselineRatedRealizedCells {
		t.Errorf("rated cells with the baseline = %d, want %d + %d", rated, budget.FullRatedRealizedCells, budget.BaselineRatedRealizedCells)
	}
	want := p.WithBaseline()
	if want.Cells != len(grid) || want.RatedCells != rated {
		t.Errorf("WithBaseline projects %d/%d cells, the registry realizes %d/%d", want.Cells, want.RatedCells, len(grid), rated)
	}
	if !slices.IsSorted(servers.BaselineNames()) {
		t.Error("BaselineNames not sorted")
	}
}

// TestBaselineRunnerMetaCarriesTheTagVersion: the runner's per-cell server meta
// (serverMetaFromRegistry) stamps the main celeris build's version only on
// Framework=="celeris" columns; a baseline column keeps v1.5.11.
func TestBaselineRunnerMetaCarriesTheTagVersion(t *testing.T) {
	meta := serverMetaFromRegistry("arm64")
	for _, n := range servers.BaselineNames() {
		m, ok := meta[n]
		if !ok || m.FrameworkVersion != "v1.5.11" || m.Framework == "celeris" {
			t.Errorf("%s: meta %+v", n, m)
		}
	}
}
