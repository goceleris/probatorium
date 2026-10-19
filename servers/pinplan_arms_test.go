package servers_test

import (
	"bytes"
	"fmt"
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

// THROWAWAY (ab/pinplan-v3, ab/pinplan-v3-90s). Pins the layout of the PINPLAN3 run (PLAN-pinplan-v3.md): the exact (scenario, column) pairs each
// dispatch glob selects, one cell per column, four arms (base = celeris tag v1.5.11; p8, w12, w16 = celeris a193acb4 = main 13fa1761 + #1049, with
// Config.Workers unset / 12 / 16 as a CONSTANT of the module), the single vector position (the standing CPU 1), the position-balanced seeded
// schedule (a Williams square, reps 1-4), and that every column's registry pin equals the celeris require of its module.
const (
	fastGlob   = "get-simple-256c/pinplan3-*-io256,get-simple-128c/pinplan3-*-ad128,get-simple-512c/pinplan3-*-ep512,post-4k/pinplan3-*-post4k"
	ninetyGlob = "get-simple-512c/pinplan3-*-io512"

	wantFast = 80 // 5 reps x 4 arms x 4 cells
	wantNine = 16 // 4 reps x 4 arms x io512

	pinBase = "v1.5.11"
	pinPin  = "v1.5.12-0.20261019091132-a193acb4e610"
)

var (
	arms      = []string{"base", "p8", "w12", "w16"}
	modOf     = map[string]string{"base": "ppbase_celeris", "p8": "pppin8_celeris", "w12": "pppinw12_celeris", "w16": "pppinw16_celeris"}
	pinOf     = map[string]string{"base": pinBase, "p8": pinPin, "w12": pinPin, "w16": pinPin}
	fwOf      = map[string]string{"base": "celeris-pp-base", "p8": "celeris-pp-pin8", "w12": "celeris-pp-pinw12", "w16": "celeris-pp-pinw16"}
	workersOf = map[string]int{"p8": 0, "w12": 12, "w16": 16}
	cells35   = []string{"io256", "ad128", "ep512", "post4k"}
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

func TestPinplan3GlobsSelectOneCellPerColumn(t *testing.T) {
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
	// the dispatch globs must not catch the previous experiments' columns, and those must not be in this branch's registry
	for name := range servers.Registry {
		if strings.HasPrefix(name, "irqplace-") || strings.HasPrefix(name, "pinplan-") || strings.HasPrefix(name, "pinplan2-") {
			t.Errorf("column %s of an earlier experiment is in this branch's registry", name)
		}
	}
	for _, old := range []string{"get-simple-256c/pinplan2-001-r1-pin-io256", "get-simple-512c/pinplan2-901-r1-main-io512", "post-4k/pinplan-001-r1-pin-post4k"} {
		for _, g := range []string{fastGlob, ninetyGlob} {
			for _, one := range strings.Split(g, ",") {
				if ok, _ := path.Match(one, old); ok {
					t.Errorf("glob %q selects the earlier experiment's column %q", one, old)
				}
			}
		}
	}
}

type col struct {
	name, arm, cell string
	seq, rep        int
}

func ppCols(t *testing.T) (fast, nine []col) {
	t.Helper()
	re := regexp.MustCompile(`^pinplan3-(\d{3})-r(\d)-(base|p8|w12|w16)-(io256|ad128|ep512|post4k|io512)$`)
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinplan3-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinplan3-<seq>-r<rep>-<base|p8|w12|w16>-<cell>", name)
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

// checkSchedule verifies one dispatch: seq numbering without gaps, reps x 4 arm slots x cells in the fixed cell order, each rep holding all four
// arms once, and the position balance registered in PLAN-pinplan-v3.md section 4: reps 1-4 are a Williams square (every arm holds every position
// once AND every ordered pair of arms is adjacent exactly once, so first-order carry-over is balanced); no arm holds one position in more than 2
// reps; no arm leads and no arm is last in two consecutive reps; the leaders are not a period-4 repeat.
func checkSchedule(t *testing.T, cols []col, firstSeq, reps int, cells []string) {
	t.Helper()
	perRep := 4 * len(cells)
	if len(cols) != reps*perRep {
		t.Fatalf("%d columns, want %d", len(cols), reps*perRep)
	}
	for i, c := range cols { // sorted name order is the execution order: seq firstSeq.. without gaps
		if c.seq != firstSeq+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, firstSeq+i)
		}
	}
	order := make([][4]string, reps) // order[rep][position] = arm
	for k := 0; k < reps; k++ {
		rb := cols[k*perRep : (k+1)*perRep]
		seen := map[string]bool{}
		for a := 0; a < 4; a++ { // arm slot: all cells of one arm, in the fixed cell order
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
	for pos := 0; pos < 4; pos++ { // reps 1-4: Latin square
		seen := map[string]bool{}
		for k := 0; k < 4; k++ {
			seen[order[k][pos]] = true
		}
		if len(seen) != 4 {
			t.Errorf("reps 1-4 are not a Latin square at position %d: %v", pos+1, order[:4])
		}
	}
	pairs := map[[2]string]int{} // reps 1-4: every ordered adjacent pair exactly once (Williams)
	for k := 0; k < 4; k++ {
		for p := 0; p < 3; p++ {
			pairs[[2]string{order[k][p], order[k][p+1]}]++
		}
	}
	if len(pairs) != 12 {
		t.Errorf("reps 1-4 are not a Williams square: %d distinct ordered adjacent pairs, want 12: %v", len(pairs), order[:4])
	}
	for pos := 0; pos < 4; pos++ {
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
		if order[k][3] == order[k+1][3] {
			t.Errorf("reps %d and %d have the same last arm %s", k+1, k+2, order[k][3])
		}
	}
	if reps >= 5 {
		period4 := true
		for k := 0; k+4 < reps; k++ {
			period4 = period4 && order[k][0] == order[k+4][0]
		}
		if period4 {
			t.Errorf("the leaders repeat with period 4: %v", order)
		}
	}
}

func TestPinplan3Layout(t *testing.T) {
	fast, nine := ppCols(t)
	checkSchedule(t, fast, 1, 5, cells35)
	checkSchedule(t, nine, 901, 4, []string{"io512"})
	for _, c := range append(append([]col{}, fast...), nine...) {
		a := servers.Registry[c.name]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != "1" || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=1 (the vector stays on its standing CPU; nothing moves it to CPU 4; the loop count is a constant of the module, not an env)", c.name, a.SUTEnv)
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
	}
}

// All four arms are copies of the committed servers/celeris except for go.mod/go.sum (the module line and the pin) and its debugvars_test.go; the
// three #1049 arms additionally differ in server.go by the one `Workers: armWorkers,` line (and its comment) and in arm.go, whose only variable
// content is the constant (0 / 12 / 16). The base arm has no arm.go and no Workers line: it is servers/celeris byte for byte.
func TestPinplan3ArmModulesAreCopiesOfServersCeleris(t *testing.T) {
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
			if e.Name() == "server.go" && arm != "base" {
				b = []byte(stripArmLines(string(b)))
			}
			if !bytes.Equal(a, b) {
				t.Errorf("%s/%s differs from servers/celeris/%s", mod, e.Name(), e.Name())
			}
		}
		have, err := os.ReadDir(mod)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range have {
			if _, err := os.Stat(filepath.Join("celeris", e.Name())); err != nil {
				if !(arm != "base" && e.Name() == "arm.go") {
					t.Errorf("%s has the extra file %s", mod, e.Name())
				}
			}
		}
	}
}

// stripArmLines removes the arm's own lines from server.go: the comment line that names the arm and the Workers line itself.
func stripArmLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "AB ARM (throwaway, ab/pinplan-v3)") || strings.TrimSpace(l) == "Workers: armWorkers," {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// The workers knob is a constant of the module (arm.go), 0 / 12 / 16 for p8 / w12 / w16, set once in server.go's celeris.Config as
// `Workers: armWorkers,` and nowhere else; the base module sets no Workers at all; and no arm reads the loop count from the environment.
func TestPinplan3WorkersKnobIsAModuleConstant(t *testing.T) {
	armRe := regexp.MustCompile(`(?s)\nconst armWorkers = (\d+)\n$`)
	for _, arm := range arms {
		mod := modOf[arm]
		entries, _ := os.ReadDir(mod)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			src, _ := os.ReadFile(filepath.Join(mod, e.Name()))
			s := string(src)
			if strings.Contains(s, "CELERIS_WORKERS") || strings.Contains(s, "GOMAXPROCS") {
				t.Errorf("%s/%s reads or sets a worker count from the environment or the runtime", mod, e.Name())
			}
			if arm == "base" && strings.Contains(s, "Workers") && e.Name() != "debugvars.go" {
				t.Errorf("%s/%s mentions Workers: the base arm must be the unchanged release default", mod, e.Name())
			}
		}
		if arm == "base" {
			if _, err := os.Stat(filepath.Join(mod, "arm.go")); err == nil {
				t.Errorf("%s has an arm.go", mod)
			}
			continue
		}
		srv, _ := os.ReadFile(filepath.Join(mod, "server.go"))
		if n := strings.Count(string(srv), "Workers: armWorkers,"); n != 1 {
			t.Errorf("%s/server.go has %d `Workers: armWorkers,` lines, want exactly 1", mod, n)
		}
		src, err := os.ReadFile(filepath.Join(mod, "arm.go"))
		if err != nil {
			t.Fatal(err)
		}
		m := armRe.FindStringSubmatch(string(src))
		if m == nil || m[1] != strconv.Itoa(workersOf[arm]) {
			t.Errorf("%s/arm.go: armWorkers is %v, want %d", mod, m, workersOf[arm])
		}
		want := fmt.Sprintf("const armWorkers = %d\n", workersOf[arm])
		if strings.Count(string(src), "armWorkers") < 2 || !strings.HasSuffix(string(src), want) {
			t.Errorf("%s/arm.go does not end in %q", mod, want)
		}
	}
}

