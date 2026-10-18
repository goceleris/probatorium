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

// THROWAWAY (ab/pinplan-v2, ab/pinplan-v2-90s). Pins the layout of the PINPLAN2 run (PLAN-pinplan-v2.md): the exact (scenario, column) pairs each
// dispatch glob selects, one cell per column, three arms (base = celeris tag v1.5.11, main = celeris 44c6ff75, pin = 126b4aa5 = main + #1049)
// whose modules differ in the celeris pin only, the single vector position (the standing CPU 1), the position-balanced seeded schedule, and that
// every column's registry pin equals the celeris require of its module.
const (
	fastGlob   = "get-simple-256c/pinplan2-*-io256,get-simple-128c/pinplan2-*-ad128,get-simple-512c/pinplan2-*-ep512,post-4k/pinplan2-*-post4k"
	ninetyGlob = "get-simple-512c/pinplan2-*-io512"

	wantFast = 60 // 5 reps x 3 arms x 4 cells
	wantNine = 12 // 4 reps x 3 arms x io512

	pinBase = "v1.5.11"
	pinMain = "v1.5.12-0.20261018173213-44c6ff757d29"
	pinPin  = "v1.5.12-0.20261018173929-126b4aa5d896"
)

var (
	arms    = []string{"base", "main", "pin"}
	modOf   = map[string]string{"base": "ppbase_celeris", "main": "ppmain_celeris", "pin": "pppin_celeris"}
	pinOf   = map[string]string{"base": pinBase, "main": pinMain, "pin": pinPin}
	fwOf    = map[string]string{"base": "celeris-pp-base", "main": "celeris-pp-main", "pin": "celeris-pp-pin"}
	cells35 = []string{"io256", "ad128", "ep512", "post4k"}
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

func TestPinplan2GlobsSelectOneCellPerColumn(t *testing.T) {
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
		if strings.HasPrefix(name, "irqplace-") || strings.HasPrefix(name, "pinplan-") {
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
	re := regexp.MustCompile(`^pinplan2-(\d{3})-r(\d)-(base|main|pin)-(io256|ad128|ep512|post4k|io512)$`)
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinplan2-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinplan2-<seq>-r<rep>-<base|main|pin>-<cell>", name)
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

// checkSchedule verifies one dispatch: seq numbering without gaps, reps x 3 arm slots x cells in the fixed cell order, each rep holding all three
// arms once, and the position balance registered in PLAN-pinplan-v2.md section 4: reps 1-3 are a Latin square (every arm holds every position
// once); no arm holds one position in more than 2 reps; no arm leads and no arm is last in two consecutive reps; the leaders are not a period-3
// repeat.
func checkSchedule(t *testing.T, cols []col, firstSeq, reps int, cells []string) {
	t.Helper()
	perRep := 3 * len(cells)
	if len(cols) != reps*perRep {
		t.Fatalf("%d columns, want %d", len(cols), reps*perRep)
	}
	for i, c := range cols { // sorted name order is the execution order: seq firstSeq.. without gaps
		if c.seq != firstSeq+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, firstSeq+i)
		}
	}
	order := make([][3]string, reps) // order[rep][position] = arm
	for k := 0; k < reps; k++ {
		rb := cols[k*perRep : (k+1)*perRep]
		seen := map[string]bool{}
		for a := 0; a < 3; a++ { // arm slot: all cells of one arm, in the fixed cell order
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
			order[k][a] = slot[0].arm
		}
	}
	for pos := 0; pos < 3; pos++ { // reps 1-3: Latin square
		seen := map[string]bool{}
		for k := 0; k < 3; k++ {
			seen[order[k][pos]] = true
		}
		if len(seen) != 3 {
			t.Errorf("reps 1-3 are not a Latin square at position %d: %v", pos+1, order[:3])
		}
	}
	for pos := 0; pos < 3; pos++ {
		for _, arm := range arms {
			n := 0
			for k := 0; k < reps; k++ {
				if order[k][pos] == arm {
					n++
				}
			}
			if n > 2 || n < 1 {
				t.Errorf("arm %s holds position %d in %d of %d reps, want 1 or 2", arm, pos+1, n, reps)
			}
		}
	}
	for k := 0; k+1 < reps; k++ {
		if order[k][0] == order[k+1][0] {
			t.Errorf("reps %d and %d have the same leader %s", k+1, k+2, order[k][0])
		}
		if order[k][2] == order[k+1][2] {
			t.Errorf("reps %d and %d have the same last arm %s", k+1, k+2, order[k][2])
		}
	}
	if reps >= 5 {
		period3 := true
		for k := 0; k+3 < reps; k++ {
			period3 = period3 && order[k][0] == order[k+3][0]
		}
		if period3 {
			t.Errorf("the leaders repeat with period 3: %v", order)
		}
	}
}

func TestPinplan2Layout(t *testing.T) {
	fast, nine := ppCols(t)
	checkSchedule(t, fast, 1, 5, cells35)
	checkSchedule(t, nine, 901, 4, []string{"io512"})
	for _, c := range append(append([]col{}, fast...), nine...) {
		a := servers.Registry[c.name]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != "1" || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=1 (the vector stays on its standing CPU; nothing moves it to CPU 4)", c.name, a.SUTEnv)
		}
		if filepath.Base(modDir(c.name)) != modOf[c.arm] || a.FrameworkVersion != pinOf[c.arm] || a.Framework != fwOf[c.arm] {
			t.Errorf("%s: module %s framework %s version %s, want %s %s %s", c.name, modDir(c.name), a.Framework, a.FrameworkVersion, modOf[c.arm], fwOf[c.arm], pinOf[c.arm])
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
			t.Errorf("%s: the module sets Workers; all arms run the default worker count (the planner / the release's default must decide it, not the harness)", c.name)
		}
	}
}

