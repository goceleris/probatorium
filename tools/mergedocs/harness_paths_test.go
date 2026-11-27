package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"sort"
	"strings"
	"testing"
)

// R3 is scoped to what can change a measurement (bot ruling msg_48951bca24af):
// REFUSALS.md carries the path lists, harness_paths.go parses and applies them.

func TestHarnessPathClasses(t *testing.T) {
	type tc struct {
		name  string
		names []string
		// want is "" (accepted) or a substring that the refusal reason must contain
		want string
	}
	cases := []tc{
		// (1) readers, validators, the composer, tools and docs may differ
		{"readers in tools", []string{"tools/govab-read.py", "tools/c2lad-read.sh", "tools/f5-read/main.py"}, ""},
		{"composer", []string{"tools/mergedocs/compose.go", "tools/mergedocs/REFUSALS.md", "tools/mergedocs/compose_test.go"}, ""},
		{"stresstally", []string{"tools/stresstally/judge.go", "tools/stresstally/testdata/good.txt"}, ""},
		{"docs and readme", []string{"README.md", "docs/STRESS.md", "ansible/RUNNER_BOOTSTRAP.md", "ops/power/README.md"}, ""},
		{"validators", []string{"cmd/validator/main.go", "cmd/conformance/main.go", "cmd/validator-checker/main.go", "cmd/validator-replay/main.go", "validation/tier1.go", "validation/checker/checker.go", "validation/markov/kitchen_sink.yaml", "validation/spec/auth_session_ratelimit.openapi.yaml", "validation/refapp_sched.go"}, ""},
		{"publish, diff and gate readers", []string{"mage_diff.go", "mage_gate.go", "mage_validate.go", "mage_fault_control.go"}, ""},
		{"tests of everything", []string{"mage_bench_test.go", "servers/gin/main_test.go", "cmd/runner/clientlock_cell_test.go", "report/testdata/x.json", "ansible/tests/test_os_upgrade.py", "ops/host-sampler/test_fine_sampler.py"}, ""},
		{"lint and test workflows", []string{".github/workflows/lint.yml", ".github/workflows/test.yml", ".github/workflows/publish-results.yml", ".github/CODEOWNERS", ".github/dependabot.yml"}, ""},
		{"host operations verified by R6 and R7", []string{"ansible/os-upgrade.yml", "ansible/reboot.yml", "ansible/files/os_upgrade.py", "ansible/files/reboot_snapshot.sh", "ansible/tasks/os_upgrade_phase.yml", ".github/workflows/cluster-reboot.yml"}, ""},

		// (2) anything that executes or configures a measurement refuses, and is named
		{"server adapter", []string{"tools/mergedocs/compose.go", "servers/gin/main.go"}, "servers/gin/main.go"},
		{"adapter lock file", []string{"servers/express/package-lock.json"}, "servers/express/package-lock.json"},
		{"another adapter go.mod", []string{"servers/gin/go.mod"}, "servers/gin/go.mod"},
		{"ansible bench task", []string{"ansible/tasks/run_bench_cell.yml"}, "ansible/tasks/run_bench_cell.yml"},
		{"ansible tuning", []string{"ansible/tasks/bench_tuning.yml"}, "ansible/tasks/bench_tuning.yml"},
		{"ansible deploy and cleanup", []string{"ansible/deploy.yml", "ansible/cleanup.yml", "ansible/bench.yml", "ansible/inventory.yml"}, "ansible/deploy.yml"},
		{"ansible role", []string{"ansible/roles/python/templates/launcher.sh.j2"}, "ansible/roles/python/templates/launcher.sh.j2"},
		{"nic irq", []string{"ansible/files/nic_irq_pin.sh"}, "ansible/files/nic_irq_pin.sh"},
		{"host sampler", []string{"ops/host-sampler/celeris-host-sampler-fine"}, "ops/host-sampler/celeris-host-sampler-fine"},
		{"runner", []string{"cmd/runner/main.go"}, "cmd/runner/main.go"},
		{"skip list", []string{"cmd/smoketest/main.go"}, "cmd/smoketest/main.go"},
		{"mage runner", []string{"mage_bench.go"}, "mage_bench.go"},
		{"mage tier", []string{"mage_tier.go"}, "mage_tier.go"},
		{"mage publish (BenchTier publishes)", []string{"mage_publish.go"}, "mage_publish.go"},
		{"runner provisioning", []string{"ansible/runner-setup.yml", "ansible/runner-teardown.yml"}, "ansible/runner-setup.yml"},
		{"known hosts script of the dispatch workflows", []string{".github/scripts/refresh-cluster-known-hosts.sh"}, ".github/scripts/refresh-cluster-known-hosts.sh"},
		{"loadgen pin (root go.mod)", []string{"go.mod"}, "go.mod"},
		{"loadgen pin (root go.sum)", []string{"go.sum"}, "go.sum"},
		{"profiles and budgets", []string{"budget/profiles.go"}, "budget/profiles.go"},
		{"scenarios", []string{"scenarios/static.go"}, "scenarios/static.go"},
		{"report schema", []string{"report/schema_v5.json"}, "report/schema_v5.json"},
		{"dispatch workflows", []string{".github/workflows/benchmark-tier.yml", ".github/workflows/matrix-weekend-tier.yml", ".github/actions/cluster-runner-up/action.yml"}, ".github/workflows/benchmark-tier.yml"},
		{"cpu power setter, host recording", []string{"ansible/tasks/cpu_power.yml", "tools/cpu_power_set.py", "ansible/files/cpu_power_record.sh"}, "ansible/tasks/cpu_power.yml"},
		{"cpu power under other spellings", []string{"tools/cpu-power.py", "ansible/tasks/cpu-power.yml", "ops/power/cpupower-set", "tools/cpu_power/set.py"}, "tools/cpu-power.py"},
		{"a setter in tools/ is not a reader", []string{"tools/power/set.py", "tools/set_governor.sh"}, "tools/power/set.py"},
		{"stall capture script", []string{"ops/stallcap.sh", "ansible/files/stall-cap.sh"}, "ops/stallcap.sh"},
		{"refapp code", []string{"validation/refapp/kitchen_sink/main.go"}, "validation/refapp/kitchen_sink/main.go"},
		{"refapp arms", []string{"validation/refapparms/arms.go"}, "validation/refapparms/arms.go"},
		{"twin module", []string{"servers/celeris_v1511/server.go"}, "servers/celeris_v1511/server.go"},
		{"twin module go.mod", []string{"servers/celeris_v1511/go.mod"}, "servers/celeris_v1511/go.mod"},

		// (3) celeris pin bumps are accepted as before
		{"pin bump", []string{"servers/celeris/go.mod", "servers/celeris/go.sum"}, ""},
		{"pin bump with the refapps", []string{"servers/celeris/go.mod", "servers/celeris/go.sum", "validation/refapp/kitchen_sink/go.mod", "validation/refapp/internal/debugvars/go.sum"}, ""},
		{"pin bump with a reader fix", []string{"servers/celeris/go.mod", "tools/mergedocs/compose.go", "docs/STRESS.md"}, ""},
		{"adapter code beside the pin", []string{"servers/celeris/go.sum", "servers/celeris/server.go"}, "servers/celeris/server.go"},
		{"sibling directory with the same prefix", []string{"servers/celeris-extra/go.mod"}, "servers/celeris-extra/go.mod"},

		// (4) a path no list knows is refused by default
		{"unknown top-level file", []string{"newthing.go"}, "newthing.go"},
		{"unknown directory", []string{"loadprofiles/x.yaml"}, "loadprofiles/x.yaml"},
		{"unknown ansible file", []string{"ansible/tasks/something_new.yml"}, "ansible/tasks/something_new.yml"},
		{"unknown workflow", []string{".github/workflows/new-dispatch.yml"}, ".github/workflows/new-dispatch.yml"},
		{"unknown next to a reader", []string{"tools/mergedocs/compose.go", "ops/newtool/run.sh"}, "ops/newtool/run.sh"},
		{"path that escapes the tree", []string{"tools/../servers/gin/main.go"}, "tools/../servers/gin/main.go"},
		{"absolute path", []string{"/servers/gin/main.go"}, "/servers/gin/main.go"},
		{"case differs", []string{"Servers/gin/main.go"}, "Servers/gin/main.go"},

		// identical trees prove nothing
		{"identical trees", nil, "no file differs"},
	}
	for _, c := range cases {
		_, why := judgeHarnessDiff(c.names)
		switch {
		case c.want == "" && why != "":
			t.Errorf("%s: want accepted, refused: %s", c.name, why)
		case c.want != "" && why == "":
			t.Errorf("%s: want refused (%s), accepted", c.name, c.want)
		case c.want != "" && !strings.Contains(why, c.want):
			t.Errorf("%s: the refusal must name %q: %s", c.name, c.want, why)
		}
	}
}

