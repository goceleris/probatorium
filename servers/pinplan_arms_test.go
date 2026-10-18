package servers_test

import (
	"bytes"
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

// THROWAWAY (ab/pinplan-v1, ab/pinplan-v1-90s). Pins the layout of the PINPLAN run (PLAN-pinplan-vs-main.md): the exact (scenario, column)
// pairs each dispatch glob selects, one cell per column, two arms (main = celeris 7b02cae0, pin = b1ef52dc = main + #1049) whose modules
// differ in the celeris pin only, the single vector position (the standing CPU 1), the arm-balanced seeded schedule, and that every column's
// registry pin equals the celeris require of its module.
const (
	fastGlob   = "get-simple-256c/pinplan-*-io256,get-simple-128c/pinplan-*-ad128,get-simple-512c/pinplan-*-ep512,post-4k/pinplan-*-post4k"
	ninetyGlob = "get-simple-512c/pinplan-*-io512"

	wantFast = 48 // 6 reps x 2 arms x 4 cells
	wantNine = 8  // 4 reps x 2 arms x io512

	pinMain = "v1.5.12-0.20261018100123-7b02cae09c50"
	pinPin  = "v1.5.12-0.20261018152654-b1ef52dc091e"
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

func TestPinplanGlobsSelectOneCellPerColumn(t *testing.T) {
	fast := matched(t, fastGlob)
	if len(fast) != wantFast {
		t.Fatalf("dispatch 1 glob selects %d columns, want %d", len(fast), wantFast)
	}
	for c, s := range fast {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1 (the stall captures assume one window per column)", c, len(s), s)
		}
	}
	ninety := matched(t, ninetyGlob)
	if len(ninety) != wantNine {
		t.Fatalf("dispatch 2 glob selects %d columns, want %d", len(ninety), wantNine)
	}
	for c, s := range ninety {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1", c, len(s), s)
		}
		if _, dup := fast[c]; dup {
			t.Errorf("column %s is in both dispatches", c)
		}
	}
	// the dispatch globs must not catch the previous experiments' columns
	for name := range servers.Registry {
		if strings.HasPrefix(name, "irqplace-") {
			t.Errorf("column %s of an earlier experiment is in this branch's registry", name)
		}
	}
}

type col struct {
	name, arm, cell string
	seq, rep        int
}

func ppCols(t *testing.T) (fast, nine []col) {
	t.Helper()
	re := regexp.MustCompile(`^pinplan-(\d{3})-r(\d)-(main|pin)-(io256|ad128|ep512|post4k|io512)$`)
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinplan-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinplan-<seq>-r<rep>-<main|pin>-<cell>", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		c := col{name, m[3], m[4], seq, rep}
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

func modDir(name string) string {
	return filepath.Join("..", servers.Registry[name].Bin.(servers.GoBinary).ModuleDir)
}

// checkSchedule verifies one dispatch: seq numbering without gaps, reps x arms x cells in the fixed cell order, each rep holding both arm slots,
// and that the arm order is balanced: every arm leads in exactly half of the reps, the two reps of each consecutive pair (1-2, 3-4, ...) have
// different leaders, and the pairs are not all oriented alike.
func checkSchedule(t *testing.T, cols []col, firstSeq, reps int, cells []string) {
	t.Helper()
	perRep := 2 * len(cells)
	if len(cols) != reps*perRep {
		t.Fatalf("%d columns, want %d", len(cols), reps*perRep)
	}
	for i, c := range cols { // sorted name order is the execution order: seq firstSeq.. without gaps
		if c.seq != firstSeq+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, firstSeq+i)
		}
	}
	lead := map[string]int{}
	var leaders []string
	for k := 0; k < reps; k++ {
		rb := cols[k*perRep : (k+1)*perRep]
		leaders = append(leaders, rb[0].arm)
		lead[rb[0].arm]++
		for a := 0; a < 2; a++ { // arm slot: all cells of one arm, in the fixed cell order
			slot := rb[a*len(cells) : (a+1)*len(cells)]
			for ci, c := range slot {
				if c.rep != k+1 || c.arm != slot[0].arm || c.cell != cells[ci] {
					t.Errorf("%s: out of place in rep %d (arm %s, cell order %v)", c.name, k+1, slot[0].arm, cells)
				}
			}
			if a == 1 && slot[0].arm == rb[0].arm {
				t.Errorf("rep %d: the two arm slots are both %s", k+1, rb[0].arm)
			}
		}
	}
	for _, arm := range []string{"main", "pin"} {
		if lead[arm] != reps/2 {
			t.Errorf("arm %s leads in %d of %d reps, want %d", arm, lead[arm], reps, reps/2)
		}
	}
	orient := map[string]bool{}
	for i := 0; i < reps; i += 2 {
		if leaders[i] == leaders[i+1] {
			t.Errorf("reps %d and %d have the same leader %s (each pair must have one of each)", i+1, i+2, leaders[i])
		}
		orient[leaders[i]] = true
	}
	if len(orient) != 2 {
		t.Errorf("all rep pairs are oriented alike (leaders %v): a period-2 alternation", leaders)
	}
}

