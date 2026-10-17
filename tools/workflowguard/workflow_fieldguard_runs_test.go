package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// probatorium#395's guard, TestEveryFieldReadFromASnapshotHasAWriter, lives
// in a module of its own (validation/properties/fieldguard) so that
// golang.org/x/tools stays out of this module and the twelve adapter modules
// that replace it. The price is that `go test ./...` here never reaches it:
// the fieldguard job in test.yml is the only thing that runs it. This pins
// that wiring. The job must run the nested module verbose and tally the
// result, ci-ok must wait on it, and its list of required tests must name
// every test function the module defines -- a guard or control added
// without being required, or dropped from the list, would otherwise stop
// being enforced without anything turning red.
func TestFieldGuardRunsInCI(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "test.yml"))
	if err != nil {
		t.Fatalf("read test.yml: %v", err)
	}
	src := string(b)

	// The job block: from its key to the next job key (or the end).
	start := regexp.MustCompile(`(?m)^  fieldguard:\n`).FindStringIndex(src)
	if start == nil {
		t.Fatal("test.yml has no fieldguard job: the Snapshot field guard runs nowhere")
	}
	job := src[start[1]:]
	if next := regexp.MustCompile(`(?m)^  [A-Za-z0-9_-]+:\n`).FindStringIndex(job); next != nil {
		job = job[:next[0]]
	}
	if !strings.Contains(job, "working-directory: validation/properties/fieldguard\n") {
		t.Error("the fieldguard job does not run in validation/properties/fieldguard")
	}
	if !regexp.MustCompile(`(?m)^\s*go test\b[^\n]*\s-v\s[^\n]*\s\./\.\.\.\s`).MatchString(job) {
		t.Error("the fieldguard job does not run `go test ... -v ... ./...`: without -v there " +
			"are no PASS lines to tally")
	}
	if !strings.Contains(job, `'--- SKIP: '`) {
		t.Error("the fieldguard job's tally no longer counts SKIP lines: a skipped control " +
			"would pass as green")
	}

	needs := regexp.MustCompile(`(?m)^\s*needs:\s*\[([^\]]*)\]`).FindStringSubmatch(src)
	if needs == nil {
		t.Fatal("test.yml has no ci-ok needs list to check")
	}
	fanned := false
	for dep := range strings.SplitSeq(needs[1], ",") {
		if strings.TrimSpace(dep) == "fieldguard" {
			fanned = true
		}
	}
	if !fanned {
		t.Errorf("ci-ok needs=[%s] does not include fieldguard: the guard could go red "+
			"without failing the one required check", strings.TrimSpace(needs[1]))
	}

	files, err := filepath.Glob(filepath.Join("validation", "properties", "fieldguard", "*_test.go"))
	if err != nil || len(files) == 0 {
		t.Fatalf("no test files in validation/properties/fieldguard (err=%v): the job "+
			"would guard an empty module", err)
	}
	testFunc := regexp.MustCompile(`(?m)^func (Test\w*)\(t \*testing\.T\)`)
	var names []string
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatalf("read %s: %v", f, err)
		}
		for _, m := range testFunc.FindAllStringSubmatch(string(b), -1) {
			names = append(names, m[1])
		}
	}
	if len(names) == 0 {
		t.Fatal("validation/properties/fieldguard defines no tests")
	}
	for _, n := range names {
		listed := regexp.MustCompile(`(?m)^\s+` + regexp.QuoteMeta(n) + `(?:\s+\\)?\n`)
		if !listed.MatchString(job) {
			t.Errorf("%s is defined in validation/properties/fieldguard but not in the "+
				"fieldguard job's required list, so nothing fails if it stops running", n)
		}
	}
	t.Logf("fieldguard job requires the module's %d test functions: %s", len(names), strings.Join(names, " "))
}
