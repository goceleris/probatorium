package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// fixture is a synthetic, valid pair that tests mutate one field at a time.
type fixture struct {
	C, P         *report.Document
	CTS, PTS     *report.TimeseriesDoc
	CMeta, PMeta map[string]string
	CBench       string // <TS>-bench-<host> dir name
	PBench       string
	Opt          Options
	Post         func(cdir, pdir string)
	Extra        func(cdir, pdir string) // extra files written before loading
}

var t0 = time.Date(2026, 11, 22, 6, 0, 0, 0, time.UTC)

func row(name, category, framework, lang string, rps map[string]float64) report.ServerResult {
	st := map[string]string{}
	sat := map[string]float64{}
	slo := map[string]map[int]int{}
	cpu := map[string]float64{}
	hdr := map[string]string{}
	for sc, v := range rps {
		sat[sc] = v
		slo[sc] = map[int]int{10: int(v / 2), 100: int(v / 2)}
		cpu[sc] = 0.07
		hdr[sc] = "HISTFAAA" + name + sc
	}
	return report.ServerResult{
		Name: name, Category: category, Language: lang, LanguageVersion: "go1.27.3", Framework: framework,
		FrameworkVersion: "v1.0.0", Engine: "e", CompileOptions: []string{"CGO_ENABLED=0"},
		SaturationModeRPS: sat, RatedModeP99AtTargetRPS: map[string]time.Duration{"s1": 1000},
		LatencyAtSLO: slo, HdrHistogramB64: hdr, LoadgenCPUP95: cpu,
		SentVsHandledDeltaPct: map[string]float64{}, CellStatuses: st,
	}
}

func series(scs []string, servers []string) *report.TimeseriesDoc {
	ts := &report.TimeseriesDoc{GeneratedAt: t0.Add(time.Hour), SchemaVersion: "timeseries/1", Arch: "x86_64"}
	for _, sc := range scs {
		for _, sv := range servers {
			ts.Scenarios = append(ts.Scenarios, report.ScenarioSeries{
				Scenario: sc, Server: sv,
				Runs: []report.RunSeries{{Run: 1, Samples: []report.SampleRow{{TSec: 1, RPS: 10}}}},
				Band: []report.BucketBand{},
			})
		}
	}
	return ts
}

func baseDoc(start time.Time, rows []report.ServerResult) *report.Document {
	return &report.Document{
		SchemaVersion: "5.24", HostArchPair: "linux/amd64",
		Environment: report.Environment{KernelSysctlsApplied: []string{}, LoadgenHost: "msa2-client", Fabric: "3-host LACP 20G", FabricLineRateBitsPerSec: 20000000000},
		BenchmarkConfig: report.BenchmarkConfig{
			StartedAt: start, FinishedAt: start.Add(30 * time.Minute), Runs: 1, Duration: 90 * time.Second, Warmup: 20 * time.Second,
			GitRef: "v1.5.12-x", LoadgenVer: "v1.4.15", CelerisVer: "v1.5.12-x",
		},
		Benchmarks: rows,
	}
}

func newFixture() *fixture {
	scs := map[string]float64{"s1": 1000, "s2": 2000}
	mk := func(rps float64, names ...string) []report.ServerResult {
		var rs []report.ServerResult
		for _, n := range names {
			m := map[string]float64{}
			for sc, v := range scs {
				m[sc] = v * rps
			}
			cat, fw := "framework", n
			lang := "go"
			switch n {
			case "lithium", "h2o":
				lang = "c"
			case "actix":
				lang = "rust"
			}
			if strings.HasPrefix(n, "celeris-") {
				cat, fw = "celeris", "celeris"
			}
			if strings.HasPrefix(n, twinPrefix) {
				cat, fw = twinCategory, twinCategory
			}
			rs = append(rs, row(n, cat, fw, lang, m))
		}
		return rs
	}
	cNames := []string{"celeris-a", "celeris-b", twinPrefix + "a", twinPrefix + "b", "gnet-h1", "fiber-h1", "stdhttp-h1", "lithium", "h2o", "actix"}
	pNames := []string{"gin-h1", "echo-h1", "gnet-h1", "fiber-h1", "stdhttp-h1", "lithium", "h2o", "actix"}
	f := &fixture{
		C:      baseDoc(t0, mk(1, cNames...)),
		P:      baseDoc(t0.Add(33*time.Hour), mk(1.01, pNames...)),
		CMeta:  map[string]string{"run_id": "111", "harness_commit": "abc123", "profile": "full", "kernel": "7.0.0-38-generic", "boot_id": "boot-1", "boot_cmdline": "BOOT_IMAGE=/vmlinuz root=UUID=x ro quiet"},
		PMeta:  map[string]string{"run_id": "222", "harness_commit": "abc123", "profile": "full", "kernel": "7.0.0-38-generic", "boot_id": "boot-1", "boot_cmdline": "BOOT_IMAGE=/vmlinuz root=UUID=x ro quiet"},
		CBench: "20261122T060000-bench-msa2-server",
		PBench: "20261123T150000-bench-msa2-server",
		Opt:    Options{Date: "20261122", Now: func() time.Time { return t0 }},
	}
	sort1(f.C)
	sort1(f.P)
	f.CTS = series([]string{"s1", "s2"}, sorted(cNames))
	f.PTS = series([]string{"s1", "s2"}, sorted(pNames))
	return f
}