func TestPinplanLayout(t *testing.T) {
	fast, nine := ppCols(t)
	checkSchedule(t, fast, 1, 6, []string{"io256", "ad128", "ep512", "post4k"})
	checkSchedule(t, nine, 901, 4, []string{"io512"})
	for _, c := range append(append([]col{}, fast...), nine...) {
		a := servers.Registry[c.name]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != "1" || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=1 (the vector stays on its standing CPU; nothing moves it to CPU 4)", c.name, a.SUTEnv)
		}
		wantMod, wantPin := map[string]string{"main": "ppmain_celeris", "pin": "pppin_celeris"}[c.arm], map[string]string{"main": pinMain, "pin": pinPin}[c.arm]
		if filepath.Base(modDir(c.name)) != wantMod || a.FrameworkVersion != wantPin || a.Framework != "celeris-pp-"+c.arm {
			t.Errorf("%s: module %s framework %s version %s, want %s celeris-pp-%s %s", c.name, modDir(c.name), a.Framework, a.FrameworkVersion, wantMod, c.arm, wantPin)
		}
		gomod, err := os.ReadFile(filepath.Join(modDir(c.name), "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gomod), "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", c.name, a.FrameworkVersion, modDir(c.name))
		}
		src, _ := os.ReadFile(filepath.Join(modDir(c.name), "server.go"))
		if strings.Contains(string(src), "Workers:") {
			t.Errorf("%s: the module sets Workers; both arms run the default worker count (the planner must decide it, not the harness)", c.name)
		}
	}
}

// The two arms must differ in the celeris pin and nothing else: pin - main is then PR #1049 (git diff 7b02cae0 b1ef52dc is the PR's own 26 files and
// has no go.mod change).
func TestPinplanArmModulesDifferOnlyInThePin(t *testing.T) {
	m, p := "ppmain_celeris", "pppin_celeris"
	ents, err := os.ReadDir(m)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.ReadDir(p)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != len(other) {
		t.Fatalf("ppmain_celeris has %d files, pppin_celeris %d", len(ents), len(other))
	}
	for _, e := range ents {
		a, _ := os.ReadFile(filepath.Join(m, e.Name()))
		b, err := os.ReadFile(filepath.Join(p, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		switch e.Name() {
		case "go.mod":
			a2 := strings.ReplaceAll(strings.ReplaceAll(string(a), "ppmain_celeris", "pppin_celeris"), pinMain, pinPin)
			if a2 != string(b) {
				t.Errorf("go.mod differs by more than the module line and the celeris pin")
			}
		case "go.sum":
			for _, l := range strings.Split(string(b), "\n") {
				if l != "" && !strings.Contains(string(a), l) && !strings.Contains(l, pinPin) {
					t.Errorf("go.sum of pppin_celeris carries %q, which is neither shared with ppmain_celeris nor the pin arm's pin", l)
				}
			}
		default:
			if !bytes.Equal(a, b) {
				t.Errorf("%s differs between ppmain_celeris and pppin_celeris", e.Name())
			}
		}
	}
}

// Both arms are copies of the committed servers/celeris except for go.mod/go.sum (the module line and the pin) and its debugvars_test.go.
func TestPinplanArmModulesAreCopiesOfServersCeleris(t *testing.T) {
	ents, err := os.ReadDir("celeris")
	if err != nil {
		t.Fatal(err)
	}
	for _, mod := range []string{"ppmain_celeris", "pppin_celeris"} {
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), "_test.go") || e.Name() == "go.mod" || e.Name() == "go.sum" {
				continue
			}
			a, _ := os.ReadFile(filepath.Join("celeris", e.Name()))
			b, err := os.ReadFile(filepath.Join(mod, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(a, b) {
				t.Errorf("%s/%s differs from servers/celeris/%s", mod, e.Name(), e.Name())
			}
		}
	}
}