// Every offender is named, not only the first.
func TestHarnessRefusalNamesEveryOffender(t *testing.T) {
	_, why := judgeHarnessDiff([]string{"servers/gin/main.go", "tools/mergedocs/compose.go", "budget/budget.go", "mystery.bin"})
	for _, w := range []string{"servers/gin/main.go", "budget/budget.go", "mystery.bin"} {
		if !strings.Contains(why, w) {
			t.Errorf("missing %s in %s", w, why)
		}
	}
	if strings.Contains(why, "tools/mergedocs/compose.go") {
		t.Errorf("an accepted file is not an offender: %s", why)
	}
	if !strings.Contains(why, "measurement path") || !strings.Contains(why, "no listed path") {
		t.Errorf("the reason must say which kind of offender each is: %s", why)
	}
}

// The record of an accepted difference says what changed, by class.
func TestHarnessDiffRecord(t *testing.T) {
	rec, why := judgeHarnessDiff([]string{"servers/celeris/go.mod", "servers/celeris/go.sum", "tools/mergedocs/compose.go", "docs/STRESS.md", "mage_bench_test.go"})
	if why != "" {
		t.Fatal(why)
	}
	if rec.FilesChanged != 5 || !reflect.DeepEqual(rec.PinBumps, []string{"servers/celeris/go.mod", "servers/celeris/go.sum"}) ||
		!reflect.DeepEqual(rec.NonMeasurement, []string{"docs/STRESS.md", "mage_bench_test.go", "tools/mergedocs/compose.go"}) {
		t.Fatalf("%+v", rec)
	}
}