// The three #1049 arms pin the SAME celeris commit (a193acb4 = main 13fa1761 + #1049), and go.mod/go.sum differ from p8's only in the module line.
func TestPinplan3PinArmsDifferOnlyInTheConstant(t *testing.T) {
	ref := "pppin8_celeris"
	for _, other := range []string{"pppinw12_celeris", "pppinw16_celeris"} {
		for _, f := range []string{"go.mod", "go.sum", "chain_handlers.go", "debugvars.go", "driver_handlers.go", "streaming_handlers.go", "server.go"} {
			a, _ := os.ReadFile(filepath.Join(ref, f))
			b, _ := os.ReadFile(filepath.Join(other, f))
			if f == "go.mod" {
				a = []byte(strings.ReplaceAll(string(a), ref, other))
			}
			if !bytes.Equal(a, b) {
				t.Errorf("%s/%s differs from %s/%s by more than the module line", other, f, ref, f)
			}
		}
	}
	gm, _ := os.ReadFile(filepath.Join(ref, "go.mod"))
	if !strings.Contains(string(gm), "github.com/goceleris/celeris "+pinPin+"\n") {
		t.Errorf("%s/go.mod does not require celeris %s", ref, pinPin)
	}
	gs, _ := os.ReadFile(filepath.Join(ref, "go.sum"))
	if !strings.Contains(string(gs), "github.com/goceleris/celeris "+pinPin+" h1:") {
		t.Errorf("%s/go.sum has no h1 line for celeris %s", ref, pinPin)
	}
	// base differs from p8 in the module line, the celeris pin and the x/* lines MVS chose (identical here), nothing else
	bm, _ := os.ReadFile(filepath.Join("ppbase_celeris", "go.mod"))
	a2 := strings.ReplaceAll(strings.ReplaceAll(string(gm), ref, "ppbase_celeris"), pinPin, pinBase)
	if a2 != string(bm) {
		t.Errorf("ppbase_celeris/go.mod differs from %s/go.mod by more than the module line and the celeris pin", ref)
	}
}
