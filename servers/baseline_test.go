package servers_test

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// The celeris v1.5.11 hashes the baseline module is pinned to. Resolved from
// the Go module proxy with the module download hash (go mod download -json:
// Sum = the zip's h1, GoModSum = the go.mod's h1; the sumdb verifies them),
// after the same procedure reproduced the known hash of the main module's
// pin d1a105bb12eb (h1:3GhVceAH...). ppbase_celeris, the throwaway A/B
// module that ran v1.5.11 on the cluster, carries the same go.sum line.
const (
	celeris1511Sum    = "h1:nrLTkL1ZfwZRXiiYVQe9+cCdbc1DBGoW6IEMlwAGD+0="
	celeris1511ModSum = "h1:ZzUUuUIf5dO7v22roZAfcICffjxSsibYJujTZpxr2Zo="
)

func readFile(t *testing.T, path ...string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(path...))
	if err != nil {
		t.Fatalf("read %s: %v", filepath.Join(path...), err)
	}
	return string(b)
}

// TestBaselineColumnsAreOptIn: the eight baseline columns are registry
// members, hidden from the default expansion and visible with the switch.
func TestBaselineColumnsAreOptIn(t *testing.T) {
	t.Setenv(servers.BaselineEnvVar, "")
	names := servers.BaselineNames()
	if len(names) != 8 {
		t.Fatalf("want 8 baseline columns (epoll x3, iouring x3, adaptive x2), got %d: %v", len(names), names)
	}
	if _, ok := servers.Registry[servers.BaselinePrefix+"std-h1"]; ok {
		t.Error("celeris-std-h1 has no baseline column")
	}
	isIn := func(n string, list []string) bool { return slices.Contains(list, n) }
	adv := func() (out []string) {
		for _, a := range servers.AdaptersSorted() {
			out = append(out, a.Name)
		}
		return out
	}
	for _, n := range names {
		a, ok := servers.Registry[n]
		if !ok || !a.Baseline {
			t.Errorf("%s: registered=%v Baseline=%v", n, ok, a.Baseline)
		}
		if isIn(n, servers.Names()) || isIn(n, adv()) {
			t.Errorf("%s is in the default expansion with %s unset", n, servers.BaselineEnvVar)
		}
	}
	for _, off := range []string{"", "0", "false", "no", "off"} {
		t.Setenv(servers.BaselineEnvVar, off)
		if servers.BaselineEnabled() {
			t.Errorf("%s=%q enables the baseline", servers.BaselineEnvVar, off)
		}
	}
	for _, on := range []string{"1", "true", "TRUE", " 1 "} {
		t.Setenv(servers.BaselineEnvVar, on)
		if !servers.BaselineEnabled() {
			t.Fatalf("%s=%q does not enable the baseline", servers.BaselineEnvVar, on)
		}
		for _, n := range names {
			if !isIn(n, servers.Names()) || !isIn(n, adv()) {
				t.Errorf("%s=%q: %s missing from the expansion", servers.BaselineEnvVar, on, n)
			}
		}
	}
	// Everything else is untouched by the switch: the non-baseline set is
	// identical with it on and off.
	nonBase := func() (out []string) {
		for _, n := range servers.Names() {
			if !servers.Registry[n].Baseline {
				out = append(out, n)
			}
		}
		return out
	}
	t.Setenv(servers.BaselineEnvVar, "1")
	on := nonBase()
	t.Setenv(servers.BaselineEnvVar, "")
	if off := nonBase(); !slices.Equal(on, off) {
		t.Errorf("the switch changed the non-baseline columns: %v vs %v", on, off)
	}
}

