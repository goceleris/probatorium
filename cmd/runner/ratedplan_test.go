package main

import (
	"bytes"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/p3a-rc1-20261110, not for merge). The registry-carried rated plan (ratedplan.go): resolution, refusals, the sweep with absolute passes, the manifest.

// fakePlanned registers a planned column for the test and removes it afterwards.
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

var okPlan = servers.RatedPlan{Fractions: []float64{0.5, 0.75}, AbsoluteRPS: []float64{16000, 24000}}

func TestResolveRatedPlanAppliesTheColumnPlan(t *testing.T) {
	fakePlanned(t, "zzplan-1", map[string]servers.RatedPlan{"get-json": okPlan, "get-simple": {AbsoluteRPS: []float64{5}}})
	cfg := ratedCfg("*/zzplan-*")
	var logb bytes.Buffer
	if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json", "zzplan-1")}, &logb); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RatedFractions, []float64{0.5, 0.75}) || !slices.Equal(cfg.RatedAbsoluteRPS, []float64{16000, 24000}) || cfg.RatedPlanFrom != "registry:zzplan-1:get-json" {
		t.Errorf("cfg after resolve: %v %v %q", cfg.RatedFractions, cfg.RatedAbsoluteRPS, cfg.RatedPlanFrom)
	}
	if got := len(cfg.ratedPasses()); got != 4 || got > budget.DefaultRatedPasses {
		t.Errorf("%d passes, want 4 (the hang guard budgets %d)", got, budget.DefaultRatedPasses)
	}
	if !strings.Contains(logb.String(), "rated plan from the registry column zzplan-1, scenario get-json: fractions=[0.5 0.75] absolute_rps=[16000 24000] passes=4") {
		t.Errorf("log %q", logb.String())
	}
	// the manifest is written from this cfg: it must say what ran
	cfg.Out = t.TempDir()
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
	if fr, _ := m.Config["RatedFractions"].([]any); !slices.Equal(fr, []any{0.5, 0.75}) {
		t.Errorf("manifest RatedFractions %v", m.Config["RatedFractions"])
	}
	if ab, _ := m.Config["RatedAbsoluteRPS"].([]any); !slices.Equal(ab, []any{16000.0, 24000.0}) || m.Config["RatedPlanFrom"] != "registry:zzplan-1:get-json" {
		t.Errorf("manifest %v %v", m.Config["RatedAbsoluteRPS"], m.Config["RatedPlanFrom"])
	}
}

// A control column: absolute passes only (an empty, non-nil fraction list; no default fraction survives).
func TestResolveRatedPlanAbsoluteOnly(t *testing.T) {
	fakePlanned(t, "zzplan-2", map[string]servers.RatedPlan{"get-json": {AbsoluteRPS: []float64{16000, 24000}}})
	cfg := ratedCfg("*/zzplan-*")
	if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json", "zzplan-2")}, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if len(cfg.RatedFractions) != 0 || len(cfg.ratedPasses()) != 2 {
		t.Errorf("fractions %v, passes %v: the default fractions must not survive", cfg.RatedFractions, cfg.ratedPasses())
	}
}

// Every column without a plan behaves as before: nothing is rewritten.
func TestResolveRatedPlanLeavesOtherColumnsAlone(t *testing.T) {
	cfg := ratedCfg("*")
	before := slices.Clone(cfg.RatedFractions)
	cells := []interleave.Cell{plannedCell(t, "get-json", "celeris-epoll-h1-sync"), plannedCell(t, "get-json", "some-ad-hoc-slug")}
	if err := resolveRatedPlan(&cfg, cells, &bytes.Buffer{}); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(cfg.RatedFractions, before) || !slices.Equal(cfg.RatedFractions, defaultRatedFractions) || cfg.RatedAbsoluteRPS != nil || cfg.RatedPlanFrom != "" {
		t.Errorf("an unplanned column was changed: %v %v %q", cfg.RatedFractions, cfg.RatedAbsoluteRPS, cfg.RatedPlanFrom)
	}
	// and so does a planned column when rated mode is off
	fakePlanned(t, "zzplan-3", map[string]servers.RatedPlan{"get-json": okPlan})
	off := ratedCfg("*")
	off.RatedMode = false
	if err := resolveRatedPlan(&off, []interleave.Cell{plannedCell(t, "get-json", "zzplan-3")}, &bytes.Buffer{}); err != nil || off.RatedAbsoluteRPS != nil {
		t.Errorf("rated mode off: err %v cfg %v", err, off.RatedAbsoluteRPS)
	}
}

