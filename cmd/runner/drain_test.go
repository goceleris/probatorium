package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// The drain sentinel must be overridable, because the test suite and any
// dev-machine run cannot write to /run/celeris.
func TestDrainFileHonoursEnvOverride(t *testing.T) {
	t.Setenv("PROBATORIUM_DRAIN_FILE", "/tmp/whatever/drain")
	if got := drainFile(); got != "/tmp/whatever/drain" {
		t.Fatalf("drainFile()=%q, want the env override", got)
	}
	t.Setenv("PROBATORIUM_DRAIN_FILE", "")
	if got := drainFile(); got != "/run/celeris/drain-requested" {
		t.Fatalf("drainFile()=%q, want the tmpfs default", got)
	}
}

// drainRequested is polled once per cell on a multi-hour run. It must report
// "no drain" for every condition other than "the file is there" -- a runner
// that abandoned a 74-hour matrix because /run hiccuped would be worse than
// the power loss it is defending against.
func TestDrainRequestedOnlyWhenSentinelExists(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "drain-requested")
	t.Setenv("PROBATORIUM_DRAIN_FILE", sentinel)

	if drainRequested() {
		t.Fatal("no sentinel, but drainRequested() reported a drain")
	}
	if err := os.WriteFile(sentinel, []byte("2026-09-09T12:00:00Z\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !drainRequested() {
		t.Fatal("sentinel present, but drainRequested() reported no drain")
	}

	// A directory at the path still counts as present: the hook creates a
	// file, and treating a surprise directory as "no drain" would silently
	// disable the guard.
	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sentinel, 0o755); err != nil {
		t.Fatal(err)
	}
	if !drainRequested() {
		t.Fatal("sentinel path exists as a directory; want drain reported")
	}
}

// acknowledgeDrain writes the two artefacts the rest of the system depends
// on: the ack that stops offbattery from cancelling a drain in progress, and
// the remaining-cell list a resumed run needs.
func TestAcknowledgeDrainWritesAckAndRemaining(t *testing.T) {
	runDir := t.TempDir()
	outDir := filepath.Join(t.TempDir(), "results")
	t.Setenv("PROBATORIUM_DRAIN_FILE", filepath.Join(runDir, "drain-requested"))

	acknowledgeDrain(Config{Out: outDir}, nil)

	if _, err := os.Stat(filepath.Join(runDir, "drain-acknowledged")); err != nil {
		t.Fatalf("drain-acknowledged not written: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(outDir, "drained-remaining.json"))
	if err != nil {
		t.Fatalf("drained-remaining.json not written: %v", err)
	}
	var doc struct {
		Reason    string `json:"reason"`
		CellsGlob string `json:"cells_glob"`
		Remaining []struct {
			Scenario string `json:"scenario"`
		} `json:"remaining"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("drained-remaining.json is not valid JSON: %v", err)
	}
	if doc.Reason != "ups-drain" {
		t.Fatalf("reason=%q, want ups-drain", doc.Reason)
	}
	// An empty remaining list must still marshal as [] rather than null, so
	// a consumer can iterate it without a nil check.
	if doc.Remaining == nil {
		t.Fatal("remaining marshalled as null; want an empty array")
	}
	if doc.CellsGlob != "" {
		t.Fatalf("cells_glob=%q for an empty remainder, want empty", doc.CellsGlob)
	}
}

// The resume input has to be usable as-is. BENCH_CELLS is a comma-separated
// list of path.Match patterns over the cell identifier "<scenario>/<server>",
// so acknowledgeDrain emits exactly that -- deduplicated, because the same
// scenario/server pair recurs once per run index and a repeated pattern would
// only pad the resume command.
func TestCellsGlobDeduplicatesInFirstSeenOrder(t *testing.T) {
	got := cellsGlob([]string{
		"plaintext/celeris-iouring",
		"plaintext/celeris-iouring", // same pair, next run index
		"json/celeris-epoll",
		"plaintext/celeris-iouring",
	})
	want := "plaintext/celeris-iouring,json/celeris-epoll"
	if got != want {
		t.Fatalf("cellsGlob = %q, want %q", got, want)
	}
	if cellsGlob(nil) != "" {
		t.Fatalf("cellsGlob(nil) = %q, want empty", cellsGlob(nil))
	}
}
