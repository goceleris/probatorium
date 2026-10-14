//go:build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Mage runs every target with the repository root as its working directory
// (see the first comment of magefile.go for the layout), and the code in this
// package reads and writes repository-relative paths ("go.mod", "servers/",
// "results/", "ansible/", ...). `go test` instead runs a package in its own
// directory, which here is magefiles/. TestMain moves the test process to the
// repository root before any test runs, so the tests exercise that code in the
// same working directory a target has, and the paths the tests spell are the
// ones they always spelled.
//
// The root is the nearest enclosing directory whose go.mod declares the
// probatorium module.
func TestMain(m *testing.M) {
	root, err := repoRoot()
	if err != nil {
		fmt.Fprintln(os.Stderr, "magefiles:", err)
		os.Exit(1)
	}
	if err := os.Chdir(root); err != nil {
		fmt.Fprintln(os.Stderr, "magefiles: chdir to the repository root:", err)
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
