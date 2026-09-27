//go:build mage

package main

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

// pairRun is one merge of the two-arch fixture under one of Bench's two
// naming schemes: a single-target run's results.json + timeseries.json.gz,
// or one arch of a BENCH_TARGET=both run (results-amd64.json +
// timeseries-amd64.json.gz).
type pairRun struct {
	name        string
	resultsName string
	sidecarName string
	// setup lays out and aggregates the run in resultsDir, and returns the
	// merge that writes the pair.
	setup func(t *testing.T, resultsDir string) func() (string, error)
}

func pairRuns() []pairRun {
	bp := benchParams{
		CelerisVer: "v0.0.0-test", Duration: "5s", Warmup: "1s", Runs: "1",
		StartedAt: time.Date(2026, 8, 29, 4, 0, 0, 0, time.UTC),
	}
	return []pairRun{
		{
			name:        "single-target",
			resultsName: "results.json",
			sidecarName: "timeseries.json.gz",
			setup: func(t *testing.T, resultsDir string) func() (string, error) {
				writeTwoArchRun(t, resultsDir)
				_ = os.RemoveAll(filepath.Join(resultsDir, "20260829T040000-bench-msr1"))
				if err := aggregatePerCellResults(resultsDir, 0); err != nil {
					t.Fatal(err)
				}
				return func() (string, error) { return mergeBenchResults(resultsDir, "msa2-server", bp) }
			},
		},
		{
			name:        "both-arch",
			resultsName: "results-amd64.json",
			sidecarName: "timeseries-amd64.json.gz",
			setup: func(t *testing.T, resultsDir string) func() (string, error) {
				writeTwoArchRun(t, resultsDir)
				if err := aggregatePerCellResults(resultsDir, 0); err != nil {
					t.Fatal(err)
				}
				return func() (string, error) {
					return mergeBenchResultsFor(resultsDir, "both", bp, "msa2-server", "results-amd64.json")
				}
			},
		},
	}
}

// runDirEntries is the run dir's top level, by name. A merge that fails
// must leave it exactly as it found it: no results document, no sidecar,
// and no staged temp file of either.
func runDirEntries(t *testing.T, dir string) []string {
	t.Helper()
	es, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	names := make([]string, 0, len(es))
	for _, e := range es {
		names = append(names, e.Name())
	}
	slices.Sort(names)
	return names
}

// TestMergeCommitsResultsWithItsSidecar: Publish keys on the results
// document (latestBenchResults / PUBLISH_RESULTS), then reads whatever
// sidecar sits beside it. A merge that exposes the results document but
// fails to put its sidecar in place leaves a run dir that publishes
// without its time series and says nothing. The results document must
// therefore become visible only once its sidecar is in place.
//
// The sidecar commit is made to fail by an obstacle at its path (a
// directory), which fails os.WriteFile and os.Rename alike, so the test
// runs unchanged against the pre-fix writer: that one wrote the results
// document first and then failed on the sidecar, leaving the document in
// place.
func TestMergeCommitsResultsWithItsSidecar(t *testing.T) {
	for _, r := range pairRuns() {
		t.Run(r.name, func(t *testing.T) {
			resultsDir := t.TempDir()
			merge := r.setup(t, resultsDir)
			if err := os.Mkdir(filepath.Join(resultsDir, r.sidecarName), 0o755); err != nil {
				t.Fatal(err)
			}
			before := runDirEntries(t, resultsDir)

			if _, err := merge(); err == nil {
				t.Fatalf("merge succeeded with a directory at the sidecar path %s", r.sidecarName)
			}

			if _, err := os.Lstat(filepath.Join(resultsDir, r.resultsName)); err == nil {
				t.Errorf("%s exists after its sidecar could not be committed: Publish would ship it without its time series", r.resultsName)
			}
			if es, err := os.ReadDir(filepath.Join(resultsDir, r.sidecarName)); err != nil || len(es) != 0 {
				t.Errorf("the obstacle at %s changed (entries %d, err %v)", r.sidecarName, len(es), err)
			}
			if after := runDirEntries(t, resultsDir); !slices.Equal(after, before) {
				t.Errorf("failed merge changed the run dir:\n before %q\n after  %q", before, after)
			}
		})
	}
}

