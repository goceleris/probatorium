//go:build mage

package main

import "testing"

// TestClusterLimitForTarget guards the property that actually matters: a bench
// scoped to one arch must never select the other arch's host, so that host can
// be rebooted or powered off mid-run without aborting the bench.
//
// Regression context (2026-08-27): bench.yml filtered non-target hosts with
// `meta: end_host`, but that is a task — ansible connects first. An amd64-only
// bench therefore still opened SSH to the arm64 box, and rebooting that box for
// unrelated work killed a 34-hour run at 63% completion via any_errors_fatal.
func TestClusterLimitForTarget(t *testing.T) {
	for _, tc := range []struct {
		name   string
		target string
		want   string
	}{
		{"single amd64 target excludes the arm64 host", "msa2-server", "msa2-server,msa2-client"},
		{"single arm64 target excludes the amd64 host", "msr1", "msr1,msa2-client"},
		{"both means no limit", "both", ""},
		{"all means no limit", "all", ""},
		{"empty means no limit", "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := clusterLimitForTarget(tc.target); got != tc.want {
				t.Errorf("clusterLimitForTarget(%q) = %q, want %q", tc.target, got, tc.want)
			}
		})
	}
}

// The loadgen must always be selected: it drives every bench regardless of which
// target is under test, and bench.yml hardcodes the same name in its end_host
// filter. If these ever diverge the bench silently loses its load generator.
func TestClusterLimitAlwaysIncludesLoadgen(t *testing.T) {
	for _, target := range []string{"msa2-server", "msr1"} {
		got := clusterLimitForTarget(target)
		if got == "" {
			t.Fatalf("clusterLimitForTarget(%q) returned no limit", target)
		}
		if !contains(got, loadgenHost) {
			t.Errorf("clusterLimitForTarget(%q) = %q, must include loadgen %q", target, got, loadgenHost)
		}
	}
}

func contains(hay, needle string) bool {
	for i := 0; i+len(needle) <= len(hay); i++ {
		if hay[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}
