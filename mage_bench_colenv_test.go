//go:build mage

package main

import (
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// withRegistryColumn registers a throwaway column for one test and removes
// it afterwards. Not parallel-safe with other Registry mutators; the tests
// using it do not call t.Parallel.
func withRegistryColumn(t *testing.T, a servers.Adapter) {
	t.Helper()
	if _, exists := servers.Registry[a.Name]; exists {
		t.Fatalf("column %q already registered", a.Name)
	}
	servers.Registry[a.Name] = a
	t.Cleanup(func() { delete(servers.Registry, a.Name) })
}

// TestResolveBenchColumnsCarriesColumnEnv (celeris#585): a column that
// shares its binary and -engine with another and differs only in a runtime
// knob (the SEND_ZC OFF arm next to the ON arm) must reach the playbook
// with that knob, or the two columns would bench the same configuration.
func TestResolveBenchColumnsCarriesColumnEnv(t *testing.T) {
	base := servers.Registry["celeris-iouring-h1-async"]
	if base.Name == "" {
		t.Fatal("registry has no celeris-iouring-h1-async column")
	}
	off := base
	off.Name = "celeris-iouring-h1-async-zzoff-test"
	off.SUTEnv = map[string]string{"CELERIS_IOURING_SEND_ZC": "off"}
	withRegistryColumn(t, off)

	cols, err := resolveBenchColumns("celeris-iouring-h1-async," + off.Name)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 2 {
		t.Fatalf("got %d columns, want 2", len(cols))
	}
	on, zc := cols[0], cols[1]
	if on.Slug != "celeris-iouring-h1-async" || zc.Slug != off.Name {
		t.Fatalf("columns out of registry order: %q, %q", on.Slug, zc.Slug)
	}
	if on.Bin != zc.Bin || on.Engine != zc.Engine {
		t.Fatalf("the arms must share binary and engine: %+v vs %+v", on, zc)
	}
	if len(on.Env) != 0 {
		t.Errorf("the registry column must carry no env, got %v", on.Env)
	}
	if zc.Env["CELERIS_IOURING_SEND_ZC"] != "off" || len(zc.Env) != 1 {
		t.Errorf("OFF arm env = %v, want exactly CELERIS_IOURING_SEND_ZC=off", zc.Env)
	}
}

// A column env reaches a root-launched process through the same playbook
// as BENCH_SUT_ENV, so it must obey the same rules; a bad one fails at mage
// start, never on the cluster.
func TestResolveBenchColumnsRejectsABadColumnEnv(t *testing.T) {
	base := servers.Registry["celeris-iouring-h1-async"]
	for i, tc := range []struct {
		env     map[string]string
		errPart string
	}{
		{map[string]string{"LD_PRELOAD": "/x.so"}, "not allowed"},
		{map[string]string{"A B": "x"}, "must match"},
		{map[string]string{"K": "has space"}, "no whitespace"},
		{map[string]string{"K": "q'uote"}, "no whitespace"},
	} {
		bad := base
		bad.Name = "celeris-iouring-h1-async-zzbad-test"
		bad.SUTEnv = tc.env
		servers.Registry[bad.Name] = bad
		_, err := resolveBenchColumns(bad.Name)
		delete(servers.Registry, bad.Name)
		if err == nil || !strings.Contains(err.Error(), tc.errPart) {
			t.Errorf("case %d %v: err=%v, want it to mention %q", i, tc.env, err, tc.errPart)
		}
	}
}
