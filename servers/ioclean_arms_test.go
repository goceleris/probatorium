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

// THROWAWAY (ab/ioclean-amd64). Pins the layout of the IOCLEAN run (PLAN-ioclean-amd64.md): the exact (scenario, column) pairs the dispatch glob
// selects (one cell per column, 150 columns), three arms (ma = MAIN, mb = MAIN again as the A/A twin: the SAME module as ma; s82 = MAIN + celeris
// PR 1082), no SUTEnv, the block layout (the three arms of a (rep, cell) run back to back) with the position balance the plan registers, and that
// every column's registry pin equals the celeris require of its module.
const (
	iocleanGlob = "get-simple-256c/ioclean-*-ep256,get-simple-256c/ioclean-*-io256,get-simple-256c/ioclean-*-ad256," +
		"post-4k/ioclean-*-io4k,post-4k/ioclean-*-ad4k,get-json-h2/ioclean-*-ioh2,ws-large-echo/ioclean-*-iolg," +
		"sse-fanout-1024/ioclean-*-iosse,ws-hub-broadcast-1024/ioclean-*-iows,sse-fanout-1024/ioclean-*-adsse"

	iocleanWant = 150 // 5 reps x 10 cells x 3 arms

	pinMain = "v1.5.12-0.20261019222325-6a6b07f9137f"
	pinS82  = "v1.5.12-0.20261019223029-ea4b0235108c"
)

var (
	iocleanArms  = []string{"ma", "mb", "s82"}
	iocleanMod   = map[string]string{"ma": "iomain_celeris", "mb": "iomain_celeris", "s82": "ios82_celeris"}
	iocleanPin   = map[string]string{"ma": pinMain, "mb": pinMain, "s82": pinS82}
	iocleanFW    = map[string]string{"ma": "celeris-io-maina", "mb": "celeris-io-mainb", "s82": "celeris-io-s82"}
	iocleanCells = map[string][2]string{ // tag -> scenario, base column
		"ep256": {"get-simple-256c", "celeris-epoll-h1-async"}, "io256": {"get-simple-256c", "celeris-iouring-h1-async"}, "ad256": {"get-simple-256c", "celeris-adaptive-h1-async"},
		"io4k": {"post-4k", "celeris-iouring-h1-async"}, "ad4k": {"post-4k", "celeris-adaptive-h1-async"},
		"ioh2": {"get-json-h2", "celeris-iouring-auto+upg-async"}, "iolg": {"ws-large-echo", "celeris-iouring-h1-async"},
		"iosse": {"sse-fanout-1024", "celeris-iouring-h1-async"}, "iows": {"ws-hub-broadcast-1024", "celeris-iouring-h1-async"},
		"adsse": {"sse-fanout-1024", "celeris-adaptive-h1-async"},
	}
	iocleanCol = regexp.MustCompile(`^ioclean-(\d{3})-r(\d)-(ma|mb|s82)-([a-z0-9]+)$`)
)

type iocleanRow struct {
	seq, rep int
	arm, tag string
	name     string
}

func iocleanRows(t *testing.T) []iocleanRow {
	t.Helper()
	var rows []iocleanRow
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "ioclean-") {
			continue
		}
		m := iocleanCol.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %q does not parse", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		rows = append(rows, iocleanRow{seq, rep, m[3], m[4], name})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].seq < rows[j].seq })
	return rows
}

