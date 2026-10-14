package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunBenchCellForwardsTheRatedCells is probatorium#418: mage Bench hands
// ansible the model's rated cells as bench_rated_cells, and the runner only
// rates the cells matching -rated-cells. The rated branch of the runner
// invocation in run_bench_cell.yml must pass it on, single-quoted like -cells
// (the glob carries '*'); without it every clean cell of a rated run gets the
// rated sweep again.
func TestRunBenchCellForwardsTheRatedCells(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("ansible", "tasks", "run_bench_cell.yml"))
	if err != nil {
		t.Fatalf("read run_bench_cell.yml: %v", err)
	}
	var rated []string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "-rated -rated-duration") {
			rated = append(rated, strings.TrimSpace(line))
		}
	}
	if len(rated) != 1 {
		t.Fatalf("want exactly one runner line turning the rated sweep on, found %d: %q", len(rated), rated)
	}
	line := rated[0]
	const want = `-rated-cells '{{ bench_rated_cells }}'`
	i := strings.Index(line, want)
	if i < 0 {
		t.Fatalf("the rated runner flags do not forward bench_rated_cells as %s:\n%s", want, line)
	}
	if j := strings.Index(line, "{% if bench_rated |"); j < 0 || j > i {
		t.Errorf("-rated-cells must sit inside the rated branch:\n%s", line)
	}
}