func TestResolveRatedPlanRefusals(t *testing.T) {
	fakePlanned(t, "zzplan-4", map[string]servers.RatedPlan{"get-json": okPlan})
	fakePlanned(t, "zzplan-5", map[string]servers.RatedPlan{"get-json": okPlan})
	fakePlanned(t, "zzplan-6", map[string]servers.RatedPlan{"get-json": {Fractions: []float64{0.5}, AbsoluteRPS: []float64{1, 2, 3, 4}}})
	fakePlanned(t, "zzplan-7", map[string]servers.RatedPlan{"get-json": {Fractions: []float64{1.5}}})
	fakePlanned(t, "zzplan-8", map[string]servers.RatedPlan{"get-json": {AbsoluteRPS: []float64{0, 5}}})
	fakePlanned(t, "zzplan-9", map[string]servers.RatedPlan{"get-json": {}})
	fakePlanned(t, "zzplan-10", map[string]servers.RatedPlan{"get-json": {AbsoluteRPS: []float64{0, 0}, Placeholder: true}})
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
		{"fraction out of range", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-7")}, "not in (0,1]"},
		{"zero absolute rate without the placeholder flag", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-8")}, "positive finite"},
		{"empty plan", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-9")}, "no pass"},
		{"placeholder rates in a real run", ratedCfg("*/zzplan-*"), []interleave.Cell{plannedCell(t, "get-json", "zzplan-10")}, "PLACEHOLDERS"},
	} {
		cfg := tc.cfg
		err := resolveRatedPlan(&cfg, tc.cells, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), tc.errSub) {
			t.Errorf("%s: err %v, want one containing %q", tc.name, err, tc.errSub)
		}
	}
}

// -dry-run lists everything and warns; a real run of the same column refuses (checked above).
func TestResolveRatedPlanPlaceholderDryRunWarns(t *testing.T) {
	fakePlanned(t, "zzplan-11", map[string]servers.RatedPlan{"get-json": {Fractions: []float64{0.5}, AbsoluteRPS: []float64{0, 0}, Placeholder: true}})
	cfg := ratedCfg("*/zzplan-*")
	cfg.DryRun = true
	var logb bytes.Buffer
	if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json", "zzplan-11")}, &logb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logb.String(), "WARNING PLACEHOLDER") || len(cfg.ratedPasses()) != 3 {
		t.Errorf("log %q passes %v", logb.String(), cfg.ratedPasses())
	}
}

// A local dry run (no -target) is information only: it does not fail on a planned column, it says so.
func TestResolveRatedPlanLocalDryRunIsInformationOnly(t *testing.T) {
	fakePlanned(t, "zzplan-12", map[string]servers.RatedPlan{"get-json": okPlan})
	cfg := ratedCfg("*")
	cfg.Target, cfg.DryRun = "", true
	var logb bytes.Buffer
	if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json", "zzplan-12"), plannedCell(t, "get-json", "celeris-epoll-h1-sync")}, &logb); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(logb.String(), "local dry run") || cfg.RatedAbsoluteRPS != nil {
		t.Errorf("log %q abs %v", logb.String(), cfg.RatedAbsoluteRPS)
	}
	cfg.DryRun = false
	if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json", "zzplan-12"), plannedCell(t, "get-json", "celeris-epoll-h1-sync")}, &logb); err == nil {
		t.Error("a real local run of a planned column must fail")
	}
}

