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

// THROWAWAY (ab/t1105-timing-<arch>, not for merge). Pins the layout of the T1105 run (PREREG-T1105.md): the RULE 74 timing read of celeris PR #1105. 105 columns (one unscored
// burn-in, then 8 reps x 13), four engines (epa, ada, eps, ioa), four arms (main = servers/celeris, mainb = its byte-identical A/A twin, pr = the PR head, v1511 = the long-run
// baseline, epa only), the exact (scenario, column) pairs the one dispatch glob selects, the seeded engine-major rotation, and that the pr and mainb modules are main plus the pin
// and nothing else. The test does not depend on the arch: the two branches differ in the generated schedule file only.
const (
	t1105Glob = "driver-mc-get/t1105-*-epa-*,driver-session-rw/t1105-*-epa-*,get-simple-1024c/t1105-*-epa-*,driver-mc-get/t1105-*-ada-*,get-simple-1024c/t1105-*-eps-*,get-simple-1024c/t1105-*-ioa-*"
	// the head of celeris PR #1105 the pr module is pinned to, and the pseudo-version `go list -m` gives for it (committer time 2026-10-24T23:36:46Z)
	t1105PRHead = "3b2ac0b24c2d3292a45a69803bbf6c186ae59bcd"
	t1105PRPin  = "v1.5.12-0.20261024233646-3b2ac0b24c2d"
	t1105MainPn = "v1.5.12-0.20261021042539-713b40dfdd0d"
	t1105Cols   = 105 // 1 burn-in + 8 reps x (4 epa + 3 ada + 3 eps + 3 ioa)
)

var (
	t1105Engines = []string{"epa", "ada", "eps", "ioa"}
	t1105Base    = map[string]string{"epa": "celeris-epoll-h1-async", "ada": "celeris-adaptive-h1-async", "eps": "celeris-epoll-h1-sync", "ioa": "celeris-iouring-h1-async"}
	t1105Mod     = map[string]string{"main": "celeris", "mainb": "celeris_mainb", "pr": "celeris_pr1105", "v1511": "celeris_v1511"}
	t1105Pin     = map[string]string{"main": t1105MainPn, "mainb": t1105MainPn, "pr": t1105PRPin, "v1511": "v1.5.11"}
	// the cells each engine column must run: the rows of PREREG-T1105.md section 2
	t1105Rows = map[string][]string{
		"epa": {"driver-mc-get", "driver-session-rw", "get-simple-1024c"},
		"ada": {"driver-mc-get"},
		"eps": {"get-simple-1024c"},
		"ioa": {"get-simple-1024c"},
	}
)

type t1105Col struct {
	name, eng, arm string
	seq, rep       int
}

func t1105Columns(t *testing.T) []t1105Col {
	t.Helper()
	re := regexp.MustCompile(`^t1105-(\d{3})-r(\d)-(epa|ada|eps|ioa)-(main|mainb|pr|v1511)$`)
	var cols []t1105Col
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "t1105-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match t1105-<seq>-r<rep>-<epa|ada|eps|ioa>-<main|mainb|pr|v1511>", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		cols = append(cols, t1105Col{name, m[3], m[4], seq, rep})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	return cols
}

func TestT1105GlobSelectsExactlyTheRowsOfEachEngine(t *testing.T) {
	got := p2Matched(t, t1105Glob)
	cols := t1105Columns(t)
	if len(got) != t1105Cols || len(cols) != t1105Cols {
		t.Fatalf("the T1105 glob selects %d columns and the registry holds %d, want %d", len(got), len(cols), t1105Cols)
	}
	for _, c := range cols {
		sc := slices.Clone(got[c.name])
		slices.Sort(sc)
		want := slices.Clone(t1105Rows[c.eng])
		slices.Sort(want)
		if !slices.Equal(sc, want) {
			t.Errorf("column %s matches scenarios %v, want %v", c.name, sc, want)
		}
	}
	for name := range got {
		if !strings.HasPrefix(name, "t1105-") {
			t.Errorf("the T1105 glob selects the foreign column %s", name)
		}
	}
	// the other experiments' globs select no T1105 column, so they never mix on one dispatch
	for _, g := range []string{p2Fast, p2Nine, p2bGlob} {
		for c := range p2Matched(t, g) {
			if strings.HasPrefix(c, "t1105-") {
				t.Errorf("the glob %q selects the T1105 column %s", g, c)
			}
		}
	}
	// benchmark-tier.yml keys its 90 s / 20 s window preset on the substring '-io512' of the cells input; T1105 runs the fast profile's own 35 s / 10 s window
	if strings.Contains(t1105Glob, "-io512") || strings.Contains(t1105Glob, "io512") {
		t.Error("the T1105 glob must not contain 'io512': that substring switches the window preset to 90 s / 20 s")
	}
}

