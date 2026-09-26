package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The celeris stress workflow runs on GitHub-hosted runners only, takes free
// text from whoever dispatches it, and is judged by tools/stresstally. These
// pin the properties a later edit could break without any job going red.

func readStressWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(".github/workflows/celeris-stress.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	return string(b)
}

// It must never reach the cluster: no self-hosted or cluster runner label
// and no share of the cluster's concurrency group, which keeps only one
// pending run and would let a stress run evict a queued release tier.
func TestStressWorkflowStaysOffTheCluster(t *testing.T) {
	src := withoutComments(readStressWorkflow(t))
	for _, banned := range []string{"self-hosted", "celeris-cluster", "msr1", "matrix-tier-cluster", "cluster-runner-up"} {
		if strings.Contains(src, banned) {
			t.Errorf("the stress workflow mentions %q; it must run on GitHub-hosted runners only", banned)
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*runs-on:\s*(.+)$`).FindAllStringSubmatch(src, -1) {
		if v := strings.TrimSpace(m[1]); v != "ubuntu-24.04" && v != "${{ matrix.runner }}" {
			t.Errorf("runs-on %q: only ubuntu-24.04, or the planned GitHub-hosted label", v)
		}
	}
	plan, err := os.ReadFile("tools/stresstally/plan.go")
	if err != nil {
		t.Fatal(err)
	}
	labels := regexp.MustCompile(`"(x86|arm64)":\s*"([^"]+)"`).FindAllStringSubmatch(string(plan), -1)
	got := map[string]string{}
	for _, l := range labels {
		got[l[1]] = l[2]
	}
	// Both arches pinned to the same Ubuntu release; a floating alias on
	// one arch would let the arches drift onto different images.
	if got["x86"] != "ubuntu-24.04" || got["arm64"] != "ubuntu-24.04-arm" {
		t.Errorf("planned runner labels %v; want the GitHub-hosted ubuntu-24.04 and ubuntu-24.04-arm", got)
	}
}

