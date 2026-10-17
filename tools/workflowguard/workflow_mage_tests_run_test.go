package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The mage build tag is a coverage hole that has re-opened twice, and it
// hides more than a helper or two: eleven source files behind //go:build
// mage carry the bench driver, the release gate, the cluster orchestration,
// publish and the validate targets, plus the tests written against them.
// Lint compiles the magefiles but never runs their tests, so for a long
// while `go test ./...` executed none of them -- including the ones that
// pin the report schema version, the constant a rebase has already
// auto-merged to the wrong value once.
//
// Two things have to hold. The Test workflow must run the tagged suite,
// and ci-ok must depend on that job: a job nobody fans in is a job branch
// protection cannot require, which is how the schedule guard describes its
// own failure mode too. Asserting on ./... rather than on a package list is
// deliberate -- a mage-tagged test dropped into a third package must be
// covered by the command that is already there, not by someone
// remembering to widen it.
func TestMageTaggedTestsRunInCI(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "test.yml"))
	if err != nil {
		t.Fatalf("read test.yml: %v", err)
	}
	src := string(b)

	// The command itself: `go test` ... `-tags mage` ... `./...`, on one line.
	runsTagged := regexp.MustCompile(`(?m)^\s*run:\s*go test\b.*\s-tags mage\b.*\s\./\.\.\.\s*$`)
	if !runsTagged.MatchString(src) {
		t.Errorf("test.yml runs no `go test -tags mage ... ./...` step: every test " +
			"behind //go:build mage (the bench driver, the release gate, the cluster " +
			"orchestration, the schema-version pins) executes in no workflow")
	}

	// And the job carrying it has to be fanned into ci-ok, or branch
	// protection cannot require it and a red tagged suite merges anyway.
	needs := regexp.MustCompile(`(?m)^\s*needs:\s*\[([^\]]*)\]`)
	m := needs.FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("test.yml has no ci-ok needs list to check")
	}
	found := false
	for _, dep := range strings.Split(m[1], ",") {
		if strings.TrimSpace(dep) == "mage" {
			found = true
		}
	}
	if !found {
		t.Errorf("ci-ok needs=[%s] does not include the mage job: the tagged suite "+
			"could go red without failing the one required check", strings.TrimSpace(m[1]))
	}
}

// A guard for a suite that does not exist is a guard that passes for the
// wrong reason. If the last mage-tagged test is ever deleted, this says so
// rather than letting TestMageTaggedTestsRunInCI keep reporting success
// over an empty set.
func TestMageTaggedTestsStillExist(t *testing.T) {
	tag := regexp.MustCompile(`(?m)^//go:build mage\b`)
	var files []string
	err := filepath.WalkDir(".", func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			// Agent worktrees under .claude hold full copies of the repo.
			if name := d.Name(); name == ".git" || name == ".claude" || name == "results" {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, "_test.go") {
			return nil
		}
		b, rerr := os.ReadFile(path)
		if rerr != nil {
			return rerr
		}
		if tag.Match(b) {
			files = append(files, path)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk: %v", err)
	}
	if len(files) == 0 {
		t.Error("no //go:build mage test files found: TestMageTaggedTestsRunInCI is " +
			"now guarding an empty suite")
	}
	t.Logf("%d mage-tagged test file(s): %s", len(files), strings.Join(files, " "))
}
