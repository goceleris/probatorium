package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/gov-ab-20261122, not for merge). The registry-carried rated plan (ratedplan.go): resolution, refusals, the manifest.

func fakePlanned(t *testing.T, name string, plans map[string]servers.RatedPlan) {
	t.Helper()
	if _, dup := servers.Registry[name]; dup {
		t.Fatalf("%s is already a column", name)
	}
	servers.Registry[name] = servers.Adapter{Name: name, Category: "remote", RatedPlans: plans}
	t.Cleanup(func() { delete(servers.Registry, name) })
}

func plannedCell(t *testing.T, scenario, column string) interleave.Cell {
	t.Helper()
	return interleave.Cell{Scenario: scenarioByName(t, scenario), Server: &adapterServer{adapter: servers.Adapter{Name: column, Category: "remote"}, features: remoteFeatureSet()}}
}

func ratedCfg(cells string) Config {
	c := DefaultConfig()
	c.RatedMode = true
	c.RatedCells = cells
	c.Target = "http://127.0.0.1:8080"
	return c
}

var okPlan = servers.RatedPlan{Fractions: []float64{0.25, 0.9}}

func TestResolveRatedPlanAppliesTheColumnPlan(t *testing.T) {
	fakePlanned(t, "zzplan-1", map[string]servers.RatedPlan{"get-json": okPlan, "get-simple": {Fractions: []float64{0.5}}})
	cfg := ratedCfg("*/zzplan-*")
	var logb bytes.Buffer
	if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json", "zzplan-1")}, &logb); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RatedFractions, []float64{0.25, 0.9}) || cfg.RatedPlanFrom != "registry:zzplan-1:get-json" {
		t.Errorf("cfg after resolve: %v %q", cfg.RatedFractions, cfg.RatedPlanFrom)
	}
	if !strings.Contains(logb.String(), "rated plan from the registry column zzplan-1, scenario get-json: fractions=[0.25 0.9] passes=2") {
		t.Errorf("log %q", logb.String())
	}
	cfg.Out = t.TempDir() // the manifest is written from this cfg: it must say what ran
	if err := writeManifest(cfg, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.Out, "manifest.json"))
	if err != nil {
		t.Fatal(err)
	}
	var m struct {
		Config map[string]any `json:"config"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if fr, _ := m.Config["RatedFractions"].([]any); !slices.Equal(fr, []any{0.25, 0.9}) || m.Config["RatedPlanFrom"] != "registry:zzplan-1:get-json" {
		t.Errorf("manifest %v %v", m.Config["RatedFractions"], m.Config["RatedPlanFrom"])
	}
}

// Every column without a plan behaves as before: nothing is rewritten.
func TestResolveRatedPlanLeavesOtherColumnsAlone(t *testing.T) {
	cfg := ratedCfg("*")
	cells := []interleave.Cell{plannedCell(t, "get-json", "celeris-epoll-h1-sync"), plannedCell(t, "get-json", "some-ad-hoc-slug")}
	if err := resolveRatedPlan(&cfg, cells, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RatedFractions, defaultRatedFractions) || cfg.RatedPlanFrom != "" {
		t.Errorf("an unplanned column was changed: %v %q", cfg.RatedFractions, cfg.RatedPlanFrom)
	}
	fakePlanned(t, "zzplan-3", map[string]servers.RatedPlan{"get-json": okPlan}) // and so does a planned column when rated mode is off
	off := ratedCfg("*")
	off.RatedMode = false
	if err := resolveRatedPlan(&off, []interleave.Cell{plannedCell(t, "get-json", "zzplan-3")}, &bytes.Buffer{}); err != nil || !slices.Equal(off.RatedFractions, defaultRatedFractions) {
		t.Errorf("rated mode off: err %v fractions %v", err, off.RatedFractions)
	}
}

func TestResolveRatedPlanRefusals(t *testing.T) {
	fakePlanned(t, "zzplan-4", map[string]servers.RatedPlan{"get-json": okPlan})
	fakePlanned(t, "zzplan-5", map[string]servers.RatedPlan{"get-json": okPlan})
	fakePlanned(t, "zzplan-6", map[string]servers.RatedPlan{"get-json": {Fractions: []float64{0.1, 0.2, 0.3, 0.4, 0.5}}})
	fakePlanned(t, "zzplan-7", map[string]servers.RatedPlan{"get-json": {Fractions: []float64{1.5}}})
	fakePlanned(t, "zzplan-8", map[string]servers.RatedPlan{"get-json": {Fractions: []float64{0}}})
	fakePlanned(t, "zzplan-9", map[string]servers.RatedPlan{"get-json": {}})
	if len(slices.Repeat([]float64{0}, budget.DefaultRatedPasses+1)) != 5 {
		t.Fatal("the too-many-passes plan above must exceed budget.DefaultRatedPasses")
	}
	for _, tc := range []struct {
		name   string
		cfg    Config
		cells  []interleave.Cell
		errSub string
	}{
		{"scenario without a plan entry", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-simple", "zzplan-4")}, "has no plan entry"},
		{"two rated scenarios", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-4"), plannedCell(t, "get-simple", "zzplan-4")}, "exactly one"},
		{"cell outside rated-cells", ratedCfg("get-simple/*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-4")}, "not in -rated-cells"},
		{"two planned columns", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-4"), plannedCell(t, "get-json", "zzplan-5")}, "2 planned columns"},
		{"shares the run with another rated column", ratedCfg("*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-4"), plannedCell(t, "get-json", "celeris-epoll-h1-sync")}, "shares the run"},
		{"too many passes", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-6")}, "hang guard"},
		{"fraction above 1", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-7")}, "not in (0,1]"},
		{"fraction zero", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-8")}, "not in (0,1]"},
		{"empty plan", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-9")}, "no pass"},
		{"a GOVAB column with no plan in the registry", ratedCfg("*"), []interleave.Cell{plannedCell(t, "ws-echo", "gabw-nonexistent")}, "NO rated plan"},
		{"a GOVAB large column with no plan", ratedCfg("*"), []interleave.Cell{plannedCell(t, "ws-large-echo", "gabl-nonexistent")}, "NO rated plan"},
	} {
		cfg := tc.cfg
		err := resolveRatedPlan(&cfg, tc.cells, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.errSub) {
			t.Errorf("%s: err %v, want one containing %q", tc.name, err, tc.errSub)
		}
	}
}

// A local dry run (no -target) is information only: it does not fail on a planned column, it says so; a real local run of a planned column fails.
func TestResolveRatedPlanLocalDryRunIsInformationOnly(t *testing.T) {
	fakePlanned(t, "zzplan-12", map[string]servers.RatedPlan{"get-json": okPlan})
	cfg := ratedCfg("*")
	cfg.Target, cfg.DryRun = "", true
	var logb bytes.Buffer
	cells := []interleave.Cell{plannedCell(t, "get-json", "zzplan-12"), plannedCell(t, "get-json", "celeris-epoll-h1-sync")}
	if err := resolveRatedPlan(&cfg, cells, &logb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logb.String(), "local dry run") || !slices.Equal(cfg.RatedFractions, defaultRatedFractions) {
		t.Errorf("log %q fractions %v", logb.String(), cfg.RatedFractions)
	}
	cfg.DryRun = false
	if err := resolveRatedPlan(&cfg, cells, &logb); err == nil {
		t.Error("a real local run of a planned column must fail")
	}
}

// The real GOVAB columns resolve to exactly their plan, on the scenario they run.
func TestResolveRatedPlanTheGovabColumns(t *testing.T) {
	for _, tc := range []struct {
		scenario, column string
		want             []float64
	}{
		{"ws-echo", "gabw-std-h1", []float64{0.25, 0.9}},
		{"ws-echo", "gabw-adaptive-h1-async", []float64{0.25, 0.9}},
		{"ws-echo", "gabw-epoll-h1-async", []float64{0.25, 0.9}},
		{"ws-large-echo", "gabl-std-h1", []float64{0.5, 0.9}},
		{"ws-large-echo", "gabl-adaptive-h1-async", []float64{0.5, 0.9}},
	} {
		cfg := ratedCfg(tc.scenario + "/" + tc.column)
		if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, tc.scenario, tc.column)}, &bytes.Buffer{}); err != nil {
			t.Errorf("%s: %v", tc.column, err)
			continue
		}
		if !slices.Equal(cfg.RatedFractions, tc.want) || cfg.RatedPlanFrom != "registry:"+tc.column+":"+tc.scenario {
			t.Errorf("%s: fractions %v from %q", tc.column, cfg.RatedFractions, cfg.RatedPlanFrom)
		}
		// the same column on the other scenario is refused: it has no plan entry for it
		other := "ws-large-echo"
		if tc.scenario == "ws-large-echo" {
			other = "ws-echo"
		}
		cfg = ratedCfg(other + "/" + tc.column)
		if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, other, tc.column)}, &bytes.Buffer{}); err == nil {
			t.Errorf("%s on %s must be refused", tc.column, other)
		}
	}
}
