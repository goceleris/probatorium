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
// their rated plans (the guard columns g0/g2/g4 have NONE), and the tie of the two env names to the knob sources of servers/celeris. The expectations come from the files that exist (go.mod, the knob sources), not
// from the generator's spec, so the test also fails when the generator is run with a stale spec or the registry is edited by hand. It does not know the absolute rates: they are
// a data file of the generator; the only thing it asserts about them is the Placeholder contract (all zero, or all positive and equal on every arm of a row).

const p3aPrefix = "p3a"

var p3aNameRE = regexp.MustCompile(`^p3a-([12])-(\d{3})-r(\d+)-(k0|k2|k4|ctl|g0|g2|g4)-(epoll-h1-sync|epoll-auto\+upg-async|std-h1|hertz-h2)$`)

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

// p3aIsGuard: the guard columns (Opus r45 R6, G2): g0 / g2 / g4 on get-json / epoll-h1-sync, saturation only.
func p3aIsGuard(c p3aCol) bool { return c.arm == "g0" || c.arm == "g2" || c.arm == "g4" }

// p3aGuardReps are the reps whose block is followed by a guard sub-block of three columns.
var p3aGuardReps = map[int]bool{1: true, 3: true, 5: true, 6: true, 8: true, 10: true}

var p3aRow = map[string]struct{ scenario, celerisEngine string }{
	"epoll-h1-sync":        {"ws-large-echo", "epoll-h1-sync"},
	"std-h1":               {"ws-large-echo", "std-h1"},
	"epoll-auto+upg-async": {"get-json-h2", "epoll-auto+upg-async"},
	"hertz-h2":             {"get-json-h2", ""},
}

