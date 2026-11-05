//go:build mage

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/c1-harness-h6h7-20261105, not for merge). H6 of the v1.6.0 attribution plan: the #858 ladder columns (servers/c2lad_arms.go). What the playbook receives from
// resolveBenchColumns (the path BenchTier takes): the candidate's bp-off and bp-on slots are the SAME binary and engine and differ only by the column env; a sha-pinned slot runs its own
// module and carries no env; and none of them accepts a dispatch-wide sut_env.
func TestResolveBenchColumnsForTheLadder(t *testing.T) {
	const knob = "PROBATORIUM_SOCKET_BUSY_POLL"
	find := func(slot, engine string) string {
		var best string
		for name := range servers.Registry {
			if strings.HasPrefix(name, "c2lad-") && strings.Contains(name, "-r1-"+slot+"-"+engine) && (best == "" || name < best) {
				best = name
			}
		}
		if best == "" {
			t.Fatalf("no c2lad r1 column for slot %s engine %s", slot, engine)
		}
		return best
	}
	const eng = "epoll-h1-async"
	mainCol, bponCol, oldCol := find("fcmain", eng), find("fcbpon", eng), find("v158", eng)
	cols, err := resolveBenchColumns(strings.Join([]string{mainCol, bponCol, oldCol}, ","))
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 3 {
		t.Fatalf("got %d columns, want 3", len(cols))
	}
	by := map[string]benchColumn{}
	for _, c := range cols {
		by[c.Slug] = c
	}
	m, b, o := by[mainCol], by[bponCol], by[oldCol]
	if m.Bin != "celeris" || b.Bin != "celeris" || m.Engine != eng || b.Engine != eng {
		t.Errorf("the candidate slots must be the celeris module on %s: %+v / %+v", eng, m, b)
	}
	if len(m.Env) != 0 || !reflect.DeepEqual(b.Env, map[string]string{knob: "50us"}) {
		t.Errorf("env of fcmain %v / fcbpon %v, want none / only %s=50us", m.Env, b.Env, knob)
	}
	if o.Bin != "celeris_v158" || o.Engine != eng || len(o.Env) != 0 {
		t.Errorf("the v1.5.8 slot resolves to %+v, want bin celeris_v158, engine %s, no env", o, eng)
	}
	for _, c := range cols {
		if err := refuseSUTEnvOnArmColumns([]string{c.Slug}, map[string]string{"A": "1"}); err == nil {
			t.Errorf("%s accepts a dispatch-wide sut_env", c.Slug)
		}
	}
}
