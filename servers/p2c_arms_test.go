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

// THROWAWAY (ab/p2c-io512-discriminate-38, not for merge). Pins the layout of the P2c run (PREREG-P2C.md; user decision U-13, 2026-10-25): the exact (scenario, column) pairs the one
// dispatch glob selects (32 columns, one cell each), four arms (main = celeris main 713b40d = servers/celeris, unmodified; noc8 = the same 713b40d with the Listen thread narrowed to
// every CPU but 8 = servers/celeris_noc8; nobp = the same 713b40d with the busy-poll socket options disabled by a one-line patch of a pinned module copy = servers/celeris_nobp;
// base = celeris v1.5.11 = servers/celeris_v1511, a reference arm), the single vector position (the standing CPU 1), the seeded Williams rotation, and that the noc8 and nobp
// modules are the main module plus the arm and nothing else.
const (
	p2cGlob = "get-simple-512c/pinp38c-*-io512"
	p2cWant = 32 // 8 reps x 4 arms
)

var (
	p2cArms = []string{"base", "main", "noc8", "nobp"}
	p2cMod  = map[string]string{"base": "celeris_v1511", "main": "celeris", "noc8": "celeris_noc8", "nobp": "celeris_nobp"}
	p2cPin  = map[string]string{"base": p2PinBase, "main": p2PinMain, "noc8": p2PinMain, "nobp": p2PinMain}
	p2cFW   = map[string]string{"base": "celeris-pp-base", "main": "celeris-pp-main", "noc8": "celeris-pp-noc8", "nobp": "celeris-pp-nobp"}
)

type p2cCol struct {
	name, arm string
	seq, rep  int
}

func p2cCols(t *testing.T) []p2cCol {
	t.Helper()
	re := regexp.MustCompile(`^pinp38c-(\d{3})-r(\d)-(base|main|noc8|nobp)-io512$`)
	var cols []p2cCol
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinp38c-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinp38c-<seq>-r<rep>-<base|main|noc8|nobp>-io512", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		cols = append(cols, p2cCol{name, m[3], seq, rep})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	return cols
}

func TestP2cGlobSelectsOneCellPerColumn(t *testing.T) {
	got := p2Matched(t, p2cGlob)
	if len(got) != p2cWant {
		t.Fatalf("the P2c glob selects %d columns, want %d", len(got), p2cWant)
	}
	for c, s := range got {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1 (the stall captures assume one window per column)", c, len(s), s)
		}
		if !strings.HasPrefix(c, "pinp38c-") || !strings.HasSuffix(c, "-io512") {
			t.Errorf("the P2c glob selects the foreign column %s", c)
		}
	}
	// the P2 and P2b globs select no P2c column and the P2c glob selects no P2 / P2b column: the experiments never mix on one dispatch
	for _, g := range []string{p2Fast, p2Nine, p2bGlob} {
		for c := range p2Matched(t, g) {
			if strings.HasPrefix(c, "pinp38c-") {
				t.Errorf("the glob %q selects the P2c column %s", g, c)
			}
		}
	}
	if !strings.Contains(p2cGlob, "-io512") {
		t.Error("the glob must contain '-io512': benchmark-tier.yml keys the 90 s / 20 s window on that substring")
	}
}