// The sweep: fractions first, then the absolute rates, each recorded with fraction = target / saturation.
func TestRatedSweepRunsFractionsThenAbsoluteRates(t *testing.T) {
	var rates []float64
	withRatedScript(t, func(_ int, cfg loadgen.Config) (ratedBench, error) {
		rates = append(rates, cfg.Rate)
		return scriptedBench{res: &loadgen.Result{Requests: 10, Duration: time.Second, RequestsPerSec: cfg.Rate, Latency: loadgen.Percentiles{P99: time.Millisecond}}}, nil
	})
	sw := runRatedSweep(context.Background(), Config{RatedFractions: []float64{0.5, 0.75}, RatedAbsoluteRPS: []float64{300, 900}, RatedDuration: time.Second, Warmup: time.Second}, loadgen.Config{}, 1000)
	if !slices.Equal(rates, []float64{500, 750, 300, 900}) {
		t.Fatalf("loadgen rates %v, want [500 750 300 900]", rates)
	}
	if len(sw.all) != 4 || len(sw.kept) != 4 || len(sw.windows) != 4 {
		t.Fatalf("records %d kept %d windows %d", len(sw.all), len(sw.kept), len(sw.windows))
	}
	for i, want := range []float64{0.5, 0.75, 0.3, 0.9} {
		if r := sw.all[i]; math.Abs(r.Fraction-want) > 1e-12 || math.Abs(r.TargetRPS-r.Fraction*1000) > 1e-9 {
			t.Errorf("pass %d: fraction %v target %v, want fraction %v and target = fraction x saturation", i, r.Fraction, r.TargetRPS, want)
		}
	}
}

// Without absolute rates the sweep is the one it always was (the same records as before the change).
func TestRatedSweepWithoutAbsoluteRatesIsUnchanged(t *testing.T) {
	withRatedScript(t, func(_ int, cfg loadgen.Config) (ratedBench, error) {
		return scriptedBench{res: &loadgen.Result{Requests: 10, Duration: time.Second, RequestsPerSec: cfg.Rate, Latency: loadgen.Percentiles{P99: time.Millisecond}}}, nil
	})
	sw := runRatedSweep(context.Background(), Config{RatedFractions: defaultRatedFractions, RatedDuration: time.Second, Warmup: time.Second}, loadgen.Config{}, 1000)
	for i, r := range sw.all {
		if r.Fraction != defaultRatedFractions[i] || r.TargetRPS != 1000*defaultRatedFractions[i] {
			t.Errorf("pass %d %+v", i, r)
		}
	}
}

// A zero absolute rate (the placeholder, if it ever got here) is recorded as a not_run bad target, never run.
func TestRatedSweepNeverRunsAZeroAbsoluteRate(t *testing.T) {
	calls := withRatedScript(t, func(_ int, cfg loadgen.Config) (ratedBench, error) {
		return scriptedBench{res: &loadgen.Result{Requests: 10, Duration: time.Second, RequestsPerSec: cfg.Rate}}, nil
	})
	sw := runRatedSweep(context.Background(), Config{RatedAbsoluteRPS: []float64{0, 500}, RatedDuration: time.Second, Warmup: time.Second}, loadgen.Config{}, 1000)
	if calls.Load() != 1 || len(sw.all) != 2 || sw.all[0].Status != report.RatedPassNotRun || sw.all[0].Reason != report.RatedReasonBadTarget {
		t.Errorf("calls %d records %+v", calls.Load(), sw.all)
	}
}

// A flagged cell records every planned pass (fractions and absolute) as not_run, the absolute ones with their known target.
func TestNotRunRatedPassesCoversAbsolutePasses(t *testing.T) {
	got := notRunRatedPasses(Config{RatedFractions: []float64{0.5, 0.75}, RatedAbsoluteRPS: []float64{16000, 24000}}, report.RatedReasonCellFlagged, "flagged")
	if len(got) != 4 {
		t.Fatalf("%d records, want 4", len(got))
	}
	for i, want := range []struct{ f, a float64 }{{0.5, 0}, {0.75, 0}, {0, 16000}, {0, 24000}} {
		if r := got[i]; r.Fraction != want.f || r.TargetRPS != want.a || r.Status != report.RatedPassNotRun || r.Reason != report.RatedReasonCellFlagged {
			t.Errorf("record %d = %+v", i, r)
		}
	}
}

