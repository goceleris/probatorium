package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// The real-document tests read the downloaded artifacts of the D854Z and L755Z
// lanes. They are skipped unless MERGEDOCS_EVIDENCE points at the evidence
// root (E in the loop notes). The artifacts are untrusted data and are only
// read: every piece is written to a temp dir.
func evidence(t *testing.T) string {
	e := os.Getenv("MERGEDOCS_EVIDENCE")
	if e == "" {
		t.Skip("MERGEDOCS_EVIDENCE not set")
	}
	return e
}

// workDir is t.TempDir(), or <MERGEDOCS_KEEP>/<name> so the split pieces stay on
// disk for the end-to-end rehearsal (rehearse.sh).
func workDir(t *testing.T, name string) string {
	if k := os.Getenv("MERGEDOCS_KEEP"); k != "" {
		d := filepath.Join(k, name)
		must(t, os.RemoveAll(d))
		must(t, os.MkdirAll(d, 0o755))
		return d
	}
	return t.TempDir()
}

func d1RunDir(t *testing.T, e string) string {
	m, _ := filepath.Glob(filepath.Join(e, "lanes-20261121/D854Z/READ-D1/rundir-44991328759/benchmark-tier-results-44991328759/*-bench-*"))
	if len(m) != 1 {
		t.Fatalf("D1 fixture: %v", m)
	}
	return m[0]
}

func l755RunDirs(t *testing.T, e string) []string {
	m, _ := filepath.Glob(filepath.Join(e, "lanes-20261121/L755Z/READ/rundir-*/benchmark-tier-results-*/*-bench-*"))
	if len(m) != 5 {
		t.Fatalf("L755Z fixtures: %v", m)
	}
	return m
}

// piece writes a run directory holding only the columns keep() selects of the
// source dispatch, with the given window and bench-dir renaming.
func piece(t *testing.T, src *Dispatch, dst string, keep func(string) bool, start, finish time.Time, benchSuffix string, meta map[string]string) (string, string) {
	t.Helper()
	doc := *src.Doc
	doc.Benchmarks = nil
	for _, b := range src.Doc.Benchmarks {
		if keep(b.Name) {
			doc.Benchmarks = append(doc.Benchmarks, b)
		}
	}
	doc.BenchmarkConfig.StartedAt, doc.BenchmarkConfig.FinishedAt = start, finish
	run := filepath.Join(dst, "run")
	must(t, os.MkdirAll(run, 0o755))
	b, err := json.MarshalIndent(&doc, "", "  ")
	must(t, err)
	must(t, os.WriteFile(filepath.Join(run, "results.json"), b, 0o644))
	if src.HasTS {
		ts := *src.TS
		ts.Scenarios = nil
		for _, s := range src.TS.Scenarios {
			if keep(s.Server) {
				ts.Scenarios = append(ts.Scenarios, s)
			}
		}
		gz, err := ts.MarshalGzip()
		must(t, err)
		must(t, os.WriteFile(filepath.Join(run, "timeseries.json.gz"), gz, 0o644))
	}
	must(t, os.MkdirAll(filepath.Join(run, "raw"), 0o755))
	for _, f := range src.RawFiles {
		must(t, copyFile(filepath.Join(src.Dir, "raw", f), filepath.Join(run, "raw", f)))
	}
	for _, bd := range src.BenchDirs {
		for _, cd := range bd.Columns {
			name := strings.SplitN(cd.Name, "-", 2)[1]
			if !keep(name) {
				continue
			}
			bn := bd.Name
			if benchSuffix != "" { // another dispatch has another <TS>
				ts, host, _ := strings.Cut(bn, "-bench-")
				bn = ts + benchSuffix + "-bench-" + host
			}
			d := filepath.Join(run, bn, cd.Name)
			must(t, os.MkdirAll(d, 0o755))
			if cd.HasRollup {
				must(t, copyFile(cd.Rollup, filepath.Join(d, "results.json")))
			}
		}
	}
	var sb strings.Builder
	for k, v := range meta {
		sb.WriteString(k + "=" + v + "\n")
	}
	mp := filepath.Join(dst, "meta.txt")
	must(t, os.WriteFile(mp, []byte(sb.String()), 0o644))
	return run, mp
}