// The globs: ** is any number of directories (also none), a segment glob never
// crosses a slash.
func TestHarnessGlob(t *testing.T) {
	for _, c := range []struct {
		pat, name string
		want      bool
	}{
		{"servers/**", "servers/gin/main.go", true},
		{"servers/**", "servers/x", true},
		{"servers/**", "servers", false},
		{"servers/**", "serversx/a", false},
		{"servers/celeris/**/go.mod", "servers/celeris/go.mod", true},
		{"servers/celeris/**/go.mod", "servers/celeris/a/b/go.mod", true},
		{"servers/celeris/**/go.mod", "servers/celeris/go.mod.bak", false},
		{"**/*_test.go", "a_test.go", true},
		{"**/*_test.go", "x/y/a_test.go", true},
		{"**/*_test.go", "x/y/a_test.gox", false},
		{"mage_bench*.go", "mage_bench_window.go", true},
		{"mage_bench*.go", "sub/mage_bench.go", false},
		{"ansible/tasks/*cpu_power*", "ansible/tasks/cpu_power.yml", true},
		{"ansible/tasks/*cpu_power*", "ansible/tasks/x/cpu_power.yml", false},
	} {
		if got := globMatch(c.pat, c.name); got != c.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", c.pat, c.name, got, c.want)
		}
	}
}

