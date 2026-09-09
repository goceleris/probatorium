package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeCell(t *testing.T, root string, runIdx int, scenario, server, status string) {
	t.Helper()
	dir := filepath.Join(root, "run"+itoaT(runIdx), scenario)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	b, err := json.Marshal(cellResultFile{
		RunIdx: runIdx, ScenarioName: scenario, ServerName: server, Status: status,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, server+".json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

func itoaT(i int) string {
	if i == 0 {
		return "0"
	}
	var b []byte
	for i > 0 {
		b = append([]byte{byte('0' + i%10)}, b...)
		i /= 10
	}
	return string(b)
}

// The distinction the whole feature turns on: markCellsInterrupted writes a
// per-cell JSON for cells that never ran, classified dnf. Treating "the file
// exists" as done would skip exactly the cells a resume must run.
func TestCellIsFinalTreatsDNFAsNotDone(t *testing.T) {
	for _, tc := range []struct {
		status string
		final  bool
		why    string
	}{
		{"ok", true, "produced a real measurement"},
		{"suspect", true, "kept its samples; the flag is integrity, not absence"},
		{"not_applicable", true, "adapter genuinely cannot serve it; another window will not change that"},
		{"", true, "legacy-OK, which every reader already treats as OK"},
		{"dnf", false, "never produced a measurement -- this is what interrupted cells get"},
		{"weird-future-status", false, "unknown must not silently shrink the matrix"},
	} {
		if got := cellIsFinal(tc.status); got != tc.final {
			t.Errorf("cellIsFinal(%q)=%v, want %v (%s)", tc.status, got, tc.final, tc.why)
		}
	}
}

func TestCompletedCellsSkipsDNFAndUnparseable(t *testing.T) {
	root := t.TempDir()
	writeCell(t, root, 0, "get-json", "celeris-iouring", "ok")
	writeCell(t, root, 0, "get-json", "celeris-epoll", "dnf")
	writeCell(t, root, 1, "post-4k", "celeris-iouring", "suspect")
	writeCell(t, root, 1, "post-4k", "celeris-epoll", "not_applicable")

	// A truncated file: a resume that skipped it would put a hole in the
	// matrix that the gate reads as a missing cell.
	bad := filepath.Join(root, "run2", "ws-echo")
	if err := os.MkdirAll(bad, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "celeris-std.json"), []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}

	done, err := completedCells(root)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]bool{
		"0/get-json/celeris-iouring": true,
		"1/post-4k/celeris-iouring":  true,
		"1/post-4k/celeris-epoll":    true,
	}
	for k := range want {
		if _, ok := done[k]; !ok {
			t.Errorf("expected %q to count as final", k)
		}
	}
	for _, k := range []string{"0/get-json/celeris-epoll", "2/ws-echo/celeris-std"} {
		if _, ok := done[k]; ok {
			t.Errorf("%q must NOT count as final", k)
		}
	}
	if len(done) != len(want) {
		t.Fatalf("completedCells returned %d entries, want %d: %v", len(done), len(want), done)
	}
}

// Run index is part of the identity: the matrix is interleaved, so the same
// scenario/server pair recurs once per run and each occurrence is its own
// measurement. Skipping run 1 because run 0 finished would silently reduce
// the sample count.
func TestCompletedCellsIsPerRunIndex(t *testing.T) {
	root := t.TempDir()
	writeCell(t, root, 0, "get-json", "celeris-iouring", "ok")

	done, err := completedCells(root)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := done["0/get-json/celeris-iouring"]; !ok {
		t.Fatal("run 0 should be final")
	}
	if _, ok := done["1/get-json/celeris-iouring"]; ok {
		t.Fatal("run 1 must NOT be inferred from run 0")
	}
}

// A missing directory is the ordinary "nothing was rescued" case. Refusing to
// start would be worse than running the full matrix.
func TestDropCompletedCellsToleratesMissingDir(t *testing.T) {
	got, err := dropCompletedCells(nil, filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("missing resume dir should not be an error, got %v", err)
	}
	if got != nil {
		t.Fatalf("expected the schedule back unchanged, got %v", got)
	}
}
