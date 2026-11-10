//go:build mage

package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/d757-<date>, not for merge). D757: what the playbook receives from resolveBenchColumns for the 26 columns of servers/d757_arms.go: a0 and dx are the SAME celeris binary and engine and
// differ only by the column env (the sampler key on both, the residence-stamp gate on dx); the hertz control is the hertz binary with no env; every column env passes validateSUTEnvMap (the SAME check the
// dispatch applies), and none of them accepts a dispatch-wide sut_env (the gate would turn a0 into dx).
func TestResolveBenchColumnsForD757(t *testing.T) {
	const (
		sched = "PROBATORIUM_SCHED_SAMPLE"
		gate  = "CELERIS_DIAG_RESID"
	)
	var names []string
	for name := range servers.Registry {
		if strings.HasPrefix(name, "d757-") {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	if len(names) != 26 {
		t.Fatalf("%d d757- columns, want 26", len(names))
	}
	cols, err := resolveBenchColumns(strings.Join(names, ","))
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 26 {
		t.Fatalf("resolveBenchColumns returned %d columns, want 26", len(cols))
	}
	seen := map[string]int{}
	for _, c := range cols {
		var arm string
		for _, a := range []string{"-a0-", "-dx-", "-ctl-"} {
			if strings.Contains(c.Slug, a) {
				arm = strings.Trim(a, "-")
			}
		}
		wantBin, wantEngine, wantEnv := "celeris", "epoll-auto+upg-async", map[string]string{sched: "1"}
		switch arm {
		case "dx":
			wantEnv[gate] = "1"
		case "ctl":
			wantBin, wantEngine, wantEnv = "hertz", "h2c", map[string]string{}
		}
		got := c.Env
		if len(got) == 0 {
			got = map[string]string{}
		}
		if c.Bin != wantBin || c.Engine != wantEngine || !reflect.DeepEqual(got, wantEnv) {
			t.Errorf("%s resolves to bin %s engine %s env %v, want %s %s %v", c.Slug, c.Bin, c.Engine, c.Env, wantBin, wantEngine, wantEnv)
		}
		if err := validateSUTEnvMap(got); err != nil {
			t.Errorf("%s: the column env does not pass validateSUTEnvMap: %v", c.Slug, err)
		}
		if err := refuseSUTEnvOnArmColumns([]string{c.Slug}, map[string]string{"A": "1"}); err == nil {
			t.Errorf("%s accepts a dispatch-wide sut_env", c.Slug)
		}
		if err := refuseSUTEnvOnArmColumns([]string{c.Slug}, map[string]string{gate: "1"}); err == nil {
			t.Errorf("%s accepts %s as a dispatch-wide override (it would turn a0 into dx)", c.Slug, gate)
		}
		seen[arm]++
	}
	if seen["a0"] != 10 || seen["dx"] != 10 || seen["ctl"] != 6 {
		t.Errorf("arms %v, want a0 10, dx 10, ctl 6", seen)
	}
}