// TestBaselineColumnsMirrorTheirMainTwin: Engine (which featureSetFor and
// the -engine flag read) and Capabilities are the twin's, and Framework /
// Category / FrameworkVersion keep the report builders from stamping the
// benched main build's version on the baseline.
func TestBaselineColumnsMirrorTheirMainTwin(t *testing.T) {
	for _, n := range servers.BaselineNames() {
		a := servers.Registry[n]
		e := strings.TrimPrefix(n, servers.BaselinePrefix)
		if n != servers.BaselineName(e) || e == n {
			t.Fatalf("%s is not baseline-celeris-v1511-<engine>", n)
		}
		main, ok := servers.Registry["celeris-"+e]
		if !ok {
			t.Errorf("%s: no main twin celeris-%s", n, e)
			continue
		}
		if a.Engine != main.Engine || a.Engine != e {
			t.Errorf("%s: Engine %q, twin %q, suffix %q: all three must agree", n, a.Engine, main.Engine, e)
		}
		if a.Capabilities != main.Capabilities || a.Language != main.Language {
			t.Errorf("%s: capabilities/language differ from the twin: %+v vs %+v", n, a, main)
		}
		// The mutant of this check is Framework: "celeris".
		if a.Framework == "celeris" || a.Category == "celeris" {
			t.Errorf("%s: Framework %q / Category %q would be stamped with the main celeris version", n, a.Framework, a.Category)
		}
		if a.FrameworkVersion != "v1.5.11" {
			t.Errorf("%s: FrameworkVersion %q, want v1.5.11", n, a.FrameworkVersion)
		}
		if gb, ok := a.Bin.(servers.GoBinary); !ok || gb.ModuleDir != servers.BaselineModule || len(gb.BuildTags) != 0 {
			t.Errorf("%s: Bin %+v, want GoBinary{%s}", n, a.Bin, servers.BaselineModule)
		}
		if len(a.SUTEnv) != 0 {
			t.Errorf("%s: SUTEnv %v: the baseline must run with the environment of its twin", n, a.SUTEnv)
		}
	}
	// Every main celeris loop-engine column has a baseline twin except std.
	for n, a := range servers.Registry {
		if a.Category != "celeris" || n == "celeris-std-h1" {
			continue
		}
		if _, ok := servers.Registry[servers.BaselineName(a.Engine)]; !ok {
			t.Errorf("main column %s has no baseline column (add its engine to baselineEngines, or exclude it deliberately)", n)
		}
	}
}

// TestEveryRegisteredGoColumnHasAModule: each Go adapter's ModuleDir is a
// directory with a go.mod and a main package, every servers/*/go.mod is the
// module of some registered column (no orphan the Deploy "all" would build for
// nothing), and every column of a celeris module is a key of that module's
// engineSpecs (the -engine value the bench passes must not exit "unknown").
func TestEveryRegisteredGoColumnHasAModule(t *testing.T) {
	used := map[string]bool{}
	for n, a := range servers.Registry {
		gb, ok := a.Bin.(servers.GoBinary)
		if !ok {
			continue
		}
		used[filepath.Base(gb.ModuleDir)] = true
		dir := filepath.Join("..", filepath.FromSlash(gb.ModuleDir)) // the test runs in servers/
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err != nil {
			t.Errorf("%s: module %s has no go.mod: %v", n, gb.ModuleDir, err)
			continue
		}
		if m, _ := filepath.Glob(filepath.Join(dir, "*.go")); len(m) == 0 {
			t.Errorf("%s: module %s has no Go source", n, gb.ModuleDir)
		}
		if strings.HasPrefix(filepath.Base(gb.ModuleDir), "celeris") {
			if !regexp.MustCompile(`(?m)^\t"` + regexp.QuoteMeta(a.Engine) + `": \{`).MatchString(readFile(t, dir, "server.go")) {
				t.Errorf("%s: %s/server.go has no engineSpecs entry %q", n, gb.ModuleDir, a.Engine)
			}
		}
	}
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if !e.IsDir() || e.Name() == "common" {
			continue
		}
		if _, err := os.Stat(filepath.Join(e.Name(), "go.mod")); err == nil && !used[e.Name()] {
			t.Errorf("servers/%s has a go.mod but no registered column runs from it", e.Name())
		}
	}
}

