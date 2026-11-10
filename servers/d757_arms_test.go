package servers_test

import (
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/d757-<date>, not for merge). Pins what d757-gen.py generates: the 26 registry columns d757-<seq>-r<rep>-<arm>-<engine> of servers/d757_arms.go (DESIGN-757 r2 section 3):
// a0 x10 (gate off), dx x10 (gate on), the hertz-h2 control x6 in reps 1, 3, 5, 6, 8, 10; every column carries the ABSOLUTE-ONLY plan 300321 / 450481 for get-json-h2; the two celeris arms are
// the same binary and differ only by the gate env. The expectations come from the files that exist (go.mod, the knob source when the sibling builder's resid_knob.go is present).

const d757Prefix = "d757"

var d757NameRE = regexp.MustCompile(`^d757-(\d{3})-r(\d+)-(a0|dx|ctl)-(epoll-auto\+upg-async|hertz-h2)$`)

type d757Col struct {
	name, arm, engine string
	seq, rep          int
}

func d757Cols(t *testing.T) []d757Col {
	t.Helper()
	var cols []d757Col
	for name := range servers.Registry {
		if !strings.HasPrefix(name, d757Prefix+"-") {
			continue
		}
		m := d757NameRE.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match d757-<seq>-r<rep>-<arm>-<engine>", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		cols = append(cols, d757Col{name, m[3], m[4], seq, rep})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	if len(cols) == 0 {
		t.Fatal("no d757 column in the registry")
	}
	return cols
}

var d757CtlReps = map[int]bool{1: true, 3: true, 5: true, 6: true, 8: true, 10: true}

func TestD757Layout(t *testing.T) {
	cols := d757Cols(t)
	if len(cols) != 26 {
		t.Fatalf("%d d757 columns, want 26 (a0 x10, dx x10, ctl x6)", len(cols))
	}
	byRep := map[int][]d757Col{}
	per := map[string]int{}
	for i, c := range cols { // sorted name order == execution order, seq 1..26
		if c.seq != i+1 {
			t.Errorf("sorted position %d has seq %d", i+1, c.seq)
		}
		byRep[c.rep] = append(byRep[c.rep], c)
		per[c.arm]++
	}
	if per["a0"] != 10 || per["dx"] != 10 || per["ctl"] != 6 {
		t.Errorf("arms %v, want a0 10, dx 10, ctl 6", per)
	}
	for rep := 1; rep <= 10; rep++ {
		b := byRep[rep]
		want := 2
		if d757CtlReps[rep] {
			want = 3
		}
		if len(b) != want {
			t.Errorf("rep %d has %d columns, want %d", rep, len(b), want)
			continue
		}
		if b[len(b)-1].seq-b[0].seq != want-1 {
			t.Errorf("rep %d is not a contiguous block", rep)
		}
		n := map[string]int{}
		for _, c := range b {
			n[c.arm]++
		}
		if n["a0"] != 1 || n["dx"] != 1 || n["ctl"] != map[bool]int{true: 1, false: 0}[d757CtlReps[rep]] {
			t.Errorf("rep %d holds %v", rep, n)
		}
	}
	if len(byRep) != 10 {
		t.Errorf("%d reps, want 10", len(byRep))
	}
}

