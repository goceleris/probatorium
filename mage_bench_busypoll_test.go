//go:build mage

package main

import (
	"reflect"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/c1-harness-h1h2h4h5-20261105, not for merge). H2 of the v1.6.0 attribution plan: the busy-poll-on arm is the SAME module and engine as the default arm, told apart
// only by the column env PROBATORIUM_SOCKET_BUSY_POLL=50us. This pins what the playbook receives: resolveBenchColumns (the path BenchTier takes) must hand the bpon column exactly the env
// the registry declares (and the default column none of it), the knob must pass the SUT env rules (the micro sign would not), and the knob key must not be forbidden.
func TestResolveBenchColumnsCarriesTheBusyPollKnobOnTheBponColumnOnly(t *testing.T) {
	const knob = "PROBATORIUM_SOCKET_BUSY_POLL"
	for _, prefix := range []string{"c1arm", "c2x86"} {
		var mainName, bponName string
		for name, a := range servers.Registry {
			switch {
			case strings.HasPrefix(name, prefix+"-") && strings.HasSuffix(name, "-main-io512") && strings.Contains(name, "-r1-"):
				mainName = a.Name
			case strings.HasPrefix(name, prefix+"-") && strings.HasSuffix(name, "-bpon-io512") && strings.Contains(name, "-r1-"):
				bponName = a.Name
			}
		}
		if mainName == "" || bponName == "" {
			t.Fatalf("%s: no main/bpon r1 column in the registry", prefix)
		}
		cols, err := resolveBenchColumns(mainName + "," + bponName)
		if err != nil {
			t.Fatal(err)
		}
		if len(cols) != 2 {
			t.Fatalf("%s: got %d columns, want 2", prefix, len(cols))
		}
		var m, b benchColumn
		for _, c := range cols {
			if c.Slug == mainName {
				m = c
			} else {
				b = c
			}
		}
		if m.Bin != b.Bin || m.Engine != b.Engine || m.Bin != "celeris" {
			t.Errorf("%s: the arms must share binary and engine and be the celeris module: %+v vs %+v", prefix, m, b)
		}
		if _, on := m.Env[knob]; on {
			t.Errorf("%s: the default column carries the knob: %v", prefix, m.Env)
		}
		wantB := map[string]string{knob: "50us"}
		wantM := map[string]string{}
		if prefix == "c1arm" {
			wantB["AB_IRQ_VECTOR_CPU"] = "1"
			wantM["AB_IRQ_VECTOR_CPU"] = "1"
		}
		sameEnv := func(got, want map[string]string) bool {
			return len(got) == len(want) && (len(want) == 0 || reflect.DeepEqual(got, want))
		}
		if !sameEnv(b.Env, wantB) || !sameEnv(m.Env, wantM) {
			t.Errorf("%s: env of main %v / bpon %v, want %v / %v", prefix, m.Env, b.Env, wantM, wantB)
		}
	}
	if sutEnvForbiddenKeys[knob] {
		t.Errorf("%s is a forbidden SUT env key: the bpon arm could not be dispatched", knob)
	}
	if err := validateSUTEnvMap(map[string]string{knob: "50us"}); err != nil {
		t.Errorf("50us does not pass the SUT env rules: %v", err)
	}
	if err := validateSUTEnvMap(map[string]string{knob: "50µs"}); err == nil {
		t.Error("50µs passes the SUT env rules: the charset changed, review servers/celeris/busypoll_knob.go and the validators")
	}
}
