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

// THROWAWAY (ab/pinplan-v5, ab/pinplan-v5-90s). Pins the layout of the PINPLAN5 run (PLAN-pinplan-v5.md): the exact (scenario, column) pairs each
// dispatch glob selects, one cell per column, four arms (base = celeris tag v1.5.11; p8 = celeris a193acb4 = main 13fa1761 + #1049; vnv and vx = celeris
// 3d0ddb0a = a193acb4 + the EXPERIMENT-ONLY CELERIS_AB_PLAN_CPUS knob, with the CPU list a CONSTANT of the module: vnv 0-1,7,9-11 and vx 0-1,6-9), the single vector position (the
// standing CPU 1), the position-balanced seeded schedule (a Williams square, reps 1-4), and that every column's registry pin equals the celeris require
// of its module.
const (
	fastGlob   = "get-simple-256c/pinplan5-*-io256,get-simple-128c/pinplan5-*-ad128,get-simple-512c/pinplan5-*-ep512,post-4k/pinplan5-*-post4k"
	ninetyGlob = "get-simple-512c/pinplan5-*-io512"

	wantFast = 80 // 5 reps x 4 arms x 4 cells
	wantNine = 16 // 4 reps x 4 arms x io512

	pinBase = "v1.5.11"
	pinPin  = "v1.5.12-0.20261019091132-a193acb4e610" // p8: celeris a193acb4
	pinVar  = "v1.5.12-0.20261019135026-3d0ddb0a8f6b" // vnv, vx: celeris 3d0ddb0a = a193acb4 + the EXPERIMENT-ONLY knob
)

