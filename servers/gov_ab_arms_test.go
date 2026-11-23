package servers_test

// GOVAB (THROWAWAY ab/gov-ab-20261122, not for merge): the five registry columns of servers/gov_ab_arms.go. The expectations are stated here, not derived from the code under test;
// the pin comes from servers/celeris/go.mod.

import (
	"os"
	"regexp"
	"slices"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

func TestGovabColumns(t *testing.T) {
	mod, err := os.ReadFile("celeris/go.mod")
	if err != nil {
		t.Fatal(err)
	}
	m := regexp.MustCompile(`(?m)^\s*github\.com/goceleris/celeris (v\S+)\s*$`).FindSubmatch(mod)
	if m == nil {
		t.Fatal("no celeris require in servers/celeris/go.mod")
	}
	for _, want := range []struct {
		name, base, engine, scenario string
		fractions                    []float64
	}{
		{"gabw-std-h1", "celeris-std-h1", "std-h1", "ws-echo", []float64{0.25, 0.9}},
		{"gabw-adaptive-h1-async", "celeris-adaptive-h1-async", "adaptive-h1-async", "ws-echo", []float64{0.25, 0.9}},
		{"gabw-epoll-h1-async", "celeris-epoll-h1-async", "epoll-h1-async", "ws-echo", []float64{0.25, 0.9}},
		{"gabl-std-h1", "celeris-std-h1", "std-h1", "ws-large-echo", []float64{0.5, 0.9}},
		{"gabl-adaptive-h1-async", "celeris-adaptive-h1-async", "adaptive-h1-async", "ws-large-echo", []float64{0.5, 0.9}},
	} {
		a, ok := servers.Registry[want.name]
		if !ok {
			t.Errorf("%s is not in the registry", want.name)
			continue
		}
		base := servers.Registry[want.base]
		if a.FrameworkVersion != string(m[1]) || a.Framework != "celeris-govab" {
			t.Errorf("%s: framework %s version %s, want celeris-govab / %s (go.mod)", want.name, a.Framework, a.FrameworkVersion, m[1])
		}
		if gb, isGo := a.Bin.(servers.GoBinary); !isGo || gb.ModuleDir != "servers/celeris" {
			t.Errorf("%s: bin %#v, want the servers/celeris module", want.name, a.Bin)
		}
		if a.Engine != want.engine || a.Engine != base.Engine || a.Capabilities != base.Capabilities || a.Category != base.Category || a.Language != base.Language {
			t.Errorf("%s differs from %s in engine / capabilities / category / language", want.name, want.base)
		}
		if len(a.SUTEnv) != 0 || len(base.SUTEnv) != 0 {
			t.Errorf("%s: SUTEnv %v (base %v), want both empty", want.name, a.SUTEnv, base.SUTEnv)
		}
		p, ok := a.RatedPlans[want.scenario]
		if !ok || len(a.RatedPlans) != 1 || !slices.Equal(p.Fractions, want.fractions) {
			t.Errorf("%s: rated plans %#v, want exactly %s: %v", want.name, a.RatedPlans, want.scenario, want.fractions)
		}
		if a.Baseline {
			t.Errorf("%s: Baseline is set: the column would be hidden unless BENCH_BASELINE", want.name)
		}
	}
	// the columns of the main registry carry no plan: nothing else changed
	for n, a := range servers.Registry {
		if a.RatedPlans != nil && n[:3] != "gab" {
			t.Errorf("column %s carries a rated plan", n)
		}
	}
}