// The three arms must differ in the celeris pin and nothing else: pin - main is then PR #1049 (git diff 44c6ff75 126b4aa5 is the PR's own 26 files
// and has no go.mod change); base is the release tag compiled by the same unchanged harness source.
func TestPinplan2ArmModulesDifferOnlyInThePin(t *testing.T) {
	ref := "ppmain_celeris"
	ents, err := os.ReadDir(ref)
	if err != nil {
		t.Fatal(err)
	}
	for _, other := range []string{"pppin_celeris", "ppbase_celeris"} {
		oe, err := os.ReadDir(other)
		if err != nil {
			t.Fatal(err)
		}
		if len(ents) != len(oe) {
			t.Fatalf("%s has %d files, %s %d", ref, len(ents), other, len(oe))
		}
		arm := map[string]string{"pppin_celeris": "pin", "ppbase_celeris": "base"}[other]
		for _, e := range ents {
			a, _ := os.ReadFile(filepath.Join(ref, e.Name()))
			b, err := os.ReadFile(filepath.Join(other, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			switch e.Name() {
			case "go.mod":
				a2 := strings.ReplaceAll(strings.ReplaceAll(string(a), ref, other), pinMain, pinOf[arm])
				if a2 != string(b) {
					t.Errorf("%s/go.mod differs from %s/go.mod by more than the module line and the celeris pin", other, ref)
				}
			case "go.sum":
				// only the celeris lines may differ: every other line of the arm's go.sum must be one of the reference's
				for _, l := range strings.Split(string(b), "\n") {
					if l != "" && !strings.HasPrefix(l, "github.com/goceleris/celeris ") && !strings.Contains(string(a), l) {
						t.Errorf("go.sum of %s carries %q, which is neither shared with %s nor a celeris line", other, l, ref)
					}
				}
				if !strings.Contains(string(b), "github.com/goceleris/celeris "+pinOf[arm]+" h1:") {
					t.Errorf("go.sum of %s has no h1 line for celeris %s", other, pinOf[arm])
				}
			default:
				if !bytes.Equal(a, b) {
					t.Errorf("%s differs between %s and %s", e.Name(), ref, other)
				}
			}
		}
	}
}

// All arms are copies of the committed servers/celeris except for go.mod/go.sum (the module line and the pin) and its debugvars_test.go.
func TestPinplan2ArmModulesAreCopiesOfServersCeleris(t *testing.T) {
	ents, err := os.ReadDir("celeris")
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range arms {
		mod := modOf[arm]
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
