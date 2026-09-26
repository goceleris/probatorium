package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

const coverageWorkflow = ".github/workflows/coverage.yml"

// The coverage workflow runs on every push and pull request, so it must stay
// off the self-hosted bench cluster (celeris#690). A job there would run
// pull-request code on the cluster hosts, and joining matrix-tier-cluster
// would queue it behind a nightly, or displace a queued one, since GitHub
// keeps only one pending run per concurrency group.
func TestCoverageWorkflowStaysOffTheCluster(t *testing.T) {
	src := readCoverageWorkflow(t)

	runsOn := regexp.MustCompile(`(?m)^\s*runs-on:\s*(.+?)\s*$`).FindAllStringSubmatch(src, -1)
	if len(runsOn) == 0 {
		t.Fatal("no runs-on found: this guard would pass without checking a job")
	}
	for _, m := range runsOn {
		if !strings.HasPrefix(m[1], "ubuntu-") {
			t.Errorf("runs-on: %s -- the coverage jobs must use a GitHub-hosted ubuntu-* runner, "+
				"never the self-hosted cluster (self-hosted, celeris-cluster, msr1)", m[1])
		}
	}

	if strings.Contains(src, "matrix-tier-cluster") {
		t.Error("the coverage workflow names the matrix-tier-cluster concurrency group; " +
			"only the cluster tiers may claim it")
	}
	if strings.Contains(src, "pull_request_target") {
		t.Error("pull_request_target runs pull-request code with the base repository's rights; " +
			"the coverage workflow uses pull_request")
	}
}

// Only the upload may mint an OIDC token: the workflow grants nothing at the
// top level, and id-token: write is granted to exactly the job that runs the
// Codecov action.
func TestCoverageWorkflowScopesTheIDToken(t *testing.T) {
	src := readCoverageWorkflow(t)

	if !regexp.MustCompile(`(?m)^permissions: \{\}\s*$`).MatchString(src) {
		t.Error("top-level permissions must be {} so every job states what it needs")
	}

	upload := jobsWithLine(src, `^\s+(- )?uses: codecov/codecov-action@`)
	if len(upload) != 1 {
		t.Fatalf("the Codecov action runs in jobs %v, want exactly one", upload)
	}
	if granted := jobsWithLine(src, `^\s+id-token: write\s*$`); !slices.Equal(granted, upload) {
		t.Errorf("id-token: write is granted to jobs %v, want exactly the upload job %v", granted, upload)
	}
}

// jobsWithLine returns, in order, the keys of the jobs whose body holds a line
// matching pattern. Lines outside `jobs:` belong to no job and are ignored.
func jobsWithLine(src, pattern string) []string {
	re := regexp.MustCompile(pattern)
	header := regexp.MustCompile(`^  ([A-Za-z0-9_-]+):\s*$`)
	var jobs []string
	inJobs, job := false, ""
	for line := range strings.SplitSeq(src, "\n") {
		switch {
		case line == "jobs:":
			inJobs, job = true, ""
			continue
		case line != "" && line[0] != ' ':
			inJobs, job = false, ""
			continue
		}
		if !inJobs {
			continue
		}
		if m := header.FindStringSubmatch(line); m != nil {
			job = m[1]
			continue
		}
		if job != "" && re.MatchString(line) && !slices.Contains(jobs, job) {
			jobs = append(jobs, job)
		}
	}
	return jobs
}

// readCoverageWorkflow returns the workflow with its YAML comments removed.
// The header comment explains what the workflow must never do, in the same
// words these tests look for, so they must read only the code.
func readCoverageWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(coverageWorkflow)
	if err != nil {
		t.Fatalf("read %s: %v", coverageWorkflow, err)
	}
	trailing := regexp.MustCompile(`\s+#\s.*$`)
	var code []string
	for line := range strings.SplitSeq(string(b), "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		code = append(code, trailing.ReplaceAllString(line, ""))
	}
	return strings.Join(code, "\n")
}
