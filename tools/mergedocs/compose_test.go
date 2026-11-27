package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/servers"
)

func TestValidPairComposes(t *testing.T) {
	f := newFixture()
	res, err := f.compose(t)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, b := range res.Doc.Benchmarks {
		names = append(names, b.Name)
	}
	want := []string{"actix", "celeris-a", "celeris-b", "echo-h1", "fiber-h1", "gin-h1", "gnet-h1", "h2o", "lithium", "stdhttp-h1"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("columns %v want %v (twins stripped by default, anchors once, sorted)", names, want)
	}
	if res.Comp.Columns["gnet-h1"] != "candidate" || res.Comp.Columns["gin-h1"] != "peers" {
		t.Fatalf("columns map %v", res.Comp.Columns)
	}
	if res.Comp.Twin.Mode != "separate" || res.Twin == nil || len(res.Comp.Twin.Columns) != 2 {
		t.Fatalf("twin default must be separate: %+v", res.Comp.Twin)
	}
	// anchors: peers is 1% faster everywhere, so drift ~0.0101; the floor wins on every scenario
	if res.Comp.MaxAnchorDrift <= 0.0099 || res.Comp.MaxAnchorDrift >= 0.0102 || len(res.Comp.Anchors.Names) != 6 {
		t.Fatalf("drift %v", res.Comp.MaxAnchorDrift)
	}
	if !reflect.DeepEqual(res.Comp.TieMarginByScenario, map[string]float64{"s1": 0.05, "s2": 0.05}) || res.Comp.Reboot.BetweenDispatches {
		t.Fatalf("margins %v reboot %+v", res.Comp.TieMarginByScenario, res.Comp.Reboot)
	}
	if res.Comp.Anchors.DriftWarning {
		t.Fatal("no loud warning at 1%")
	}
	if res.Comp.RefusalsSHA256 != refusalsSHA256() || len(res.Comp.RefusalsSHA256) != 64 {
		t.Fatal("refusals hash")
	}
	// the anchor ships from the candidate dispatch
	for _, b := range res.Doc.Benchmarks {
		if b.Name == "gnet-h1" && b.SaturationModeRPS["s1"] != 1000 {
			t.Fatalf("anchor row must be the candidate's: %v", b.SaturationModeRPS)
		}
	}
	// started = min, finished = max
	bc := res.Doc.BenchmarkConfig
	if !bc.StartedAt.Equal(t0) || !bc.FinishedAt.Equal(t0.Add(33*time.Hour+30*time.Minute)) {
		t.Fatalf("window %v..%v", bc.StartedAt, bc.FinishedAt)
	}
	if res.Comp.GapHours < 32 || res.Comp.GapHours > 33 {
		t.Fatalf("gap %v", res.Comp.GapHours)
	}
	if res.Comp.Publish.Date != "20261122" || res.Comp.Publish.RunID != "run-1" {
		t.Fatal(res.Comp.Publish.Date)
	}
}

