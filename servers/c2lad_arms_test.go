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

// THROWAWAY (ab/c1-harness-h6h7-20261105, not for merge). Pins what ladder-gen.py (harness change H6, the #858 sha ladder) generates: the registry columns c2lad-<seq>-r<rep>-<slot>-<engine>
// of servers/c2lad_arms.go, the sha-pinned modules servers/celeris_<slot>, and the marker blocks of .gitignore and .github/dependabot.yml. The expectations come from the files that
// exist (go.mod, the sources of servers/celeris, the dependabot file), not from the generator's spec, so the test also fails when the generator is run with a stale spec or a module is
// edited by hand. The ladder is range-agnostic: adding a rung means a new slot in the spec and a regeneration; nothing in this file names a sha.

const c2ladPrefix = "c2lad"

// The files a generated module carries. server.go is compared after its header (the generator replaces it); debugvars_test.go comes from servers/celeris_v1511.
var (
	c2ladSources = []string{"server.go", "chain_handlers.go", "driver_handlers.go", "streaming_handlers.go", "debugvars.go", "busypoll_knob.go", "pprof_sidecar.go"}
	c2ladTests   = []string{"busypoll_knob_test.go", "pprof_sidecar_test.go"}
	// byte-equal in EVERY module that has them (servers/celeris, servers/celeris_v1511, the generated ones): the two knob files must not drift in a mirror.
	c2ladWholeFile = []string{"busypoll_knob.go", "pprof_sidecar.go", "busypoll_knob_test.go", "pprof_sidecar_test.go"}
)

type c2ladCol struct {
	name, slot, engine string
	seq, rep           int
}

func c2ladCols(t *testing.T) []c2ladCol {
	t.Helper()
	re := regexp.MustCompile(`^` + c2ladPrefix + `-(\d{3})-r(\d+)-([a-z0-9]{2,12})-([a-z0-9+-]+)$`)
	var cols []c2ladCol
	for name := range servers.Registry {
		if !strings.HasPrefix(name, c2ladPrefix+"-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match %s-<seq>-r<rep>-<slot>-<engine>", name, c2ladPrefix)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		cols = append(cols, c2ladCol{name, m[3], m[4], seq, rep})
	}
	sort.Slice(cols, func(i, j int) bool { return cols[i].name < cols[j].name })
	if len(cols) == 0 {
		t.Fatal("no c2lad column in the registry")
	}
	return cols
}

// afterPackage returns the file from its package clause on (the headers of server.go legitimately differ).
func afterPackage(t *testing.T, s, what string) string {
	t.Helper()
	m := regexp.MustCompile(`(?m)^package main$`).FindStringIndex(s)
	if m == nil {
		t.Fatalf("%s has no package clause", what)
	}
	return s[m[0]:]
}

// c2ladModules lists the generated modules: servers/celeris_* with a go.mod, minus the frozen v1.5.11 baseline.
func c2ladModules(t *testing.T) []string {
	t.Helper()
	ents, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	for _, e := range ents {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), "celeris_") || e.Name() == "celeris_v1511" {
			continue
		}
		if _, err := os.Stat(filepath.Join(e.Name(), "go.mod")); err == nil {
			out = append(out, e.Name())
		}
	}
	sort.Strings(out)
	return out
}