// A shard runs the code under test, which may be anyone's pull request. Its
// job token must have no Actions cache access (cache-mode: none, enforced by
// GitHub with scoped cache tokens), on the workflow and again on the shard
// job; and no job may restore or save a cache on its own, or a later run's
// test binary could be built from what an earlier run's code under test saved.
func TestStressWorkflowNeverTouchesTheActionsCache(t *testing.T) {
	src := withoutComments(readStressWorkflow(t))
	if !regexp.MustCompile(`(?m)^cache-mode: none$`).MatchString(src) {
		t.Error("the workflow lacks a top-level cache-mode: none")
	}
	shard := regexp.MustCompile(`(?s)\n  shard:\n(.*?)\n  summary:\n`).FindStringSubmatch(src)
	if shard == nil || !regexp.MustCompile(`(?m)^    cache-mode: none$`).MatchString(shard[1]) {
		t.Error("the shard job, which runs the code under test, lacks its own cache-mode: none")
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*cache-mode:\s*(\S+)`).FindAllStringSubmatch(src, -1) {
		if m[1] != "none" {
			t.Errorf("cache-mode %s: every job of this workflow must have none", m[1])
		}
	}
	if strings.Contains(src, "actions/cache") {
		t.Error("the workflow uses actions/cache")
	}
	setups := regexp.MustCompile(`(?m)^\s*- uses: actions/setup-go@\S+ # v\S+\n\s+with:\n((?:\s{10}.*\n)+)`).FindAllStringSubmatch(src, -1)
	if len(setups) == 0 || len(setups) != strings.Count(src, "actions/setup-go@") {
		t.Fatalf("found %d setup-go steps with a with: block, %d setup-go uses", len(setups), strings.Count(src, "actions/setup-go@"))
	}
	for _, s := range setups {
		if !strings.Contains(s[1], "cache: false\n") {
			t.Errorf("a setup-go step without cache: false:\n%s", s[1])
		}
	}
	// actionlint does not know cache-mode yet; its ignore must stay limited
	// to that one message in this one file.
	cfg, err := os.ReadFile(".github/actionlint.yaml")
	if err != nil {
		t.Fatal(err)
	}
	ign := regexp.MustCompile(`(?s)\npaths:\n(.*)$`).FindStringSubmatch(string(cfg))
	if ign == nil {
		t.Fatal("no paths: section in .github/actionlint.yaml")
	}
	var entries []string
	for _, l := range strings.Split(withoutComments(ign[1]), "\n") {
		if strings.TrimSpace(l) != "" {
			entries = append(entries, strings.TrimSpace(l))
		}
	}
	want := []string{".github/workflows/celeris-stress.yml:", "ignore:", `- 'unexpected key "cache-mode" for "(workflow|job)" section'`}
	if !slices.Equal(entries, want) {
		t.Errorf("actionlint paths: section %q, want exactly %q", entries, want)
	}
}

// goceleris is on the Free plan: 20 hosted jobs at once across the whole
// organization. One run may hold 4 shard jobs, so the base-vs-branch pair
// the docs describe holds 8 and leaves 12: room for a whole celeris CI push,
// 11 jobs on a pull request and 12 on main (docs/STRESS.md).
func TestStressWorkflowCapsItsShareOfTheOrgRunners(t *testing.T) {
	src := withoutComments(readStressWorkflow(t))
	strat := regexp.MustCompile(`(?s)\n    strategy:\n((?:      .*\n)+)`).FindAllStringSubmatch(src, -1)
	if len(strat) != 1 {
		t.Fatalf("%d strategy blocks, want the shard job's one", len(strat))
	}
	if !strings.Contains(strat[0][1], "      max-parallel: 4\n") {
		t.Errorf("the shard matrix is not capped at max-parallel: 4:\n%s", strat[0][1])
	}
}

// withoutComments drops the YAML comment lines, which may name what the
// workflow must not use.
func withoutComments(src string) string {
	var out []string
	for _, l := range strings.Split(src, "\n") {
		if !strings.HasPrefix(strings.TrimSpace(l), "#") {
			out = append(out, l)
		}
	}
	return strings.Join(out, "\n")
}

// Inputs reach a script only through env:. A ${{ }} inside a run: block is
// the template-injection hole zizmor looks for; this catches it without zizmor.
func TestStressWorkflowNeverExpandsIntoAScript(t *testing.T) {
	lines := strings.Split(readStressWorkflow(t), "\n")
	for i := 0; i < len(lines); i++ {
		m := regexp.MustCompile(`^(\s*)(?:- )?run:\s*(.*)$`).FindStringSubmatch(lines[i])
		if m == nil {
			continue
		}
		if strings.Contains(m[2], "${{") {
			t.Errorf("line %d: expression in a run: script: %s", i+1, lines[i])
		}
		if strings.TrimSpace(m[2]) != "|" {
			continue
		}
		indent := len(m[1])
		for j := i + 1; j < len(lines); j++ {
			l := lines[j]
			if strings.TrimSpace(l) != "" && len(l)-len(strings.TrimLeft(l, " ")) <= indent {
				break
			}
			if strings.Contains(l, "${{") {
				t.Errorf("line %d: expression in a run: script: %s", j+1, l)
			}
		}
	}
}

func TestStressWorkflowPinsEveryActionAndReadsOnly(t *testing.T) {
	src := readStressWorkflow(t)
	uses := regexp.MustCompile(`(?m)^\s*(?:- )?uses:\s*(\S+)(.*)$`).FindAllStringSubmatch(src, -1)
	if len(uses) == 0 {
		t.Fatal("no uses: lines found")
	}
	pinned := regexp.MustCompile(`^[\w.-]+/[\w.-]+@[0-9a-f]{40}$`)
	for _, u := range uses {
		if !pinned.MatchString(u[1]) || !regexp.MustCompile(`^\s+# v\d`).MatchString(u[2]) {
			t.Errorf("uses %s%s: pin by full commit sha with a # vX comment", u[1], u[2])
		}
	}
	perms := regexp.MustCompile(`(?m)^permissions:\n((?:[ ]+.*\n)+)`).FindStringSubmatch(src)
	if perms == nil || strings.TrimSpace(perms[1]) != "contents: read" {
		t.Errorf("top-level permissions must be exactly contents: read, got %q", perms)
	}
	if regexp.MustCompile(`(?m)^[ ]+permissions:`).MatchString(src) {
		t.Error("a job widens permissions; the workflow needs none beyond contents: read")
	}
	if strings.Contains(src, "pull_request_target") || strings.Contains(src, "secrets.") {
		t.Error("the workflow must not use pull_request_target or any secret")
	}
	if strings.Count(src, "persist-credentials: false") != strings.Count(src, "actions/checkout@") {
		t.Error("every checkout must set persist-credentials: false")
	}
}

// The workflow hands each dispatch input to `stresstally plan` as IN_<NAME>;
// the tool reads exactly those names. A renamed input on one side only would
// arrive empty and be refused at best, or silently take a default at worst.
func TestStressWorkflowInputsMatchWhatThePlanReads(t *testing.T) {
	src := readStressWorkflow(t)
	block := regexp.MustCompile(`(?s)workflow_dispatch:\s*\n\s*inputs:\n(.*?)\n  pull_request:`).FindStringSubmatch(src)
	if block == nil {
		t.Fatal("no workflow_dispatch inputs block")
	}
	var inputs []string
	for _, m := range regexp.MustCompile(`(?m)^      ([a-z_]+):$`).FindAllStringSubmatch(block[1], -1) {
		inputs = append(inputs, m[1])
	}
	var passed []string
	for _, m := range regexp.MustCompile(`(?m)^\s+IN_([A-Z_]+): \$\{\{ inputs\.([a-z_]+) \}\}$`).FindAllStringSubmatch(src, -1) {
		if strings.ToLower(m[1]) != m[2] {
			t.Errorf("IN_%s carries inputs.%s", m[1], m[2])
		}
		passed = append(passed, m[2])
	}
	tool, err := os.ReadFile("tools/stresstally/main.go")
	if err != nil {
		t.Fatal(err)
	}
	var read []string
	for _, m := range regexp.MustCompile(`getenv\("IN_([A-Z_]+)"\)`).FindAllStringSubmatch(string(tool), -1) {
		read = append(read, strings.ToLower(m[1]))
	}
	slices.Sort(inputs)
	slices.Sort(passed)
	slices.Sort(read)
	if len(inputs) == 0 || !slices.Equal(inputs, passed) || !slices.Equal(inputs, read) {
		t.Errorf("dispatch inputs %v, passed as IN_* %v, read by stresstally plan %v: all three must match", inputs, passed, read)
	}
	if len(inputs) > 10 {
		t.Errorf("%d dispatch inputs; keep to 10, the long-standing workflow_dispatch limit", len(inputs))
	}
}

// The pull_request self-test must run whenever the workflow or the tool
// changes, and only then.
func TestStressWorkflowSelfTestsItsOwnChanges(t *testing.T) {
	src := readStressWorkflow(t)
	paths := regexp.MustCompile(`(?s)  pull_request:\n    branches: \[main\]\n    paths:\n((?:      - .*\n)+)`).FindStringSubmatch(src)
	if paths == nil {
		t.Fatal("pull_request trigger must be limited to main and to paths")
	}
	for _, want := range []string{".github/workflows/celeris-stress.yml", "tools/stresstally/**"} {
		if !strings.Contains(paths[1], "- "+want+"\n") {
			t.Errorf("pull_request paths lack %s", want)
		}
	}
}

// A shard runs the celeris code under test. On the default branch that code
// could write to the Actions cache every workflow here restores from, so the
// plan must learn the run's ref and the default branch (and refuse a match),
// and the shard job must keep the same rule.
func TestStressWorkflowKeepsTheCodeUnderTestOffTheDefaultBranch(t *testing.T) {
	src := readStressWorkflow(t)
	for _, want := range []string{
		"          STRESS_REF: ${{ github.ref }}\n",
		"          STRESS_DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}\n",
		"    if: ${{ github.event_name == 'pull_request' || github.ref != format('refs/heads/{0}', github.event.repository.default_branch) }}\n    runs-on: ${{ matrix.runner }}\n",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the workflow lacks %q", strings.TrimSpace(want))
		}
	}
	tool, err := os.ReadFile("tools/stresstally/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tool), `refuseDefaultBranch(getenv("STRESS_REF"), getenv("STRESS_DEFAULT_BRANCH"))`) {
		t.Error("stresstally plan no longer refuses a dispatch on the default branch")
	}
}

// The plan records the probatorium commit the run came from, so compare can
// refuse two arms that ran different workflows or tallies; the tool refuses
// to plan without it.
func TestStressWorkflowRecordsTheProbatoriumCommit(t *testing.T) {
	src := readStressWorkflow(t)
	plan := regexp.MustCompile(`(?s)\n      - name: Validate the inputs and plan the shards\n(.*?)\n        run: go run ./tools/stresstally plan\n`).FindStringSubmatch(src)
	if plan == nil || !strings.Contains(plan[1], "\n          STRESS_PROBATORIUM_SHA: ${{ github.sha }}\n") {
		t.Error("the plan step does not pass STRESS_PROBATORIUM_SHA: ${{ github.sha }}")
	}
	tool, err := os.ReadFile("tools/stresstally/main.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(tool), `getenv("STRESS_PROBATORIUM_SHA")`) {
		t.Error("stresstally plan no longer reads STRESS_PROBATORIUM_SHA")
	}
}
