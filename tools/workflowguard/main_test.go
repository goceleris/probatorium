package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The workflow guards read the repository's workflows, playbooks and scripts
// by repository-relative path (".github/workflows/test.yml",
// "ansible/validate.yml", ...). They used to live in the repository root,
// where `go test` runs a package with the root as its working directory;
// here it runs in tools/workflowguard. TestMain therefore moves the process
// to the repository root before any test runs, so every path the tests spell
// resolves exactly as it did, and a path added later is written the same way.
//
// The root is the nearest enclosing directory whose go.mod declares the
// probatorium module (not a nested module's go.mod, and not a stray one
// higher up the tree).
func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "workflowguard:", err)
		os.Exit(1)
	}
	if err := os.Chdir(root); err != nil {
		fmt.Fprintln(os.Stderr, "workflowguard: chdir to the repository root:", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func repoRoot() (string, error) {
	dir, err := os.Getwd()
	if err != nil {
		return "", err
	}
	for {
		b, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err == nil {
			for line := range strings.SplitSeq(string(b), "\n") {
				if strings.TrimSpace(line) == "module github.com/goceleris/probatorium" {
					return dir, nil
				}
			}
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("no go.mod declaring github.com/goceleris/probatorium at or above the test directory")
		}
		dir = parent
	}
}
