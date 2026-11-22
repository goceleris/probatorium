package servers

import (
	"sort"
	"strings"
	"testing"
)

// TestScreenArms pins the pre-freeze screen registry (screen_arms.go): per loop engine four columns, A1 A2 the
// main binary, B1 B2 the v1.5.11 module, same Engine and Capabilities, never Baseline (so visible with baseline=false),
// sorted order A1 B1 B2 A2, and no std-h1 arms.
func TestScreenArms(t *testing.T) {
	engines := []string{"epoll-h1-sync", "epoll-h1-async", "epoll-auto+upg-async", "iouring-h1-sync", "iouring-h1-async",
		"iouring-auto+upg-async", "adaptive-h1-async", "adaptive-auto+upg-async"}
	var arms []string
	for n := range Registry {
		if strings.HasSuffix(n, "-v1511-b1") || strings.HasSuffix(n, "-v1511-b2") || strings.HasSuffix(n, "-vmain") {
			arms = append(arms, n)
		}
	}
	if len(arms) != 24 {
		t.Fatalf("screen arms: %d registered, want 24 (8 engines x 3)", len(arms))
	}
	names := Names()
	idx := map[string]int{}
	for i, n := range names {
		idx[n] = i
	}
	for _, e := range engines {
		m := Registry["celeris-"+e]
		order := []string{"celeris-" + e, "celeris-" + e + "-v1511-b1", "celeris-" + e + "-v1511-b2", "celeris-" + e + "-vmain"}
		for i, n := range order {
			a, ok := Registry[n]
			if !ok {
				t.Fatalf("%s missing", n)
			}
			if a.Baseline {
				t.Errorf("%s is Baseline: invisible with baseline=false", n)
			}
			if a.Engine != m.Engine || a.Capabilities != m.Capabilities || a.Category != m.Category || len(a.SUTEnv) != 0 {
				t.Errorf("%s: engine/capabilities/category/env differ from main", n)
			}
			wantB := i == 1 || i == 2
			gb, _ := a.Bin.(GoBinary)
			if wantB {
				if gb.ModuleDir != BaselineModule || a.Framework != "celeris-baseline" || a.FrameworkVersion != BaselineCelerisVersion {
					t.Errorf("%s: B arm must run %s as celeris-baseline %s, got %q %q %q", n, BaselineModule, BaselineCelerisVersion, gb.ModuleDir, a.Framework, a.FrameworkVersion)
				}
			} else if gb.ModuleDir != "servers/celeris" || a.Framework != "celeris" {
				t.Errorf("%s: A arm must run servers/celeris as framework celeris, got %q %q", n, gb.ModuleDir, a.Framework)
			}
			if i > 0 && idx[n] != idx[order[0]]+i {
				t.Errorf("%s: not contiguous after %s in Names() order (A1 B1 B2 A2)", n, order[0])
			}
		}
	}
	for _, n := range []string{"celeris-std-h1-v1511-b1", "celeris-std-h1-vmain"} {
		if _, ok := Registry[n]; ok {
			t.Errorf("%s must not exist (no std-h1 arms)", n)
		}
	}
	sort.Strings(arms)
}