func TestClientLockLimitsCountAbsolutePasses(t *testing.T) {
	a := Config{RatedMode: true, Warmup: time.Second, Duration: time.Second, RatedDuration: time.Second, RatedFractions: []float64{0.5, 0.75}}
	b := a
	b.RatedAbsoluteRPS = []float64{1, 2}
	ha, _ := a.clientLockLimits()
	hb, _ := b.clientLockLimits()
	if want := 2 * (5*time.Second + 2*time.Second + 60*time.Second); hb-ha != want {
		t.Errorf("two absolute passes add %s to the hold limit, want %s", hb-ha, want)
	}
}

// Every p3a column of the registry resolves, from its own scenario, to a valid plan that fits the hang guard, and the manifest of a real column run carries it.
func TestP3aColumnsResolveToValidPlans(t *testing.T) {
	n := 0
	for name, a := range servers.Registry {
		if !strings.HasPrefix(name, "p3a-") {
			continue
		}
		n++
		if strings.Contains(name, "-g0-") || strings.Contains(name, "-g2-") || strings.Contains(name, "-g4-") {
			continue // the guard columns carry no plan: TestP3aGuardColumnsAreSaturationOnly
		}
		if len(a.RatedPlans) != 1 {
			t.Errorf("%s: %d plans, want exactly one (a column runs one scenario)", name, len(a.RatedPlans))
			continue
		}
		for sc, p := range a.RatedPlans {
			cfg := ratedCfg("*/p3a-*")
			cfg.DryRun = true // the registry may be a placeholder build; a dry run resolves it with a warning
			if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, sc, name)}, &bytes.Buffer{}); err != nil {
				t.Errorf("%s / %s: %v", name, sc, err)
			}
			if got := len(cfg.ratedPasses()); got != len(p.Fractions)+len(p.AbsoluteRPS) || got > budget.DefaultRatedPasses || got == 0 {
				t.Errorf("%s: %d passes", name, got)
			}
		}
	}
	if n == 0 {
		t.Skip("no p3a column in the registry")
	}
}

// The guard columns (Opus r45 R6, G2: g0 / g2 / g4 on get-json / epoll-h1-sync) are saturation-only columns: an EXPLICIT empty registry plan entry (RatedPlan{SaturationOnly}), the guard
// glob is in `cells` and NOT in `rated_cells`, so under the dispatch's rated_cells the runner leaves the config alone, says so, and the cell is not rated (a saturation pass, zero rated records).
func TestP3aGuardColumnsAreSaturationOnly(t *testing.T) {
	n := 0
	for name, a := range servers.Registry {
		if !strings.HasPrefix(name, "p3a-") || !(strings.Contains(name, "-g0-") || strings.Contains(name, "-g2-") || strings.Contains(name, "-g4-")) {
			continue
		}
		n++
		if p, ok := a.RatedPlans["get-json"]; len(a.RatedPlans) != 1 || !ok || !p.SaturationOnly || validateRatedPlan(p) != nil {
			t.Errorf("%s: RatedPlans %v, want exactly the explicit saturation-only get-json entry", name, a.RatedPlans)
		}
		for _, d := range []string{"1", "2"} {
			ratedCells := "ws-large-echo/p3a-" + d + "-*-k?-epoll-h1-sync,ws-large-echo/p3a-" + d + "-*-ctl-std-h1,get-json-h2/p3a-" + d + "-*-k?-epoll-auto+upg-async,get-json-h2/p3a-" + d + "-*-ctl-hertz-h2"
			cfg := ratedCfg(ratedCells)
			cfg.DryRun = false // a real (remote-target) run
			before := slices.Clone(cfg.RatedFractions)
			var logb bytes.Buffer
			if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json", name)}, &logb); err != nil {
				t.Errorf("%s: resolveRatedPlan: %v (a guard column must not be refused)", name, err)
			}
			if !slices.Equal(cfg.RatedFractions, before) || cfg.RatedAbsoluteRPS != nil || cfg.RatedPlanFrom != "" {
				t.Errorf("%s: a saturation-only guard column rewrote the config: %v %v %q", name, cfg.RatedFractions, cfg.RatedAbsoluteRPS, cfg.RatedPlanFrom)
			}
			if !strings.Contains(logb.String(), "saturation-only column "+name+" (registry plan entry SaturationOnly): zero rated passes") {
				t.Errorf("%s: the run does not say it is saturation-only: %q", name, logb.String())
			}
			if cfg.ratesCell("get-json/" + name) {
				t.Errorf("%s: the guard cell is in rated_cells %q: it would get the default 0.25..0.9 sweep", name, ratedCells)
			}
		}
	}
	if n != 18 {
		t.Errorf("%d guard columns in the registry, want 18", n)
	}
}

