package servers_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/p2b-io512-3arm-38, not for merge). Pins the layout of the P2b run (PREREG-P2B.md; user decision U-11, 2026-10-25): the exact (scenario, column) pairs the one
// dispatch glob selects (24 columns, one cell each), three arms (base = celeris v1.5.11 = servers/celeris_v1511; main = celeris main 713b40d = servers/celeris; vnv = the same
// 713b40d with the Listen thread narrowed = servers/celeris_vnv), the single vector position (the standing CPU 1), the seeded rotation, and that the vnv module is the main module
// plus the arm and nothing else.
const (
	p2bGlob = "get-simple-512c/pinp38b-*-io512"
	p2bWant = 24 // 8 reps x 3 arms
)

var (
	p2bArms = []string{"base", "main", "vnv"}
	p2bMod  = map[string]string{"base": "celeris_v1511", "main": "celeris", "vnv": "celeris_vnv"}
	p2bPin  = map[string]string{"base": p2PinBase, "main": p2PinMain, "vnv": p2PinMain}
	p2bFW   = map[string]string{"base": "celeris-pp-base", "main": "celeris-pp-main", "vnv": "celeris-pp-vnv"}
)

type p2bCol struct {
	name, arm string
	seq, rep  int
}

func p2bCols(t *testing.T) []p2bCol {
	t.Helper()
	re := regexp.MustCompile(`^pinp38b-(\d{3})-r(\d)-(base|main|vnv)-io512$`)
	var cols []p2bCol
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinp38b-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinp38b-<seq>-r<rep>-<base|main|vnv>-io512", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		cols = append(cols, p2bCol{name, m[3], seq, rep})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	return cols
}

func TestP2bGlobSelectsOneCellPerColumn(t *testing.T) {
	got := p2Matched(t, p2bGlob)
	if len(got) != p2bWant {
		t.Fatalf("the P2b glob selects %d columns, want %d", len(got), p2bWant)
	}
	for c, s := range got {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1 (the stall captures assume one window per column)", c, len(s), s)
		}
		if !strings.HasPrefix(c, "pinp38b-") || !strings.HasSuffix(c, "-io512") {
			t.Errorf("the P2b glob selects the foreign column %s", c)
		}
	}
	// the P2 globs (pinp38-) select no P2b column and the P2b glob selects no P2 column: the two experiments never mix on one dispatch
	for _, g := range []string{p2Fast, p2Nine} {
		for c := range p2Matched(t, g) {
			if strings.HasPrefix(c, "pinp38b-") {
				t.Errorf("the P2 glob %q selects the P2b column %s", g, c)
			}
		}
	}
	if !strings.Contains(p2bGlob, "-io512") {
		t.Error("the glob must contain '-io512': benchmark-tier.yml keys the 90 s / 20 s window on that substring")
	}
	// the earlier experiments' columns other than P2's own 72 are not in the registry
	for name := range servers.Registry {
		for _, old := range []string{"irqplace-", "pinplan-", "pinplan2-", "pinplan3-", "pinplan4-", "pinplan5-", "pinplan6-", "pinplan7-"} {
			if strings.HasPrefix(name, old) {
				t.Errorf("column %s of an earlier experiment is in this branch's registry", name)
			}
		}
	}
}

func TestP2bLayout(t *testing.T) {
	cols := p2bCols(t)
	if len(cols) != p2bWant {
		t.Fatalf("%d P2b columns, want %d", len(cols), p2bWant)
	}
	const reps = 8
	var sched [][]string
	for i, c := range cols { // sorted name order is the execution order
		if c.seq != 1+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, 1+i)
		}
		if c.rep != i/3+1 {
			t.Errorf("%s: rep %d at slot %d, want rep %d", c.name, c.rep, i, i/3+1)
		}
		if i%3 == 0 {
			sched = append(sched, nil)
		}
		sched[len(sched)-1] = append(sched[len(sched)-1], c.arm)
	}
	if len(sched) != reps {
		t.Fatalf("%d reps, want %d", len(sched), reps)
	}
	for k, s := range sched { // every rep holds the three arms once
		c := slices.Clone(s)
		slices.Sort(c)
		if !slices.Equal(c, p2bArms) {
			t.Errorf("rep %d holds %v, want one slot per arm %v", k+1, s, p2bArms)
		}
	}
	// reps 1-6 are the six distinct permutations
	seen := map[string]bool{}
	for _, s := range sched[:6] {
		seen[strings.Join(s, ",")] = true
	}
	if len(seen) != 6 {
		t.Errorf("reps 1-6 hold %d distinct permutations, want all 6", len(seen))
	}
	lead := make([]string, reps)
	for k, s := range sched {
		lead[k] = s[0]
	}
	count := func(arm string) (n int) {
		for _, l := range lead {
			if l == arm {
				n++
			}
		}
		return n
	}
	var counts []int
	for _, a := range p2bArms {
		counts = append(counts, count(a))
	}
	slices.Sort(counts)
	if !slices.Equal(counts, []int{2, 3, 3}) {
		t.Errorf("lead counts %v (leaders %v), want 3 / 3 / 2 over the 8 reps", counts, lead)
	}
	for k := 0; k+1 < reps; k++ {
		if sched[k][0] == sched[k+1][0] {
			t.Errorf("%s leads reps %d and %d", sched[k][0], k+1, k+2)
		}
		if sched[k][2] == sched[k+1][2] {
			t.Errorf("%s is last in reps %d and %d", sched[k][2], k+1, k+2)
		}
	}
	for pos := 0; pos < 3; pos++ {
		for _, a := range p2bArms {
			n := 0
			for _, s := range sched {
				if s[pos] == a {
					n++
				}
			}
			if n > 3 {
				t.Errorf("arm %s holds position %d in %d of 8 reps (max 3)", a, pos+1, n)
			}
		}
	}
	// per-column registry facts
	for _, c := range cols {
		a := servers.Registry[c.name]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != "1" || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=1 (the vector stays on its standing CPU; nothing moves it)", c.name, a.SUTEnv)
		}
		gb, ok := a.Bin.(servers.GoBinary)
		if !ok {
			t.Fatalf("%s: Bin is not a GoBinary", c.name)
		}
		dir := filepath.Join("..", gb.ModuleDir)
		if filepath.Base(dir) != p2bMod[c.arm] || a.FrameworkVersion != p2bPin[c.arm] || a.Framework != p2bFW[c.arm] {
			t.Errorf("%s: module %s framework %s version %s, want %s %s %s", c.name, dir, a.Framework, a.FrameworkVersion, p2bMod[c.arm], p2bFW[c.arm], p2bPin[c.arm])
		}
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gomod), "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", c.name, a.FrameworkVersion, dir)
		}
		tw := servers.Registry["celeris-iouring-h1-async"]
		if a.Engine != tw.Engine || a.Language != tw.Language || a.Capabilities != tw.Capabilities {
			t.Errorf("%s: engine/language/capabilities differ from celeris-iouring-h1-async", c.name)
		}
	}
}