// TestBaselineModuleIsPinnedToTheRelease: the module requires exactly
// celeris v1.5.11 (no other celeris version, no replace of celeris) and its
// go.sum carries the released tag's hashes; its go directive is at least the
// root module's (the module `replace`s the root, so Go raises it to that).
func TestBaselineModuleIsPinnedToTheRelease(t *testing.T) {
	if servers.BaselineCelerisVersion != "v1.5.11" {
		t.Fatalf("BaselineCelerisVersion = %q: the release criterion is the v1.5.11 tag (maintainer decision U3)", servers.BaselineCelerisVersion)
	}
	dir := filepath.Join("celeris_v1511")
	mod := readFile(t, dir, "go.mod")
	sum := readFile(t, dir, "go.sum")
	if !strings.HasPrefix(mod, "module github.com/goceleris/probatorium/servers/celeris_v1511\n") {
		t.Errorf("module path:\n%s", mod)
	}
	var celerisReqs []string
	for _, l := range strings.Split(mod, "\n") {
		l = strings.TrimSpace(l)
		if strings.Contains(l, "github.com/goceleris/celeris") && !strings.HasPrefix(l, "module ") {
			celerisReqs = append(celerisReqs, l)
		}
	}
	if len(celerisReqs) != 1 || celerisReqs[0] != "github.com/goceleris/celeris v1.5.11" {
		t.Errorf("go.mod must require exactly github.com/goceleris/celeris v1.5.11 and nothing else of celeris: %q", celerisReqs)
	}
	if strings.Contains(mod, "=> ../celeris") || regexp.MustCompile(`(?m)^replace\s+github.com/goceleris/celeris\b`).MatchString(mod) {
		t.Error("go.mod replaces celeris: the baseline must build the released tag")
	}
	for _, want := range []string{
		"github.com/goceleris/celeris v1.5.11 " + celeris1511Sum,
		"github.com/goceleris/celeris v1.5.11/go.mod " + celeris1511ModSum,
	} {
		if !strings.Contains(sum, want+"\n") {
			t.Errorf("go.sum lacks %q", want)
		}
	}

	goLine := regexp.MustCompile(`(?m)^go (\d+)\.(\d+)(?:\.(\d+))?$`)
	ver := func(s string) [3]int {
		m := goLine.FindStringSubmatch(s)
		if m == nil {
			t.Fatalf("no go directive in:\n%s", s)
		}
		var v [3]int
		for i := range v {
			if m[i+1] != "" {
				v[i] = atoi(m[i+1])
			}
		}
		return v
	}
	b, r := ver(mod), ver(readFile(t, "..", "go.mod"))
	if b[0] < r[0] || (b[0] == r[0] && (b[1] < r[1] || (b[1] == r[1] && b[2] < r[2]))) {
		t.Errorf("baseline go directive %v is below the root module's %v", b, r)
	}
}

func atoi(s string) (n int) {
	for _, c := range s {
		n = n*10 + int(c-'0')
	}
	return n
}

var routeRe = regexp.MustCompile(`\b(?:srv|grp|g)\.(GET|POST|PUT|PATCH|DELETE|HEAD|OPTIONS|Any|Group)\(\s*("[^"]*"(?:\s*\+\s*\w+)?)`)

// routes lists "METHOD path" for every route registration in a module's
// non-test sources.
func routes(t *testing.T, dir string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(dir, "*.go"))
	var out []string
	for _, f := range files {
		if strings.HasSuffix(f, "_test.go") {
			continue
		}
		for _, m := range routeRe.FindAllStringSubmatch(readFile(t, f), -1) {
			out = append(out, filepath.Base(f)+" "+m[1]+" "+strings.Join(strings.Fields(m[2]), ""))
		}
	}
	sort.Strings(out)
	return out
}

// TestBaselineRoutesMatchTheMainModule is the drift guard of the frozen fork.
// servers/celeris keeps moving; the baseline serves the same routes from the
// same sources so a cell on a baseline-celeris-v1511-* column hits the handler its
// twin hits. A route (or an -engine value) added to servers/celeris fails
// here until it is mirrored in servers/celeris_v1511 - or, when v1.5.11
// cannot serve it, until the column set is reconsidered on purpose.
func TestBaselineRoutesMatchTheMainModule(t *testing.T) {
	main, base := routes(t, "celeris"), routes(t, "celeris_v1511")
	if len(main) < 20 {
		t.Fatalf("route scan found only %d registrations in servers/celeris: the scan is stale: %v", len(main), main)
	}
	if !slices.Equal(main, base) {
		t.Errorf("route registrations differ.\nservers/celeris:        %v\nservers/celeris_v1511:  %v", main, base)
	}
	// Same sources: the handler files are byte-identical bar the documented
	// differences (server.go's header comment, debugvars_test.go).
	body := func(s string) string {
		if i := strings.Index(s, "package main"); i >= 0 {
			return s[i:]
		}
		return s
	}
	for _, f := range []string{"server.go", "chain_handlers.go", "driver_handlers.go", "streaming_handlers.go", "debugvars.go"} {
		if body(readFile(t, "celeris", f)) != body(readFile(t, "celeris_v1511", f)) {
			t.Errorf("%s differs between servers/celeris and servers/celeris_v1511 after the package clause: a main-module change must be mirrored (or the difference documented here)", f)
		}
	}
}