var (
	arms    = []string{"base", "p8", "vnv", "vx"}
	modOf   = map[string]string{"base": "ppbase_celeris", "p8": "pppin8_celeris", "vnv": "ppvnv_celeris", "vx": "ppvx_celeris"}
	pinOf   = map[string]string{"base": pinBase, "p8": pinPin, "vnv": pinVar, "vx": pinVar}
	fwOf    = map[string]string{"base": "celeris-pp-base", "p8": "celeris-pp-pin8", "vnv": "celeris-pp-vnv", "vx": "celeris-pp-vx"}
	cpusOf  = map[string]string{"vnv": "0-1,7,9-11", "vx": "0-1,6-9"} // the EXPERIMENT-ONLY CELERIS_AB_PLAN_CPUS list of each variant (a constant of its module)
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

func TestPinplan5GlobsSelectOneCellPerColumn(t *testing.T) {
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
		if strings.HasPrefix(name, "irqplace-") || strings.HasPrefix(name, "pinplan-") || strings.HasPrefix(name, "pinplan2-") || strings.HasPrefix(name, "pinplan3-") || strings.HasPrefix(name, "pinplan4-") {
			t.Errorf("column %s of an earlier experiment is in this branch's registry", name)
		}
	}
	for _, old := range []string{"get-simple-256c/pinplan2-001-r1-pin-io256", "get-simple-512c/pinplan2-901-r1-main-io512", "post-4k/pinplan-001-r1-pin-post4k", "get-simple-256c/pinplan3-001-r1-p8-io256", "get-simple-512c/pinplan3-901-r1-w12-io512", "get-simple-256c/pinplan4-001-r1-vl2-io256", "get-simple-512c/pinplan4-901-r1-base-io512"} {
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
	re := regexp.MustCompile(`^pinplan5-(\d{3})-r(\d)-(base|p8|vnv|vx)-(io256|ad128|ep512|post4k|io512)$`)
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinplan5-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinplan5-<seq>-r<rep>-<base|p8|vnv|vx>-<cell>", name)
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
// arms once, and the position balance registered in PLAN-pinplan-v5.md section 3: reps 1-4 are a Williams square (every arm holds every position
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

func TestPinplan5Layout(t *testing.T) {
	fast, nine := ppCols(t)
	checkSchedule(t, fast, 1, 5, cells35)
	checkSchedule(t, nine, 901, 4, []string{"io512"})
	for _, c := range append(append([]col{}, fast...), nine...) {
		a := servers.Registry[c.name]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != "1" || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=1 (the vector stays on its standing CPU; nothing moves it; the loop CPUs are a constant of the module, not an env)", c.name, a.SUTEnv)
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
// three #1049-based arms additionally differ in server.go by the one `Workers: armWorkers,` line (and its comment) and in arm.go. The base arm has no
// arm.go and no Workers line: it is servers/celeris byte for byte.
func TestPinplan5ArmModulesAreCopiesOfServersCeleris(t *testing.T) {
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

// stripArmLines removes the arm's own lines from server.go: the comment line that names the arm and the Workers line itself (the lines are the v3 ones:
// the three #1049-based modules reuse pppin8_celeris/server.go byte for byte).
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

const knobEnv = "CELERIS_AB_PLAN_CPUS"

// The loop CPUs of a variant are a constant of its module (arm.go: `const armPlanCPUs = "<list>"` and an init() that os.Setenv's the knob from it, so
// a value the run's environment carries is overwritten and nothing can unset it); Config.Workers stays unset (armWorkers = 0) in every #1049-based
// arm; base and p8 never mention the knob (the p8 and base celeris commits do not have it); no other file of any module mentions it or reads the
// environment for a CPU list.
func TestPinplan5PlanKnobIsAModuleConstant(t *testing.T) {
	for _, arm := range arms {
		mod := modOf[arm]
		entries, _ := os.ReadDir(mod)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			src, _ := os.ReadFile(filepath.Join(mod, e.Name()))
			s := string(src)
			if strings.Contains(s, "GOMAXPROCS") || strings.Contains(s, "CELERIS_WORKERS") || strings.Contains(s, "Getenv(\"CELERIS_") {
				t.Errorf("%s/%s reads or sets a worker count or a CPU list from the environment or the runtime", mod, e.Name())
			}
			if strings.Contains(s, knobEnv) && !(e.Name() == "arm.go" && cpusOf[arm] != "") {
				t.Errorf("%s/%s mentions %s: only the variants' arm.go may", mod, e.Name(), knobEnv)
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
		s := string(src)
		if n := strings.Count(s, "const armWorkers = 0\n"); n != 1 {
			t.Errorf("%s/arm.go: want exactly one `const armWorkers = 0` (Config.Workers unset in every #1049-based arm)", mod)
		}
		want := cpusOf[arm]
		if want == "" { // p8: no knob at all
			if strings.Contains(s, "armPlanCPUs") || strings.Contains(s, "Setenv") {
				t.Errorf("%s/arm.go must not carry the knob", mod)
			}
			continue
		}
		if n := strings.Count(s, "const armPlanCPUs = \""+want+"\"\n"); n != 1 {
			t.Errorf("%s/arm.go: want exactly one `const armPlanCPUs = %q`", mod, want)
		}
		if n := strings.Count(s, "os.Setenv(\""+knobEnv+"\", armPlanCPUs)"); n != 1 || strings.Count(s, "Setenv") != 1 || !strings.Contains(s, "func init() {") {
			t.Errorf("%s/arm.go: want exactly one os.Setenv(%q, armPlanCPUs) inside init()", mod, knobEnv)
		}
		if strings.Contains(s, "os.Getenv") || strings.Contains(s, "os.Unsetenv") || strings.Contains(s, "LookupEnv") {
			t.Errorf("%s/arm.go reads or unsets the environment", mod)
		}
	}
}

// p8 reuses the v3 module and the celeris commit a193acb4; the variants (vnv, vx) differ from p8 ONLY in arm.go, in the module line and the celeris pin of go.mod
// and in the celeris lines of go.sum: everything else is byte for byte p8's, and vx differs from vnv only in the CPU list (and the comment).
func TestPinplan5VariantsDifferFromPin8OnlyInTheKnob(t *testing.T) {
	ref := "pppin8_celeris"
	for _, other := range []string{"ppvnv_celeris", "ppvx_celeris"} {
		for _, f := range []string{"chain_handlers.go", "debugvars.go", "driver_handlers.go", "streaming_handlers.go", "server.go"} {
			a, _ := os.ReadFile(filepath.Join(ref, f))
			b, _ := os.ReadFile(filepath.Join(other, f))
			if !bytes.Equal(a, b) {
				t.Errorf("%s/%s differs from %s/%s", other, f, ref, f)
			}
		}
		a, _ := os.ReadFile(filepath.Join(ref, "go.mod"))
		b, _ := os.ReadFile(filepath.Join(other, "go.mod"))
		if got := strings.ReplaceAll(strings.ReplaceAll(string(a), ref, other), pinPin, pinVar); got != string(b) {
			t.Errorf("%s/go.mod differs from %s/go.mod by more than the module line and the celeris pin", other, ref)
		}
		ga, _ := os.ReadFile(filepath.Join(ref, "go.sum"))
		gb, _ := os.ReadFile(filepath.Join(other, "go.sum"))
		rest := func(s string) string { // go.sum without its two celeris lines
			var out []string
			for _, l := range strings.Split(s, "\n") {
				if !strings.HasPrefix(l, "github.com/goceleris/celeris ") {
					out = append(out, l)
				}
			}
			return strings.Join(out, "\n")
		}
		if rest(string(ga)) != rest(string(gb)) {
			t.Errorf("%s/go.sum differs from %s/go.sum outside the celeris lines", other, ref)
		}
		if !strings.Contains(string(gb), "github.com/goceleris/celeris "+pinVar+" h1:") || !strings.Contains(string(gb), "github.com/goceleris/celeris "+pinVar+"/go.mod h1:") || strings.Contains(string(gb), pinPin) {
			t.Errorf("%s/go.sum must hold exactly the h1 lines of celeris %s", other, pinVar)
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
	// vnv and vx: arm.go identical after the CPU list is masked, from the constant on
	norm := func(mod string) string {
		b, _ := os.ReadFile(filepath.Join(mod, "arm.go"))
		s := string(b)
		s = s[strings.Index(s, "const armWorkers"):]
		return strings.ReplaceAll(strings.ReplaceAll(s, cpusOf["vnv"], "LIST"), cpusOf["vx"], "LIST")
	}
	if norm("ppvnv_celeris") != norm("ppvx_celeris") {
		t.Error("ppvnv_celeris/arm.go and ppvx_celeris/arm.go differ by more than the CPU list")
	}
	// base differs from p8 in the module line and the celeris pin of go.mod, nothing else (the v3 relation, kept)
	bm, _ := os.ReadFile(filepath.Join("ppbase_celeris", "go.mod"))
	a2 := strings.ReplaceAll(strings.ReplaceAll(string(gm), ref, "ppbase_celeris"), pinPin, pinBase)
	if a2 != string(bm) {
		t.Errorf("ppbase_celeris/go.mod differs from %s/go.mod by more than the module line and the celeris pin", ref)
	}
}

func expandCPUs(t *testing.T, list string) map[int]bool {
	t.Helper()
	out := map[int]bool{}
	for _, tok := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(tok, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			t.Fatalf("bad CPU list %q", list)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				t.Fatalf("bad CPU list %q", list)
			}
		}
		for c := a; c <= b; c++ {
			out[c] = true
		}
	}
	return out
}

// The two variants differ ONLY in which six big CPUs get a loop: V-NV frees CPUs 6 and 8 (the two busiest NIC-vector CPUs) and keeps 10 and 11; X keeps 6 and 8 and frees 10 and 11
// (the two highest-numbered big CPUs). Same loop count, same CPU 0, 1, 7, 9 loops, no little CPU in either.
func TestPinplan5VariantCPUSets(t *testing.T) {
	vnv, vx := expandCPUs(t, cpusOf["vnv"]), expandCPUs(t, cpusOf["vx"])
	if len(vnv) != 6 || len(vx) != 6 {
		t.Fatalf("loop counts: vnv %d vx %d, want 6 and 6", len(vnv), len(vx))
	}
	for _, c := range []int{0, 1, 7, 9} {
		if !vnv[c] || !vx[c] {
			t.Errorf("CPU %d must carry a loop in both variants", c)
		}
	}
	if vnv[6] || vnv[8] || !vnv[10] || !vnv[11] {
		t.Errorf("vnv must free CPUs 6 and 8 and keep 10 and 11: %v", vnv)
	}
	if !vx[6] || !vx[8] || vx[10] || vx[11] {
		t.Errorf("vx must keep CPUs 6 and 8 and free 10 and 11: %v", vx)
	}
	for c := 2; c <= 5; c++ {
		if vnv[c] || vx[c] {
			t.Errorf("little CPU %d carries a loop", c)
		}
	}
}
