package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// probatorium#411 broke every Benchmark Tier dispatch with an apostrophe in
// a comment of a free-form shell block in ansible/tasks/run_bench_cell.yml:
// ansible could not load the file, and nothing in CI loads ansible files.
// The ansible job in test.yml now runs ansible/load-check.sh, which imports
// every tasks file statically so --syntax-check parses it (an include_tasks
// is never opened by --syntax-check). This pins that wiring, and that the
// job checks with the ansible the cluster runs: the ansible-core and
// ansible.posix pins of ansible/runner-setup.yml.
func TestAnsibleLoadCheckRunsInCI(t *testing.T) {
	wf, err := os.ReadFile(filepath.Join(".github", "workflows", "test.yml"))
	if err != nil {
		t.Fatalf("read test.yml: %v", err)
	}
	src := string(wf)
	start := regexp.MustCompile(`(?m)^  ansible:\n`).FindStringIndex(src)
	if start == nil {
		t.Fatal("test.yml has no ansible job: no CI step loads the ansible files the cluster runs")
	}
	job := src[start[1]:]
	if next := regexp.MustCompile(`(?m)^  [A-Za-z0-9_-]+:\n`).FindStringIndex(job); next != nil {
		job = job[:next[0]]
	}
	if !regexp.MustCompile(`(?m)^\s*run: bash ansible/load-check\.sh\s*$`).MatchString(job) {
		t.Error("the ansible job does not run `bash ansible/load-check.sh`")
	}

	setup, err := os.ReadFile(filepath.Join("ansible", "runner-setup.yml"))
	if err != nil {
		t.Fatalf("read runner-setup.yml: %v", err)
	}
	pin := func(key string) string {
		t.Helper()
		m := regexp.MustCompile(`(?m)^\s*` + key + `:\s*"([^"]+)"`).FindStringSubmatch(string(setup))
		if m == nil {
			t.Fatalf("runner-setup.yml has no %s pin", key)
		}
		return m[1]
	}
	for _, want := range []string{
		`"ansible-core==` + pin("ansible_core_version") + `"`,
		`"ansible.posix:==` + pin("ansible_posix_version") + `"`,
		"ansible.posix.git," + pin("ansible_posix_commit") + `"`,
	} {
		if !strings.Contains(job, want) {
			t.Errorf("the ansible job does not install %s, the cluster's pin (ansible/runner-setup.yml): "+
				"a load check with another ansible can pass a file the cluster cannot load", want)
		}
	}

	script, err := os.ReadFile(filepath.Join("ansible", "load-check.sh"))
	if err != nil {
		t.Fatalf("read ansible/load-check.sh: %v", err)
	}
	for _, want := range []string{
		"ansible-playbook --syntax-check",
		`"$here"/tasks/*.yml`,
		"ansible.builtin.import_tasks:",
		`"$here"/roles/*/tasks/*.yml`,
		"ansible.builtin.import_role:",
		"export ANSIBLE_DUPLICATE_YAML_DICT_KEY=error",
	} {
		if !strings.Contains(string(script), want) {
			t.Errorf("ansible/load-check.sh lacks %q: it would stop loading part of what the cluster loads", want)
		}
	}
}

// P2c dispatch 40235051270 died at task load on one apostrophe ("the SUT's port") inside the free-form shell block of run_bench_cell.yml. ansible/tests/test_split_args.py runs
// ansible's own splitter over every such string (with a self-test that it is live); the ansible job of test.yml must run both, and the script must cover the same files
// load-check.sh does.
func TestAnsibleSplitArgsGuardRunsInCI(t *testing.T) {
	wf, err := os.ReadFile(filepath.Join(".github", "workflows", "test.yml"))
	if err != nil {
		t.Fatalf("read test.yml: %v", err)
	}
	for _, want := range []string{"python3 ansible/tests/test_split_args.py --selftest", "python3 ansible/tests/test_split_args.py\n"} {
		if !strings.Contains(string(wf), want) {
			t.Errorf("test.yml does not run %q", strings.TrimSpace(want))
		}
	}
	py, err := os.ReadFile(filepath.Join("ansible", "tests", "test_split_args.py"))
	if err != nil {
		t.Fatalf("read test_split_args.py: %v", err)
	}
	for _, want := range []string{"from ansible.parsing.splitter import split_args", `"tasks", "*.yml"`, `"roles", "*", "tasks", "*.yml"`, `"*.yml"`, "--selftest"} {
		if !strings.Contains(string(py), want) {
			t.Errorf("test_split_args.py lacks %q", want)
		}
	}
}