func TestTwinModes(t *testing.T) {
	for _, mode := range []string{"separate", "strip", "keep"} {
		f := newFixture()
		f.Opt.Twin = mode
		res, err := f.compose(t)
		if err != nil {
			t.Fatal(mode, err)
		}
		n := 0
		for _, b := range res.Doc.Benchmarks {
			if isTwin(&b) {
				n++
			}
		}
		switch mode {
		case "keep":
			if n != 2 || res.Twin != nil {
				t.Fatalf("keep: %d twins, sidecar %v", n, res.Twin)
			}
			// twin series stay in the timeseries
			if !hasSeries(res.TS, twinPrefix+"a") {
				t.Fatal("keep must keep the twin series")
			}
		case "strip":
			if n != 0 || res.Twin != nil || hasSeries(res.TS, twinPrefix+"a") {
				t.Fatalf("strip: %d twins, sidecar %v", n, res.Twin)
			}
		case "separate":
			if n != 0 || res.Twin == nil || hasSeries(res.TS, twinPrefix+"a") {
				t.Fatalf("separate: %d twins", n)
			}
			if len(res.Twin.Rows.Benchmarks) != 2 || len(res.Twin.Rows.Series) != 4 {
				t.Fatalf("lossless sidecar: %d rows %d series", len(res.Twin.Rows.Benchmarks), len(res.Twin.Rows.Series))
			}
			if len(res.Twin.Comparison.Rows) != 2 || len(res.Twin.Comparison.Unmatched) != 0 {
				t.Fatalf("comparison %+v", res.Twin.Comparison)
			}
			r := res.Twin.Comparison.Rows[0].Scenarios["s1"]
			if r.Ratio == nil || *r.Ratio != 1 {
				t.Fatalf("ratio %+v", r)
			}
		}
	}
	f := newFixture()
	f.Opt.Twin = "bogus"
	if _, err := f.compose(t); err == nil || strings.Contains(err.Error(), "REFUSE") {
		t.Fatalf("a bad --twin is a usage error, got %v", err)
	}
}

func hasSeries(ts *report.TimeseriesDoc, server string) bool {
	for _, s := range ts.Scenarios {
		if s.Server == server {
			return true
		}
	}
	return false
}

func TestTwinPrefixMatchesRegistry(t *testing.T) {
	if twinPrefix != servers.BaselinePrefix {
		t.Fatalf("twinPrefix %q != servers.BaselinePrefix %q", twinPrefix, servers.BaselinePrefix)
	}
	if twinCategory != "celeris-baseline" {
		t.Fatal("category")
	}
}

func TestAnchorDriftAndWarning(t *testing.T) {
	f := newFixture()
	// stdhttp-h1 is 20% slower in the peers dispatch on s1 only (d1/d2 = 1.2376); fiber 10% faster on s1 and s2
	for i := range f.P.Benchmarks {
		switch f.P.Benchmarks[i].Name {
		case "stdhttp-h1":
			f.P.Benchmarks[i].SaturationModeRPS["s1"] *= 0.8
		case "fiber-h1":
			for k := range f.P.Benchmarks[i].SaturationModeRPS {
				f.P.Benchmarks[i].SaturationModeRPS[k] *= 1.1
			}
		}
	}
	res, err := f.compose(t)
	if err != nil {
		t.Fatal(err)
	}
	a := res.Comp.Anchors
	if !a.DriftWarning || a.MaxDriftAnchor != "stdhttp-h1" {
		t.Fatalf("%+v", a)
	}
	if a.MaxAnchorDrift < 0.237 || a.MaxAnchorDrift > 0.238 {
		t.Fatalf("max %v", a.MaxAnchorDrift)
	}
	// per scenario: the noisy scenario s1 takes the big margin, the quiet s2 only fiber's 11.1%
	s1, s2 := a.ByScenario["s1"], a.ByScenario["s2"]
	if s1.Margin != s1.MaxDrift || s1.DrivenBy != "stdhttp-h1" || s1.Margin < 0.237 || s1.Margin > 0.238 {
		t.Fatalf("s1 %+v", s1)
	}
	if s2.DrivenBy != "fiber-h1" || s2.Margin < 0.11 || s2.Margin > 0.112 {
		t.Fatalf("s2 %+v", s2)
	}
	if res.Comp.TieMarginByScenario["s1"] != s1.Margin {
		t.Fatal("composition margin map")
	}
	found := false
	for _, w := range res.Comp.Warnings {
		if strings.HasPrefix(w, "LOUD: anchor stdhttp-h1") {
			found = true
		}
	}
	if !found {
		t.Fatalf("no loud warning in %v", res.Comp.Warnings)
	}
	for _, p := range a.Per {
		if p.Name == "fiber-h1" && (p.MaxDrift < 0.11 || p.MaxDrift > 0.112) {
			t.Fatalf("fiber %v", p.MaxDrift)
		}
		if len(p.Metrics["saturation_mode_rps"]) != 2 || len(p.Metrics["latency_at_slo"]) != 4 || len(p.Metrics["loadgen_cpu_p95"]) != 2 {
			t.Fatalf("%s metrics %v", p.Name, p.Metrics)
		}
	}
}

