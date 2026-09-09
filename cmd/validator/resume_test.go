package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/goceleris/probatorium/report"
)

func writeDoc(t *testing.T, dir string, cells []report.ValidationCellResult) {
	t.Helper()
	doc := report.Document{
		SchemaVersion: report.SchemaVersion,
		Validation:    &report.ValidationResults{Cells: cells},
	}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "validate-results.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// A cell recorded but empty is what an interrupted run leaves behind.
// Treating it as done is how a resume would quietly hand the absolute gate a
// matrix that is short a cell.
func TestMatrixCellIsFinalRequiresEvidenceOfWork(t *testing.T) {
	if matrixCellIsFinal(report.ValidationCellResult{Refapp: "kitchen_sink", Engine: "epoll"}) {
		t.Error("an empty cell must NOT count as final")
	}
	if !matrixCellIsFinal(report.ValidationCellResult{
		Tier1: &report.Tier1Summary{RequestsSent: 1000},
	}) {
		t.Error("a cell that sent requests must count as final")
	}
	if !matrixCellIsFinal(report.ValidationCellResult{PropertiesPassed: 3}) {
		t.Error("a cell with property verdicts must count as final")
	}
	if !matrixCellIsFinal(report.ValidationCellResult{PropertiesFailed: 1}) {
		t.Error("a FAILED property is still a verdict; re-running would hide it")
	}
}

func TestDropCompletedMatrixCells(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, []report.ValidationCellResult{
		{Refapp: "kitchen_sink", Engine: "iouring", Tier1: &report.Tier1Summary{RequestsSent: 500}},
		{Refapp: "kitchen_sink", Engine: "epoll"}, // recorded but never ran
		{Refapp: "observability", Engine: "std", PropertiesPassed: 2},
	})
	plan := []matrixCell{
		{Refapp: "kitchen_sink", Engine: "iouring"},
		{Refapp: "kitchen_sink", Engine: "epoll"},
		{Refapp: "observability", Engine: "std"},
		{Refapp: "driver_redis", Engine: "iouring"},
	}
	got, skipped := dropCompletedMatrixCells(plan, dir)
	if skipped != 2 {
		t.Fatalf("skipped=%d, want 2 (the two with evidence of work)", skipped)
	}
	if len(got) != 2 {
		t.Fatalf("remaining=%d, want 2: %+v", len(got), got)
	}
	want := map[string]bool{"kitchen_sink/epoll": true, "driver_redis/iouring": true}
	for _, mc := range got {
		if !want[mc.Refapp+"/"+mc.Engine] {
			t.Errorf("unexpected remaining cell %s/%s", mc.Refapp, mc.Engine)
		}
	}
}

// Refusing to start because there is nothing to resume from would be worse
// than simply running the full matrix.
func TestDropCompletedMatrixCellsToleratesMissingDoc(t *testing.T) {
	plan := []matrixCell{{Refapp: "kitchen_sink", Engine: "epoll"}}
	got, skipped := dropCompletedMatrixCells(plan, filepath.Join(t.TempDir(), "nope"))
	if skipped != 0 || len(got) != 1 {
		t.Fatalf("missing doc should leave the plan intact, got skipped=%d len=%d", skipped, len(got))
	}
}

func TestDrainRequestedHonoursEnvOverride(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "drain-requested")
	t.Setenv("PROBATORIUM_DRAIN_FILE", sentinel)
	if drainRequested() {
		t.Fatal("no sentinel, but a drain was reported")
	}
	if err := os.WriteFile(sentinel, []byte("now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !drainRequested() {
		t.Fatal("sentinel present, but no drain reported")
	}
}