// The vnv module is servers/celeris plus the arm: same files byte for byte, the same go.sum, a go.mod that differs in the module line only, a server.go that differs by ONE added
// line (the call armBeforeStart() right before srv.Start()), and the arm's own files. Anything else would make the vnv column a different server, not a different loop placement.
func TestP2bVnvModuleIsMainPlusTheArm(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	armOnly := map[string]bool{"arm_linux.go": true, "arm_other.go": true, "arm_linux_test.go": true}
	mainFiles, _ := filepath.Glob("celeris/*")
	vnvFiles, _ := filepath.Glob("celeris_vnv/*")
	var haveVnv []string
	for _, f := range vnvFiles {
		if !armOnly[filepath.Base(f)] {
			haveVnv = append(haveVnv, filepath.Base(f))
		}
	}
	var haveMain []string
	for _, f := range mainFiles {
		if fi, _ := os.Stat(f); fi.IsDir() || filepath.Base(f) == "celeris" { // a built binary is not part of the module
			continue
		}
		haveMain = append(haveMain, filepath.Base(f))
	}
	slices.Sort(haveVnv)
	slices.Sort(haveMain)
	if !slices.Equal(haveMain, haveVnv) {
		t.Fatalf("celeris has %v, celeris_vnv has %v apart from the arm files", haveMain, haveVnv)
	}
	for _, name := range haveMain {
		m, v := read("celeris/"+name), read("celeris_vnv/"+name)
		switch name {
		case "go.mod":
			want := strings.Replace(m, "module github.com/goceleris/probatorium/servers/celeris\n", "module github.com/goceleris/probatorium/servers/celeris_vnv\n", 1)
			if v != want || v == m {
				t.Errorf("celeris_vnv/go.mod is not celeris/go.mod with the module line renamed")
			}
		case "server.go":
			want := strings.Replace(m, "\tif err := srv.Start(); err != nil {", "\tarmBeforeStart()\n\tif err := srv.Start(); err != nil {", 1)
			if strings.Count(m, "srv.Start()") != 1 || v != want {
				t.Errorf("celeris_vnv/server.go is not celeris/server.go plus the one armBeforeStart() call before srv.Start()")
			}
		default:
			if m != v {
				t.Errorf("celeris_vnv/%s differs from celeris/%s", name, name)
			}
		}
	}
	for f := range armOnly {
		if _, err := os.Stat("celeris_vnv/" + f); err != nil {
			t.Errorf("celeris_vnv/%s missing", f)
		}
	}
}

// Neither the vnv module nor the main one sets Config.Workers, reads a worker count or a CPU list, or carries the old experiment knob in CODE (comments may name it): the vnv arm
// differs from main by the Listen thread's mask and nothing else.
func TestP2bArmsLeaveTheWorkerPlanToTheEngine(t *testing.T) {
	for _, mod := range []string{"celeris", "celeris_vnv"} {
		ents, err := os.ReadDir(mod)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			src, _ := os.ReadFile(filepath.Join(mod, e.Name()))
			var code []string
			for _, l := range strings.Split(string(src), "\n") {
				if i := strings.Index(l, "//"); i >= 0 {
					l = l[:i]
				}
				code = append(code, l)
			}
			s := strings.Join(code, "\n")
			for _, bad := range []string{"Workers:", "GOMAXPROCS", "CELERIS_WORKERS", "CELERIS_AB_PLAN_CPUS", "Setenv"} {
				if strings.Contains(s, bad) {
					t.Errorf("%s/%s contains %q in code", mod, e.Name(), bad)
				}
			}
		}
	}
	// the narrowing set is the registered one: every CPU but 6 and 8 (the littles stay in so that the planner still sees the heterogeneous capacities)
	src, _ := os.ReadFile("celeris_vnv/arm_linux.go")
	if !strings.Contains(string(src), `var armListenCPUs = "0-5,7,9-11"`) {
		t.Error(`celeris_vnv/arm_linux.go: armListenCPUs is not "0-5,7,9-11"`)
	}
}
