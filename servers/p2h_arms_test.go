package servers_test

import (
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/p2h-io512-amd64, not for merge). Pins the layout of the P2h run (PREREG-P2H.md; E4 of lane D9, maintainer decision 2026-11-01): the amd64 counterpart of
// P2g. The exact (scenario, column) pairs the one dispatch glob selects (16 columns, one cell each), two arms (main = celeris main 713b40d = servers/celeris, unmodified; nobp =
// the same 713b40d with the busy-poll socket options disabled = servers/celeris_nobp; the module equivalence of nobp to main is pinned by TestP2cArmModulesAreMainPlusTheArm,
// which still runs), NO SUTEnv (the msr1-only IRQPLACE block of run_bench_cell.yml must stay off), and the rotation (strictly alternating, nobp leads rep 1).
const (
	p2hGlob = "get-simple-512c/pinp38h-*-io512"
	p2hWant = 16 // 8 reps x 2 arms
)

var (
	p2hMod = map[string]string{"main": "celeris", "nobp": "celeris_nobp"}
	p2hFW  = map[string]string{"main": "celeris-pp-main", "nobp": "celeris-pp-nobp"}
)

type p2hCol struct {
	name, arm string
	seq, rep  int
}

func p2hCols(t *testing.T) []p2hCol {
	t.Helper()
	re := regexp.MustCompile(`^pinp38h-(\d{3})-r(\d+)-(main|nobp)-io512$`)
	var cols []p2hCol
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinp38h-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinp38h-<seq>-r<rep>-<main|nobp>-io512", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		cols = append(cols, p2hCol{name, m[3], seq, rep})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	return cols
}

func TestP2hGlobSelectsOneCellPerColumn(t *testing.T) {
	got := p2Matched(t, p2hGlob)
	if len(got) != p2hWant {
		t.Fatalf("the P2h glob selects %d columns, want %d", len(got), p2hWant)
	}
	for c, s := range got {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1 (the stall captures assume one window per column)", c, len(s), s)
		}
		if !strings.HasPrefix(c, "pinp38h-") || !strings.HasSuffix(c, "-io512") {
			t.Errorf("the P2h glob selects the foreign column %s", c)
		}
	}
	// no earlier glob selects a P2h column and the P2h glob selects no earlier column: the experiments never mix on one dispatch
	for _, g := range []string{p2Fast, p2Nine, p2bGlob, p2cGlob, "get-simple-512c/pinp38c-*-main-io512,get-simple-512c/pinp38c-*-nobp-io512"} {
		for c := range p2Matched(t, g) {
			if strings.HasPrefix(c, "pinp38h-") {
				t.Errorf("the glob %q selects the P2h column %s", g, c)
			}
		}
	}
	if !strings.Contains(p2hGlob, "-io512") {
		t.Error("the glob must contain '-io512': benchmark-tier.yml keys the 90 s / 20 s window on that substring")
	}
}

func TestP2hLayout(t *testing.T) {
	cols := p2hCols(t)
	if len(cols) != p2hWant {
		t.Fatalf("%d P2h columns, want %d", len(cols), p2hWant)
	}
	const reps, k = 8, 2
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
	// the pre-declared rotation: reps 1-8 of p2c-gen.py's seed-20267277 rotation with noc8 removed = strictly alternating, nobp first in the odd reps
	for r, s := range sched {
		want := []string{"nobp", "main"}
		if r%2 == 1 {
			want = []string{"main", "nobp"}
		}
		if !slices.Equal(s, want) {
			t.Errorf("rep %d runs %v, want %v", r+1, s, want)
		}
	}
	// per-column registry facts
	tw := servers.Registry["celeris-iouring-h1-async"]
	for _, c := range cols {
		a := servers.Registry[c.name]
		if len(a.SUTEnv) != 0 {
			t.Errorf("%s: SUTEnv %v, want none (AB_IRQ_VECTOR_CPU would trip the msr1-only IRQPLACE assert on msa2-server)", c.name, a.SUTEnv)
		}
		gb, ok := a.Bin.(servers.GoBinary)
		if !ok {
			t.Fatalf("%s: Bin is not a GoBinary", c.name)
		}
		dir := filepath.Join("..", gb.ModuleDir)
		if filepath.Base(dir) != p2hMod[c.arm] || a.FrameworkVersion != p2PinMain || a.Framework != p2hFW[c.arm] {
			t.Errorf("%s: module %s framework %s version %s, want %s %s %s", c.name, dir, a.Framework, a.FrameworkVersion, p2hMod[c.arm], p2hFW[c.arm], p2PinMain)
		}
		gomod := p2cRead(t, filepath.Join(dir, "go.mod"))
		if !strings.Contains(gomod, "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", c.name, a.FrameworkVersion, dir)
		}
		if a.Engine != tw.Engine || a.Language != tw.Language || a.Capabilities != tw.Capabilities {
			t.Errorf("%s: engine/language/capabilities differ from celeris-iouring-h1-async", c.name)
		}
	}
}
