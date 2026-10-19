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

// THROWAWAY (ab/ssenonread-amd64). Pins the layout of the SSENR run (PLAN-ssenonread-amd64.md): the exact (scenario, column) pairs the dispatch glob
// selects (one cell per column, 150 columns), three arms (ma = MAIN, mb = MAIN again as the A/A twin: the SAME module as ma; s77 = MAIN + celeris
// PR 1077), no SUTEnv, the block layout (the three arms of a (rep, cell) run back to back) with the position balance the plan registers, and that
// every column's registry pin equals the celeris require of its module.
const (
	ssenrGlob = "get-simple-256c/ssenr-*-ep256,get-simple-256c/ssenr-*-ad256,get-simple-256c/ssenr-*-io256," +
		"post-4k/ssenr-*-ep4k,post-4k/ssenr-*-ad4k,post-4k/ssenr-*-io4k," +
		"sse-fanout-1024/ssenr-*-epsse,sse-fanout-1024/ssenr-*-iosse," +
		"ws-hub-broadcast-1024/ssenr-*-epws,ws-hub-broadcast-1024/ssenr-*-iows"

	ssenrWant = 150 // 5 reps x 10 cells x 3 arms

	pinMain = "v1.5.12-0.20261018191028-13fa1761fe5f"
	pinS77  = "v1.5.12-0.20261019093559-4c09926c50b3"
)

var (
	ssenrArms  = []string{"ma", "mb", "s77"}
	ssenrMod   = map[string]string{"ma": "ssmain_celeris", "mb": "ssmain_celeris", "s77": "sss77_celeris"}
	ssenrPin   = map[string]string{"ma": pinMain, "mb": pinMain, "s77": pinS77}
	ssenrFW    = map[string]string{"ma": "celeris-ss-maina", "mb": "celeris-ss-mainb", "s77": "celeris-ss-s77"}
	ssenrCells = map[string][2]string{ // tag -> scenario, base column
		"ep256": {"get-simple-256c", "celeris-epoll-h1-async"}, "ad256": {"get-simple-256c", "celeris-adaptive-h1-async"}, "io256": {"get-simple-256c", "celeris-iouring-h1-async"},
		"ep4k": {"post-4k", "celeris-epoll-h1-async"}, "ad4k": {"post-4k", "celeris-adaptive-h1-async"}, "io4k": {"post-4k", "celeris-iouring-h1-async"},
		"epsse": {"sse-fanout-1024", "celeris-epoll-h1-async"}, "iosse": {"sse-fanout-1024", "celeris-iouring-h1-async"},
		"epws": {"ws-hub-broadcast-1024", "celeris-epoll-h1-async"}, "iows": {"ws-hub-broadcast-1024", "celeris-iouring-h1-async"},
	}
	ssenrCol = regexp.MustCompile(`^ssenr-(\d{3})-r(\d)-(ma|mb|s77)-([a-z0-9]+)$`)
)

type ssenrRow struct {
	seq, rep int
	arm, tag string
	name     string
}

func ssenrRows(t *testing.T) []ssenrRow {
	t.Helper()
	var rows []ssenrRow
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "ssenr-") {
			continue
		}
		m := ssenrCol.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %q does not parse", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		rows = append(rows, ssenrRow{seq, rep, m[3], m[4], name})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })
	return rows
}

