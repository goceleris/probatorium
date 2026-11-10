package servers_test

import (
	"fmt"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/p3a-rc1-20261110, not for merge). Pins what p3a-gen.py generates: the registry columns p3a-<D>-<seq>-r<rep>-<arm>-<engine> of servers/p3a_arms.go, their env,
// their rated plans, and the tie of the two env names to the knob sources of servers/celeris. The expectations come from the files that exist (go.mod, the knob sources), not
// from the generator's spec, so the test also fails when the generator is run with a stale spec or the registry is edited by hand. It does not know the absolute rates: they are
// a data file of the generator; the only thing it asserts about them is the Placeholder contract (all zero, or all positive and equal on every arm of a row).

const p3aPrefix = "p3a"

var p3aNameRE = regexp.MustCompile(`^p3a-([12])-(\d{3})-r(\d+)-(k0|k2|k4|ctl)-(epoll-h1-sync|epoll-auto\+upg-async|std-h1|hertz-h2)$`)

type p3aCol struct {
	name, arm, engine string
	d, seq, rep       int
}

func p3aCols(t *testing.T) []p3aCol {
	t.Helper()
	var cols []p3aCol
	for name := range servers.Registry {
		if !strings.HasPrefix(name, p3aPrefix+"-") {
			continue
		}
		m := p3aNameRE.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match %s-<D>-<seq>-r<rep>-<arm>-<engine>", name, p3aPrefix)
			continue
		}
		d, _ := strconv.Atoi(m[1])
		seq, _ := strconv.Atoi(m[2])
		rep, _ := strconv.Atoi(m[3])
		cols = append(cols, p3aCol{name, m[4], m[5], d, seq, rep})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	if len(cols) == 0 {
		t.Fatal("no p3a column in the registry")
	}
	return cols
}

var p3aRow = map[string]struct{ scenario, celerisEngine string }{
	"epoll-h1-sync":        {"ws-large-echo", "epoll-h1-sync"},
	"std-h1":               {"ws-large-echo", "std-h1"},
	"epoll-auto+upg-async": {"get-json-h2", "epoll-auto+upg-async"},
	"hertz-h2":             {"get-json-h2", ""},
}

func TestP3aLayout(t *testing.T) {
	cols := p3aCols(t)
	if len(cols) != 80 {
		t.Fatalf("%d p3a columns, want 80 (10 reps x 8)", len(cols))
	}
	perD := map[int][]p3aCol{}
	for _, c := range cols {
		perD[c.d] = append(perD[c.d], c)
	}
	for d, want := range map[int][2]int{1: {1, 5}, 2: {6, 10}} {
		cs := perD[d]
		if len(cs) != 40 {
			t.Errorf("dispatch %d has %d columns, want 40", d, len(cs))
		}
		for i, c := range cs { // sorted name order == execution order, seq 1..40
			if c.seq != i+1 {
				t.Errorf("dispatch %d: sorted position %d has seq %d", d, i+1, c.seq)
			}
			if c.rep < want[0] || c.rep > want[1] {
				t.Errorf("%s: rep %d outside dispatch %d's %d-%d", c.name, c.rep, d, want[0], want[1])
			}
		}
		byRep := map[int][]p3aCol{}
		for _, c := range cs {
			byRep[c.rep] = append(byRep[c.rep], c)
		}
		for rep, b := range byRep {
			if len(b) != 8 {
				t.Errorf("dispatch %d rep %d has %d columns, want 8", d, rep, len(b))
			}
			if b[len(b)-1].seq-b[0].seq != 7 {
				t.Errorf("dispatch %d rep %d is not a contiguous block of 8", d, rep)
			}
			n := map[string]int{}
			for _, c := range b {
				n[c.arm+"/"+c.engine]++
			}
			for _, k := range []string{"k0/epoll-h1-sync", "k2/epoll-h1-sync", "k4/epoll-h1-sync", "ctl/std-h1", "k0/epoll-auto+upg-async", "k2/epoll-auto+upg-async", "k4/epoll-auto+upg-async", "ctl/hertz-h2"} {
				if n[k] != 1 {
					t.Errorf("dispatch %d rep %d holds %q %d times, want once", d, rep, k, n[k])
				}
			}
		}
	}
	per := map[string]int{}
	for _, c := range cols {
		per[c.arm+"/"+c.engine]++
	}
	if len(per) != 8 {
		t.Errorf("%d (arm, engine) pairs, want 8: %v", len(per), per)
	}
	for k, n := range per {
		if n != 10 {
			t.Errorf("%s has %d columns, want 10 (reps 1-10)", k, n)
		}
	}
}