func TestC2LadColumnsMatchTheirModules(t *testing.T) {
	mainPin := celerisRequire(t, "celeris")
	used := map[string]bool{}
	for _, c := range c2ladCols(t) {
		a := servers.Registry[c.name]
		base, ok := servers.Registry["celeris-"+c.engine]
		if !ok {
			t.Errorf("%s: no base column celeris-%s", c.name, c.engine)
			continue
		}
		gb, ok := a.Bin.(servers.GoBinary)
		if !ok {
			t.Fatalf("%s: Bin is not a GoBinary", c.name)
		}
		mod := filepath.Base(gb.ModuleDir)
		used[mod] = true
		if gb.ModuleDir != "servers/"+mod {
			t.Errorf("%s: module dir %q", c.name, gb.ModuleDir)
		}
		if a.Framework != "celeris-lad-"+c.slot {
			t.Errorf("%s: framework %q, want celeris-lad-%s (never \"celeris\": the report builders stamp the main pin on it)", c.name, a.Framework, c.slot)
		}
		if strings.Contains(c.name, "io512") {
			t.Errorf("%s: io512 in the name would switch the 90 s / 20 s window of benchmark-tier.yml on", c.name)
		}
		// the module's own go.mod decides the version; the registry may not say anything else
		if got := celerisRequire(t, mod); a.FrameworkVersion != got {
			t.Errorf("%s: registry version %s, but servers/%s/go.mod requires %s: run ladder-gen.py again", c.name, a.FrameworkVersion, mod, got)
		}
		switch mod {
		case "celeris":
			if a.FrameworkVersion != mainPin {
				t.Errorf("%s: main slot at %s, servers/celeris pins %s", c.name, a.FrameworkVersion, mainPin)
			}
			switch {
			case c.slot == "fcbpon":
				if !reflect.DeepEqual(a.SUTEnv, map[string]string{knobEnv: knobValue}) {
					t.Errorf("%s: env %v, want exactly %s=%s", c.name, a.SUTEnv, knobEnv, knobValue)
				}
			default:
				if len(a.SUTEnv) != 0 {
					t.Errorf("%s: env %v, want none", c.name, a.SUTEnv)
				}
			}
		default:
			if mod != "celeris_"+c.slot {
				t.Errorf("%s: slot %s runs module %s, want celeris_%s", c.name, c.slot, mod, c.slot)
			}
			if len(a.SUTEnv) != 0 {
				t.Errorf("%s: env %v on a sha-pinned module", c.name, a.SUTEnv)
			}
		}
		// same column as its twin in every other respect (engine, capabilities, category, language)
		norm := a
		norm.Name, norm.Framework, norm.FrameworkVersion, norm.Bin, norm.SUTEnv = base.Name, base.Framework, base.FrameworkVersion, base.Bin, base.SUTEnv
		if !reflect.DeepEqual(norm, base) {
			t.Errorf("%s differs from celeris-%s in more than Name/Framework/Version/Bin/SUTEnv:\ncolumn %+v\nbase   %+v", c.name, c.engine, a, base)
		}
	}
	for _, m := range c2ladModules(t) {
		if !used[m] {
			t.Errorf("servers/%s is a generated module that no c2lad column runs", m)
		}
	}
}

// The schedule: every (slot, engine) once per block, blocks of ABBA pairs for an even rep count, sorted names = execution order, nobody leads or ends two consecutive blocks.
func TestC2LadLayout(t *testing.T) {
	cols := c2ladCols(t)
	items := map[string]bool{}
	for _, c := range cols {
		items[c.slot+"|"+c.engine] = true
	}
	k := len(items)
	if len(cols)%k != 0 {
		t.Fatalf("%d columns is not a whole number of blocks of %d items", len(cols), k)
	}
	reps := len(cols) / k
	var blocks [][]string
	nrep := map[string]int{}
	for i, c := range cols {
		if c.seq != 1+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, 1+i)
		}
		it := c.slot + "|" + c.engine
		nrep[it]++
		if c.rep != nrep[it] {
			t.Errorf("%s: replicate %d, want %d", c.name, c.rep, nrep[it])
		}
		if i%k == 0 {
			blocks = append(blocks, nil)
		}
		blocks[len(blocks)-1] = append(blocks[len(blocks)-1], it)
	}
	for i, b := range blocks {
		c := slices.Clone(b)
		slices.Sort(c)
		if len(slices.Compact(slices.Clone(c))) != k {
			t.Errorf("block %d repeats an item: %v", i+1, b)
		}
		if c := cols[i*k]; c.rep != i+1 {
			t.Errorf("block %d starts with replicate %d", i+1, c.rep)
		}
	}
	if reps%2 == 0 {
		pos := map[string]int{}
		for j := 0; j < reps; j += 2 {
			rev := slices.Clone(blocks[j])
			slices.Reverse(rev)
			if !slices.Equal(rev, blocks[j+1]) {
				t.Errorf("blocks %d and %d are not an ABBA pair", j+1, j+2)
			}
		}
		for _, b := range blocks {
			for p, it := range b {
				pos[it] += p
			}
		}
		for it, v := range pos {
			if v != pos[blocks[0][0]] {
				t.Errorf("item %s has position sum %d, others %d: the ABBA balance is broken", it, v, pos[blocks[0][0]])
			}
		}
	} else {
		t.Logf("odd reps (%d): the last block is not balanced by a reversal", reps)
	}
	capN := (reps + k - 1) / k
	lead, last := map[string]int{}, map[string]int{}
	for b, bl := range blocks {
		lead[bl[0]]++
		last[bl[k-1]]++
		if b+1 < len(blocks) {
			if bl[0] == blocks[b+1][0] {
				t.Errorf("%s leads blocks %d and %d", bl[0], b+1, b+2)
			}
			if bl[k-1] == blocks[b+1][k-1] {
				t.Errorf("%s is last in blocks %d and %d", bl[k-1], b+1, b+2)
			}
		}
	}
	for it, n := range lead {
		if n > capN {
			t.Errorf("%s leads %d blocks (cap %d)", it, n, capN)
		}
	}
	for it, n := range last {
		if n > capN {
			t.Errorf("%s is last in %d blocks (cap %d)", it, n, capN)
		}
	}
}