func ssenrMatched(t *testing.T, globs string) map[string][]string { // column -> scenarios
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

func TestSsenrGlobSelectsOneCellPerColumn(t *testing.T) {
	got := ssenrMatched(t, ssenrGlob)
	if len(got) != ssenrWant {
		t.Fatalf("glob selects %d columns, want %d", len(got), ssenrWant)
	}
	rows := ssenrRows(t)
	if len(rows) != ssenrWant {
		t.Fatalf("registry holds %d ssenr columns, want %d", len(rows), ssenrWant)
	}
	for _, r := range rows {
		s := got[r.name]
		want := ssenrCells[r.tag][0]
		if len(s) != 1 || s[0] != want {
			t.Errorf("column %s matches scenarios %v, want exactly [%s] (one window per column)", r.name, s, want)
		}
	}
	for c := range got {
		if !strings.HasPrefix(c, "ssenr-") {
			t.Errorf("glob also selects the non-ssenr column %s", c)
		}
	}
}

func TestSsenrColumnsAreWhatTheyAreRegisteredAs(t *testing.T) {
	for _, r := range ssenrRows(t) {
		v := servers.Registry[r.name]
		base := servers.Registry[ssenrCells[r.tag][1]]
		if v.Framework == "celeris" || v.Framework != ssenrFW[r.arm] {
			t.Errorf("%s: Framework %q, want %q (never \"celeris\": the harness would stamp servers/celeris's pin)", r.name, v.Framework, ssenrFW[r.arm])
		}
		if v.FrameworkVersion != ssenrPin[r.arm] {
			t.Errorf("%s: FrameworkVersion %q, want %q", r.name, v.FrameworkVersion, ssenrPin[r.arm])
		}
		if gb, ok := v.Bin.(servers.GoBinary); !ok || gb.ModuleDir != "servers/"+ssenrMod[r.arm] {
			t.Errorf("%s: build spec %v, want GoBinary{ModuleDir: servers/%s}", r.name, v.Bin, ssenrMod[r.arm])
		}
		if len(v.SUTEnv) != 0 {
			t.Errorf("%s: SUTEnv %v, want none (the A/B differs only in the celeris code)", r.name, v.SUTEnv)
		}
		if v.Engine != base.Engine {
			t.Errorf("%s: engine %q, want the base column's %q", r.name, v.Engine, base.Engine)
		}
	}
	// the twin: ma and mb are the SAME module, s77 another
	if ssenrMod["ma"] != ssenrMod["mb"] || ssenrMod["ma"] == ssenrMod["s77"] {
		t.Fatal("test table: the A/A twin must share one module and S77 must have its own")
	}
}

func TestSsenrLayout(t *testing.T) {
	rows := ssenrRows(t)
	if len(rows) != ssenrWant {
		t.Fatalf("%d columns", len(rows))
	}
	for i, r := range rows {
		if r.seq != i+1 {
			t.Fatalf("seq %d at position %d: seq must be 1..%d without gaps", r.seq, i+1, ssenrWant)
		}
	}
	// blocks: (rep, cell) = three adjacent seqs holding the three arms
	type key struct {
		rep int
		tag string
	}
	order := map[key][]string{}
	var blocks []key
	for i := 0; i < len(rows); i += 3 {
		k := key{rows[i].rep, rows[i].tag}
		for j := 0; j < 3; j++ {
			if (key{rows[i+j].rep, rows[i+j].tag}) != k {
				t.Fatalf("seq %d..%d is not one (rep, cell) block: %v", rows[i].seq, rows[i+2].seq, rows[i:i+3])
			}
			order[k] = append(order[k], rows[i+j].arm)
		}
		blocks = append(blocks, k)
		got := append([]string(nil), order[k]...)
		sort.Strings(got)
		if strings.Join(got, ",") != "ma,mb,s77" {
			t.Errorf("block %v holds arms %v, want each of ma, mb, s77 exactly once", k, order[k])
		}
	}
	if len(blocks) != 50 {
		t.Fatalf("%d blocks, want 50", len(blocks))
	}
	lead, last := map[string]int{}, map[string]int{}
	for tag := range ssenrCells {
		pos := map[string]map[int]int{}
		before := 0
		reps := 0
		for rep := 1; rep <= 5; rep++ {
			o, ok := order[key{rep, tag}]
			if !ok {
				t.Fatalf("no block for rep %d cell %s", rep, tag)
			}
			reps++
			for i, a := range o {
				if pos[a] == nil {
					pos[a] = map[int]int{}
				}
				pos[a][i]++
			}
			ia, ib := -1, -1
			for i, a := range o {
				if a == "ma" {
					ia = i
				}
				if a == "mb" {
					ib = i
				}
			}
			if ia < ib {
				before++
			}
			lead[o[0]]++
			last[o[2]]++
		}
		for _, a := range ssenrArms {
			for p := 0; p < 3; p++ {
				if n := pos[a][p]; n < 1 || n > 2 {
					t.Errorf("cell %s: arm %s holds position %d in %d of 5 reps, want 1 or 2", tag, a, p, n)
				}
			}
		}
		if before != 2 && before != 3 {
			t.Errorf("cell %s: ma precedes mb in %d of 5 reps, want 2 or 3 (the twin's order is balanced)", tag, before)
		}
		if reps != 5 {
			t.Errorf("cell %s has %d reps", tag, reps)
		}
	}
	for _, a := range ssenrArms {
		if lead[a] < 14 || lead[a] > 19 || last[a] < 14 || last[a] > 19 {
			t.Errorf("arm %s leads %d and is last %d of 50 blocks, want 14..19 each", a, lead[a], last[a])
		}
	}
	for i := 0; i+1 < len(blocks); i++ {
		if blocks[i].rep != blocks[i+1].rep && blocks[i].tag == blocks[i+1].tag {
			t.Errorf("the same cell %s ends rep %d and starts rep %d", blocks[i].tag, blocks[i].rep, blocks[i+1].rep)
		}
	}
}

func readFile(t *testing.T, p string) []byte {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

var celerisRequire = regexp.MustCompile(`(?m)^\tgithub\.com/goceleris/celeris (\S+)$`)

func TestSsenrModules(t *testing.T) {
	for _, m := range []string{"ssmain_celeris", "sss77_celeris"} {
		if strings.HasPrefix(m, "celeris") {
			t.Errorf("module dir %s starts with celeris (the port guard matches the binary by substring)", m)
		}
	}
	pins := map[string]string{"ssmain_celeris": pinMain, "sss77_celeris": pinS77}
	for m, pin := range pins {
		gm := readFile(t, filepath.Join(m, "go.mod"))
		if !bytes.Contains(gm, []byte("module github.com/goceleris/probatorium/servers/"+m+"\n")) {
			t.Errorf("%s/go.mod: wrong module line", m)
		}
		sm := celerisRequire.FindSubmatch(gm)
		if sm == nil || string(sm[1]) != pin {
			t.Errorf("%s/go.mod: celeris require %q, want %s", m, sm, pin)
		}
		if strings.Contains(string(gm), "\nreplace github.com/goceleris/celeris") {
			t.Errorf("%s/go.mod: a replace of celeris would make the registry pin meaningless", m)
		}
		if !bytes.Contains(readFile(t, filepath.Join(m, "go.sum")), []byte("github.com/goceleris/celeris "+pin+" h1:")) {
			t.Errorf("%s/go.sum: no h1 line for %s", m, pin)
		}
	}
	// ssmain, sss77 and servers/celeris carry byte-identical sources; go.mod differs in the module line and the celeris require only
	norm := func(m string) string {
		s := string(readFile(t, filepath.Join(m, "go.mod")))
		s = strings.ReplaceAll(s, "servers/"+m, "servers/X")
		return celerisRequire.ReplaceAllString(s, "\tgithub.com/goceleris/celeris PIN")
	}
	if norm("ssmain_celeris") != norm("sss77_celeris") {
		t.Errorf("ssmain_celeris/go.mod and sss77_celeris/go.mod differ beyond the module line and the celeris pin")
	}
	var srcs []string
	ents, _ := os.ReadDir("ssmain_celeris")
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".go") {
			srcs = append(srcs, e.Name())
		}
	}
	if len(srcs) < 5 {
		t.Fatalf("ssmain_celeris has %d .go files: %v", len(srcs), srcs)
	}
	for _, f := range srcs {
		a := readFile(t, filepath.Join("ssmain_celeris", f))
		for _, other := range []string{"sss77_celeris", "celeris"} {
			if !bytes.Equal(a, readFile(t, filepath.Join(other, f))) {
				t.Errorf("%s differs between ssmain_celeris and %s", f, other)
			}
		}
	}
	ents, _ = os.ReadDir("sss77_celeris")
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join("ssmain_celeris", e.Name())); err != nil {
			t.Errorf("sss77_celeris has %s that ssmain_celeris lacks", e.Name())
		}
	}
}
