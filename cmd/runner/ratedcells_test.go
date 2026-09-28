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

// ratedRows is the rated scope the maintainer chose for probatorium#418
// (option B, 2026-09-28): the 14 rows first rated by the fix (the static
// headline rows, churn-close and the 11 driver rows) plus ws-echo. It is
// spelled out here, not read from budget.RatedScenarios, so that a change to
// the rated scope fails a test that names the decision.
var ratedRows = []string{
	"get-json", "post-4k", "churn-close",
	"driver-pg-read", "driver-pg-write", "driver-pg-update-tx", "driver-pg-read-range",
	"driver-redis-get", "driver-redis-set", "driver-redis-pipeline",
	"driver-mc-get", "driver-mc-set", "driver-mc-multiget",
	"driver-session-rw",
	"ws-echo",
}

// TestRatedScopeIsTheFourteenRowsAndWSEcho is option B of probatorium#418.
// ws-echo is the one row outside the 14 whose rated number measures the
// server, and no rated row repeats it: the celeris loop engines' WebSocket
// echo tail at light load (celeris#755), which saturation hides. Given the
// flags a rated BenchTier column receives, the runner must rate every cell of
// the 15 rows in the '*/*' grid and no other cell. ws-echo must be
// capability-gated exactly as in the saturation grid: rated on every column
// that declares WebSocket over HTTP/1.1, and on no other column.
func TestRatedScopeIsTheFourteenRowsAndWSEcho(t *testing.T) {
	grid := realizedGrid(t)
	var gridWS []string // the columns the saturation grid runs ws-echo on
	for _, id := range grid {
		if sc, srv, _ := strings.Cut(id, "/"); sc == "ws-echo" {
			gridWS = append(gridWS, srv)
		}
	}
	slices.Sort(gridWS)
	var wsCols []string // the columns that declare WebSocket over HTTP/1.1
	for _, a := range servers.AdaptersSorted() {
		if fs := featureSetFor(a, false); a.Capabilities.WS && fs.HTTP1 {
			wsCols = append(wsCols, a.Name)
		}
	}
	slices.Sort(wsCols)
	if len(gridWS) == 0 || !slices.Equal(gridWS, wsCols) {
		t.Fatalf("the '*/*' grid runs ws-echo on %d columns %q; %d columns declare WebSocket over HTTP/1.1 %q",
			len(gridWS), gridWS, len(wsCols), wsCols)
	}

	for _, p := range []budget.Profile{budget.HeadlineWeekly(), budget.Full()} {
		cfg, err := ParseArgs(ratedArgs(p), io.Discard)
		if err != nil {
			t.Fatalf("%s: ParseArgs(%q): %v", p.Name, ratedArgs(p), err)
		}
		var ratedWS, missing, extra []string
		want := 0
		for _, id := range grid {
			sc, srv, _ := strings.Cut(id, "/")
			in := slices.Contains(ratedRows, sc)
			if in {
				want++
			}
			switch rated := cfg.ratesCell(id); {
			case rated && !in:
				extra = append(extra, id)
			case !rated && in:
				missing = append(missing, id)
			case rated && sc == "ws-echo":
				ratedWS = append(ratedWS, srv)
			}
		}
		slices.Sort(ratedWS)
		if !slices.Equal(ratedWS, gridWS) {
			t.Errorf("%s: ws-echo is rated on %d of the %d columns that serve it: rated %q, want %q",
				p.Name, len(ratedWS), len(gridWS), ratedWS, gridWS)
		}
		if len(missing) > 0 {
			t.Errorf("%s: %d of the %d cells of the 14 rows and ws-echo get no rated sweep: %s",
				p.Name, len(missing), want, strings.Join(missing, " "))
		}
		if len(extra) > 0 {
			t.Errorf("%s: %d cells outside the 14 rows and ws-echo get the rated sweep: %s",
				p.Name, len(extra), strings.Join(extra, " "))
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

// TestRatedCellsScope pins the rest of -rated-cells: rated mode off rates
// nothing whatever the glob; an empty glob rates every cell (what an ad-hoc
// `runner -rated` always did); "!" exclusions work as in -cells; and a
// malformed glob fails the run before any cell instead of silently rating
// nothing.
func TestRatedCellsScope(t *testing.T) {
	grid := realizedGrid(t)
	count := func(cfg Config) int {
		n := 0
		for _, id := range grid {
			if cfg.ratesCell(id) {
				n++
			}
		}
		return n
	}
	if n := count(Config{RatedMode: false, RatedCells: "*"}); n != 0 {
		t.Errorf("rated mode off rates %d cells, want 0", n)
	}
	if n := count(Config{RatedMode: true}); n != len(grid) {
		t.Errorf("-rated with no -rated-cells rates %d of %d cells, want all of them", n, len(grid))
	}

	cfg := Config{RatedMode: true, RatedCells: "get-json/*, !get-json/celeris-*"}
	for id, want := range map[string]bool{
		"get-json/gin-h1":                true,
		"get-json/celeris-epoll-h1-sync": false,
		"post-4k/gin-h1":                 false,
	} {
		if got := cfg.ratesCell(id); got != want {
			t.Errorf("-rated-cells %q: ratesCell(%q) = %v, want %v", cfg.RatedCells, id, got, want)
		}
	}

	err := run(Config{Runs: 1, Services: "none", DryRun: true, RatedMode: true, RatedCells: "get-json/[", Cells: "nonexistent/*"})
	if err == nil || !strings.Contains(err.Error(), "-rated-cells") {
		t.Errorf("run with a malformed -rated-cells: err = %v, want a -rated-cells error", err)
	}
}
