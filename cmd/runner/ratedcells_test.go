package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
)

// realizedGrid returns the "<scenario>/<server>" id of every cell a
// `-cells '*/*'` bench schedules: the same filter and capability gate run()
// applies, with no TLS terminator (the cluster bench passes none).
func realizedGrid(t *testing.T) []string {
	t.Helper()
	scs, advs, err := filterCells(scenarios.Registry(), servers.AdaptersSorted(), "*/*")
	if err != nil {
		t.Fatalf("filterCells: %v", err)
	}
	srvs := make([]servers.Server, 0, len(advs))
	for _, a := range advs {
		srvs = append(srvs, &adapterServer{adapter: a, features: featureSetFor(a, false)})
	}
	var ids []string
	for _, c := range interleave.Schedule(1, scs, srvs) {
		ids = append(ids, c.Scenario.Name()+"/"+c.Server.Name())
	}
	return ids
}

// ratedArgs are the runner flags run_bench_cell.yml passes to a column of a
// rated BenchTier run of profile p.
func ratedArgs(p budget.Profile) []string {
	return []string{"-rated", "-rated-duration", p.RatedDuration.String(), "-rated-cells", budget.RatedGlob(p)}
}

// TestRatedSweepRunsOnlyOnTheModelsRatedCells is probatorium#418: the budget
// model plans the rated sweep on the RatedScenarios cells (388), but the runner
// rated every cell whose saturation pass came back clean (~770 of 813 in every
// published rated run), at 1.55-2x the projected wall clock. Given the flags a
// rated BenchTier column receives, exactly the model's rated cells may get the
// sweep, and every rated scenario must still get it somewhere.
func TestRatedSweepRunsOnlyOnTheModelsRatedCells(t *testing.T) {
	grid := realizedGrid(t)
	if len(grid) != budget.FullRealizedCells {
		t.Fatalf("the '*/*' grid realizes %d cells, budget.FullRealizedCells pins %d: re-pin the budget first", len(grid), budget.FullRealizedCells)
	}
	for _, p := range []budget.Profile{budget.HeadlineWeekly(), budget.Full()} {
		cfg, err := ParseArgs(ratedArgs(p), io.Discard)
		if err != nil {
			t.Fatalf("%s: ParseArgs(%q): %v", p.Name, ratedArgs(p), err)
		}
		rated := 0
		got := map[string]bool{}
		for _, id := range grid {
			if !cfg.ratesCell(id) {
				continue
			}
			rated++
			sc, _, _ := strings.Cut(id, "/")
			got[sc] = true
		}
		if rated != p.RatedCells {
			t.Errorf("%s: the runner rates %d of the %d cells; the budget model (FitWithin) plans the rated sweep on %d",
				p.Name, rated, len(grid), p.RatedCells)
		}
		var outside []string
		for sc := range got {
			if !slices.Contains(budget.RatedScenarios, sc) {
				outside = append(outside, sc)
			}
		}
		if len(outside) > 0 {
			slices.Sort(outside)
			t.Errorf("%s: %d scenarios outside budget.RatedScenarios get the rated sweep: %s",
				p.Name, len(outside), strings.Join(outside, " "))
		}
		for _, sc := range budget.RatedScenarios {
			if !got[sc] {
				t.Errorf("%s: rated scenario %s gets no rated cell", p.Name, sc)
			}
		}
	}
}

// TestExecuteCellRatesOnlyTheRatedCells drives the real executeCell against a
// loopback server, as a cluster column does (remote-target mode), with the
// flags a rated BenchTier column receives. The in-scope cell (get-json) must
// come back with its rated sweep, the out-of-scope one (get-simple) without:
// no RatedSamples on the outcome and no rated_passes or saturation_mode_rps in
// the per-cell JSON the cluster merge reads.
func TestExecuteCellRatesOnlyTheRatedCells(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"message":"ok"}`))
	}))
	defer srv.Close()

	cfg, err := ParseArgs(append(ratedArgs(budget.Full()),
		"-target", srv.URL, "-server-name", "e2e-remote",
		"-duration", "300ms", "-warmup", "100ms", "-rated-duration", "150ms",
		"-rated-fractions", "0.5", "-out", t.TempDir()), io.Discard)
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	server := &adapterServer{adapter: servers.Adapter{Name: "e2e-remote", Category: "remote"}, features: remoteFeatureSet()}
	for sc, wantRated := range map[string]bool{"get-json": true, "get-simple": false} {
		cell := interleave.Cell{Scenario: scenarioByName(t, sc), Server: server}
		oc, err := executeCell(context.Background(), cfg, cell)
		if err != nil || oc.Status != report.CellOK {
			t.Fatalf("%s: executeCell: status %q err %v", sc, oc.Status, err)
		}
		if got := len(oc.RatedSamples) > 0; got != wantRated {
			t.Errorf("%s: rated sweep ran = %v (%d samples), want %v", sc, got, len(oc.RatedSamples), wantRated)
		}
		raw, err := os.ReadFile(filepath.Join(cfg.Out, "run0", sc, "e2e-remote.json"))
		if err != nil {
			t.Fatalf("%s: per-cell JSON: %v", sc, err)
		}
		var cf cellResultFile
		if err := json.Unmarshal(raw, &cf); err != nil {
			t.Fatalf("%s: decode per-cell JSON: %v", sc, err)
		}
		if got := len(cf.RatedPasses) > 0; got != wantRated {
			t.Errorf("%s: per-cell JSON has %d rated_passes, want rated=%v", sc, len(cf.RatedPasses), wantRated)
		}
		if got := cf.SaturationModeRPS > 0; got != wantRated {
			t.Errorf("%s: per-cell saturation_mode_rps = %v, want set=%v", sc, cf.SaturationModeRPS, wantRated)
		}
	}
}
