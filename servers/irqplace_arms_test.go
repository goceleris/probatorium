package servers_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/irqplace-v1). Pins the layout of the IRQPLACE run: the exact (scenario, column) pairs each dispatch glob selects, one cell per
// column, the vector CPU per block, and that every column's registry pin equals the celeris require of its module.
const (
	fastGlob   = "get-simple-256c/irqplace-*-io256,get-simple-128c/irqplace-*-ad128,get-simple-512c/irqplace-*-ep512,post-4k/irqplace-*-post4k"
	ninetyGlob = "get-simple-512c/irqplace-*-io512"
)

func matched(t *testing.T, globs string) map[string][]string { // column -> scenarios
	t.Helper()
	got := map[string][]string{}
	for _, sc := range scenarios.Registry() {
		for name := range servers.Registry {
			for _, g := range strings.Split(globs, ",") {
				if ok, _ := path.Match(g, sc.Name()+"/"+name); ok {
					got[name] = append(got[name], sc.Name())
					break
				}
			}
		}
	}
	return got
}

// blockOneArms is 4 (c1, p, pw12, d), or 5 when the registry was regenerated with irqplace-gen.py --with-m.
func blockOneArms() int {
	for name := range servers.Registry {
		if strings.HasPrefix(name, "irqplace-") && strings.Contains(name, "-b1r1-m-") {
			return 5
		}
	}
	return 4
}

// wantFast is the dispatch-1 column count: block 1 = arms x 4 reps x 4 cells, block 2 = 2 arms x 2 reps x 4 cells.
func wantFast() int { return blockOneArms()*4*4 + 2*2*4 }

func TestIRQPlaceGlobsSelectOneCellPerColumn(t *testing.T) {
	fast := matched(t, fastGlob)
	if len(fast) != wantFast() {
		t.Fatalf("dispatch 1 glob selects %d columns, want %d", len(fast), wantFast())
	}
	for c, s := range fast {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1 (the stall captures assume one window per column)", c, len(s), s)
		}
	}
	ninety := matched(t, ninetyGlob)
	if len(ninety) != blockOneArms() {
		t.Fatalf("dispatch 2 glob selects %d columns, want %d", len(ninety), blockOneArms())
	}
	for c := range ninety {
		if _, dup := fast[c]; dup {
			t.Errorf("column %s is in both dispatches", c)
		}
	}
}

func TestIRQPlaceLayout(t *testing.T) {
	re := regexp.MustCompile(`^irqplace-(\d{3})-b([12])r(\d)-(c1|p|pw12|d|m)-(io256|ad128|ep512|post4k|io512)$`)
	type key struct{ blk, arm string }
	reps := map[key]map[string]bool{}
	var fast []string
	for name, a := range servers.Registry {
		m := re.FindStringSubmatch(name)
		if m == nil {
			continue
		}
		wantVec := map[string]string{"1": "4", "2": "1"}[m[2]]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != wantVec || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=%s", name, a.SUTEnv, wantVec)
		}
		if m[2] == "2" && m[4] != "c1" && m[4] != "p" { // block 2 never holds the m, pw12 or d arms
			t.Errorf("%s: block 2 holds only c1 and p", name)
		}
		if m[5] == "io512" {
			if m[2] != "1" || m[1][0] != '9' {
				t.Errorf("%s: the 90 s cell is block 1, seq 9xx", name)
			}
			continue
		}
		fast = append(fast, name)
		k := key{m[2], m[4]}
		if reps[k] == nil {
			reps[k] = map[string]bool{}
		}
		reps[k][m[3]] = true
		gomod, err := os.ReadFile(filepath.Join("..", a.Bin.(servers.GoBinary).ModuleDir, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gomod), "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", name, a.FrameworkVersion, a.Bin.(servers.GoBinary).ModuleDir)
		}
		src, _ := os.ReadFile(filepath.Join("..", a.Bin.(servers.GoBinary).ModuleDir, "server.go"))
		w12 := strings.Contains(string(src), "Workers: 12")
		if w12 != (m[4] == "pw12" || m[4] == "d") { // m is #1049 + the main merge at the default worker count
			t.Errorf("%s: Workers: 12 in the module is %v", name, w12)
		}
	}
	if len(fast) != wantFast() {
		t.Fatalf("%d dispatch-1 columns, want %d", len(fast), wantFast())
	}
	armsB1 := []string{"c1", "p", "pw12", "d"}
	if blockOneArms() == 5 {
		armsB1 = append(armsB1, "m")
	}
	for _, arm := range armsB1 {
		if n := len(reps[key{"1", arm}]); n != 4 {
			t.Errorf("block 1 arm %s has %d reps, want 4", arm, n)
		}
	}
	for _, arm := range []string{"c1", "p"} {
		if n := len(reps[key{"2", arm}]); n != 2 {
			t.Errorf("block 2 arm %s has %d reps, want 2", arm, n)
		}
	}
	sort.Strings(fast)
	for i, n := range fast { // sorted name order is the execution order: seq 001..N, no gaps
		if !strings.HasPrefix(n, "irqplace-"+pad3(i+1)+"-") {
			t.Fatalf("sorted column %d is %s, want seq %s", i, n, pad3(i+1))
		}
	}
}

func pad3(i int) string {
	return string([]byte{byte('0' + i/100), byte('0' + i/10%10), byte('0' + i%10)})
}

func TestIRQPlaceAMD64PlanLineColumnsCarryNoEnv(t *testing.T) {
	got := matched(t, "get-simple-128c/irqplace-amd64-*")
	if len(got) != 6 {
		t.Fatalf("amd64 glob selects %d columns, want 6 (m and p for adaptive, epoll, iouring)", len(got))
	}
	for c := range got {
		a := servers.Registry[c]
		if len(a.SUTEnv) != 0 {
			t.Errorf("%s: SUTEnv %v; the amd64 columns must carry no AB_IRQ_VECTOR_CPU, so that no IRQ write can happen on msa2-server", c, a.SUTEnv)
		}
		if !strings.Contains(c, "-m-") && !strings.Contains(c, "-p-") {
			t.Errorf("%s: only m (d8b2a0f, the base-matched control) and p", c)
		}
	}
}