func TestP2cLayout(t *testing.T) {
	cols := p2cCols(t)
	if len(cols) != p2cWant {
		t.Fatalf("%d P2c columns, want %d", len(cols), p2cWant)
	}
	const reps, k = 8, 4
	var sched [][]string
	for i, c := range cols { // sorted name order is the execution order
		if c.seq != 1+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, 1+i)
		}
		if c.rep != i/k+1 {
			t.Errorf("%s: rep %d at slot %d, want rep %d", c.name, c.rep, i, i/k+1)
		}
		if i%k == 0 {
			sched = append(sched, nil)
		}
		sched[len(sched)-1] = append(sched[len(sched)-1], c.arm)
	}
	if len(sched) != reps {
		t.Fatalf("%d reps, want %d", len(sched), reps)
	}
	for r, s := range sched { // every rep holds the four arms once
		c, w := slices.Clone(s), slices.Clone(p2cArms)
		slices.Sort(c)
		slices.Sort(w)
		if !slices.Equal(c, w) {
			t.Errorf("rep %d holds %v, want one slot per arm %v", r+1, s, p2cArms)
		}
	}
	// every arm holds every slot exactly twice; leads twice; is last twice
	for pos := 0; pos < k; pos++ {
		for _, a := range p2cArms {
			n := 0
			for _, s := range sched {
				if s[pos] == a {
					n++
				}
			}
			if n != 2 {
				t.Errorf("arm %s holds slot %d in %d of 8 reps, want 2", a, pos+1, n)
			}
		}
	}
	// each half (reps 1-4, reps 5-8) is a Williams design: every ordered pair of arms is adjacent exactly once
	for h, half := range [][][]string{sched[:4], sched[4:]} {
		adj := map[[2]string]int{}
		for _, s := range half {
			for i := 0; i+1 < k; i++ {
				adj[[2]string{s[i], s[i+1]}]++
			}
		}
		if len(adj) != k*(k-1) {
			t.Errorf("half %d: %d distinct ordered adjacent pairs, want %d", h+1, len(adj), k*(k-1))
		}
		for p, n := range adj {
			if n != 1 {
				t.Errorf("half %d: the pair %v is adjacent %d times, want 1", h+1, p, n)
			}
		}
	}
	for r := 0; r+1 < reps; r++ {
		if sched[r][0] == sched[r+1][0] {
			t.Errorf("%s leads reps %d and %d", sched[r][0], r+1, r+2)
		}
		if sched[r][k-1] == sched[r+1][k-1] {
			t.Errorf("%s is last in reps %d and %d", sched[r][k-1], r+1, r+2)
		}
	}
	// per-column registry facts
	tw := servers.Registry["celeris-iouring-h1-async"]
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
		if filepath.Base(dir) != p2cMod[c.arm] || a.FrameworkVersion != p2cPin[c.arm] || a.Framework != p2cFW[c.arm] {
			t.Errorf("%s: module %s framework %s version %s, want %s %s %s", c.name, dir, a.Framework, a.FrameworkVersion, p2cMod[c.arm], p2cFW[c.arm], p2cPin[c.arm])
		}
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gomod), "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", c.name, a.FrameworkVersion, dir)
		}
		if a.Engine != tw.Engine || a.Language != tw.Language || a.Capabilities != tw.Capabilities {
			t.Errorf("%s: engine/language/capabilities differ from celeris-iouring-h1-async", c.name)
		}
	}
}