// The cell glob selects exactly one scenario per ladder column, and no ladder column leaks into another experiment's glob.
func TestC2LadGlobSelectsOneCellPerColumn(t *testing.T) {
	cols := c2ladCols(t)
	got := p2Matched(t, "ws-hub-broadcast-128/"+c2ladPrefix+"-*")
	if len(got) != len(cols) {
		t.Fatalf("the glob selects %d columns, want %d", len(got), len(cols))
	}
	for c, s := range got {
		if len(s) != 1 || s[0] != "ws-hub-broadcast-128" {
			t.Errorf("column %s matches scenarios %v, want exactly ws-hub-broadcast-128", c, s)
		}
	}
	for _, g := range []string{p2Fast, p2Nine, "get-simple-512c/c1arm-*-io512", "get-simple-512c/c2x86-*-io512"} {
		for c := range p2Matched(t, g) {
			if strings.HasPrefix(c, c2ladPrefix+"-") {
				t.Errorf("the glob %q selects the ladder column %s", g, c)
			}
		}
	}
}

func TestC2LadModulesAreCopiesOfTheMainAdapter(t *testing.T) {
	mods := c2ladModules(t)
	if len(mods) == 0 {
		t.Skip("no generated ladder module in the tree")
	}
	anchor := readFile(t, "celeris_v1511", "go.mod")
	xre := regexp.MustCompile(`(?m)^\s*(golang\.org/x/\S+) (v\S+) // indirect$`)
	wantX := xre.FindAllString(anchor, -1)
	if len(wantX) < 3 {
		t.Fatalf("servers/celeris_v1511/go.mod lists %d x/* requires, want 3", len(wantX))
	}
	for _, m := range mods {
		gm := readFile(t, m, "go.mod")
		if !strings.HasPrefix(gm, "module github.com/goceleris/probatorium/servers/"+m+"\n") {
			t.Errorf("servers/%s/go.mod: wrong module path", m)
		}
		if !strings.Contains(gm, "replace github.com/goceleris/probatorium => ../..\n") {
			t.Errorf("servers/%s/go.mod: no replace of the root module", m)
		}
		// celeris is the only dependency that differs between two rungs: the x/* set is the v1.5.11 module's, in every module
		if got := xre.FindAllString(gm, -1); !slices.Equal(got, wantX) {
			t.Errorf("servers/%s/go.mod holds %v, want the x/* set of servers/celeris_v1511: %v", m, got, wantX)
		}
		ver := celerisRequire(t, m)
		if pm := regexp.MustCompile(`^v\d+\.\d+\.\d+(?:-0\.\d{14}-([0-9a-f]{12}))?$`).FindStringSubmatch(ver); pm == nil {
			t.Errorf("servers/%s/go.mod requires %q: want a release tag or a pseudo-version of a squash-merge commit", m, ver)
		}
		sum := readFile(t, m, "go.sum")
		for _, want := range []string{"github.com/goceleris/celeris " + ver + " h1:", "github.com/goceleris/celeris " + ver + "/go.mod h1:"} {
			if !strings.Contains(sum, want) {
				t.Errorf("servers/%s/go.sum lacks %q", m, want)
			}
		}
		lines := strings.Split(strings.TrimSpace(sum), "\n")
		if !sort.StringsAreSorted(lines) {
			t.Errorf("servers/%s/go.sum is not sorted", m)
		}
		// sources
		ents, _ := os.ReadDir(m)
		want := map[string]bool{"go.mod": true, "go.sum": true, "debugvars_test.go": true}
		for _, f := range append(slices.Clone(c2ladSources), c2ladTests...) {
			want[f] = true
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			if !want[e.Name()] && e.Name() != m { // the built binary is named after the directory (.gitignore)
				t.Errorf("servers/%s/%s is not a file of the generator", m, e.Name())
			}
		}
		hdr := readFile(t, m, "server.go")
		if !strings.HasPrefix(hdr, "// Code generated by ladder-gen.py") {
			t.Errorf("servers/%s/server.go does not start with the generated-code header", m)
		}
		for _, f := range c2ladSources {
			if afterPackage(t, readFile(t, m, f), m+"/"+f) != afterPackage(t, readFile(t, "celeris", f), "celeris/"+f) {
				t.Errorf("servers/%s/%s differs from servers/celeris/%s after the package clause: run ladder-gen.py again (the copies follow the main adapter)", m, f, f)
			}
		}
		for _, f := range c2ladTests {
			if readFile(t, m, f) != readFile(t, "celeris", f) {
				t.Errorf("servers/%s/%s differs from servers/celeris/%s", m, f, f)
			}
		}
		if readFile(t, m, "debugvars_test.go") != readFile(t, "celeris_v1511", "debugvars_test.go") {
			t.Errorf("servers/%s/debugvars_test.go differs from servers/celeris_v1511/debugvars_test.go", m)
		}
	}
	// the two knob files and their tests are one file in EVERY module, the frozen v1.5.11 baseline included
	for _, f := range c2ladWholeFile {
		ref := readFile(t, "celeris", f)
		for _, m := range append([]string{"celeris_v1511"}, mods...) {
			if readFile(t, m, f) != ref {
				t.Errorf("servers/%s/%s differs from servers/celeris/%s", m, f, f)
			}
		}
	}
	// the port-ownership probe of run_bench_cell.yml matches competitors/<bin> as a substring: no generated dir name may be a prefix of another module dir.
	// (servers/celeris is a prefix of every one of them: pre-existing, see the report; the probe also checks -engine and the launched env.)
	all := append([]string{"celeris_v1511"}, mods...)
	for _, a := range all {
		for _, b := range all {
			if a != b && strings.HasPrefix(b, a) {
				t.Errorf("module dir %s is a prefix of %s: the bench port-ownership probe would accept the wrong binary", a, b)
			}
		}
	}
}