func iocleanMatched(t *testing.T, globs string) map[string][]string { // column -> scenarios
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

func TestIocleanGlobSelectsOneCellPerColumn(t *testing.T) {
	got := iocleanMatched(t, iocleanGlob)
	if len(got) != iocleanWant {
		t.Fatalf("glob selects %d columns, want %d", len(got), iocleanWant)
	}
	rows := iocleanRows(t)
	if len(rows) != iocleanWant {
		t.Fatalf("registry holds %d ioclean columns, want %d", len(rows), iocleanWant)
	}
	for _, r := range rows {
		s := got[r.name]
		want := iocleanCells[r.tag][0]
		if len(s) != 1 || s[0] != want {
			t.Errorf("column %s matches scenarios %v, want exactly [%s] (one window per column)", r.name, s, want)
		}
	}
	for c := range got {
		if !strings.HasPrefix(c, "ioclean-") {
			t.Errorf("glob also selects the non-ioclean column %s", c)
		}
	}
}

func TestIocleanColumnsAreWhatTheyAreRegisteredAs(t *testing.T) {
	for _, r := range iocleanRows(t) {
		v := servers.Registry[r.name]
		base := servers.Registry[iocleanCells[r.tag][1]]
		if v.Framework == "celeris" || v.Framework != iocleanFW[r.arm] {
			t.Errorf("%s: Framework %q, want %q (never \"celeris\": the harness would stamp servers/celeris's pin)", r.name, v.Framework, iocleanFW[r.arm])
		}
		if v.FrameworkVersion != iocleanPin[r.arm] {
			t.Errorf("%s: FrameworkVersion %q, want %q", r.name, v.FrameworkVersion, iocleanPin[r.arm])
		}
		if gb, ok := v.Bin.(servers.GoBinary); !ok || gb.ModuleDir != "servers/"+iocleanMod[r.arm] {
			t.Errorf("%s: build spec %v, want GoBinary{ModuleDir: servers/%s}", r.name, v.Bin, iocleanMod[r.arm])
		}
		if len(v.SUTEnv) != 0 {
			t.Errorf("%s: SUTEnv %v, want none (the A/B differs only in the celeris code)", r.name, v.SUTEnv)
		}
		if v.Engine != base.Engine {
			t.Errorf("%s: engine %q, want the base column's %q", r.name, v.Engine, base.Engine)
		}
	}
	// the twin: ma and mb are the SAME module, s82 another
	if iocleanMod["ma"] != iocleanMod["mb"] || iocleanMod["ma"] == iocleanMod["s82"] {
		t.Fatal("test table: the A/A twin must share one module and S82 must have its own")
	}
}

func TestIocleanLayout(t *testing.T) {
	rows := iocleanRows(t)
	if len(rows) != iocleanWant {
		t.Fatalf("%d columns", len(rows))
	}
	for i, r := range rows {
		if r.seq != i+1 {
			t.Fatalf("seq %d at position %d: seq must be 1..%d without gaps", r.seq, i+1, iocleanWant)
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
		if strings.Join(got, ",") != "ma,mb,s82" {
			t.Errorf("block %v holds arms %v, want each of ma, mb, s82 exactly once", k, order[k])
		}
	}
	if len(blocks) != 50 {
		t.Fatalf("%d blocks, want 50", len(blocks))
	}
	lead, last := map[string]int{}, map[string]int{}
	for tag := range iocleanCells {
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
		for _, a := range iocleanArms {
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
	for _, a := range iocleanArms {
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

func TestIocleanModules(t *testing.T) {
	for _, m := range []string{"iomain_celeris", "ios82_celeris"} {
		if strings.HasPrefix(m, "celeris") {
			t.Errorf("module dir %s starts with celeris (the port guard matches the binary by substring)", m)
		}
	}
	pins := map[string]string{"iomain_celeris": pinMain, "ios82_celeris": pinS82}
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
	// ssmain, sss82 and servers/celeris carry byte-identical sources; go.mod differs in the module line and the celeris require only
	norm := func(m string) string {
		s := string(readFile(t, filepath.Join(m, "go.mod")))
		s = strings.ReplaceAll(s, "servers/"+m, "servers/X")
		return celerisRequire.ReplaceAllString(s, "\tgithub.com/goceleris/celeris PIN")
	}
	if norm("iomain_celeris") != norm("ios82_celeris") {
		t.Errorf("iomain_celeris/go.mod and ios82_celeris/go.mod differ beyond the module line and the celeris pin")
	}
	var srcs []string
	ents, _ := os.ReadDir("iomain_celeris")
	for _, e := range ents {
		if strings.HasSuffix(e.Name(), ".go") {
			srcs = append(srcs, e.Name())
		}
	}
	if len(srcs) < 5 {
		t.Fatalf("iomain_celeris has %d .go files: %v", len(srcs), srcs)
	}
	for _, f := range srcs {
		a := readFile(t, filepath.Join("iomain_celeris", f))
		for _, other := range []string{"ios82_celeris", "celeris"} {
			if !bytes.Equal(a, readFile(t, filepath.Join(other, f))) {
				t.Errorf("%s differs between iomain_celeris and %s", f, other)
			}
		}
	}
	ents, _ = os.ReadDir("ios82_celeris")
	for _, e := range ents {
		if _, err := os.Stat(filepath.Join("iomain_celeris", e.Name())); err != nil {
			t.Errorf("ios82_celeris has %s that iomain_celeris lacks", e.Name())
		}
	}
}