// Loudly, not silently: the explicit saturation-only entry is the ONLY way a p3a column runs without rated passes. A p3a column with no plan entry at all is refused (it would run
// the default 0.25..0.9 sweep), in a real run and in a remote-target dry run; a saturation-only column with a cell in -rated-cells, with a mixed plan, or with a scenario that has no
// entry is refused too; a non-p3a column without a plan is unchanged (TestResolveRatedPlanLeavesOtherColumnsAlone).
func TestResolveRatedPlanP3aNoPlanFailsLoudly(t *testing.T) {
	fakePlanned(t, "p3a-zz-noplan", nil)
	if _, ok := servers.Registry["p3a-zz-noplan"]; !ok {
		t.Fatal("fixture not registered")
	}
	for _, dry := range []bool{false, true} {
		cfg := ratedCfg("*/p3a-*")
		cfg.DryRun = dry
		err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json", "p3a-zz-noplan")}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "has NO rated plan entry") {
			t.Errorf("dry=%v: a p3a column with no plan entry must be refused, got %v", dry, err)
		}
	}
	// the local cross-product dry run (no -target) is information only and does not refuse
	loc := ratedCfg("*/p3a-*")
	loc.Target, loc.DryRun = "", true
	if err := resolveRatedPlan(&loc, []interleave.Cell{plannedCell(t, "get-json", "p3a-zz-noplan")}, &bytes.Buffer{}); err != nil {
		t.Errorf("local dry run: %v", err)
	}
	// rated mode off: unchanged
	off := ratedCfg("*/p3a-*")
	off.RatedMode = false
	if err := resolveRatedPlan(&off, []interleave.Cell{plannedCell(t, "get-json", "p3a-zz-noplan")}, &bytes.Buffer{}); err != nil {
		t.Errorf("rated off: %v", err)
	}
}

func TestResolveRatedPlanSaturationOnlyRules(t *testing.T) {
	sat := map[string]servers.RatedPlan{"get-json": {SaturationOnly: true}}
	fakePlanned(t, "p3a-zz-sat", sat)
	cell := []interleave.Cell{plannedCell(t, "get-json", "p3a-zz-sat")}
	// ok: the cell is outside rated_cells; cfg untouched; the log says zero rated passes
	cfg := ratedCfg("ws-large-echo/p3a-*")
	var logb bytes.Buffer
	if err := resolveRatedPlan(&cfg, cell, &logb); err != nil || !strings.Contains(logb.String(), "zero rated passes") || cfg.RatedAbsoluteRPS != nil || cfg.RatedPlanFrom != "" || !slices.Equal(cfg.RatedFractions, defaultRatedFractions) {
		t.Errorf("saturation-only column: err %v log %q cfg %v %v %q", err, logb.String(), cfg.RatedFractions, cfg.RatedAbsoluteRPS, cfg.RatedPlanFrom)
	}
	// refused: its cell is in rated_cells
	in := ratedCfg("get-json/p3a-*")
	if err := resolveRatedPlan(&in, cell, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "default sweep") {
		t.Errorf("a saturation-only cell inside rated_cells must be refused, got %v", err)
	}
	// refused: another rated cell shares the run
	cfg2 := ratedCfg("ws-large-echo/other")
	two := append([]interleave.Cell{plannedCell(t, "ws-large-echo", "other")}, cell...)
	if err := resolveRatedPlan(&cfg2, two, &bytes.Buffer{}); err == nil {
		t.Error("a saturation-only column sharing the run with a rated cell must be refused")
	}
	// refused: a scenario without an entry
	cfg3 := ratedCfg("ws-large-echo/p3a-*")
	if err := resolveRatedPlan(&cfg3, []interleave.Cell{plannedCell(t, "get-simple", "p3a-zz-sat")}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "no plan entry") {
		t.Errorf("a saturation-only column on a scenario without an entry must be refused, got %v", err)
	}
	// refused: a mixed plan; and the plan itself must be empty
	fakePlanned(t, "p3a-zz-mixed", map[string]servers.RatedPlan{"get-json": {SaturationOnly: true}, "get-simple": okPlan})
	cfg4 := ratedCfg("ws-large-echo/p3a-*")
	if err := resolveRatedPlan(&cfg4, []interleave.Cell{plannedCell(t, "get-json", "p3a-zz-mixed")}, &bytes.Buffer{}); err == nil || !strings.Contains(err.Error(), "mixes") {
		t.Errorf("a mixed plan must be refused, got %v", err)
	}
	if validateRatedPlan(servers.RatedPlan{SaturationOnly: true, AbsoluteRPS: []float64{1}}) == nil || validateRatedPlan(servers.RatedPlan{SaturationOnly: true}) != nil {
		t.Error("validateRatedPlan: a saturation-only plan must hold no pass")
	}
}

