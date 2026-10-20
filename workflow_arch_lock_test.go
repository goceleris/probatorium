package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRunBenchCellPassesTheArchLockToTheRunner is probatorium#442: mage Bench
// hands ansible bench_client_lock / bench_client_lock_policy /
// bench_cell_order for a both-arch run, and the runner only holds the client
// lock (and walks its cells in reverse) when the runner invocation in
// run_bench_cell.yml passes them on. Without that line the lock never engages
// and the two arches overlap again, with every unit test still green.
func TestRunBenchCellPassesTheArchLockToTheRunner(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("ansible", "tasks", "run_bench_cell.yml"))
	if err != nil {
		t.Fatalf("read run_bench_cell.yml: %v", err)
	}
	var lines []string
	for _, line := range strings.Split(string(src), "\n") {
		if strings.Contains(line, "-client-lock ") {
			lines = append(lines, strings.TrimSpace(line))
		}
	}
	if len(lines) != 1 {
		t.Fatalf("want exactly one runner line passing -client-lock, found %d: %q", len(lines), lines)
	}
	line := lines[0]
	for _, want := range []string{
		`{% if bench_client_lock is defined and bench_client_lock %}`,
		`-client-lock '{{ bench_client_lock }}'`,
		`-client-lock-policy {{ bench_client_lock_policy | default("heavy") }}`,
		`-cell-order reverse`,
	} {
		if !strings.Contains(line, want) {
			t.Errorf("the runner line lacks %s:\n%s", want, line)
		}
	}
	// ansible's free-form splitter refuses a scalar with an odd number of quotes.
	if n := strings.Count(line, "'"); n%2 != 0 {
		t.Errorf("odd number of single quotes (%d) in the runner line:\n%s", n, line)
	}
	if n := strings.Count(line, `"`); n%2 != 0 {
		t.Errorf("odd number of double quotes (%d) in the runner line:\n%s", n, line)
	}
	// the line must sit inside the runner command: the next line is its -services flag.
	all := strings.Split(string(src), "\n")
	for k, l := range all {
		if strings.TrimSpace(l) == line {
			if k+1 >= len(all) || !strings.Contains(all[k+1], "-services none") {
				t.Errorf("the -client-lock line must be followed by -services none, inside the runner command")
			}
		}
	}
}
