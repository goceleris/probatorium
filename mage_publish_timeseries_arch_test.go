//go:build mage

package main

import (
	"bytes"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/goceleris/loadgen"
	"github.com/goceleris/probatorium/report"
)

// archFixtureCell is one (scenario, server) cell of the two-arch fixture:
// the per-second rps each machine measured. The two arches carry
// different series for the same cell, so a published file shows at a
// glance which machine it came from.
type archFixtureCell struct {
	scenario, server string
	x86, arm         []float64
}

// twoArchFixture is the smallest run with the probatorium#422 shape: every
// cell ran on BOTH bench targets. The arm64 get-json series stalls to 0 for
// three seconds, as the published v1.5.8/20260829 one did, so a reader who
// gets the wrong arch's series is blaming the wrong machine for a stall.
var twoArchFixture = []archFixtureCell{
	{"get-json", "celeris-epoll-h1-sync",
		[]float64{519136, 504608, 523232, 513632, 513120},
		[]float64{421152, 408496, 6640, 0, 0}},
	{"post-4k", "celeris-epoll-h1-sync",
		[]float64{301000, 302000, 303000, 304000, 305000},
		[]float64{201000, 202000, 203000, 204000, 205000}},
	{"get-json", "gin-h1",
		[]float64{120000, 121000, 122000, 123000, 124000},
		[]float64{80000, 81000, 82000, 83000, 84000}},
}

// writeTwoArchRun lays the fixture out the way the bench playbook leaves a
// BENCH_TARGET=both run: one <TS>-bench-<host> dir per bench target, one
// column dir per server, one runner cell per scenario, each carrying the
// loadgen 1 Hz series.
func writeTwoArchRun(t *testing.T, resultsDir string) {
	t.Helper()
	for _, host := range []string{"msa2-server", "msr1"} {
		benchDir := filepath.Join(resultsDir, "20260829T040000-bench-"+host)
		for _, c := range twoArchFixture {
			series := c.x86
			if host == "msr1" {
				series = c.arm
			}
			pts := make([]loadgen.TimeseriesPoint, len(series))
			for i, rps := range series {
				pts[i] = loadgen.TimeseriesPoint{TimestampSec: float64(i + 1), RequestsPerSec: rps, P99Ms: 1}
			}
			col := filepath.Join(benchDir, "00-"+c.server)
			writeRunnerCell(t, col, c.scenario, c.server, "ok", "", &loadgen.Result{
				Requests:       int64(series[0]) * 5,
				Duration:       5 * time.Second,
				RequestsPerSec: series[0],
				Latency:        loadgen.Percentiles{P50: 100 * time.Microsecond, P99: time.Millisecond},
				Timeseries:     pts,
			})
			if err := os.WriteFile(filepath.Join(col, "results.json"), []byte("{}"), 0o644); err != nil {
				t.Fatalf("write rollup: %v", err)
			}
		}
	}
}