// D757 (ab/d757-<date>): the same refusal for the d757- columns. Every d757 column carries an ABSOLUTE-ONLY plan (Fractions nil, two absolute rates); one with NO plan entry at all would silently
// run the default 0.25..0.9 sweep, so it is refused in a real run and in a remote-target dry run (never in the local cross-product dry run, never with rated mode off).
func TestResolveRatedPlanD757NoPlanFailsLoudly(t *testing.T) {
	fakePlanned(t, "d757-zz-noplan", nil)
	for _, dry := range []bool{false, true} {
		cfg := ratedCfg("*/d757-*")
		cfg.DryRun = dry
		err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json-h2", "d757-zz-noplan")}, &bytes.Buffer{})
		if err == nil || !strings.Contains(err.Error(), "has NO rated plan entry") {
			t.Errorf("dry=%v: a d757 column with no plan entry must be refused, got %v", dry, err)
		}
	}
	loc := ratedCfg("*/d757-*")
	loc.Target, loc.DryRun = "", true
	if err := resolveRatedPlan(&loc, []interleave.Cell{plannedCell(t, "get-json-h2", "d757-zz-noplan")}, &bytes.Buffer{}); err != nil {
		t.Errorf("local dry run: %v", err)
	}
	off := ratedCfg("*/d757-*")
	off.RatedMode = false
	if err := resolveRatedPlan(&off, []interleave.Cell{plannedCell(t, "get-json-h2", "d757-zz-noplan")}, &bytes.Buffer{}); err != nil {
		t.Errorf("rated off: %v", err)
	}
}

// An absolute-only celeris plan (Fractions nil) resolves like the hertz control of P3a did: the manifest carries RatedFractions [] and the two absolute rates.
func TestResolveRatedPlanD757AbsoluteOnlyCelerisColumn(t *testing.T) {
	fakePlanned(t, "d757-zz-abs", map[string]servers.RatedPlan{"get-json-h2": {AbsoluteRPS: []float64{300321, 450481}}})
	cfg := ratedCfg("get-json-h2/d757-*")
	var logb bytes.Buffer
	if err := resolveRatedPlan(&cfg, []interleave.Cell{plannedCell(t, "get-json-h2", "d757-zz-abs")}, &logb); err != nil {
		t.Fatal(err)
	}
	if len(cfg.RatedFractions) != 0 || !slices.Equal(cfg.RatedAbsoluteRPS, []float64{300321, 450481}) || cfg.RatedPlanFrom != "registry:d757-zz-abs:get-json-h2" {
		t.Errorf("cfg after resolve: %v %v %q", cfg.RatedFractions, cfg.RatedAbsoluteRPS, cfg.RatedPlanFrom)
	}
	if n := len(cfg.ratedPasses()); n != 2 {
		t.Errorf("%d passes, want 2 (the two absolute rates, no fraction pass)", n)
	}
}