func gunzip(t *testing.T, p string) []byte {
	f, err := os.Open(p)
	must(t, err)
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	must(t, err)
	b, err := io.ReadAll(zr)
	must(t, err)
	return b
}

// tree publishes a document through the real report.WriteTree with fixed meta and
// returns the four files' contents (the gz ones decompressed).
func tree(t *testing.T, doc *report.Document, tsGz []byte) map[string][]byte {
	root := t.TempDir()
	meta := report.SplitMeta{Version: "v1.6.0", Arch: archTag(doc.HostArchPair), Date: "20261122", RunID: "run-1", GeneratedAt: t0}
	cell, err := report.WriteTree(root, doc, tsGz, meta)
	must(t, err)
	out := map[string][]byte{}
	for _, f := range []string{"summary.json", "env.json"} {
		b, err := os.ReadFile(filepath.Join(cell, f))
		must(t, err)
		out[f] = b
	}
	for _, f := range []string{"histograms.json.gz", "timeseries.json.gz"} {
		out[f] = gunzip(t, filepath.Join(cell, f))
	}
	return out
}

// merge(split(x)) == x on every key the publish path reads: the results.json
// bytes and the four files WriteTree writes from it.
func TestRealD1SplitMergeIsIdentity(t *testing.T) {
	e := evidence(t)
	src, err := LoadDispatch("candidate", d1RunDir(t, e), "")
	must(t, err)
	if src.RoundTripErr != "" || len(src.Doc.Benchmarks) != 14 {
		t.Fatalf("fixture: %q, %d columns", src.RoundTripErr, len(src.Doc.Benchmarks))
	}
	anchor := "celeris-epoll-h1-async"
	first := map[string]bool{}
	for i, b := range src.Doc.Benchmarks {
		if i < 7 {
			first[b.Name] = true
		}
	}
	mid := src.Doc.BenchmarkConfig.StartedAt.Add(10 * time.Minute)
	meta := func(run string) map[string]string {
		return map[string]string{"run_id": run, "harness_commit": "d534a9f", "profile": "full", "kernel": "7.0.0-38-generic", "boot_id": "35730f68", "host": "msa2-server"}
	}
	root := workDir(t, "d1-split")
	ar, am := piece(t, src, filepath.Join(root, "a"), func(n string) bool { return first[n] || n == anchor }, src.Doc.BenchmarkConfig.StartedAt, mid, "", meta("1"))
	br, bm := piece(t, src, filepath.Join(root, "b"), func(n string) bool { return !first[n] || n == anchor }, mid, src.Doc.BenchmarkConfig.FinishedAt, "x", meta("2"))
	c, err := LoadDispatch("candidate", ar, am)
	must(t, err)
	p, err := LoadDispatch("peers", br, bm)
	must(t, err)
	res, err := Compose(c, p, Options{Anchors: []string{anchor}, Twin: "keep", Date: "20261122", Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(res.ResultsJSON, src.DocRaw) {
		t.Fatalf("merged results.json (%d bytes) != original (%d bytes)", len(res.ResultsJSON), len(src.DocRaw))
	}
	mgz, err := res.TS.MarshalGzip()
	must(t, err)
	want, got := tree(t, src.Doc, src.TSGz), tree(t, res.Doc, mgz)
	for f := range want {
		if !bytes.Equal(want[f], got[f]) {
			t.Errorf("%s differs after merge(split(x)): %d vs %d bytes", f, len(want[f]), len(got[f]))
		}
	}
	if res.Comp.MaxAnchorDrift != 0 || len(res.Comp.TieMarginByScenario) == 0 || res.Comp.TieMarginByScenario["driver-redis-pipeline"] != 0.05 {
		t.Fatalf("an anchor identical in both pieces has drift 0: %v %v", res.Comp.MaxAnchorDrift, res.Comp.TieMarginByScenario)
	}
	// and the output directory carries the audit inputs of both pieces
	out := filepath.Join(root, "out")
	must(t, WriteOutput(res, out))
	for _, f := range []string{"raw/d1-msa2-server.json", "raw/d2-msa2-server.json"} {
		if _, err := os.Stat(filepath.Join(out, f)); err != nil {
			t.Error(err)
		}
	}
}

// The real drift case: two dispatches of the same config, 1.2 h apart on arm64
// (the L755Z runs), composed as columns 1-5 of the first and 6-10 of the
// second with celeris-std-h1 as the anchor. It is a positive case: the drift is
// measured, recorded, and the tie margin follows it.
func TestRealL755ZDriftPair(t *testing.T) {
	e := evidence(t)
	dirs := l755RunDirs(t, e)
	var ds []*Dispatch
	for _, d := range dirs {
		x, err := LoadDispatch("candidate", d, "")
		must(t, err)
		if x.RoundTripErr != "" {
			t.Fatal(x.RoundTripErr)
		}
		ds = append(ds, x)
	}
	// order by start time
	for i := range ds {
		for j := i + 1; j < len(ds); j++ {
			if ds[j].Doc.BenchmarkConfig.StartedAt.Before(ds[i].Doc.BenchmarkConfig.StartedAt) {
				ds[i], ds[j] = ds[j], ds[i]
			}
		}
	}
	a, b := ds[0], ds[1]
	if a.Meta.Kernel == "" || a.Meta.BootID == "" || a.Meta.SUTHost != "msr1" {
		t.Fatalf("forensics discovery on the real artifact layout: %+v", a.Meta)
	}
	anchor := "celeris-std-h1"
	names := []string{}
	for _, r := range a.Doc.Benchmarks {
		names = append(names, r.Name)
	}
	lo := map[string]bool{}
	for _, n := range names[:5] {
		lo[n] = true
	}
	meta := func(run string) map[string]string {
		return map[string]string{"run_id": run, "harness_commit": "ed7835e", "profile": "full", "kernel": a.Meta.Kernel, "boot_id": a.Meta.BootID, "host": "msr1"}
	}
	root := workDir(t, "l755z-pair")
	ar, am := piece(t, a, filepath.Join(root, "a"), func(n string) bool { return lo[n] || n == anchor }, a.Doc.BenchmarkConfig.StartedAt, a.Doc.BenchmarkConfig.FinishedAt, "", meta("1"))
	br, bm := piece(t, b, filepath.Join(root, "b"), func(n string) bool { return !lo[n] || n == anchor }, b.Doc.BenchmarkConfig.StartedAt, b.Doc.BenchmarkConfig.FinishedAt, "", meta("2"))
	c, err := LoadDispatch("candidate", ar, am)
	must(t, err)
	p, err := LoadDispatch("peers", br, bm)
	must(t, err)
	// a and b are different dispatches; their bench dirs have different <TS> names
	res, err := Compose(c, p, Options{Anchors: []string{anchor}, Date: "20261122", Now: func() time.Time { return t0 }})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Doc.Benchmarks) != 10 {
		t.Fatalf("%d columns", len(res.Doc.Benchmarks))
	}
	an := res.Comp.Anchors.Per[0]
	if an.Paired != 1 || res.Comp.MaxAnchorDrift <= 0 {
		t.Fatalf("anchor %+v", an)
	}
	t.Logf("L755Z anchor %s: d1/d2 = %v, drift %.4f, tie margins %v, warnings %v", anchor, *an.Metrics["saturation_mode_rps"][0].Ratio, res.Comp.MaxAnchorDrift, res.Comp.TieMarginByScenario, res.Comp.Warnings)
}

func TestRealForensicsDiscovery(t *testing.T) {
	e := evidence(t)
	d, err := LoadDispatch("candidate", d1RunDir(t, e), "")
	must(t, err)
	if d.Meta.RunID != "44991328759" || d.Meta.SUTHost != "msa2-server" ||
		d.Meta.Kernel != "7.0.0-38-generic" || d.Meta.BootID != "35730f68-5b52-4a44-8014-8f1860b5ec87" {
		t.Fatalf("%+v", d.Meta)
	}
	// the wrapper's params.txt agrees with the artifact (kernel given as 7.0.0-38)
	pm := filepath.Join(e, "lanes-20261121/D854Z/READ-D1/read-44991328759/params.txt")
	d2, err := LoadDispatch("candidate", d1RunDir(t, e), pm)
	must(t, err)
	if len(d2.Meta.conflicts) != 0 || d2.Meta.HarnessCommit != "d534a9fbe181ab6133040594d6c8194aebbd7f0b" {
		t.Fatalf("%+v", d2.Meta)
	}
}
