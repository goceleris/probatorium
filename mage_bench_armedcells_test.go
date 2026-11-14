//go:build mage

package main

import (
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/c1-harness-h7fix2-20261105, not for merge). Review r6 F1: the pprof collector watches the FIRST measurement window of a column, so an armed column (c1pp-) must run exactly one
// scenario. That was proven for one glob string in servers/c1pp_arms_test.go and enforced nowhere; the plan's own LF-1 selection (driver-redis-pipeline/* plus driver-mc-*/*) selects all 14
// armed columns with four scenarios each. mage Bench now refuses such a dispatch before Deploy (refuseArmedColumnsOnManyScenarios).

func scopeOf(t *testing.T, competitors, cells string) []string {
	t.Helper()
	cs, err := cellsGlobServers(cells)
	if err != nil {
		t.Fatal(err)
	}
	return scopeSlugs(competitors, cs)
}

func TestArmedColumnsMustRunExactlyOneScenario(t *testing.T) {
	const lf1 = "driver-redis-pipeline/*,driver-mc-get/*,driver-mc-multiget/*,driver-mc-set/*" // plan section 3, step 2a, as written
	good := "driver-redis-pipeline/celeris-*,driver-mc-get/celeris-*,driver-mc-multiget/celeris-*,driver-mc-set/celeris-*,driver-redis-pipeline/c1pp-dump-*"
	for _, tc := range []struct {
		name, competitors, cells string
		wantErr                  string // substring of the refusal, "" = accepted
	}{
		{"the plan LF-1 string with the armed columns in scope", "", lf1, "would run 4 scenario(s)"},
		{"the same string with an explicit competitors CSV that names an armed column", "c1pp-dump-r1-main-epoll-h1-async,celeris-epoll-h1-async", lf1, "c1pp-dump-r1-main-epoll-h1-async would run 4"},
		{"empty cells select every scenario", "", "", "would run"},
		{"star cells", "", "*", "would run"},
		{"star over every cell", "", "*/*", "would run"},
		{"the working LF-1 shape: unarmed on /celeris-*, armed on the dump cell only", "", good, ""},
		{"dump columns on the memcached cell", "", "driver-mc-get/c1pp-dump-*", "must run exactly one of [driver-redis-pipeline]"},
		{"dump columns on two cells", "", "driver-redis-pipeline/c1pp-dump-*,driver-mc-get/c1pp-dump-*", "would run 2"},
		{"profile columns on the redis cell", "", "driver-redis-pipeline/c1pp-profile-*", "must run exactly one of [driver-mc-get driver-mc-multiget driver-mc-set]"},
		{"profile columns on all three memcached cells", "", "driver-mc-*/c1pp-profile-*", "would run 3"},
		{"profile columns on driver-mc-get only", "", "driver-mc-get/c1pp-profile-*", ""},
		{"profile columns on driver-mc-set only", "", "driver-mc-set/c1pp-profile-*", ""},
		{"an exclusion that leaves one scenario is fine", "", "driver-redis-pipeline/c1pp-dump-*,driver-mc-get/c1pp-dump-*,!driver-mc-get/*", ""},
		{"no armed column in scope: nothing to refuse", "celeris-epoll-h1-async", "*/*", ""},
		{"unarmed globs only", "", "driver-redis-pipeline/celeris-*,driver-mc-*/celeris-*,driver-mc-*/baseline-*", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			err := refuseArmedColumnsOnManyScenarios(scopeOf(t, tc.competitors, tc.cells), tc.cells)
			switch {
			case tc.wantErr == "" && err != nil:
				t.Errorf("refused: %v", err)
			case tc.wantErr != "" && (err == nil || !strings.Contains(err.Error(), tc.wantErr)):
				t.Errorf("err = %v, want a refusal containing %q", err, tc.wantErr)
			}
		})
	}
	// the refusal names a way out
	err := refuseArmedColumnsOnManyScenarios(scopeOf(t, "", lf1), lf1)
	if err == nil || !strings.Contains(err.Error(), "cells=driver-redis-pipeline/c1pp-dump-*") {
		t.Errorf("the refusal must name the working cells shape: %v", err)
	}
	// every registered armed column passes with its own cell, and only with it
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "c1pp-") {
			continue
		}
		mode, _, _ := strings.Cut(strings.TrimPrefix(name, "c1pp-"), "-")
		cell := map[string]string{"dump": "driver-redis-pipeline", "profile": "driver-mc-get"}[mode]
		if err := refuseArmedColumnsOnManyScenarios([]string{name}, cell+"/"+name); err != nil {
			t.Errorf("%s with its own cell: %v", name, err)
		}
		if err := refuseArmedColumnsOnManyScenarios([]string{name}, "driver-*/"+name); err == nil {
			t.Errorf("%s with driver-*: want a refusal", name)
		}
	}
	if err := refuseArmedColumnsOnManyScenarios([]string{"c1pp-both-r1-main-epoll-h1-async"}, "driver-redis-pipeline/*"); err == nil {
		t.Error("an unknown armed mode must be refused")
	}
}

// Bench calls the refusal before the auto-deploy, right after the sut_env refusal, on the same scope.
func TestBenchRefusesArmedColumnsBeforeDeploy(t *testing.T) {
	src := readRepoFile(t, "mage_bench.go")
	call := "refuseArmedColumnsOnManyScenarios(scopeSlugs(competitors, cellsSlugs), cells)"
	i, j, k := strings.Index(src, "refuseSUTEnvOnArmColumns(scopeSlugs(competitors, cellsSlugs), sutEnv)"), strings.Index(src, call), strings.Index(src, "Auto-deploy when neither bench target")
	if i < 0 || j < 0 || k < 0 || !(i < j && j < k) {
		t.Fatalf("the armed-column refusal is not called between the sut_env refusal and the auto-deploy (%d %d %d)", i, j, k)
	}
}