func TestP3aLayout(t *testing.T) {
	cols := p3aCols(t)
	if len(cols) != 98 {
		t.Fatalf("%d p3a columns, want 98 (80 rated: 10 reps x 8, plus 18 guard: 6 sub-blocks x 3)", len(cols))
	}
	perD := map[int][]p3aCol{}
	for _, c := range cols {
		perD[c.d] = append(perD[c.d], c)
	}
	for d, want := range map[int][2]int{1: {1, 5}, 2: {6, 10}} {
		cs := perD[d]
		if len(cs) != 49 {
			t.Errorf("dispatch %d has %d columns, want 49 (40 rated + 9 guard)", d, len(cs))
		}
		for i, c := range cs { // sorted name order == execution order, seq 1..49
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
		guardReps := 0
		for rep, b := range byRep {
			wantN := 8
			if p3aGuardReps[rep] {
				wantN, guardReps = 11, guardReps+1
			}
			if len(b) != wantN {
				t.Errorf("dispatch %d rep %d has %d columns, want %d", d, rep, len(b), wantN)
			}
			if b[len(b)-1].seq-b[0].seq != wantN-1 {
				t.Errorf("dispatch %d rep %d is not a contiguous block of %d", d, rep, wantN)
			}
			n := map[string]int{}
			for i, c := range b {
				n[c.arm+"/"+c.engine]++
				if p3aIsGuard(c) != (i >= 8) { // the guard sub-block FOLLOWS the 8 rated columns
					t.Errorf("%s (position %d of rep %d): a guard column must be one of the last three, and only those", c.name, i+1, rep)
				}
			}
			for _, k := range []string{"k0/epoll-h1-sync", "k2/epoll-h1-sync", "k4/epoll-h1-sync", "ctl/std-h1", "k0/epoll-auto+upg-async", "k2/epoll-auto+upg-async", "k4/epoll-auto+upg-async", "ctl/hertz-h2"} {
				if n[k] != 1 {
					t.Errorf("dispatch %d rep %d holds %q %d times, want once", d, rep, k, n[k])
				}
			}
			for _, k := range []string{"g0/epoll-h1-sync", "g2/epoll-h1-sync", "g4/epoll-h1-sync"} {
				if want := map[bool]int{true: 1, false: 0}[p3aGuardReps[rep]]; n[k] != want {
					t.Errorf("dispatch %d rep %d holds %q %d times, want %d", d, rep, k, n[k], want)
				}
			}
		}
		if guardReps != 3 {
			t.Errorf("dispatch %d has %d guard reps, want 3", d, guardReps)
		}
	}
	per := map[string]int{}
	firstG := map[string]int{}
	for _, c := range cols {
		per[c.arm+"/"+c.engine]++
	}
	// each guard arm leads exactly two of the six sub-blocks (and one per dispatch)
	for _, d := range []int{1, 2} {
		seen := map[int]bool{}
		for _, c := range perD[d] {
			if p3aIsGuard(c) && !seen[c.rep] {
				seen[c.rep] = true
				firstG[c.arm]++
			}
		}
	}
	if len(per) != 11 {
		t.Errorf("%d (arm, engine) pairs, want 11 (8 rated + 3 guard): %v", len(per), per)
	}
	for k, n := range per {
		want := 10
		if strings.HasPrefix(k, "g") {
			want = 6
		}
		if n != want {
			t.Errorf("%s has %d columns, want %d", k, n, want)
		}
	}
	for _, a := range []string{"g0", "g2", "g4"} {
		if firstG[a] != 2 {
			t.Errorf("guard arm %s leads %d sub-blocks, want exactly 2 (seeded rotation)", a, firstG[a])
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
			if c.arm == "k2" || c.arm == "g2" {
				want[workers] = "14"
			}
			if c.arm == "k4" || c.arm == "g4" {
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
		if _, has := a.SUTEnv[workers]; (c.arm == "k0" || c.arm == "g0") && has {
			t.Errorf("%s: k0 / g0 must leave %s unset (the genuine default), has %q", c.name, workers, a.SUTEnv[workers])
		}
	}
	// WORKERS: k2 + k4 on two rows x 10 reps = 40, plus g2 + g4 x 6 sub-blocks = 12; SAMPLE: every celeris column (3 arms + std ctl on ws, 3 arms on h2) x 10 = 70, plus 3 guard arms x 6 = 18
	if seenW != 52 || seenS != 88 {
		t.Errorf("%s on %d columns (want 52), %s on %d (want 88)", workers, seenW, sched, seenS)
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
		if p3aIsGuard(c) { // saturation only: the EXPLICIT empty plan entry for get-json (zero rated passes), nothing else
			p, ok := a.RatedPlans["get-json"]
			if len(a.RatedPlans) != 1 || !ok || !p.SaturationOnly || len(p.Fractions) != 0 || len(p.AbsoluteRPS) != 0 || p.Placeholder {
				t.Errorf("%s: a guard column must carry exactly the explicit saturation-only plan {get-json: SaturationOnly, no pass}, has %v", c.name, a.RatedPlans)
			}
			continue
		}
		if p, ok := a.RatedPlans[row.scenario]; ok && p.SaturationOnly {
			t.Errorf("%s: only the guard columns may be saturation-only", c.name)
		}
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

// The dispatch cell globs (PREREG-P3A statement 9, with the engine in the glob): `cells` = the four rated globs + the guard glob selects exactly the 49 columns of the dispatch, ONE
// scenario each (the guard columns get-json, the rest their row's); `rated_cells` = the first four globs selects the 40 rated columns and NO guard column (the guard glob lands in
// `cells` only: the guard is saturation only); no other experiment's glob selects a p3a column.
func TestP3aGlobsSelectOneCellPerColumn(t *testing.T) {
	cols := p3aCols(t)
	for d := 1; d <= 2; d++ {
		rated := fmt.Sprintf("ws-large-echo/p3a-%[1]d-*-k?-epoll-h1-sync,ws-large-echo/p3a-%[1]d-*-ctl-std-h1,get-json-h2/p3a-%[1]d-*-k?-epoll-auto+upg-async,get-json-h2/p3a-%[1]d-*-ctl-hertz-h2", d)
		guard := fmt.Sprintf("get-json/p3a-%d-*-g?-epoll-h1-sync", d)
		all, ratedOnly, guardOnly := p2Matched(t, rated+","+guard), p2Matched(t, rated), p2Matched(t, guard)
		wantAll, wantRated, wantGuard := 0, 0, 0
		for _, c := range cols {
			if c.d != d {
				for _, m := range []map[string][]string{all, ratedOnly, guardOnly} {
					if _, leaked := m[c.name]; leaked {
						t.Errorf("the D=%d globs select the D=%d column %s", d, c.d, c.name)
					}
				}
				continue
			}
			wantAll++
			wantScenario := p3aRow[c.engine].scenario
			if p3aIsGuard(c) {
				wantGuard++
				wantScenario = "get-json"
				if _, in := ratedOnly[c.name]; in {
					t.Errorf("%s: the guard column is selected by the four rated globs (rated_cells must not carry the guard glob)", c.name)
				}
				if s := guardOnly[c.name]; len(s) != 1 || s[0] != "get-json" {
					t.Errorf("%s matches the guard glob on scenarios %v, want exactly [get-json]", c.name, s)
				}
			} else {
				wantRated++
				if _, in := guardOnly[c.name]; in {
					t.Errorf("%s: a rated column is selected by the guard glob", c.name)
				}
				if s := ratedOnly[c.name]; len(s) != 1 || s[0] != wantScenario {
					t.Errorf("%s matches the rated globs on scenarios %v, want exactly [%s]", c.name, s, wantScenario)
				}
			}
			if s := all[c.name]; len(s) != 1 || s[0] != wantScenario {
				t.Errorf("%s matches scenarios %v under `cells`, want exactly [%s] (one cell per column)", c.name, s, wantScenario)
			}
		}
		if len(all) != wantAll || wantAll != 49 || len(ratedOnly) != wantRated || wantRated != 40 || len(guardOnly) != wantGuard || wantGuard != 9 {
			t.Errorf("the D=%d globs select %d / rated %d / guard %d columns, want 49 / 40 / 9", d, len(all), len(ratedOnly), len(guardOnly))
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