// Every generated module is exempt from Dependabot (a comment in the exemption block, NEVER an entry of `directories:`), and ignored for its binary.
func TestC2LadModulesAreExemptFromDependabotAndIgnored(t *testing.T) {
	mods := c2ladModules(t)
	dep := readFile(t, "..", ".github", "dependabot.yml")
	gi := readFile(t, "..", ".gitignore")
	blk := regexp.MustCompile(`(?s)# BEGIN ladder-gen[^\n]*\n(.*?)# END ladder-gen`).FindStringSubmatch(dep)
	gblk := regexp.MustCompile(`(?s)# BEGIN ladder-gen[^\n]*\n(.*?)# END ladder-gen`).FindStringSubmatch(gi)
	if len(mods) == 0 {
		return
	}
	if blk == nil || gblk == nil {
		t.Fatal("dependabot.yml or .gitignore has no ladder-gen block")
	}
	dirs := regexp.MustCompile(`(?m)^\s+- (/\S*)\s*$`).FindAllStringSubmatch(dep, -1)
	listed := map[string]bool{}
	for _, d := range dirs {
		listed[d[1]] = true
	}
	for _, m := range append([]string{"celeris_v1511"}, mods...) {
		if listed["/servers/"+m] {
			t.Errorf("/servers/%s is in dependabot.yml's `directories:`: Dependabot would move a pinned arm", m)
		}
	}
	for _, m := range mods {
		if !strings.Contains(blk[1], "# /servers/"+m+"\n") {
			t.Errorf("dependabot.yml's ladder-gen block does not name /servers/%s", m)
		}
		if !strings.Contains(gblk[1], "/servers/"+m+"/"+m+"\n") {
			t.Errorf(".gitignore's ladder-gen block does not ignore the binary /servers/%s/%s", m, m)
		}
	}
	// no stale names, and the block does not hide a directories entry
	for _, l := range strings.Split(blk[1], "\n") {
		if strings.HasPrefix(strings.TrimSpace(l), "- ") {
			t.Errorf("dependabot ladder-gen block carries a list entry %q", l)
		}
		if m := regexp.MustCompile(`^\s*# /servers/(\S+)$`).FindStringSubmatch(l); m != nil && !slices.Contains(mods, m[1]) {
			t.Errorf("dependabot ladder-gen block exempts /servers/%s, which is not a generated module", m[1])
		}
	}
	// the exempted celeris pins are exactly the ones in the registry
	for _, d := range dirs {
		if strings.HasPrefix(d[1], "/servers/celeris_") {
			t.Errorf("dependabot watches %s", d[1])
		}
	}
}