func TestT1105Layout(t *testing.T) {
	cols := t1105Columns(t)
	if len(cols) != t1105Cols {
		t.Fatalf("%d T1105 columns, want %d", len(cols), t1105Cols)
	}
	if cols[0].name != "t1105-000-r0-epa-main" {
		t.Errorf("the first column (the unscored burn-in) is %s, want t1105-000-r0-epa-main", cols[0].name)
	}
	byRep := map[int][]t1105Col{}
	for i, c := range cols { // sorted name order is the execution order
		if c.seq != i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, i)
		}
		byRep[c.rep] = append(byRep[c.rep], c)
	}
	if len(byRep[0]) != 1 {
		t.Errorf("rep 0 (burn-in) has %d columns, want 1", len(byRep[0]))
	}
	order := map[int][]string{}     // rep -> engine order
	arms := map[string][][]string{} // engine -> per rep (1..8) arm order
	for e := range t1105Base {
		arms[e] = make([][]string, 8)
	}
	for rep := 1; rep <= 8; rep++ {
		rc := byRep[rep]
		if len(rc) != 13 {
			t.Fatalf("rep %d has %d columns, want 13", rep, len(rc))
		}
		lastEng := ""
		for _, c := range rc {
			if c.eng != lastEng {
				if slices.Contains(order[rep], c.eng) {
					t.Errorf("rep %d: engine %s is split into two blocks (the arms of an engine must run back to back)", rep, c.eng)
				}
				order[rep] = append(order[rep], c.eng)
				lastEng = c.eng
			}
			arms[c.eng][rep-1] = append(arms[c.eng][rep-1], c.arm)
		}
		so := slices.Clone(order[rep])
		slices.Sort(so)
		if want := slices.Clone(t1105Engines); !slices.Equal(so, func() []string { slices.Sort(want); return want }()) {
			t.Errorf("rep %d runs the engines %v", rep, order[rep])
		}
	}
	for pos := 0; pos < 4; pos++ { // every engine holds every position exactly twice
		for _, e := range t1105Engines {
			n := 0
			for rep := 1; rep <= 8; rep++ {
				if order[rep][pos] == e {
					n++
				}
			}
			if n != 2 {
				t.Errorf("engine %s holds engine-position %d in %d of the 8 reps, want 2", e, pos+1, n)
			}
		}
	}
	for e, per := range arms {
		want := []string{"main", "mainb", "pr"}
		if e == "epa" {
			want = []string{"main", "mainb", "pr", "v1511"}
		}
		sw := slices.Clone(want)
		slices.Sort(sw)
		for rep, s := range per {
			c := slices.Clone(s)
			slices.Sort(c)
			if !slices.Equal(c, sw) {
				t.Errorf("engine %s rep %d holds arms %v, want one column per arm %v", e, rep+1, s, want)
			}
		}
		for pos := range want { // position balance
			for _, a := range want {
				n := 0
				for _, s := range per {
					if s[pos] == a {
						n++
					}
				}
				if e == "epa" && n != 2 {
					t.Errorf("epa: arm %s holds position %d in %d of 8 reps, want exactly 2", a, pos+1, n)
				}
				if e != "epa" && n > 3 {
					t.Errorf("%s: arm %s holds position %d in %d of 8 reps, want at most 3", e, a, pos+1, n)
				}
			}
		}
		for i := 0; i < 7; i++ { // nobody leads two consecutive reps
			if per[i][0] == per[i+1][0] {
				t.Errorf("%s: arm %s leads reps %d and %d", e, per[i][0], i+1, i+2)
			}
		}
	}
	// v1511 is the long-run baseline column: epa only, 8 reps, plus nothing in the burn-in
	n1511 := 0
	for _, c := range cols {
		if c.arm == "v1511" {
			n1511++
			if c.eng != "epa" || c.rep < 1 {
				t.Errorf("%s: v1511 columns exist for epa reps 1-8 only", c.name)
			}
		}
	}
	if n1511 != 8 {
		t.Errorf("%d v1511 columns, want 8", n1511)
	}
	for _, c := range cols {
		a := servers.Registry[c.name]
		gb, ok := a.Bin.(servers.GoBinary)
		if !ok {
			t.Errorf("%s: not a Go binary", c.name)
			continue
		}
		dir := filepath.Join("..", gb.ModuleDir)
		if filepath.Base(dir) != t1105Mod[c.arm] || a.FrameworkVersion != t1105Pin[c.arm] || a.Framework != "celeris-t1105-"+c.arm {
			t.Errorf("%s: module %s framework %s version %s, want %s celeris-t1105-%s %s", c.name, dir, a.Framework, a.FrameworkVersion, t1105Mod[c.arm], c.arm, t1105Pin[c.arm])
		}
		if len(a.SUTEnv) != 0 {
			t.Errorf("%s: SUTEnv %v, want none (nothing is read by the server and no vector is moved)", c.name, a.SUTEnv)
		}
		gomod, err := os.ReadFile(filepath.Join(dir, "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gomod), "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", c.name, a.FrameworkVersion, dir)
		}
		tw := servers.Registry[t1105Base[c.eng]]
		if a.Engine != tw.Engine || a.Language != tw.Language || a.Capabilities != tw.Capabilities || a.Category != tw.Category {
			t.Errorf("%s: engine/language/capabilities differ from %s", c.name, t1105Base[c.eng])
		}
	}
}

