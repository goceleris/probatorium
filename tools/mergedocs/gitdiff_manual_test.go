package main

import (
	"os"
	"testing"
)

// Manual: MERGEDOCS_HARNESS_REPO=<checkout> MERGEDOCS_PIN_A=<sha> MERGEDOCS_PIN_B=<sha>
// runs the real git wrapper on two commits that must differ only by a celeris
// pin bump.
func TestGitHarnessDiffManual(t *testing.T) {
	repo, a, b := os.Getenv("MERGEDOCS_HARNESS_REPO"), os.Getenv("MERGEDOCS_PIN_A"), os.Getenv("MERGEDOCS_PIN_B")
	if repo == "" || a == "" || b == "" {
		t.Skip("MERGEDOCS_HARNESS_REPO, MERGEDOCS_PIN_A, MERGEDOCS_PIN_B not set")
	}
	names, err := gitHarnessDiff(repo)(a, b)
	if err != nil {
		t.Fatal(err)
	}
	if why := classifyHarnessDiff(names); why != "" {
		t.Fatalf("%s..%s: %s", a, b, why)
	}
	t.Logf("%s..%s differ only in go.mod and go.sum under servers/celeris/ and validation/refapp/ (%d files)", a, b, len(names))
	if _, err := gitHarnessDiff(repo)(a, "no-such-ref"); err == nil {
		t.Fatal("a bad ref must be an error")
	}
}