func sorted(s []string) []string {
	o := append([]string(nil), s...)
	for i := range o {
		for j := i + 1; j < len(o); j++ {
			if o[j] < o[i] {
				o[i], o[j] = o[j], o[i]
			}
		}
	}
	return o
}

func sort1(d *report.Document) {
	b := d.Benchmarks
	for i := range b {
		for j := i + 1; j < len(b); j++ {
			if b[j].Name < b[i].Name {
				b[i], b[j] = b[j], b[i]
			}
		}
	}
}

// writeDispatch writes one run directory the way the runner lays it out.
func writeDispatch(t testing.TB, dir string, d *report.Document, ts *report.TimeseriesDoc, meta map[string]string, bench string) string {
	t.Helper()
	run := filepath.Join(dir, "run")
	must(t, os.MkdirAll(filepath.Join(run, "raw"), 0o755))
	b, err := json.MarshalIndent(d, "", "  ")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(run, "results.json"), b, 0o644))
	if ts != nil {
		gz, err := ts.MarshalGzip()
		must(t, err)
		must(t, os.WriteFile(filepath.Join(run, "timeseries.json.gz"), gz, 0o644))
	}
	bv := map[string]any{}
	for _, r := range d.Benchmarks {
		bv[r.Name] = map[string]string{"bin": "x"}
	}
	bj, _ := json.Marshal(map[string]any{"competitor_columns": bv})
	must(t, os.WriteFile(filepath.Join(run, "bench-vars.json"), bj, 0o644))
	must(t, os.WriteFile(filepath.Join(run, "raw", "msa2-server.json"), []byte(`{"cells":[]}`), 0o644))
	for _, r := range d.Benchmarks {
		cd := filepath.Join(run, bench, "00-"+r.Name)
		must(t, os.MkdirAll(cd, 0o755))
		must(t, os.WriteFile(filepath.Join(cd, "results.json"), []byte(`{}`), 0o644))
	}
	if meta != nil {
		var sb strings.Builder
		for k, v := range meta {
			fmt.Fprintf(&sb, "%s=%s\n", k, v)
		}
		must(t, os.WriteFile(filepath.Join(dir, "meta.txt"), []byte(sb.String()), 0o644))
	}
	return run
}

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// load writes the fixture and loads both dispatches.
func (f *fixture) load(t testing.TB) (*Dispatch, *Dispatch, string, string) {
	t.Helper()
	root := t.TempDir()
	cdir, pdir := filepath.Join(root, "c"), filepath.Join(root, "p")
	cr := writeDispatch(t, cdir, f.C, f.CTS, f.CMeta, f.CBench)
	pr := writeDispatch(t, pdir, f.P, f.PTS, f.PMeta, f.PBench)
	if f.Extra != nil {
		f.Extra(cr, pr)
	}
	c, err := LoadDispatch("candidate", cr, filepath.Join(cdir, "meta.txt"))
	must(t, err)
	p, err := LoadDispatch("peers", pr, filepath.Join(pdir, "meta.txt"))
	must(t, err)
	return c, p, cr, pr
}

func (f *fixture) compose(t testing.TB) (*Result, error) {
	t.Helper()
	c, p, _, _ := f.load(t)
	opt := f.Opt
	if len(opt.Anchors) == 0 {
		opt.Anchors = DefaultAnchors
	}
	return Compose(c, p, opt)
}