// A quiet scenario's margin is not decided by a noisy one, and a scenario no
// anchor covers takes the widest margin (flagged), not the floor.
func TestPerScenarioMarginAndNoAnchorScenario(t *testing.T) {
	f := newFixture()
	for _, d := range []*report.Document{f.C, f.P} {
		for i := range d.Benchmarks {
			d.Benchmarks[i].SaturationModeRPS["s3"] = 500 // s3: measured by everyone...
		}
	}
	for i := range f.P.Benchmarks { // ...but every peers anchor lacks s3 ok
		if n := f.P.Benchmarks[i].Name; n == "gnet-h1" || n == "fiber-h1" || n == "stdhttp-h1" || n == "lithium" || n == "h2o" || n == "actix" {
			f.P.Benchmarks[i].CellStatuses = map[string]string{"s3": "dnf"}
		}
		if f.P.Benchmarks[i].Name == "gnet-h1" {
			f.P.Benchmarks[i].SaturationModeRPS["s1"] *= 0.9 // 10.0% on s1
		}
	}
	res, err := f.compose(t)
	if err != nil {
		t.Fatal(err)
	}
	m := res.Comp.Anchors.ByScenario
	if m["s2"].Margin != 0.05 || m["s1"].Margin < 0.10 || m["s1"].Margin > 0.101 {
		t.Fatalf("s1 %+v s2 %+v", m["s1"], m["s2"])
	}
	if !m["s3"].NoAnchor || m["s3"].Margin != m["s1"].Margin {
		t.Fatalf("s3 %+v", m["s3"])
	}
}

// R7 accepts a reboot only with the same kernel and command line and anchors
// within the floor on every scenario; the composition then says so.
func TestRebootAccepted(t *testing.T) {
	f := newFixture()
	f.PMeta["boot_id"] = "boot-2"
	res, err := f.compose(t)
	if err != nil {
		t.Fatal(err)
	}
	if !res.Comp.Reboot.BetweenDispatches || !strings.Contains(res.Comp.Reboot.Note, "rebooted between") {
		t.Fatalf("%+v", res.Comp.Reboot)
	}
	if res.Comp.Dispatches["peers"].BootID != "boot-2" || res.Comp.Dispatches["peers"].BootCmdline == "" {
		t.Fatalf("%+v", res.Comp.Dispatches["peers"])
	}
}

func TestAnchorNotOKCellIsRecordedNotSkipped(t *testing.T) {
	f := newFixture()
	for i := range f.P.Benchmarks {
		if f.P.Benchmarks[i].Name == "gnet-h1" {
			f.P.Benchmarks[i].CellStatuses = map[string]string{"s2": "dnf"}
		}
	}
	res, err := f.compose(t)
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range res.Comp.Anchors.Per {
		if p.Name == "gnet-h1" {
			if !reflect.DeepEqual(p.NotOK, []string{"s2"}) || p.Paired != 1 {
				t.Fatalf("%+v", p)
			}
		}
	}
	if len(res.Comp.Warnings) == 0 {
		t.Fatal("a non-ok anchor cell must be warned about")
	}
}