// TestBothArchPublishWritesEachArchItsOwnTimeseries is the probatorium#422
// regression. It drives a BENCH_TARGET=both run through the same steps the
// cluster path takes: aggregatePerCellResults, then one mergeBenchResultsFor
// per bench target with Bench's own results-<arch>.json naming, then, for
// each arch, the BenchTier publish step (loadPublishInputsFrom on that
// arch's file and report.WriteTree into a docs tree).
//
// The published v1.5.8/20260829 tree is what this pins against: both arch
// dirs got ONE timeseries.json.gz (same sha256), holding 1,626 entries for
// 813 cells and no arch, so the dashboard's first match per cell showed
// x86_64's series under arm64 as well. Each arch dir must instead hold
// exactly one series per (scenario, server), and it must be that
// machine's.
func TestBothArchPublishWritesEachArchItsOwnTimeseries(t *testing.T) {
	resultsDir := t.TempDir()
	writeTwoArchRun(t, resultsDir)

	if err := aggregatePerCellResults(resultsDir, 0); err != nil {
		t.Fatalf("aggregatePerCellResults: %v", err)
	}
	bp := benchParams{
		CelerisVer: "v0.0.0-test",
		Duration:   "5s",
		Warmup:     "1s",
		Conns:      "64",
		Runs:       "1",
		Seed:       "1",
		StartedAt:  time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC),
	}
	for _, h := range []string{"msa2-server", "msr1"} {
		if _, err := mergeBenchResultsFor(resultsDir, "both", bp, h,
			"results-"+benchTargetArch(h)+".json"); err != nil {
			t.Fatalf("mergeBenchResultsFor(%s): %v", h, err)
		}
	}

	t.Setenv("BENCH_START_DATE", "20260829")
	docsRoot := filepath.Join(t.TempDir(), "results")
	published := map[string][]byte{} // arch tag -> published timeseries.json.gz
	for _, goarch := range []string{"amd64", "arm64"} {
		meta, doc, tsGz, _, err := loadPublishInputsFrom(
			filepath.Join(resultsDir, "results-"+goarch+".json"), "v0.0.0-test")
		if err != nil {
			t.Fatalf("loadPublishInputsFrom(%s): %v", goarch, err)
		}
		cell, err := report.WriteTree(docsRoot, doc, tsGz, meta)
		if err != nil {
			t.Fatalf("WriteTree(%s): %v", meta.Arch, err)
		}
		b, err := os.ReadFile(filepath.Join(cell, report.TimeseriesFile))
		if err != nil {
			t.Fatalf("%s: no %s published: %v", meta.Arch, report.TimeseriesFile, err)
		}
		published[meta.Arch] = b
	}

	for arch, b := range published {
		var ts report.TimeseriesDoc
		if err := ts.UnmarshalGzip(b); err != nil {
			t.Fatalf("%s/%s: %v", arch, report.TimeseriesFile, err)
		}
		entries := map[[2]string][]report.ScenarioSeries{}
		for _, s := range ts.Scenarios {
			k := [2]string{s.Scenario, s.Server}
			entries[k] = append(entries[k], s)
		}
		if len(ts.Scenarios) != len(twoArchFixture) {
			t.Errorf("%s/%s holds %d series for %d cells: want exactly one series per (scenario, server), from this machine only",
				arch, report.TimeseriesFile, len(ts.Scenarios), len(twoArchFixture))
		}
		for _, c := range twoArchFixture {
			got := entries[[2]string{c.scenario, c.server}]
			if len(got) != 1 {
				t.Errorf("%s/%s: %s/%s appears %d times, want 1 (a second copy is the other arch's series, with nothing saying which is which)",
					arch, report.TimeseriesFile, c.scenario, c.server, len(got))
				continue
			}
			want := c.x86
			if arch == "arm64" {
				want = c.arm
			}
			if len(got[0].Runs) != 1 {
				t.Errorf("%s/%s: %s/%s has %d runs, want 1", arch, report.TimeseriesFile, c.scenario, c.server, len(got[0].Runs))
				continue
			}
			var rps []float64
			for _, s := range got[0].Runs[0].Samples {
				rps = append(rps, s.RPS)
			}
			if !slices.Equal(rps, want) {
				t.Errorf("%s/%s: %s/%s rps = %v, want this machine's %v",
					arch, report.TimeseriesFile, c.scenario, c.server, rps, want)
			}
		}
	}
	if x, a := published["x86_64"], published["arm64"]; x != nil && bytes.Equal(x, a) {
		t.Errorf("x86_64 and arm64 got byte-identical %s: one file was written to both arch dirs", report.TimeseriesFile)
	}

	// A both-arch run dir must not keep a combined sidecar either: any
	// reader that looks for timeseries.json.gz next to a results file
	// would pick up both machines' series again.
	if _, err := os.Stat(filepath.Join(resultsDir, report.TimeseriesFile)); err == nil {
		t.Errorf("both-arch run dir still carries a combined %s holding both machines' series", report.TimeseriesFile)
	}
}