// TestBaselineNamesMatchTheObserverRuleAndStayOutOfTheCelerisFamily ties the
// slug to its two consumers. run_bench_cell.yml scrapes /debug/vars for the
// slugs its regex_search matches: it must match every baseline column and
// its twin (the observer sees both alike) and no competitor. goceleris/docs
// (src/lib/results/taxonomy.ts) counts every id starting with "celeris" and
// every category "celeris" as a Celeris result, so a baseline must be neither.
func TestBaselineNamesMatchTheObserverRuleAndStayOutOfTheCelerisFamily(t *testing.T) {
	m := regexp.MustCompile(`regex_search\("([^"]+)"\)`).FindStringSubmatch(readFile(t, "..", "ansible", "tasks", "run_bench_cell.yml"))
	if m == nil {
		t.Fatal("run_bench_cell.yml: no regex_search rule for the observer's -metrics-url")
	}
	rule, err := regexp.Compile(m[1])
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range servers.BaselineNames() {
		if !rule.MatchString(n) {
			t.Errorf("the observer rule %q does not match %s: its /debug/vars would not be scraped, unlike its twin's", m[1], n)
		}
		if strings.HasPrefix(n, "celeris") || servers.Registry[n].Category == "celeris" {
			t.Errorf("%s would be counted as a Celeris column by goceleris/docs", n)
		}
	}
	for n, a := range servers.Registry {
		if a.Baseline {
			continue
		}
		if want := strings.HasPrefix(n, "celeris"); rule.MatchString(n) != want {
			t.Errorf("observer rule %q on %s = %v, want %v", m[1], n, !want, want)
		}
	}
}

// TestInterleaveBaselinePutsEachBaselineBesideItsTwin: with the full sorted
// column list the eight baseline columns would run in one block ahead of the
// celeris-* block (about fifteen hours before their twins); InterleaveBaseline
// puts every one directly beside its twin, alternates which of the two runs
// first, and changes nothing else.
func TestInterleaveBaselinePutsEachBaselineBesideItsTwin(t *testing.T) {
	t.Setenv(servers.BaselineEnvVar, "1")
	in := servers.Names()
	out := servers.InterleaveBaseline(in)
	if len(out) != len(in) {
		t.Fatalf("length %d -> %d", len(in), len(out))
	}
	pos := map[string]int{}
	for i, n := range out {
		pos[n] = i
	}
	if len(pos) != len(in) {
		t.Fatal("a column was duplicated or lost")
	}
	baseFirst := 0
	var firsts []bool
	for _, n := range servers.BaselineNames() {
		tw := "celeris-" + servers.Registry[n].Engine
		d := pos[n] - pos[tw]
		if d != 1 && d != -1 {
			t.Errorf("%s is %d columns from its twin %s", n, d, tw)
		}
		firsts = append(firsts, d == -1)
		if d == -1 {
			baseFirst++
		}
	}
	if baseFirst != 4 {
		t.Errorf("baseline runs first in %d of 8 pairs, want 4 (alternating)", baseFirst)
	}
	for i := 1; i < len(firsts); i++ {
		if firsts[i] == firsts[i-1] {
			t.Errorf("pair orientation does not alternate in sorted order: %v", firsts)
			break
		}
	}
	// Everything else keeps its relative order.
	var a, b []string
	for _, n := range in {
		if !servers.Registry[n].Baseline {
			a = append(a, n)
		}
	}
	for _, n := range out {
		if !servers.Registry[n].Baseline {
			b = append(b, n)
		}
	}
	if !slices.Equal(a, b) {
		t.Error("the non-baseline columns were reordered")
	}

	// No twin in the list (a baseline-only CSV): untouched. Switch off: the
	// default list has no baseline column, so it is returned as is.
	only := []string{servers.BaselineName("epoll-h1-async"), servers.BaselineName("epoll-h1-sync")}
	if got := servers.InterleaveBaseline(only); !slices.Equal(got, only) {
		t.Errorf("baseline-only list reordered: %v", got)
	}
	t.Setenv(servers.BaselineEnvVar, "")
	def := servers.Names()
	if got := servers.InterleaveBaseline(def); !slices.Equal(got, def) {
		t.Error("the default column list was reordered")
	}
}
