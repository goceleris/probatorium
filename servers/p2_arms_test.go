package servers_test

import (
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/p2-pinplan-confirm-38, not for merge). Pins the layout of the P2 run (PREREG-P2.md; campaign plan 2026-10-24 section 2): the exact (scenario, column)
// pairs each dispatch glob selects, one cell per column, two arms (base = celeris v1.5.11 = servers/celeris_v1511; main = celeris main 713b40d = servers/celeris, both
// modules main already carries, no copies), the single vector position (the standing CPU 1), the seeded schedule with main leading rep 1, and that every column's registry
// pin equals the celeris require of its module.
const (
	p2Fast = "get-simple-256c/pinp38-*-io256,get-simple-128c/pinp38-*-ad128,get-simple-512c/pinp38-*-ep512,post-4k/pinp38-*-post4k"
	p2Nine = "get-simple-512c/pinp38-*-io512"

	p2WantFast = 64 // 8 reps x 2 arms x 4 cells
	p2WantNine = 8  // 4 reps x 2 arms x io512

	p2PinBase = "v1.5.11"
	p2PinMain = "v1.5.12-0.20261021042539-713b40dfdd0d" // probatorium #533: celeris main 713b40dfdd0d
)

var (
	p2Arms  = []string{"base", "main"}
	p2Mod   = map[string]string{"base": "celeris_v1511", "main": "celeris"}
	p2Pin   = map[string]string{"base": p2PinBase, "main": p2PinMain}
	p2FW    = map[string]string{"base": "celeris-pp-base", "main": "celeris-pp-main"}
	p2Cells = []string{"io256", "ad128", "ep512", "post4k"}
)

func p2Matched(t *testing.T, globs string) map[string][]string { // column -> scenarios
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

type p2Col struct {
	name, arm, cell string
	seq, rep        int
}

func p2Cols(t *testing.T) (fast, nine []p2Col) {
	t.Helper()
	re := regexp.MustCompile(`^pinp38-(\d{3})-r(\d)-(base|main)-(io256|ad128|ep512|post4k|io512)$`)
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinp38-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinp38-<seq>-r<rep>-<base|main>-<cell>", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		c := p2Col{name, m[3], m[4], seq, rep}
		if c.cell == "io512" {
			nine = append(nine, c)
		} else {
			fast = append(fast, c)
		}
	}
	sort.Slice(fast, func(i, j int) bool { return fast[i].name < fast[j].name })
	sort.Slice(nine, func(i, j int) bool { return nine[i].name < nine[j].name })
	return fast, nine
}

func TestP2GlobsSelectOneCellPerColumn(t *testing.T) {
	fast := p2Matched(t, p2Fast)
	if len(fast) != p2WantFast {
		t.Fatalf("dispatch 1 glob selects %d columns, want %d", len(fast), p2WantFast)
	}
	for c, s := range fast {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1 (the stall captures assume one window per column)", c, len(s), s)
		}
		if !strings.HasPrefix(c, "pinp38-") {
			t.Errorf("dispatch 1 glob selects the foreign column %s", c)
		}
	}
	nine := p2Matched(t, p2Nine)
	if len(nine) != p2WantNine {
		t.Fatalf("dispatch 2 glob selects %d columns, want %d", len(nine), p2WantNine)
	}
	for c, s := range nine {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1", c, len(s), s)
		}
		if _, dup := fast[c]; dup {
			t.Errorf("column %s is in both dispatches", c)
		}
	}
	// no column of an earlier experiment is in this branch's registry or selected by a glob
	for name := range servers.Registry {
		for _, old := range []string{"irqplace-", "pinplan-", "pinplan2-", "pinplan3-", "pinplan4-", "pinplan5-", "pinplan6-", "pinplan7-"} {
			if strings.HasPrefix(name, old) {
				t.Errorf("column %s of an earlier experiment is in this branch's registry", name)
			}
		}
	}
	// the cell globs of the dispatches name '-io512' only for the 90 s dispatch: the workflow keys its 90 s / 20 s window on that substring
	if strings.Contains(p2Fast, "-io512") || !strings.Contains(p2Nine, "-io512") {
		t.Error("the 35 s glob must not contain '-io512' and the 90 s glob must (benchmark-tier.yml BENCH_DURATION keys on it)")
	}
}