// The env names of the registry are the constants of the knob sources, character for character (the gate name is decided by the sibling celeris builder: until servers/celeris/resid_knob.go
// exists the gate tie is only logged, never skipped silently).
func TestD757EnvAndPlans(t *testing.T) {
	sched := regexp.MustCompile(`(?m)^const schedSampleEnv = "([A-Z0-9_]+)"`).FindStringSubmatch(readFile(t, "celeris", "sched_sampler.go"))
	if sched == nil {
		t.Fatal("servers/celeris/sched_sampler.go has no const schedSampleEnv")
	}
	const gate = "PROBATORIUM_RESID_STAMPS"
	if b, err := os.ReadFile(filepath.Join("celeris", "resid_knob.go")); err != nil {
		t.Logf("servers/celeris/resid_knob.go is not present (the sibling builder's file): the gate name %s is NOT tied to the knob source yet", gate)
	} else if !strings.Contains(string(b), `"`+gate+`"`) {
		t.Errorf("servers/celeris/resid_knob.go does not carry the gate name %q", gate)
	}
	pin := celerisRequire(t, "celeris")
	for _, c := range d757Cols(t) {
		a := servers.Registry[c.name]
		var wantEnv map[string]string
		switch c.arm {
		case "a0":
			wantEnv = map[string]string{sched[1]: "1"}
		case "dx":
			wantEnv = map[string]string{sched[1]: "1", gate: "1"}
		}
		if !reflect.DeepEqual(a.SUTEnv, wantEnv) {
			t.Errorf("%s: SUTEnv %v, want %v", c.name, a.SUTEnv, wantEnv)
		}
		if _, has := a.SUTEnv["PROBATORIUM_WORKERS"]; has {
			t.Errorf("%s: no d757 column sets PROBATORIUM_WORKERS (16 loops everywhere)", c.name)
		}
		if _, has := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; has {
			t.Errorf("%s: AB_IRQ_VECTOR_CPU trips the msr1-only IRQPLACE block on msa2-server", c.name)
		}
		// plan: ONE entry for get-json-h2, no fractions, the two absolute rates, no placeholder, not saturation-only
		p, ok := a.RatedPlans["get-json-h2"]
		if len(a.RatedPlans) != 1 || !ok || len(p.Fractions) != 0 || !slices.Equal(p.AbsoluteRPS, []float64{300321, 450481}) || p.Placeholder || p.SaturationOnly {
			t.Errorf("%s: rated plans %v, want exactly {get-json-h2: absolute 300321, 450481}", c.name, a.RatedPlans)
		}
		// binary identity
		base := "celeris-" + c.engine
		if c.engine == "hertz-h2" {
			base = "hertz-h2"
		}
		b, ok := servers.Registry[base]
		if !ok {
			t.Fatalf("%s: no base column %s", c.name, base)
		}
		if a.Name != c.name || a.Engine != b.Engine || a.Category != b.Category || a.Language != b.Language || a.Capabilities != b.Capabilities || a.Baseline {
			t.Errorf("%s differs from %s in more than Name/Framework/Version/Bin/SUTEnv/RatedPlans", c.name, base)
		}
		gb, isGo := a.Bin.(servers.GoBinary)
		if !isGo {
			t.Fatalf("%s: Bin is not a GoBinary", c.name)
		}
		if c.engine == "hertz-h2" {
			if a.Framework != "hertz" || a.FrameworkVersion != b.FrameworkVersion || gb.ModuleDir != "servers/hertz" {
				t.Errorf("%s: the hertz control must keep framework/version/module", c.name)
			}
			continue
		}
		if a.Framework != "celeris-d757-"+c.arm || a.Framework == "celeris" {
			t.Errorf("%s: framework %q", c.name, a.Framework)
		}
		if a.FrameworkVersion != pin || gb.ModuleDir != "servers/celeris" {
			t.Errorf("%s: version %s module %s, want %s servers/celeris (go.mod)", c.name, a.FrameworkVersion, gb.ModuleDir, pin)
		}
	}
	// no other column carries a plan except the p3a and d757 ones (their runner behaviour is unchanged)
	for name, a := range servers.Registry {
		if !strings.HasPrefix(name, d757Prefix+"-") && !strings.HasPrefix(name, "p3a-") && a.RatedPlans != nil {
			t.Errorf("%s (not a p3a / d757 column) carries a rated plan", name)
		}
	}
}

// The dispatch cell globs: `cells` = `rated_cells` = the three globs of d757-gen.py select exactly the 26 columns, ONE scenario each (get-json-h2); no other experiment's glob selects a d757 column.
func TestD757GlobsSelectOneCellPerColumn(t *testing.T) {
	globs := "get-json-h2/d757-*-a0-epoll-auto+upg-async,get-json-h2/d757-*-dx-epoll-auto+upg-async,get-json-h2/d757-*-ctl-hertz-h2"
	got := p2Matched(t, globs)
	cols := d757Cols(t)
	if len(got) != len(cols) {
		t.Errorf("the globs select %d columns, want %d", len(got), len(cols))
	}
	for _, c := range cols {
		if s := got[c.name]; len(s) != 1 || s[0] != "get-json-h2" {
			t.Errorf("%s matches scenarios %v, want exactly [get-json-h2]", c.name, s)
		}
	}
	for _, g := range []string{p2Fast, p2Nine, "ws-hub-broadcast-128/c2lad-*", "get-simple-512c/c1arm-*-io512", "*/celeris-*", "get-json-h2/p3a-1-*-k?-epoll-auto+upg-async", "get-json-h2/p3a-1-*-ctl-hertz-h2"} {
		for c := range p2Matched(t, g) {
			if strings.HasPrefix(c, d757Prefix+"-") {
				t.Errorf("the glob %q selects the d757 column %s", g, c)
			}
		}
	}
}
