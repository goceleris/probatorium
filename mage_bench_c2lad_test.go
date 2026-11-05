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

// H7' armed columns (servers/c1pp_arms.go): the playbook receives exactly the gate as the column env, and a dispatch-wide sut_env can neither arm anything (the key is refused) nor ride with them.
func TestResolveBenchColumnsForTheArmedPprofColumns(t *testing.T) {
	var dump, prof string
	for name := range servers.Registry {
		if strings.HasPrefix(name, "c1pp-dump-r1-main-epoll-h1-async") {
			dump = name
		}
		if strings.HasPrefix(name, "c1pp-profile-r1-main-epoll-h1-async") {
			prof = name
		}
	}
	if dump == "" || prof == "" {
		t.Fatal("no c1pp r1 main epoll-h1-async dump/profile column")
	}
	cols, err := resolveBenchColumns(dump + "," + prof)
	if err != nil || len(cols) != 2 {
		t.Fatalf("%v, %d columns", err, len(cols))
	}
	for _, c := range cols {
		want := map[string]string{"PROBATORIUM_PPROF": "dump"}
		if c.Slug == prof {
			want["PROBATORIUM_PPROF"] = "profile"
		}
		if c.Bin != "celeris" || c.Engine != "epoll-h1-async" || !reflect.DeepEqual(c.Env, want) {
			t.Errorf("%s resolves to %+v, want bin celeris, engine epoll-h1-async, env %v", c.Slug, c, want)
		}
		if err := validateSUTEnvMap(c.Env); err != nil {
			t.Errorf("%s: %v", c.Slug, err)
		}
		if err := refuseSUTEnvOnArmColumns([]string{c.Slug}, map[string]string{"A": "1"}); err == nil {
			t.Errorf("%s accepts a dispatch-wide sut_env", c.Slug)
		}
	}
	// the gate is refused in sut_env for ANY column, ordinary ones included
	for _, slug := range []string{"celeris-epoll-h1-async", dump} {
		if err := refuseSUTEnvOnArmColumns([]string{slug}, map[string]string{"PROBATORIUM_PPROF": "dump"}); err == nil || !strings.Contains(err.Error(), "PROBATORIUM_PPROF") {
			t.Errorf("sut_env PROBATORIUM_PPROF on %s: err = %v, want a refusal", slug, err)
		}
	}
	if _, err := parseSUTEnv("PROBATORIUM_PPROF=dump"); err != nil {
		t.Errorf("the key itself must stay a legal SUT env key for a registry column: %v", err)
	}
}