// The pin of the pr module is the PR head: the pseudo-version's 12-character hash is a prefix of the head sha, its time is the head's committer time, and go.sum holds it.
func TestT1105PinIsThePRHead(t *testing.T) {
	m := regexp.MustCompile(`^v1\.5\.12-0\.(\d{14})-([0-9a-f]{12})$`).FindStringSubmatch(t1105PRPin)
	if m == nil || !strings.HasPrefix(t1105PRHead, m[2]) || m[1] != "20261024233646" {
		t.Fatalf("pin %s is not the pseudo-version of %s", t1105PRPin, t1105PRHead)
	}
	sum, err := os.ReadFile("celeris_pr1105/go.sum")
	if err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{" h1:", "/go.mod h1:"} {
		if !strings.Contains(string(sum), "github.com/goceleris/celeris "+t1105PRPin+suffix) {
			t.Errorf("celeris_pr1105/go.sum lacks the %s line of %s", strings.TrimSpace(suffix), t1105PRPin)
		}
	}
	if strings.Contains(string(sum), t1105MainPn) {
		t.Error("celeris_pr1105/go.sum still lists the main pin")
	}
}

// The twin and the PR module are servers/celeris plus the pin and nothing else: the same files byte for byte, a go.mod that differs in the module line (and, for the PR module,
// the celeris require), a go.sum identical (twin) or differing in the two celeris lines (PR). Anything else would make the column a different server, not a different celeris.
func TestT1105ModulesAreMainPlusThePin(t *testing.T) {
	read := func(p string) string {
		b, err := os.ReadFile(p)
		if err != nil {
			t.Fatal(err)
		}
		return string(b)
	}
	files := func(dir string) []string {
		ents, _ := filepath.Glob(dir + "/*")
		var out []string
		for _, f := range ents {
			if fi, _ := os.Stat(f); fi.IsDir() || filepath.Base(f) == filepath.Base(dir) { // a built binary is not part of the module
				continue
			}
			out = append(out, filepath.Base(f))
		}
		slices.Sort(out)
		return out
	}
	mainFiles := files("celeris")
	for _, mod := range []string{"celeris_mainb", "celeris_pr1105"} {
		if got := files(mod); !slices.Equal(mainFiles, got) {
			t.Fatalf("celeris has %v, %s has %v", mainFiles, mod, got)
		}
		for _, name := range mainFiles {
			m, v := read("celeris/"+name), read(mod+"/"+name)
			switch {
			case name == "go.mod":
				want := strings.Replace(m, "module github.com/goceleris/probatorium/servers/celeris\n", "module github.com/goceleris/probatorium/servers/"+mod+"\n", 1)
				if mod == "celeris_pr1105" {
					want = strings.Replace(want, "github.com/goceleris/celeris "+t1105MainPn+"\n", "github.com/goceleris/celeris "+t1105PRPin+"\n", 1)
				}
				if v != want || v == m {
					t.Errorf("%s/go.mod is not celeris/go.mod with the module line%s renamed", mod, map[bool]string{true: " and the celeris pin", false: ""}[mod == "celeris_pr1105"])
				}
			case name == "go.sum" && mod == "celeris_pr1105":
				strip := func(s string) string {
					var out []string
					for _, l := range strings.Split(s, "\n") {
						if !strings.HasPrefix(l, "github.com/goceleris/celeris ") {
							out = append(out, l)
						}
					}
					return strings.Join(out, "\n")
				}
				if strip(m) != strip(v) {
					t.Errorf("celeris_pr1105/go.sum differs from celeris/go.sum beyond the two celeris lines")
				}
			default:
				if m != v {
					t.Errorf("%s/%s differs from celeris/%s", mod, name, name)
				}
			}
		}
	}
}