// TestMergeStagingFailureLeavesNoPartialFile: a write that fails part-way
// (a full disk) while staging either file of the pair must leave nothing
// in the run dir: no results document, no truncated sidecar, no staged
// temp. os.WriteFile truncates its target first, so a sidecar that failed
// mid-write used to leave a partial file under its real name.
func TestMergeStagingFailureLeavesNoPartialFile(t *testing.T) {
	errDiskFull := errors.New("injected: no space left on device")
	for _, r := range pairRuns() {
		for _, failing := range []string{r.sidecarName, r.resultsName} {
			t.Run(r.name+"/"+failing, func(t *testing.T) {
				resultsDir := t.TempDir()
				merge := r.setup(t, resultsDir)
				before := runDirEntries(t, resultsDir)

				orig := writeStaged
				t.Cleanup(func() { writeStaged = orig })
				hit := false
				writeStaged = func(f *os.File, data []byte) error {
					if !strings.HasPrefix(filepath.Base(f.Name()), "."+failing+".tmp-") {
						return orig(f, data)
					}
					hit = true
					if _, err := f.Write(data[:len(data)/2]); err != nil {
						return err
					}
					return errDiskFull
				}

				_, err := merge()
				if !hit {
					t.Fatalf("%s was never staged", failing)
				}
				if !errors.Is(err, errDiskFull) {
					t.Fatalf("merge error %v, want the injected write failure", err)
				}
				if after := runDirEntries(t, resultsDir); !slices.Equal(after, before) {
					t.Errorf("failed merge changed the run dir:\n before %q\n after  %q", before, after)
				}
			})
		}
	}
}

// TestMergeRollsBackTheSidecarWhenResultsCannotLand: the sidecar is renamed
// into place first, so a results document that then cannot land must take
// the new sidecar back out. Otherwise a results document already at that
// name (a re-merge) would publish with a sidecar from another merge.
func TestMergeRollsBackTheSidecarWhenResultsCannotLand(t *testing.T) {
	for _, r := range pairRuns() {
		t.Run(r.name, func(t *testing.T) {
			resultsDir := t.TempDir()
			merge := r.setup(t, resultsDir)
			if err := os.Mkdir(filepath.Join(resultsDir, r.resultsName), 0o755); err != nil {
				t.Fatal(err)
			}
			before := runDirEntries(t, resultsDir)

			if _, err := merge(); err == nil {
				t.Fatalf("merge succeeded with a directory at the results path %s", r.resultsName)
			}

			if _, err := os.Lstat(filepath.Join(resultsDir, r.sidecarName)); err == nil {
				t.Errorf("%s is in place although %s could not land", r.sidecarName, r.resultsName)
			}
			if after := runDirEntries(t, resultsDir); !slices.Equal(after, before) {
				t.Errorf("failed merge changed the run dir:\n before %q\n after  %q", before, after)
			}
		})
	}
}

// TestMergeLeavesExactlyThePair: a merge that succeeds adds the results
// document and its sidecar to the run dir and nothing else (no staged
// temp file survives a commit), both at os.WriteFile's historical 0644.
func TestMergeLeavesExactlyThePair(t *testing.T) {
	for _, r := range pairRuns() {
		t.Run(r.name, func(t *testing.T) {
			resultsDir := t.TempDir()
			merge := r.setup(t, resultsDir)
			before := runDirEntries(t, resultsDir)

			out, err := merge()
			if err != nil {
				t.Fatal(err)
			}
			if out != filepath.Join(resultsDir, r.resultsName) {
				t.Errorf("merge returned %s, want %s", out, filepath.Join(resultsDir, r.resultsName))
			}
			want := slices.Sorted(slices.Values(append(slices.Clone(before), r.resultsName, r.sidecarName)))
			if after := runDirEntries(t, resultsDir); !slices.Equal(after, want) {
				t.Errorf("run dir after merge:\n got  %q\n want %q", after, want)
			}
			for _, n := range []string{r.resultsName, r.sidecarName} {
				st, err := os.Stat(filepath.Join(resultsDir, n))
				if err != nil {
					t.Fatal(err)
				}
				if !st.Mode().IsRegular() || st.Mode().Perm() != 0o644 || st.Size() == 0 {
					t.Errorf("%s: mode %v size %d, want a non-empty regular 0644 file", n, st.Mode(), st.Size())
				}
			}
		})
	}
}
