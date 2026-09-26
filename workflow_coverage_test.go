package main

import (
	"os"
	"regexp"
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
// top level, and id-token: write appears on exactly one job.
func TestCoverageWorkflowScopesTheIDToken(t *testing.T) {
	src := readCoverageWorkflow(t)

	if !regexp.MustCompile(`(?m)^permissions: \{\}\s*$`).MatchString(src) {
		t.Error("top-level permissions must be {} so every job states what it needs")
	}
	if n := len(regexp.MustCompile(`(?m)^\s+id-token: write\b`).FindAllString(src, -1)); n != 1 {
		t.Errorf("id-token: write appears %d times, want exactly 1 (the Codecov upload job)", n)
	}
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