// The env names of the registry are the constants of the knob sources, character for character.
func TestP3aEnvNamesAreTheKnobConstants(t *testing.T) {
	re := func(file, name string) string {
		m := regexp.MustCompile(`(?m)^const ` + name + ` = "([A-Z0-9_]+)"`).FindStringSubmatch(readFile(t, "celeris", file))
		if m == nil {
			t.Fatalf("servers/celeris/%s has no const %s", file, name)
		}
		return m[1]
	}
	workers, sched := re("workers_knob.go", "workersEnv"), re("sched_sampler.go", "schedSampleEnv")
	seenW, seenS := 0, 0
	for _, c := range p3aCols(t) {
		a := servers.Registry[c.name]
		want := map[string]string{}
		switch {
		case c.engine == "hertz-h2":
		default:
			want[sched] = "1"
			if c.arm == "k2" {
				want[workers] = "14"
			}
			if c.arm == "k4" {
				want[workers] = "12"
			}
		}
		if len(want) == 0 {
			want = nil
		}
		if !reflect.DeepEqual(a.SUTEnv, want) {
			t.Errorf("%s: SUTEnv %v, want %v", c.name, a.SUTEnv, want)
		}
		if _, ok := a.SUTEnv[workers]; ok {
			seenW++
		}
		if _, ok := a.SUTEnv[sched]; ok {
			seenS++
		}
		if c.arm == "k0" && a.SUTEnv[workers] != "" {
			t.Errorf("%s: k0 must leave %s unset (the genuine default), has %q", c.name, workers, a.SUTEnv[workers])
		}
	}
	if seenW != 40 || seenS != 70 { // k2 + k4 on two rows x 10 reps; every celeris column (3 arms + std ctl on ws, 3 arms on h2) x 10
		t.Errorf("%s on %d columns (want 40), %s on %d (want 70)", workers, seenW, sched, seenS)
	}
}

func TestP3aColumnsDifferFromTheirBaseOnlyWhereIntended(t *testing.T) {
	pin := celerisRequire(t, "celeris")
	for _, c := range p3aCols(t) {
		a := servers.Registry[c.name]
		baseName := "celeris-" + c.engine
		if c.engine == "hertz-h2" {
			baseName = "hertz-h2"
		}
		base, ok := servers.Registry[baseName]
		if !ok {
			t.Fatalf("%s: no base column %s", c.name, baseName)
		}
		if a.Name != c.name || a.Engine != base.Engine || a.Category != base.Category || a.Language != base.Language || a.Capabilities != base.Capabilities || a.Baseline {
			t.Errorf("%s differs from %s in more than Name/Framework/Version/Bin/SUTEnv/RatedPlans: %+v vs %+v", c.name, baseName, a, base)
		}
		gb, isGo := a.Bin.(servers.GoBinary)
		if !isGo {
			t.Fatalf("%s: Bin is not a GoBinary", c.name)
		}
		if c.engine == "hertz-h2" {
			if a.Framework != "hertz" || a.FrameworkVersion != base.FrameworkVersion || gb.ModuleDir != "servers/hertz" {
				t.Errorf("%s: the hertz control must keep framework/version/module: %s %s %s", c.name, a.Framework, a.FrameworkVersion, gb.ModuleDir)
			}
			continue
		}
		if a.Framework != "celeris-p3a-"+c.arm || a.Framework == "celeris" { // never "celeris": the report builders stamp the main pin on that framework
			t.Errorf("%s: framework %q", c.name, a.Framework)
		}
		if a.FrameworkVersion != pin || gb.ModuleDir != "servers/celeris" {
			t.Errorf("%s: version %s module %s, want %s servers/celeris (go.mod)", c.name, a.FrameworkVersion, gb.ModuleDir, pin)
		}
		if _, hasIRQ := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; hasIRQ {
			t.Errorf("%s: AB_IRQ_VECTOR_CPU trips the msr1-only IRQPLACE block on msa2-server", c.name)
		}
	}
}