func TestWriteOutputLayout(t *testing.T) {
	f := newFixture()
	res, err := f.compose(t)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := WriteOutput(res, out); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"results.json", "timeseries.json.gz", "bench-vars.json", "COMPOSITION.json", "twin-comparison.json", "twin-rows.json",
		"raw/d1-msa2-server.json", "raw/d2-msa2-server.json",
		"20261122T060000-bench-msa2-server/00-celeris-a/results.json", "20261123T150000-bench-msa2-server/00-gin-h1/results.json"} {
		if _, err := os.Stat(filepath.Join(out, p)); err != nil {
			t.Errorf("missing %s", p)
		}
	}
	// a second write into the non-empty dir refuses
	if err := WriteOutput(res, out); err == nil {
		t.Fatal("overwriting a non-empty out dir must fail")
	}
	// the merged results.json re-decodes and publishes through the real WriteTree
	c2, err := LoadDispatch("candidate", out, "")
	if err != nil || c2.RoundTripErr != "" {
		t.Fatalf("%v %s", err, c2.RoundTripErr)
	}
	root := t.TempDir()
	meta := report.SplitMeta{Version: "v1.6.0", Arch: "x86_64", Date: "20261122", RunID: "run-1", GeneratedAt: t0}
	gz, _ := os.ReadFile(filepath.Join(out, "timeseries.json.gz"))
	if _, err := report.WriteTree(root, c2.Doc, gz, meta); err != nil {
		t.Fatal(err)
	}
}

func TestNoOverrideFlags(t *testing.T) {
	// the frozen list has no escape hatch: no flag name may suggest one
	b, _ := os.ReadFile("main.go")
	for _, w := range []string{"force", "ignore", "allow", "skip", "no-check"} {
		if strings.Contains(strings.ToLower(string(b)), `fs.string("`+w) || strings.Contains(strings.ToLower(string(b)), `fs.bool("`+w) {
			t.Fatalf("override-like flag %q", w)
		}
	}
}

