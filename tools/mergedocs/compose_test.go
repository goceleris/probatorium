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
	want := []string{"celeris-a", "celeris-b", "echo-h1", "fiber-h1", "gin-h1", "gnet-h1", "stdhttp-h1"}
	if !reflect.DeepEqual(names, want) {
		t.Fatalf("columns %v want %v (twins stripped by default, anchors once, sorted)", names, want)
	}
	if res.Comp.Columns["gnet-h1"] != "candidate" || res.Comp.Columns["gin-h1"] != "peers" {
		t.Fatalf("columns map %v", res.Comp.Columns)
	}
	if res.Comp.Twin.Mode != "separate" || res.Twin == nil || len(res.Comp.Twin.Columns) != 2 {
		t.Fatalf("twin default must be separate: %+v", res.Comp.Twin)
	}
	// anchors: peers is 1% faster everywhere, so drift = 1/1.01-1 ratio-wise ~0.0101; tie floor wins
	if res.Comp.TieMargin != 0.05 || res.Comp.MaxAnchorDrift <= 0.0099 || res.Comp.MaxAnchorDrift >= 0.0102 {
		t.Fatalf("drift %v tie %v", res.Comp.MaxAnchorDrift, res.Comp.TieMargin)
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
	// stdhttp-h1 is 20% slower in the peers dispatch (d1/d2 = 1.25); fiber 10% faster
	for i := range f.P.Benchmarks {
		switch f.P.Benchmarks[i].Name {
		case "stdhttp-h1":
			for k := range f.P.Benchmarks[i].SaturationModeRPS {
				f.P.Benchmarks[i].SaturationModeRPS[k] *= 0.8
			}
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
	// d1/d2 for a 0.8 peers value = 1/0.8/1.01... peers base 1.01 -> 0.808: r = 1/0.808 = 1.2376
	if a.MaxAnchorDrift < 0.237 || a.MaxAnchorDrift > 0.238 || res.Comp.TieMargin != a.MaxAnchorDrift {
		t.Fatalf("max %v tie %v", a.MaxAnchorDrift, res.Comp.TieMargin)
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
	// symmetric: fiber is faster in d2 (r<1) and drift is 1/r-1 = 1.1*1.01-1 = 0.111
	for _, p := range a.Per {
		if p.Name == "fiber-h1" && (p.MaxDrift < 0.11 || p.MaxDrift > 0.112) {
			t.Fatalf("fiber %v", p.MaxDrift)
		}
		if len(p.Metrics["saturation_mode_rps"]) != 2 || len(p.Metrics["latency_at_slo"]) != 4 || len(p.Metrics["loadgen_cpu_p95"]) != 2 {
			t.Fatalf("%s metrics %v", p.Name, p.Metrics)
		}
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
