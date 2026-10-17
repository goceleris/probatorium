//go:build mage

package main

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// TestTimeseriesSidecarName pins the one mapping both the writer
// (mergeBenchResultsFor) and the reader (loadPublishInputsFrom) use to pair
// a results file with its time-series sidecar.
func TestTimeseriesSidecarName(t *testing.T) {
	for in, want := range map[string]string{
		"results.json":       "timeseries.json.gz",
		"results-amd64.json": "timeseries-amd64.json.gz",
		"results-arm64.json": "timeseries-arm64.json.gz",
		"results-multi.json": "timeseries-multi.json.gz",
		"results-.json":      "timeseries.json.gz", // no arch: canonical name
		"pinned.json":        "timeseries.json.gz", // PUBLISH_RESULTS=<any>: canonical name
		"results-arm64.txt":  "timeseries.json.gz",
	} {
		if got := timeseriesSidecarName(in); got != want {
			t.Errorf("timeseriesSidecarName(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSingleTargetMergeWritesItsLabelledSidecar: a single-target run keeps
// its layout (results.json + timeseries.json.gz, no per-arch files), and the
// sidecar is labelled with the arch Publish files the Document under, so
// WriteTree accepts it.
func TestSingleTargetMergeWritesItsLabelledSidecar(t *testing.T) {
	for _, tc := range []struct{ host, arch string }{
		{"msa2-server", "x86_64"},
		{"msr1", "arm64"},
	} {
		t.Run(tc.host, func(t *testing.T) {
			resultsDir := t.TempDir()
			writeTwoArchRun(t, resultsDir)
			// Keep only this target's bench dir: a single-target run has one.
			entries, _ := os.ReadDir(resultsDir)
			for _, e := range entries {
				if e.Name() != "20260829T040000-bench-"+tc.host {
					_ = os.RemoveAll(filepath.Join(resultsDir, e.Name()))
				}
			}
			if err := aggregatePerCellResults(resultsDir, 0); err != nil {
				t.Fatal(err)
			}
			out, err := mergeBenchResults(resultsDir, tc.host, benchParams{
				CelerisVer: "v0.0.0-test", Duration: "5s", Warmup: "1s", Runs: "1",
				StartedAt: time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC),
			})
			if err != nil {
				t.Fatal(err)
			}
			b, err := os.ReadFile(filepath.Join(resultsDir, report.TimeseriesFile))
			if err != nil {
				t.Fatalf("single-target run has no %s: %v", report.TimeseriesFile, err)
			}
			var ts report.TimeseriesDoc
			if err := ts.UnmarshalGzip(b); err != nil {
				t.Fatal(err)
			}
			if ts.Arch != tc.arch {
				t.Errorf("sidecar labelled %q, want %q", ts.Arch, tc.arch)
			}
			if len(ts.Scenarios) != len(twoArchFixture) {
				t.Errorf("sidecar holds %d series, want %d", len(ts.Scenarios), len(twoArchFixture))
			}
			for _, goarch := range []string{"amd64", "arm64"} {
				if _, err := os.Stat(filepath.Join(resultsDir, "timeseries-"+goarch+".json.gz")); err == nil {
					t.Errorf("single-target run grew a per-arch timeseries-%s.json.gz", goarch)
				}
			}

			t.Setenv("BENCH_START_DATE", "20260829")
			meta, doc, tsGz, _, err := loadPublishInputsFrom(out, "v0.0.0-test")
			if err != nil {
				t.Fatal(err)
			}
			if meta.Arch != tc.arch {
				t.Fatalf("publish arch %q, want %q", meta.Arch, tc.arch)
			}
			if _, err := report.WriteTree(filepath.Join(t.TempDir(), "results"), doc, tsGz, meta); err != nil {
				t.Errorf("WriteTree refused the single-target sidecar: %v", err)
			}
		})
	}
}

// TestPublishIgnoresAPre422CombinedSidecar: a both-arch run dir merged
// before probatorium#422 has results-<arch>.json plus ONE combined
// timeseries.json.gz holding both machines' series. Publishing either arch
// from it must ship no timeseries rather than that file.
func TestPublishIgnoresAPre422CombinedSidecar(t *testing.T) {
	resultsDir := t.TempDir()
	writeTwoArchRun(t, resultsDir)
	if err := aggregatePerCellResults(resultsDir, 0); err != nil {
		t.Fatal(err)
	}
	bp := benchParams{CelerisVer: "v0.0.0-test", Duration: "5s", Warmup: "1s", Runs: "1",
		StartedAt: time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC)}
	for _, h := range []string{"msa2-server", "msr1"} {
		if _, err := mergeBenchResultsFor(resultsDir, "both", bp, h, "results-"+benchTargetArch(h)+".json"); err != nil {
			t.Fatal(err)
		}
	}
	// Recreate the old layout: replace the per-arch sidecars with ONE
	// unlabelled doc holding both machines' series, as the pre-#422
	// writer produced it.
	combined := &report.TimeseriesDoc{GeneratedAt: time.Now().UTC(), SchemaVersion: report.TimeseriesSchemaVersion}
	for _, goarch := range []string{"amd64", "arm64"} {
		p := filepath.Join(resultsDir, "timeseries-"+goarch+".json.gz")
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		var d report.TimeseriesDoc
		if err := d.UnmarshalGzip(b); err != nil {
			t.Fatal(err)
		}
		combined.Scenarios = append(combined.Scenarios, d.Scenarios...)
		if err := os.Remove(p); err != nil {
			t.Fatal(err)
		}
	}
	combinedGz, err := combined.MarshalGzip()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(resultsDir, report.TimeseriesFile), combinedGz, 0o644); err != nil {
		t.Fatal(err)
	}
	if err := report.CheckTimeseriesForArch(combinedGz, "x86_64"); err == nil {
		t.Fatal("fixture: the combined sidecar should hold every cell twice")
	}

	t.Setenv("BENCH_START_DATE", "20260829")
	for _, goarch := range []string{"amd64", "arm64"} {
		meta, doc, tsGz, _, err := loadPublishInputsFrom(filepath.Join(resultsDir, "results-"+goarch+".json"), "v0.0.0-test")
		if err != nil {
			t.Fatal(err)
		}
		if tsGz != nil {
			t.Errorf("%s: publish picked up a sidecar (%d bytes) from a run that has only the combined one", meta.Arch, len(tsGz))
		}
		cell, err := report.WriteTree(filepath.Join(t.TempDir(), "results"), doc, tsGz, meta)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := os.Stat(filepath.Join(cell, report.TimeseriesFile)); err == nil {
			t.Errorf("%s: a timeseries was published from a pre-#422 both-arch run", meta.Arch)
		}
	}
}

// TestSidecarSkipsRunsWithoutAMeasurement: the sidecar is now built from the
// raw/<host>.json payload, where a run that measured nothing (DNF, N/A)
// carries "loadgen": null rather than the nil it had in memory. Such a run
// must stay out of the series, as it always did, instead of becoming an
// empty run next to the real one.
func TestSidecarSkipsRunsWithoutAMeasurement(t *testing.T) {
	resultsDir := t.TempDir()
	writeTwoArchRun(t, resultsDir)
	_ = os.RemoveAll(filepath.Join(resultsDir, "20260829T040000-bench-msr1"))
	// Run 1 of one cell died before measuring anything.
	col := filepath.Join(resultsDir, "20260829T040000-bench-msa2-server", "01-celeris-epoll-h1-sync")
	writeRunnerCell(t, col, "get-json", "celeris-epoll-h1-sync", "dnf",
		"interrupted: cell cancelled mid-run (requests=0 errors=0)", nil)
	if err := os.WriteFile(filepath.Join(col, "results.json"), []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := aggregatePerCellResults(resultsDir, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := mergeBenchResults(resultsDir, "msa2-server", benchParams{
		CelerisVer: "v0.0.0-test", Duration: "5s", Warmup: "1s", Runs: "2",
		StartedAt: time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(filepath.Join(resultsDir, report.TimeseriesFile))
	if err != nil {
		t.Fatal(err)
	}
	var ts report.TimeseriesDoc
	if err := ts.UnmarshalGzip(b); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, s := range ts.Scenarios {
		if s.Scenario != "get-json" || s.Server != "celeris-epoll-h1-sync" {
			continue
		}
		found = true
		if len(s.Runs) != 1 || len(s.Runs[0].Samples) != len(twoArchFixture[0].x86) {
			t.Errorf("get-json/celeris-epoll-h1-sync: %d runs (samples per run %v), want the one measured run of %d samples",
				len(s.Runs), runLens(s.Runs), len(twoArchFixture[0].x86))
		}
	}
	if !found {
		t.Fatal("get-json/celeris-epoll-h1-sync missing from the sidecar")
	}
}

func runLens(rs []report.RunSeries) []int {
	out := make([]int, len(rs))
	for i, r := range rs {
		out[i] = len(r.Samples)
	}
	return out
}