func p2cRead(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// moduleFiles lists the plain files of a module directory (a built binary named like the module is not part of it).
func p2cModuleFiles(dir string) []string {
	ents, _ := os.ReadDir(dir)
	var out []string
	for _, e := range ents {
		if e.IsDir() || e.Name() == filepath.Base(dir) {
			continue
		}
		out = append(out, e.Name())
	}
	slices.Sort(out)
	return out
}

// noc8 and nobp are servers/celeris plus the arm: the same files byte for byte (the same go.sum for noc8; nobp's go.sum is main's minus the two celeris lines, proved in
// servers/celeris_nobp's own test), a go.mod that differs in the module line only (nobp: plus the replace line), a server.go that differs by ONE added line (the call
// armBeforeStart before srv.Start()), and the arm's own files. Anything else would make the column a different server, not a different loop placement / socket option set.
func TestP2cArmModulesAreMainPlusTheArm(t *testing.T) {
	type spec struct {
		dir, startCall string
		armFiles       []string
	}
	for _, sp := range []spec{
		{"celeris_noc8", "\tarmBeforeStart(*bind)\n", []string{"arm_linux.go", "arm_other.go", "arm_linux_test.go", "probe_linux.go", "probe_linux_test.go"}},
		{"celeris_nobp", "\tarmBeforeStart(*bind)\n", []string{"arm_linux.go", "arm_other.go", "probe_linux.go", "probe_linux_test.go", "nobp_test.go"}},
	} {
		var haveMain, haveArm []string
		for _, f := range p2cModuleFiles("celeris") {
			haveMain = append(haveMain, f)
		}
		for _, f := range p2cModuleFiles(sp.dir) {
			if !slices.Contains(sp.armFiles, f) {
				haveArm = append(haveArm, f)
			}
		}
		if !slices.Equal(haveMain, haveArm) {
			t.Fatalf("celeris has %v, %s has %v apart from the arm files", haveMain, sp.dir, haveArm)
		}
		for _, f := range sp.armFiles {
			if _, err := os.Stat(filepath.Join(sp.dir, f)); err != nil {
				t.Errorf("%s/%s missing", sp.dir, f)
			}
		}
		for _, name := range haveMain {
			m, v := p2cRead(t, "celeris/"+name), p2cRead(t, sp.dir+"/"+name)
			switch name {
			case "go.mod":
				want := strings.Replace(m, "module github.com/goceleris/probatorium/servers/celeris\n", "module github.com/goceleris/probatorium/servers/"+sp.dir+"\n", 1)
				if sp.dir == "celeris_nobp" {
					want += "replace github.com/goceleris/celeris => ./_celeris\n"
				}
				if v != want || v == m {
					t.Errorf("%s/go.mod is not celeris/go.mod with the module line renamed%s", sp.dir, map[bool]string{true: " plus the replace line", false: ""}[sp.dir == "celeris_nobp"])
				}
			case "go.sum":
				if sp.dir == "celeris_nobp" {
					continue // main's minus the two celeris lines: servers/celeris_nobp/nobp_test.go
				}
				if m != v {
					t.Errorf("%s/go.sum differs from celeris/go.sum", sp.dir)
				}
			case "server.go":
				want := strings.Replace(m, "\tif err := srv.Start(); err != nil {", sp.startCall+"\tif err := srv.Start(); err != nil {", 1)
				if strings.Count(m, "srv.Start()") != 1 || v != want {
					t.Errorf("%s/server.go is not celeris/server.go plus the one call before srv.Start()", sp.dir)
				}
			default:
				if m != v {
					t.Errorf("%s/%s differs from celeris/%s", sp.dir, name, name)
				}
			}
		}
	}
	// noc8's mask is the registered one: every CPU but 8 (the littles stay in so that the planner still sees the heterogeneous capacities); nobp has no mask code at all.
	if !strings.Contains(p2cRead(t, "celeris_noc8/arm_linux.go"), `var armListenCPUs = "0-7,9-11"`) {
		t.Error(`celeris_noc8/arm_linux.go: armListenCPUs is not "0-7,9-11"`)
	}
	if strings.Contains(p2cRead(t, "celeris_nobp/arm_linux.go"), "sched_setaffinity") || strings.Contains(p2cRead(t, "celeris_nobp/arm_linux.go"), "SCHED_SETAFFINITY") {
		t.Error("celeris_nobp must not touch affinity")
	}
	// the patched copy lives in a directory the go tool ignores (leading underscore) and is committed
	if _, err := os.Stat("celeris_nobp/_celeris/internal/sockopts/sockopts_linux.go"); err != nil {
		t.Errorf("the pinned copy is missing: %v", err)
	}
}

// None of the modules sets Config.Workers, reads a worker count or a CPU list, or carries an old experiment knob in CODE (comments may name it): the arms differ from main by the
// Listen thread's mask (noc8) or by the busy-poll option block (nobp) and nothing else.
func TestP2cArmsLeaveTheWorkerPlanToTheEngine(t *testing.T) {
	for _, mod := range []string{"celeris", "celeris_noc8", "celeris_nobp"} {
		ents, err := os.ReadDir(mod)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			if !strings.HasSuffix(e.Name(), ".go") || strings.HasSuffix(e.Name(), "_test.go") {
				continue
			}
			var code []string
			for _, l := range strings.Split(p2cRead(t, filepath.Join(mod, e.Name())), "\n") {
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
}

// The accepted-socket probe is ONE file in two modules: the comment of each says so, so this says it too.
func TestP2cProbeFileIsIdenticalInNoc8AndNobp(t *testing.T) {
	a, b := p2cRead(t, "celeris_noc8/probe_linux.go"), p2cRead(t, "celeris_nobp/probe_linux.go")
	if a != b {
		t.Error("servers/celeris_noc8/probe_linux.go and servers/celeris_nobp/probe_linux.go differ")
	}
	for _, f := range []string{"celeris_noc8/probe_linux_test.go", "celeris_nobp/probe_linux_test.go"} {
		if !strings.Contains(p2cRead(t, f), "probeAcceptedSocket(") {
			t.Errorf("%s does not exercise probeAcceptedSocket", f)
		}
	}
}