// Each column carries exactly one plan, for its row's scenario: celeris columns 0.5f 0.75f + two absolute passes, controls two absolute passes; the absolute rates are the same on
// every column of a row, and are all zero (Placeholder) or all positive (frozen) - never a mix.
func TestP3aRatedPlans(t *testing.T) {
	abs := map[string][]float64{}
	ph := map[string]bool{}
	for _, c := range p3aCols(t) {
		a := servers.Registry[c.name]
		row := p3aRow[c.engine]
		if len(a.RatedPlans) != 1 {
			t.Errorf("%s: %d rated plans, want 1", c.name, len(a.RatedPlans))
			continue
		}
		p, ok := a.RatedPlans[row.scenario]
		if !ok {
			t.Errorf("%s: no plan for its scenario %s (has %v)", c.name, row.scenario, a.RatedPlans)
			continue
		}
		wantF := []float64{0.5, 0.75}
		if c.arm == "ctl" {
			wantF = nil
		}
		if !slices.Equal(p.Fractions, wantF) {
			t.Errorf("%s: fractions %v, want %v", c.name, p.Fractions, wantF)
		}
		if len(p.AbsoluteRPS) != 2 {
			t.Errorf("%s: %d absolute rates, want 2", c.name, len(p.AbsoluteRPS))
			continue
		}
		if n := len(p.Fractions) + len(p.AbsoluteRPS); n == 0 || n > 4 {
			t.Errorf("%s: %d passes (the hang guard budgets 4)", c.name, n)
		}
		if prev, seen := abs[row.scenario]; seen {
			if !slices.Equal(prev, p.AbsoluteRPS) || ph[row.scenario] != p.Placeholder {
				t.Errorf("%s: absolute rates %v (placeholder %v) differ from the row's %v (%v)", c.name, p.AbsoluteRPS, p.Placeholder, prev, ph[row.scenario])
			}
		}
		abs[row.scenario], ph[row.scenario] = p.AbsoluteRPS, p.Placeholder
		if p.Placeholder {
			if p.AbsoluteRPS[0] != 0 || p.AbsoluteRPS[1] != 0 {
				t.Errorf("%s: placeholder rates %v must be 0", c.name, p.AbsoluteRPS)
			}
		} else if !(p.AbsoluteRPS[0] > 0 && p.AbsoluteRPS[1] > p.AbsoluteRPS[0]) {
			t.Errorf("%s: frozen rates %v must be positive and increasing", c.name, p.AbsoluteRPS)
		}
	}
	if ph["ws-large-echo"] != ph["get-json-h2"] {
		t.Errorf("one row is a placeholder and the other is not: %v", ph)
	}
	// no other column of the registry carries a plan (their runner behaviour is unchanged)
	for name, a := range servers.Registry {
		if !strings.HasPrefix(name, p3aPrefix+"-") && a.RatedPlans != nil {
			t.Errorf("%s (not a p3a column) carries a rated plan", name)
		}
	}
}

// The dispatch cell globs (PREREG-P3A statement 9, with the engine in the glob) select exactly the 40 columns of their dispatch, ONE scenario each, and no other experiment's glob
// selects a p3a column.
func TestP3aGlobsSelectOneCellPerColumn(t *testing.T) {
	cols := p3aCols(t)
	for d := 1; d <= 2; d++ {
		glob := fmt.Sprintf("ws-large-echo/p3a-%[1]d-*-k?-epoll-h1-sync,ws-large-echo/p3a-%[1]d-*-ctl-std-h1,get-json-h2/p3a-%[1]d-*-k?-epoll-auto+upg-async,get-json-h2/p3a-%[1]d-*-ctl-hertz-h2", d)
		got := p2Matched(t, glob)
		want := 0
		for _, c := range cols {
			if c.d != d {
				if _, leaked := got[c.name]; leaked {
					t.Errorf("the D=%d glob selects the D=%d column %s", d, c.d, c.name)
				}
				continue
			}
			want++
			s := got[c.name]
			if len(s) != 1 || s[0] != p3aRow[c.engine].scenario {
				t.Errorf("%s matches scenarios %v, want exactly [%s]", c.name, s, p3aRow[c.engine].scenario)
			}
		}
		if len(got) != want || want != 40 {
			t.Errorf("the D=%d glob selects %d columns, want 40", d, len(got))
		}
	}
	for _, g := range []string{p2Fast, p2Nine, "ws-hub-broadcast-128/c2lad-*", "get-simple-512c/c1arm-*-io512", "get-simple-512c/c2x86-*-io512", "*/celeris-*", "driver-redis-pipeline/c1pp-dump-*"} {
		for c := range p2Matched(t, g) {
			if strings.HasPrefix(c, p3aPrefix+"-") {
				t.Errorf("the glob %q selects the p3a column %s", g, c)
			}
		}
	}
}