// The lists are parsed from REFUSALS.md, so the hashed text is the rule.
func TestHarnessListsComeFromRefusalsMD(t *testing.T) {
	lists := harnessLists()
	for name, l := range map[string][]string{"pin": lists.Pin, "inert": lists.Inert, "measurement": lists.Measurement, "non-measurement": lists.NonMeasurement} {
		if len(l) == 0 {
			t.Errorf("list %s is empty", name)
		}
		seen := map[string]bool{}
		for _, g := range l {
			if seen[g] {
				t.Errorf("list %s repeats %s", name, g)
			}
			seen[g] = true
			if !validGlob(g) {
				t.Errorf("list %s: bad glob %q", name, g)
			}
			if !strings.Contains(string(refusalsMD), g) {
				t.Errorf("glob %q is not in REFUSALS.md", g)
			}
		}
	}
	for _, g := range []string{"servers/**", "go.mod", "go.sum", "ansible/tasks/run_bench_cell.yml", "ansible/tasks/bench_tuning.yml", "cmd/runner/**", "budget/**", "ansible/tasks/cpu_power.yml", "**/*cpu?power*", "mage_publish.go", "ansible/runner-setup.yml"} {
		if !contains(lists.Measurement, g) {
			t.Errorf("measurement list lacks %s", g)
		}
	}
	for _, g := range []string{"tools/mergedocs/**", "tools/stresstally/**", "docs/**", "validation/**", "**/*_test.go"} {
		if !contains(append(append([]string(nil), lists.Inert...), lists.NonMeasurement...), g) {
			t.Errorf("non-measurement lists lack %s", g)
		}
	}
	if _, err := parseHarnessLists([]byte("## R3\n```r3-measurement\nservers/**\n```\n")); err == nil {
		t.Error("a REFUSALS.md without all four lists must not parse")
	}
	if _, err := parseHarnessLists([]byte("```r3-pin\n[\n```\n```r3-inert\na\n```\n```r3-measurement\nb\n```\n```r3-non-measurement\nc\n```\n")); err == nil {
		t.Error("a bad glob must not parse")
	}
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// Every tracked file of this checkout is classified by a list; the unknown-path
// refusal is only safe to rely on while the lists keep up with the tree. Set
// MERGEDOCS_STRICT_TREE=1 to turn the report into a failure.
func TestHarnessListsCoverTree(t *testing.T) {
	out, err := exec.Command("git", "ls-files", "-z", "--full-name", ":/").Output()
	if err != nil {
		t.Skip("not a git checkout: " + err.Error())
	}
	var unknown []string
	n := 0
	for _, f := range strings.Split(string(out), "\x00") {
		if f == "" {
			continue
		}
		n++
		if c, _ := classifyHarnessPath(f); c == classUnknown {
			unknown = append(unknown, f)
		}
	}
	if n < 50 {
		t.Skip("too few tracked files for a probatorium checkout")
	}
	sort.Strings(unknown)
	if len(unknown) > 0 {
		msg := "tracked files that no R3 list classifies (a diff touching them is refused): " + strings.Join(unknown, ", ")
		if os.Getenv("MERGEDOCS_STRICT_TREE") == "1" {
			t.Error(msg)
		} else {
			t.Log(msg)
		}
	}
}

// Rename and delete cases run through the real git wrapper in a scratch repo.
func TestGitHarnessDiffRenameDelete(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("no git")
	}
	repo := t.TempDir()
	run := func(args ...string) string {
		t.Helper()
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = append(os.Environ(), "GIT_AUTHOR_NAME=mergedocs-test", "GIT_AUTHOR_EMAIL=test@invalid", "GIT_COMMITTER_NAME=mergedocs-test", "GIT_COMMITTER_EMAIL=test@invalid", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_SYSTEM=/dev/null")
		b, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("git %v: %v: %s", args, err, b)
		}
		return strings.TrimSpace(string(b))
	}
	put := func(name, body string) {
		t.Helper()
		p := filepath.Join(repo, filepath.FromSlash(name))
		if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	run("init", "-q")
	run("config", "commit.gpgsign", "false")
	// a body large enough that git detects a rename by similarity
	body := strings.Repeat("func handler() { /* adapter */ }\n", 40)
	put("servers/gin/main.go", body)
	put("servers/gin/go.mod", "module gin\n")
	put("servers/celeris/go.mod", "module celeris\nrequire x v1.0.0\n")
	put("tools/mergedocs/compose.go", "package main\n")
	put("docs/a.md", "a\n")
	put("ansible/tasks/run_bench_cell.yml", strings.Repeat("- name: cell\n", 40))
	run("add", "-A")
	run("commit", "-q", "-m", "base")
	base := run("rev-parse", "HEAD")

	diff := gitHarnessDiff(repo)
	step := func(msg string, mutate func()) []string {
		t.Helper()
		run("checkout", "-q", "--detach", base)
		mutate()
		run("add", "-A")
		run("commit", "-q", "-m", msg)
		names, err := diff(base, run("rev-parse", "HEAD"))
		if err != nil {
			t.Fatal(err)
		}
		return names
	}
	mv := func(from, to string) {
		t.Helper()
		if err := os.MkdirAll(filepath.Dir(filepath.Join(repo, to)), 0o755); err != nil {
			t.Fatal(err)
		}
		run("mv", from, to)
	}

	// an adapter renamed into the docs tree: the OLD path must show, or the
	// deletion of the adapter hides behind an accepted destination
	names := step("rename adapter into docs", func() { mv("servers/gin/main.go", "docs/gin-main.md") })
	if !contains(names, "servers/gin/main.go") || !contains(names, "docs/gin-main.md") {
		t.Fatalf("a rename must report both paths: %v", names)
	}
	if why := classifyHarnessDiff(names); !strings.Contains(why, "servers/gin/main.go") {
		t.Errorf("rename out of an adapter must be refused, naming the old path: %q", why)
	}

	// an adapter deleted
	names = step("delete adapter", func() { run("rm", "-q", "servers/gin/main.go") })
	if why := classifyHarnessDiff(names); !strings.Contains(why, "servers/gin/main.go") {
		t.Errorf("deleting an adapter file must be refused: %q (%v)", why, names)
	}

	// a bench task renamed into tools
	names = step("rename task into tools", func() { mv("ansible/tasks/run_bench_cell.yml", "tools/mergedocs/run_bench_cell.yml") })
	if why := classifyHarnessDiff(names); !strings.Contains(why, "ansible/tasks/run_bench_cell.yml") {
		t.Errorf("a bench task moved out of ansible must be refused: %q (%v)", why, names)
	}

	// a reader renamed within the accepted trees stays accepted
	names = step("rename a reader", func() { mv("tools/mergedocs/compose.go", "tools/mergedocs/compose2.go") })
	if why := classifyHarnessDiff(names); why != "" {
		t.Errorf("a rename inside tools/ is accepted: %q (%v)", why, names)
	}

	// a document renamed with spaces in its name, and a name that git would quote
	names = step("odd names", func() {
		put("docs/with space.md", "x\n")
		put("docs/ünï.md", "x\n")
	})
	if why := classifyHarnessDiff(names); why != "" || !contains(names, "docs/with space.md") || !contains(names, "docs/ünï.md") {
		t.Errorf("odd names must arrive intact (-z): %q (%v)", why, names)
	}

	// the pin bump alone
	names = step("pin bump", func() { put("servers/celeris/go.mod", "module celeris\nrequire x v1.1.0\n") })
	if why := classifyHarnessDiff(names); why != "" {
		t.Errorf("pin bump: %q (%v)", why, names)
	}

	// a bad ref is an error
	if _, err := diff(base, "no-such-ref"); err == nil {
		t.Error("a bad ref must be an error")
	}
}
