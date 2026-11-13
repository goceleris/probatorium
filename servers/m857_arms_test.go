package servers_test

import (
	"os"
	"path"
	"path/filepath"
	"reflect"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/l857f-diag-20261113, not for merge). Pins what m857-gen.py generates: the 36 registry columns m857-<seq>-r<block>-<arm>-<engine> of servers/m857_arms.go (L857F, celeris#857):
// 4 blocks x 9 columns, A and B on three celeris engines, G on epoll-auto+upg-async, R and R16 on fasthttp-h1; the arms differ only by the column env of one of two binaries; the dispatch cell globs
// select exactly the 120 registered cells.

const m857Prefix = "m857-"

var m857NameRE = regexp.MustCompile(`^m857-(\d{3})-r(\d)-(a|b|g|r|r16)-(epoll-auto\+upg-async|iouring-h1-async|iouring-h1-sync|fasthttp-h1)$`)

// the dispatch cells= globs of the queue line (PREREG-L857F S14); rated_cells=nothing/*.
var m857Globs = []string{"driver-mc-get/m857-*", "driver-mc-multiget/m857-*", "driver-mc-set/m857-*",
	"driver-redis-get/m857-*-r-fasthttp-h1", "driver-redis-get/m857-*-a-epoll-auto+upg-async", "driver-redis-get/m857-*-b-epoll-auto+upg-async"}

type m857Col struct {
	name, arm, engine string
	seq, block        int
}

func m857Cols(t *testing.T) []m857Col {
	t.Helper()
	var cols []m857Col
	for name := range servers.Registry {
		if !strings.HasPrefix(name, m857Prefix) {
			continue
		}
		m := m857NameRE.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match m857-<seq>-r<block>-<arm>-<engine>", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		block, _ := strconv.Atoi(m[2])
		cols = append(cols, m857Col{name, m[3], m[4], seq, block})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	if len(cols) == 0 {
		t.Fatal("no m857 column in the registry")
	}
	return cols
}

func m857Read(t *testing.T, p ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(p...))
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func m857Require(t *testing.T, mod, module string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^\s*` + regexp.QuoteMeta(module) + ` (v\S+)`).FindStringSubmatch(m857Read(t, mod, "go.mod"))
	if m == nil {
		t.Fatalf("servers/%s/go.mod has no require of %s", mod, module)
	}
	return m[1]
}

// S1 = R, Ae, Bs, Ba, G, As, Be, R16, Aa; blocks = S1, reverse, rotate(S1, 4), reverse of that (PREREG-L857F S5).
var m857Blocks = func() [][]string {
	s1 := []string{"R", "Ae", "Bs", "Ba", "G", "As", "Be", "R16", "Aa"}
	rev := func(x []string) []string {
		o := make([]string, len(x))
		for i, v := range x {
			o[len(x)-1-i] = v
		}
		return o
	}
	b3 := append(append([]string{}, s1[4:]...), s1[:4]...)
	return [][]string{s1, rev(s1), b3, rev(b3)}
}()

func m857Label(c m857Col) string {
	switch c.arm {
	case "r":
		return "R"
	case "r16":
		return "R16"
	case "g":
		return "G"
	}
	return strings.ToUpper(c.arm) + map[string]string{"epoll-auto+upg-async": "e", "iouring-h1-async": "a", "iouring-h1-sync": "s"}[c.engine]
}

func TestM857Layout(t *testing.T) {
	cols := m857Cols(t)
	if len(cols) != 36 {
		t.Fatalf("%d m857 columns, want 36 (4 blocks x 9)", len(cols))
	}
	for i, c := range cols { // sorted name order == execution order, seq 1..36
		if c.seq != i+1 {
			t.Errorf("sorted position %d has seq %d", i+1, c.seq)
		}
		if c.block != i/9+1 {
			t.Errorf("%s: block %d, want %d (9 contiguous columns per block)", c.name, c.block, i/9+1)
		}
		if got, want := m857Label(c), m857Blocks[i/9][i%9]; got != want {
			t.Errorf("%s: label %s at block %d position %d, want %s (S5 order)", c.name, got, c.block, i%9+1, want)
		}
	}
	per := map[string]int{}
	for _, c := range cols {
		per[c.arm]++
	}
	if !reflect.DeepEqual(per, map[string]int{"a": 12, "b": 12, "g": 4, "r": 4, "r16": 4}) {
		t.Errorf("arms %v, want a 12, b 12, g 4, r 4, r16 4", per)
	}
	// every column type has mean position 5 over the four blocks
	pos := map[string]int{}
	for i, c := range cols {
		pos[m857Label(c)] += i%9 + 1
	}
	for l, s := range pos {
		if s != 20 {
			t.Errorf("column type %s: position sum %d over 4 blocks, want 20 (mean 5)", l, s)
		}
	}
}

func TestM857EnvAndIdentity(t *testing.T) {
	pin := m857Require(t, "celeris", "github.com/goceleris/celeris")
	wantEnv := map[string]map[string]string{"a": nil, "b": {"PROBATORIUM_MC_MAXOPEN": "128"}, "g": {"PROBATORIUM_MC_CLIENT": "gomemcache"}, "r": nil, "r16": {"PROBATORIUM_MC_CAP": "16"}}
	for _, c := range m857Cols(t) {
		a := servers.Registry[c.name]
		if !reflect.DeepEqual(a.SUTEnv, wantEnv[c.arm]) {
			t.Errorf("%s: SUTEnv %v, want %v", c.name, a.SUTEnv, wantEnv[c.arm])
		}
		base := "celeris-" + c.engine
		fh := c.arm == "r" || c.arm == "r16"
		if fh {
			base = "fasthttp-h1"
		}
		b, ok := servers.Registry[base]
		if !ok {
			t.Fatalf("%s: no base column %s", c.name, base)
		}
		if a.Name != c.name || a.Engine != b.Engine || a.Category != b.Category || a.Language != b.Language || a.Capabilities != b.Capabilities || a.Baseline {
			t.Errorf("%s differs from %s in more than Name/Framework/FrameworkVersion/Bin/SUTEnv", c.name, base)
		}
		gb, isGo := a.Bin.(servers.GoBinary)
		if !isGo {
			t.Fatalf("%s: Bin is not a GoBinary", c.name)
		}
		if fh {
			if a.Framework != "fasthttp-m857-"+c.arm || a.FrameworkVersion != b.FrameworkVersion || gb.ModuleDir != "servers/fasthttp" {
				t.Errorf("%s: framework %q version %q module %q, want fasthttp-m857-%s / %s / servers/fasthttp", c.name, a.Framework, a.FrameworkVersion, gb.ModuleDir, c.arm, b.FrameworkVersion)
			}
			continue
		}
		if a.Framework != "celeris-m857-"+c.arm || a.Framework == "celeris" {
			t.Errorf("%s: framework %q (never the bare celeris: the report builders stamp the main pin on it)", c.name, a.Framework)
		}
		if a.FrameworkVersion != pin || gb.ModuleDir != "servers/celeris" {
			t.Errorf("%s: version %s module %s, want %s servers/celeris (servers/celeris/go.mod)", c.name, a.FrameworkVersion, gb.ModuleDir, pin)
		}
	}
	if pin != "v1.5.12-0.20261111121706-4bc7372ea80b" {
		t.Errorf("servers/celeris pins %s, the L857F plan registers v1.5.12-0.20261111121706-4bc7372ea80b (4bc7372ea80b)", pin)
	}
	if g, f := m857Require(t, "celeris", "github.com/bradfitz/gomemcache"), m857Require(t, "fasthttp", "github.com/bradfitz/gomemcache"); g != f {
		t.Errorf("gomemcache: servers/celeris %s, servers/fasthttp %s (arm G must use the fasthttp module's version)", g, f)
	}
	// the env names of the registry are the constants of the adapters' knob sources, character for character, and each adapter prints the [m857] line
	for _, k := range []struct{ file, name string }{{"celeris/mc_knobs.go", "PROBATORIUM_MC_MAXOPEN"}, {"celeris/mc_knobs.go", "PROBATORIUM_MC_CLIENT"}, {"fasthttp/mc_cap.go", "PROBATORIUM_MC_CAP"}} {
		if !strings.Contains(m857Read(t, k.file), `"`+k.name+`"`) {
			t.Errorf("servers/%s does not define the env %s", k.file, k.name)
		}
	}
	if !strings.Contains(m857Read(t, "celeris/mc_knobs.go"), `"[m857] client=%s maxopen=%d cap=0"`) || !strings.Contains(m857Read(t, "fasthttp/mc_cap.go"), `"[m857] client=gomemcache maxopen=0 cap=%d"`) {
		t.Error("an adapter does not print the registered [m857] startup line")
	}
}

// the dispatch globs select exactly the 36 columns' 120 cells (path.Match semantics, as the runner's matchCellGlob): 3 mc cells per column, the redis-get control on Ae, Be and R only.
func TestM857GlobsSelectTheRegisteredCells(t *testing.T) {
	cols := m857Cols(t)
	want := map[string]bool{}
	for _, c := range cols {
		for _, s := range []string{"driver-mc-get", "driver-mc-multiget", "driver-mc-set"} {
			want[s+"/"+c.name] = true
		}
		if l := m857Label(c); l == "Ae" || l == "Be" || l == "R" {
			want["driver-redis-get/"+c.name] = true
		}
	}
	if len(want) != 120 {
		t.Fatalf("expected cells %d, want 120", len(want))
	}
	got := map[string]bool{}
	for _, sc := range scenarios.Registry() {
		for name := range servers.Registry {
			id := sc.Name() + "/" + name
			for _, g := range m857Globs {
				if ok, err := path.Match(g, id); err != nil {
					t.Fatalf("glob %q: %v", g, err)
				} else if ok {
					got[id] = true
					break
				}
			}
		}
	}
	if !reflect.DeepEqual(got, want) {
		for id := range got {
			if !want[id] {
				t.Errorf("a glob selects %s, which is not a registered m857 cell", id)
			}
		}
		for id := range want {
			if !got[id] {
				t.Errorf("registered cell %s is selected by no glob", id)
			}
		}
	}
}
