package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// ci-ok is the only required check that test.yml produces, so a job that is
// not in its needs list cannot block a merge: it can go red while ci-ok stays
// green. The validation zombie guard job was added without being listed
// (probatorium#415), which left its own checks (no SKIP, every required test
// printed PASS, the guard counted through /proc) outside the gate. This
// fails when any job in test.yml other than ci-ok is missing from ci-ok's
// needs, or when needs names a job that does not exist.
func TestCIOkNeedsEveryTestJob(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "test.yml"))
	if err != nil {
		t.Fatalf("read test.yml: %v", err)
	}
	src := string(b)
	start := regexp.MustCompile(`(?m)^jobs:\n`).FindStringIndex(src)
	if start == nil {
		t.Fatal("test.yml has no jobs: block")
	}
	jobsBlock := src[start[1]:]
	if end := regexp.MustCompile(`(?m)^\S`).FindStringIndex(jobsBlock); end != nil {
		jobsBlock = jobsBlock[:end[0]]
	}
	var jobs []string
	for _, m := range regexp.MustCompile(`(?m)^  ([A-Za-z0-9_-]+):[ \t]*$`).FindAllStringSubmatch(jobsBlock, -1) {
		jobs = append(jobs, m[1])
	}
	if !slices.Contains(jobs, "ci-ok") {
		t.Fatalf("test.yml has no ci-ok job (jobs: %v)", jobs)
	}

	ciOK := jobsBlock[regexp.MustCompile(`(?m)^  ci-ok:[ \t]*$`).FindStringIndex(jobsBlock)[0]:]
	if next := regexp.MustCompile(`(?m)^  [A-Za-z0-9_-]+:[ \t]*$`).FindAllStringIndex(ciOK, 2); len(next) == 2 {
		ciOK = ciOK[:next[1][0]]
	}
	m := regexp.MustCompile(`(?m)^    needs:\s*\[([^\]]*)\]`).FindStringSubmatch(ciOK)
	if m == nil {
		t.Fatal("ci-ok has no one-line needs: [...] list")
	}
	var needs []string
	for dep := range strings.SplitSeq(m[1], ",") {
		needs = append(needs, strings.TrimSpace(dep))
	}
	if !strings.Contains(ciOK, "if: always()") {
		t.Error("ci-ok is not `if: always()`: it would be skipped, and a skipped required check passes, when a job it needs fails")
	}

	for _, j := range jobs {
		if j != "ci-ok" && !slices.Contains(needs, j) {
			t.Errorf("job %q is not in ci-ok's needs %v: it can fail without blocking a merge", j, needs)
		}
	}
	for _, n := range needs {
		if !slices.Contains(jobs, n) {
			t.Errorf("ci-ok needs %q, which is not a job in test.yml (jobs: %v)", n, jobs)
		}
	}
	t.Logf("jobs %v; ci-ok needs %v", jobs, needs)
}
