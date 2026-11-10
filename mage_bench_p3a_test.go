//go:build mage

package main

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/p3a-rc1-20261110, not for merge). P3a: what the playbook receives from resolveBenchColumns for the loop-count columns (servers/p3a_arms.go): the three arms of a row are
// the SAME binary and engine and differ only by the column env (PROBATORIUM_WORKERS unset / 14 / 12, the sampler key on all of them; the guard arms g0 / g2 / g4 on get-json /
// epoll-h1-sync likewise); the hertz control is the hertz binary with no env;
// and none of them accepts a dispatch-wide sut_env.
func TestResolveBenchColumnsForP3a(t *testing.T) {
	const (
		workers = "PROBATORIUM_WORKERS"
		sched   = "PROBATORIUM_SCHED_SAMPLE"
	)
	var d1 []string
	for name := range servers.Registry {
		if strings.HasPrefix(name, "p3a-1-") {
			d1 = append(d1, name)
		}
	}
	sort.Strings(d1)
	if len(d1) != 49 {
		t.Fatalf("%d p3a-1- columns, want 49 (40 rated + 9 guard)", len(d1))
	}
	cols, err := resolveBenchColumns(strings.Join(d1, ","))
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 49 {
		t.Fatalf("resolveBenchColumns returned %d columns, want 49", len(cols))
	}
	seen := map[string]int{}
	for _, c := range cols {
		var arm, engine string
		for _, a := range []string{"-k0-", "-k2-", "-k4-", "-ctl-", "-g0-", "-g2-", "-g4-"} {
			if i := strings.LastIndex(c.Slug, a); i >= 0 {
				arm, engine = strings.Trim(a, "-"), c.Slug[i+len(a):]
			}
		}
		var wantBin, wantEngine string
		var wantEnv map[string]string
		switch {
		case engine == "hertz-h2":
			wantBin, wantEngine, wantEnv = "hertz", "h2c", map[string]string{}
		default:
			wantBin, wantEngine = "celeris", engine
			wantEnv = map[string]string{sched: "1"}
			switch arm {
			case "k2", "g2":
				wantEnv[workers] = "14"
			case "k4", "g4":
				wantEnv[workers] = "12"
			}
		}
		got := c.Env
		if len(got) == 0 {
			got = map[string]string{}
		}
		if c.Bin != wantBin || c.Engine != wantEngine || !reflect.DeepEqual(got, wantEnv) {
			t.Errorf("%s resolves to bin %s engine %s env %v, want %s %s %v", c.Slug, c.Bin, c.Engine, c.Env, wantBin, wantEngine, wantEnv)
		}
		if arm == "k0" || arm == "g0" {
			if _, has := c.Env[workers]; has {
				t.Errorf("%s: k0 / g0 must not set %s", c.Slug, workers)
			}
		}
		seen[arm+"/"+engine]++
		if err := refuseSUTEnvOnArmColumns([]string{c.Slug}, map[string]string{"A": "1"}); err == nil {
			t.Errorf("%s accepts a dispatch-wide sut_env", c.Slug)
		}
		if err := refuseSUTEnvOnArmColumns([]string{c.Slug}, map[string]string{workers: "8"}); err == nil {
			t.Errorf("%s accepts %s as a dispatch-wide override (it would turn k0 into another arm)", c.Slug, workers)
		}
	}
	if len(seen) != 11 {
		t.Errorf("%d (arm, engine) pairs in dispatch 1, want 11 (8 rated + 3 guard): %v", len(seen), seen)
	}
	for k, n := range seen {
		want := 5 // reps 1-5
		if strings.HasPrefix(k, "g") {
			want = 3 // the guard sub-blocks of reps 1, 3, 5
		}
		if n != want {
			t.Errorf("%s: %d columns in dispatch 1, want %d", k, n, want)
		}
	}
}