// Study case C2b: a union must not lose histograms. Every kept column keeps all
// its histograms through WriteTree, and the sidecar has one key per column.
func TestNoHistogramOrSeriesLoss(t *testing.T) {
	f := newFixture()
	res, err := f.compose(t)
	if err != nil {
		t.Fatal(err)
	}
	gz, err := res.TS.MarshalGzip()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	cell, err := report.WriteTree(root, res.Doc, gz, report.SplitMeta{Version: "v", Arch: "x86_64", Date: "20261122", RunID: "run-1", GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	h := gunzip(t, filepath.Join(cell, "histograms.json.gz"))
	var hd report.HistogramDoc
	if err := json.Unmarshal(h, &hd); err != nil {
		t.Fatal(err)
	}
	if len(hd.Histograms) != len(res.Doc.Benchmarks) {
		t.Fatalf("%d histogram keys for %d columns", len(hd.Histograms), len(res.Doc.Benchmarks))
	}
	for _, b := range res.Doc.Benchmarks {
		if len(hd.Histograms[b.Name]) != len(b.HdrHistogramB64) || len(b.HdrHistogramB64) != 2 {
			t.Fatalf("%s: histograms lost", b.Name)
		}
	}
	// one series per (scenario, kept column)
	if len(res.TS.Scenarios) != 2*len(res.Doc.Benchmarks) {
		t.Fatalf("%d series for %d columns x 2 scenarios", len(res.TS.Scenarios), len(res.Doc.Benchmarks))
	}
	// the anchor series is the candidate's, once
	n := 0
	for _, s := range res.TS.Scenarios {
		if s.Server == "stdhttp-h1" && s.Scenario == "s1" {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("%d anchor series", n)
	}
}

// The published composition carries the drift figures, the per-scenario margins,
// the reboot note and the column map, and nothing local; `install` attaches it
// to the cell mage Publish will write (before it), and only for the same columns.
func TestPublicCompositionAndInstall(t *testing.T) {
	f := newFixture()
	f.PMeta["boot_id"] = "boot-2"
	res, err := f.compose(t)
	if err != nil {
		t.Fatal(err)
	}
	out := filepath.Join(t.TempDir(), "out")
	if err := WriteOutput(res, out); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(out, "publish-extras", "composition.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), f.CBench) || strings.Contains(string(b), `"dir"`) || strings.Contains(string(b), os.TempDir()) {
		t.Fatalf("a local path leaked into the published composition: %s", b)
	}
	var pc PublicComposition
	if err := json.Unmarshal(b, &pc); err != nil {
		t.Fatal(err)
	}
	if pc.Schema != "composition/1" || pc.Arch != "x86_64" || pc.PublishDate != "20261122" || !pc.Reboot.BetweenDispatches ||
		pc.TieMargin["s1"] != 0.05 || len(pc.Anchors) != 6 || pc.Columns["gin-h1"] != "peers" || pc.Columns["gnet-h1"] != "candidate" ||
		pc.RefusalsSHA256 != refusalsSHA256() || pc.Dispatches["peers"].RunID != "222" {
		t.Fatalf("%+v", pc)
	}
	if a := pc.Anchors["gnet-h1"].Scenarios["s1"]; a.D1 != 1000 || a.D2 != 1010 || a.Ratio == nil || a.Drift < 0.0099 || a.Drift > 0.0102 {
		t.Fatalf("%+v", a)
	}
	if _, err := os.Stat(filepath.Join(out, "publish-extras", "twin.json")); err != nil {
		t.Fatal("twin.json missing with --twin=separate")
	}

	// Install BEFORE publish (into the DOCS_REPO_DIR that mage Publish will use):
	// publishViaGit writes the four files into the cell, deletes nothing and
	// `git add`s the whole cell directory, so the extras ride in the same commit.
	docs := t.TempDir()
	got, err := Install(out, docs, "v1.6.0")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"composition.json", "twin.json"} {
		if _, err := os.Stat(filepath.Join(got, n)); err != nil {
			t.Error(err)
		}
	}
	gz, _ := res.TS.MarshalGzip()
	cell, err := report.WriteTree(filepath.Join(docs, "results"), res.Doc, gz, report.SplitMeta{Version: "v1.6.0", Arch: "x86_64", Date: "20261122", RunID: "run-1", GeneratedAt: t0})
	if err != nil {
		t.Fatal(err)
	}
	if cell != got {
		t.Fatalf("cell %s != install target %s", cell, got)
	}
	for _, n := range []string{"composition.json", "twin.json", "summary.json", "env.json", "histograms.json.gz", "timeseries.json.gz"} {
		if _, err := os.Stat(filepath.Join(cell, n)); err != nil {
			t.Errorf("after the publish write the cell lacks %s: %v", n, err)
		}
	}
	// idempotent after publish, and it re-checks against the published summary.json
	if _, err := Install(out, docs, "v1.6.0"); err != nil {
		t.Fatal(err)
	}
	// a composed results.json that is not the composition's refuses
	raw, err := os.ReadFile(filepath.Join(out, "results.json"))
	must(t, err)
	var d report.Document
	must(t, json.Unmarshal(raw, &d))
	d.Benchmarks = d.Benchmarks[1:]
	tampered, _ := json.MarshalIndent(&d, "", "  ")
	must(t, os.WriteFile(filepath.Join(out, "results.json"), tampered, 0o644))
	if _, err := Install(out, t.TempDir(), "v1.6.0"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("install of a tampered composition: %v", err)
	}
	must(t, os.WriteFile(filepath.Join(out, "results.json"), raw, 0o644))
	// a published summary.json with other columns refuses too
	docs2 := t.TempDir()
	res2, err := newFixture().compose(t)
	must(t, err)
	res2.Doc.Benchmarks = res2.Doc.Benchmarks[1:]
	_, err = report.WriteTree(filepath.Join(docs2, "results"), res2.Doc, gz, report.SplitMeta{Version: "v1.6.0", Arch: "x86_64", Date: "20261122", RunID: "run-1", GeneratedAt: t0})
	must(t, err)
	if _, err := Install(out, docs2, "v1.6.0"); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("install into another cell: %v", err)
	}
}