// checkP2Schedule verifies one dispatch: seq numbering without gaps; per rep two arm slots of len(cells) cells in the fixed cell order, each arm once; main leads rep 1;
// each arm leads exactly half of the reps and exactly half of each half of the reps; no arm leads three reps in a row; the leaders are not strictly alternating.
func checkP2Schedule(t *testing.T, cols []p2Col, firstSeq, reps int, cells []string) {
	t.Helper()
	perRep := 2 * len(cells)
	if len(cols) != reps*perRep {
		t.Fatalf("%d columns, want %d", len(cols), reps*perRep)
	}
	for i, c := range cols { // sorted name order is the execution order
		if c.seq != firstSeq+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, firstSeq+i)
		}
	}
	lead := make([]string, reps)
	for k := 0; k < reps; k++ {
		rb := cols[k*perRep : (k+1)*perRep]
		seen := map[string]bool{}
		for a := 0; a < 2; a++ {
			slot := rb[a*len(cells) : (a+1)*len(cells)]
			for ci, c := range slot {
				if c.rep != k+1 || c.arm != slot[0].arm || c.cell != cells[ci] {
					t.Errorf("%s: out of place in rep %d (arm %s, cell order %v)", c.name, k+1, slot[0].arm, cells)
				}
			}
			if seen[slot[0].arm] {
				t.Errorf("rep %d: arm %s holds two slots", k+1, slot[0].arm)
			}
			seen[slot[0].arm] = true
			if a == 0 {
				lead[k] = slot[0].arm
			}
		}
	}
	if lead[0] != "main" {
		t.Errorf("rep 1 must be led by main (the first Listen of the main build is the closest the run gets to near-zero since-boot counters), got %s", lead[0])
	}
	count := func(s []string, arm string) (n int) {
		for _, a := range s {
			if a == arm {
				n++
			}
		}
		return n
	}
	half := reps / 2
	for _, arm := range p2Arms {
		if count(lead, arm) != reps/2 || count(lead[:half], arm) != half/2 || count(lead[half:], arm) != half/2 {
			t.Errorf("arm %s leads %d of %d reps (%d in the first half, %d in the second), want %d, %d, %d", arm, count(lead, arm), reps, count(lead[:half], arm), count(lead[half:], arm), reps/2, half/2, half/2)
		}
	}
	alt := true
	for k := 0; k+1 < reps; k++ {
		if lead[k] == lead[k+1] {
			alt = false
		}
		if k+2 < reps && lead[k] == lead[k+1] && lead[k+1] == lead[k+2] {
			t.Errorf("%s leads reps %d-%d", lead[k], k+1, k+3)
		}
	}
	if alt {
		t.Errorf("the leaders strictly alternate: %v", lead)
	}
}

func TestP2Layout(t *testing.T) {
	fast, nine := p2Cols(t)
	checkP2Schedule(t, fast, 1, 8, p2Cells)
	checkP2Schedule(t, nine, 901, 4, []string{"io512"})
	for _, c := range append(append([]p2Col{}, fast...), nine...) {
		a := servers.Registry[c.name]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != "1" || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=1 (the vector stays on its standing CPU; nothing moves it)", c.name, a.SUTEnv)
		}
		gb, ok := a.Bin.(servers.GoBinary)
		if !ok {
			t.Fatalf("%s: Bin is not a GoBinary", c.name)
		}
		dir := filepath.Join("..", gb.ModuleDir)
		if filepath.Base(dir) != p2Mod[c.arm] || a.FrameworkVersion != p2Pin[c.arm] || a.Framework != p2FW[c.arm] {
			t.Errorf("%s: module %s framework %s version %s, want %s %s %s", c.name, dir, a.Framework, a.FrameworkVersion, p2Mod[c.arm], p2FW[c.arm], p2Pin[c.arm])
		}
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gomod), "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", c.name, a.FrameworkVersion, dir)
		}
		// the same engine variant and capabilities as the twin celeris column of the cell
		twin := map[string]string{"io256": "celeris-iouring-h1-async", "ad128": "celeris-adaptive-h1-async", "ep512": "celeris-epoll-h1-async", "post4k": "celeris-epoll-h1-async", "io512": "celeris-iouring-h1-async"}[c.cell]
		if tw := servers.Registry[twin]; a.Engine != tw.Engine || a.Language != tw.Language || a.Capabilities != tw.Capabilities {
			t.Errorf("%s: engine/language/capabilities differ from %s", c.name, twin)
		}
	}
}

// Both arms run the modules main already carries: servers/celeris is pinned by probatorium's own go.mod pin policy (the baseline module by servers/baseline_test.go), and
// neither server.go sets Config.Workers or reads a worker count or a CPU list from the environment: the engine's planner decides, which is what the run measures.
func TestP2ArmsLeaveTheWorkerPlanToTheEngine(t *testing.T) {
	for _, mod := range []string{"celeris", "celeris_v1511"} {
		ents, err := os.ReadDir(mod)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			src, _ := os.ReadFile(filepath.Join(mod, e.Name()))
			s := string(src)
			if strings.Contains(s, "Workers:") || strings.Contains(s, "GOMAXPROCS") || strings.Contains(s, "CELERIS_WORKERS") || strings.Contains(s, "CELERIS_AB_PLAN_CPUS") {
				t.Errorf("%s/%s sets or reads a worker count / CPU list", mod, e.Name())
			}
		}
		if _, err := os.Stat(filepath.Join(mod, "arm.go")); err == nil {
			t.Errorf("%s has an arm.go: the P2 arms are the unmodified main modules", mod)
		}
	}
	if servers.BaselineModule != "servers/celeris_v1511" || servers.BaselineCelerisVersion != p2PinBase {
		t.Errorf("the baseline module moved: %s %s", servers.BaselineModule, servers.BaselineCelerisVersion)
	}
}
