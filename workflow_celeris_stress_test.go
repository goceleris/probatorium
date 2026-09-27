package main

import (
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

// The celeris stress workflow takes free text from whoever dispatches it and
// is judged by tools/stresstally. By default it runs on GitHub-hosted
// runners; `target: cluster` runs it on the bare-metal cluster through ONE
// gated job that calls .github/workflows/celeris-stress-cluster.yml. These pin
// the properties a later edit could break without any job going red.

const (
	stressWorkflow        = ".github/workflows/celeris-stress.yml"
	stressClusterWorkflow = ".github/workflows/celeris-stress-cluster.yml"
)

func readStressWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(stressWorkflow)
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	return string(b)
}

func readStressClusterWorkflow(t *testing.T) string {
	t.Helper()
	b, err := os.ReadFile(stressClusterWorkflow)
	if err != nil {
		t.Fatalf("read the cluster workflow: %v", err)
	}
	return string(b)
}

// jobBlock returns one job of a workflow, from its header line to the next
// job header or the end of the jobs: section.
func jobBlock(t *testing.T, src, job string) string {
	t.Helper()
	m := regexp.MustCompile(`(?s)\n  ` + regexp.QuoteMeta(job) + `:\n(.*?)(\n  [A-Za-z0-9_-]+:\n|\z)`).FindStringSubmatch(src)
	if m == nil {
		t.Fatalf("no job %q", job)
	}
	return m[1]
}

// The GitHub target, and the pull_request self-test, never reach the cluster:
// no runner label, host name or cluster composite appears in this file; the
// only cluster reference is the one job that calls the cluster workflow, and
// that job runs only for a dispatch the plan made a cluster plan, after the
// guard allowed it. The hosted shard job runs only for a github plan.
func TestStressWorkflowReachesTheClusterOnlyThroughItsGatedJob(t *testing.T) {
	src := withoutComments(readStressWorkflow(t))
	for _, banned := range []string{"self-hosted", "celeris-cluster", "msr1", "msa2-server", "cluster-runner-up", "cluster-runner-down"} {
		if strings.Contains(src, banned) {
			t.Errorf("the stress workflow mentions %q; everything cluster-side belongs in %s", banned, stressClusterWorkflow)
		}
	}
	for _, m := range regexp.MustCompile(`(?m)^\s*runs-on:\s*(.+)$`).FindAllStringSubmatch(src, -1) {
		if v := strings.TrimSpace(m[1]); v != "ubuntu-24.04" && v != "${{ matrix.runner }}" {
			t.Errorf("runs-on %q: only ubuntu-24.04, or the planned GitHub-hosted label", v)
		}
	}
	if n := strings.Count(src, "matrix-tier-cluster"); n != 1 {
		t.Errorf("matrix-tier-cluster appears %d times; exactly once, in the cluster job's concurrency", n)
	}
	cluster := jobBlock(t, src, "cluster")
	for _, want := range []string{
		"    uses: ./.github/workflows/celeris-stress-cluster.yml\n",
		"    concurrency:\n      group: matrix-tier-cluster\n      cancel-in-progress: false\n",
		"github.event_name == 'workflow_dispatch'",
		"needs.plan.outputs.target == 'cluster'",
		"needs.cluster-guard.result == 'success'",
		"github.ref != format('refs/heads/{0}', github.event.repository.default_branch)",
	} {
		if !strings.Contains(cluster, want) {
			t.Errorf("the cluster job lacks %q:\n%s", strings.TrimSpace(want), cluster)
		}
	}
	if !regexp.MustCompile(`(?m)^    needs: \[plan, cluster-guard\]$`).MatchString(cluster) {
		t.Errorf("the cluster job must need exactly [plan, cluster-guard]:\n%s", cluster)
	}
	guard := jobBlock(t, src, "cluster-guard")
	for _, want := range []string{"github.event_name == 'workflow_dispatch'", "needs.plan.outputs.target == 'cluster'", "bash tools/stresstally/guard.sh"} {
		if !strings.Contains(guard, want) {
			t.Errorf("the cluster-guard job lacks %q", want)
		}
	}
	if strings.Contains(guard, "concurrency:") {
		t.Error("the guard must run OUTSIDE matrix-tier-cluster: it decides whether entering it would cancel a pending run")
	}
	if shard := jobBlock(t, src, "shard"); !strings.Contains(shard, "needs.plan.outputs.target == 'github'") {
		t.Error("the hosted shard job must run only for a github plan")
	}
	plan, err := os.ReadFile("tools/stresstally/plan.go")
	if err != nil {
		t.Fatal(err)
	}
	labels := regexp.MustCompile(`"(x86|arm64)":\s*"(ubuntu[^"]+)"`).FindAllStringSubmatch(string(plan), -1)
	got := map[string]string{}
	for _, l := range labels {
		got[l[1]] = l[2]
	}
	// Both arches pinned to the same Ubuntu release; a floating alias on
	// one arch would let the arches drift onto different images.
	if got["x86"] != "ubuntu-24.04" || got["arm64"] != "ubuntu-24.04-arm" {
		t.Errorf("planned runner labels %v; want the GitHub-hosted ubuntu-24.04 and ubuntu-24.04-arm", got)
	}
	hosts := regexp.MustCompile(`"(x86|arm64)":\s*"(msa2-server|msr1|msa2-client)"`).FindAllStringSubmatch(string(plan), -1)
	gotHosts := map[string]string{}
	for _, h := range hosts {
		gotHosts[h[1]] = h[2]
	}
	if gotHosts["x86"] != "msa2-server" || gotHosts["arm64"] != "msr1" || len(hosts) != 2 {
		t.Errorf("planned cluster hosts %v; want x86 on msa2-server and arm64 on msr1, never the load generator", gotHosts)
	}
}

// The called workflow: bootstrap, one self-hosted job per host, teardown
// that always runs. No concurrency of its own (the caller's job holds
// matrix-tier-cluster; a group here would wait on the caller forever), and
// callable only from another workflow.
func TestStressClusterWorkflowShape(t *testing.T) {
	src := withoutComments(readStressClusterWorkflow(t))
	on := regexp.MustCompile(`(?s)\non:\n(.*?)\n[a-z]`).FindStringSubmatch("\n" + src)
	if on == nil || !strings.HasPrefix(strings.TrimSpace(on[1]), "workflow_call:") ||
		regexp.MustCompile(`(?m)^  (workflow_dispatch|push|pull_request|pull_request_target|schedule):`).MatchString(on[1]) {
		t.Errorf("the cluster workflow must be callable only (on: workflow_call):\n%v", on)
	}
	if strings.Contains(src, "concurrency:") {
		t.Error("the cluster workflow declares a concurrency group; the caller's job already holds matrix-tier-cluster, and a second hold deadlocks")
	}
	boot := jobBlock(t, src, "bootstrap")
	if !strings.Contains(boot, "    name: bootstrap cluster runners\n") || !strings.Contains(boot, "    timeout-minutes: 25\n") ||
		!strings.Contains(boot, "uses: ./.github/actions/cluster-runner-up") || !strings.Contains(boot, `wait-for-manifest-clear:   "true"`) {
		t.Errorf("bootstrap must be the tiers' cluster-runner-up, 25 minutes, waiting for a manifest to clear:\n%s", boot)
	}
	host := jobBlock(t, src, "host")
	if !strings.Contains(host, "    runs-on: [self-hosted, celeris-cluster, '${{ matrix.host }}']\n") {
		t.Errorf("the host job must run on the planned host's self-hosted runner:\n%s", host)
	}
	if !strings.Contains(host, "    needs: bootstrap\n") || !strings.Contains(host, "    timeout-minutes: ${{ matrix.job_timeout }}\n") {
		t.Error("the host job must need bootstrap and take its limit from the plan")
	}
	if !regexp.MustCompile(`(?m)^    cache-mode: none$`).MatchString(host) {
		t.Error("the host job runs the code under test: it needs its own cache-mode: none")
	}
	down := jobBlock(t, src, "teardown")
	for _, want := range []string{"    name: teardown cluster runners\n", "    needs: [bootstrap, host]\n", "    if: always()\n",
		"uses: ./.github/actions/cluster-runner-down"} {
		if !strings.Contains(down, want) {
			t.Errorf("teardown lacks %q:\n%s", strings.TrimSpace(want), down)
		}
	}
}

// Secrets reach the hosted bootstrap and teardown jobs only, never the host
// job that runs the code under test; the caller passes exactly the four the
// cluster composites need, by name (never `secrets: inherit`).
func TestStressClusterSecretsStayOffTheHosts(t *testing.T) {
	cluster := withoutComments(readStressClusterWorkflow(t))
	if got := jobsWithLine(cluster, `secrets\.`); !slices.Equal(got, []string{"bootstrap", "teardown"}) {
		t.Errorf("secrets are read in jobs %v; only bootstrap and teardown may read them", got)
	}
	caller := withoutComments(readStressWorkflow(t))
	if strings.Contains(caller, "secrets: inherit") {
		t.Error("secrets: inherit hands every repository secret to the cluster workflow; pass the four it needs by name")
	}
	if got := jobsWithLine(caller, `secrets\.`); !slices.Equal(got, []string{"cluster"}) {
		t.Errorf("secrets are read in caller jobs %v; only the cluster job passes them on", got)
	}
	var names []string
	for _, m := range regexp.MustCompile(`secrets\.([A-Z_]+)`).FindAllStringSubmatch(caller, -1) {
		names = append(names, m[1])
	}
	slices.Sort(names)
	if want := []string{"CLUSTER_SSH_KEY", "RUNNER_PAT", "TS_OAUTH_CLIENT_ID", "TS_OAUTH_SECRET"}; !slices.Equal(names, want) {
		t.Errorf("the caller passes secrets %v, want exactly %v", names, want)
	}
}

// The guard must know every workflow that shares matrix-tier-cluster: one it
// did not know about could sit pending unseen, and this run would evict it.
func TestStressClusterGuardKnowsEveryClusterWorkflow(t *testing.T) {
	entries, err := os.ReadDir(filepath.Join(".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	var want []string
	for _, e := range entries {
		b, err := os.ReadFile(filepath.Join(".github", "workflows", e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(withoutComments(string(b)), "matrix-tier-cluster") && e.Name() != "celeris-stress.yml" {
			want = append(want, ".github/workflows/"+e.Name())
		}
	}
	guard, err := os.ReadFile("tools/stresstally/guard.go")
	if err != nil {
		t.Fatal(err)
	}
	block := regexp.MustCompile(`(?s)var clusterGroupWorkflows = \[\]string\{(.*?)\}`).FindStringSubmatch(string(guard))
	if block == nil {
		t.Fatal("tools/stresstally/guard.go has no clusterGroupWorkflows list")
	}
	var got []string
	for _, m := range regexp.MustCompile(`"([^"]+)"`).FindAllStringSubmatch(block[1], -1) {
		got = append(got, m[1])
	}
	slices.Sort(got)
	slices.Sort(want)
	if len(want) < 6 || !slices.Equal(got, want) {
		t.Errorf("the guard knows %v; the workflows naming matrix-tier-cluster are %v", got, want)
	}
}

// A shard runs the code under test, which may be anyone's pull request (on
// GitHub-hosted runners). Its job token must have no Actions cache access
// (cache-mode: none, enforced by GitHub with scoped cache tokens), on each
// workflow and again on each job that runs that code; and no job may restore
// or save a cache on its own, or a later run's test binary could be built
// from what an earlier run's code under test saved.
func TestStressWorkflowNeverTouchesTheActionsCache(t *testing.T) {
	for _, file := range []string{stressWorkflow, stressClusterWorkflow} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := withoutComments(string(b))
		if !regexp.MustCompile(`(?m)^cache-mode: none$`).MatchString(src) {
			t.Errorf("%s lacks a top-level cache-mode: none", file)
		}
		for _, m := range regexp.MustCompile(`(?m)^\s*cache-mode:\s*(\S+)`).FindAllStringSubmatch(src, -1) {
			if m[1] != "none" {
				t.Errorf("%s: cache-mode %s: every job of this workflow must have none", file, m[1])
			}
		}
		if strings.Contains(src, "actions/cache") {
			t.Errorf("%s uses actions/cache", file)
		}
		setups := regexp.MustCompile(`(?m)^\s*- uses: actions/setup-go@\S+ # v\S+\n\s+with:\n((?:\s{10}.*\n)+)`).FindAllStringSubmatch(src, -1)
		if len(setups) == 0 || len(setups) != strings.Count(src, "actions/setup-go@") {
			t.Fatalf("%s: found %d setup-go steps with a with: block, %d setup-go uses", file, len(setups), strings.Count(src, "actions/setup-go@"))
		}
		for _, s := range setups {
			if !strings.Contains(s[1], "cache: false\n") {
				t.Errorf("%s: a setup-go step without cache: false:\n%s", file, s[1])
			}
		}
	}
	shard := jobBlock(t, withoutComments(readStressWorkflow(t)), "shard")
	if !regexp.MustCompile(`(?m)^    cache-mode: none$`).MatchString(shard) {
		t.Error("the shard job, which runs the code under test, lacks its own cache-mode: none")
	}
	// actionlint does not know cache-mode yet; its ignore must stay limited
	// to that one message in these two files.
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
	msg := `- 'unexpected key "cache-mode" for "(workflow|job)" section'`
	want := []string{stressClusterWorkflow + ":", "ignore:", msg, stressWorkflow + ":", "ignore:", msg}
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
	for _, file := range []string{stressWorkflow, stressClusterWorkflow} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		lines := strings.Split(string(b), "\n")
		for i := 0; i < len(lines); i++ {
			m := regexp.MustCompile(`^(\s*)(?:- )?run:\s*(.*)$`).FindStringSubmatch(lines[i])
			if m == nil {
				continue
			}
			if strings.Contains(m[2], "${{") {
				t.Errorf("%s line %d: expression in a run: script: %s", file, i+1, lines[i])
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
					t.Errorf("%s line %d: expression in a run: script: %s", file, j+1, l)
				}
			}
		}
	}
}

// Every action is pinned by sha (local composites and the local reusable
// workflow excepted). The token is read-only: contents: read at the top, and
// only the jobs that must list workflow runs (the guard, and the cluster call
// for its bootstrap's eviction audit) add actions: read, nothing else. No
// pull_request_target anywhere.
func TestStressWorkflowPinsEveryActionAndReadsOnly(t *testing.T) {
	for _, file := range []string{stressWorkflow, stressClusterWorkflow} {
		b, err := os.ReadFile(file)
		if err != nil {
			t.Fatal(err)
		}
		src := string(b)
		uses := regexp.MustCompile(`(?m)^\s*(?:- )?uses:\s*(\S+)(.*)$`).FindAllStringSubmatch(src, -1)
		if len(uses) == 0 {
			t.Fatalf("%s: no uses: lines found", file)
		}
		pinned := regexp.MustCompile(`^[\w.-]+/[\w.-]+@[0-9a-f]{40}$`)
		for _, u := range uses {
			if strings.HasPrefix(u[1], "./.github/") {
				continue
			}
			if !pinned.MatchString(u[1]) || !regexp.MustCompile(`^\s+# v\d`).MatchString(u[2]) {
				t.Errorf("%s: uses %s%s: pin by full commit sha with a # vX comment", file, u[1], u[2])
			}
		}
		perms := regexp.MustCompile(`(?m)^permissions:\n((?:[ ]+.*\n)+)`).FindStringSubmatch(src)
		if perms == nil || strings.TrimSpace(perms[1]) != "contents: read" {
			t.Errorf("%s: top-level permissions must be exactly contents: read, got %q", file, perms)
		}
		for _, m := range regexp.MustCompile(`(?m)^    permissions:\n((?:      .*\n)+)`).FindAllStringSubmatch(src, -1) {
			got := strings.Fields(m[1])
			if !slices.Equal(got, []string{"actions:", "read", "contents:", "read"}) && !slices.Equal(got, []string{"contents:", "read"}) {
				t.Errorf("%s: a job widens permissions to %q; at most actions: read and contents: read", file, got)
			}
		}
		if strings.Contains(src, "pull_request_target") || strings.Contains(src, "write-all") {
			t.Errorf("%s must not use pull_request_target or write-all", file)
		}
		if strings.Count(src, "persist-credentials: false") != strings.Count(src, "actions/checkout@") {
			t.Errorf("%s: every checkout must set persist-credentials: false", file)
		}
	}
	src := withoutComments(readStressWorkflow(t))
	if got := jobsWithLine(src, `^    permissions:`); !slices.Equal(got, []string{"cluster-guard", "cluster"}) {
		t.Errorf("jobs declaring permissions %v; only cluster-guard and cluster read workflow runs", got)
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
	// docs.github.com workflow syntax, read 2026-09-27: "The maximum number
	// of top-level properties for inputs is 25." (It was 10 until late 2025.)
	if len(inputs) > 25 {
		t.Errorf("%d dispatch inputs; workflow_dispatch takes at most 25", len(inputs))
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
	for _, want := range []string{".github/workflows/celeris-stress.yml", ".github/workflows/celeris-stress-cluster.yml", "tools/stresstally/**"} {
		if !strings.Contains(paths[1], "- "+want+"\n") {
			t.Errorf("pull_request paths lack %s", want)
		}
	}
}

// A shard runs the celeris code under test. On the default branch that code
// could write to the Actions cache every workflow here restores from, so the
// plan must learn the run's ref and the default branch (and refuse a match),
// and every job that runs that code must keep the same rule.
func TestStressWorkflowKeepsTheCodeUnderTestOffTheDefaultBranch(t *testing.T) {
	src := readStressWorkflow(t)
	for _, want := range []string{
		"          STRESS_REF: ${{ github.ref }}\n",
		"          STRESS_DEFAULT_BRANCH: ${{ github.event.repository.default_branch }}\n",
	} {
		if !strings.Contains(src, want) {
			t.Errorf("the workflow lacks %q", strings.TrimSpace(want))
		}
	}
	rule := "github.ref != format('refs/heads/{0}', github.event.repository.default_branch)"
	for _, job := range []string{"shard", "cluster-guard", "cluster"} {
		if !strings.Contains(jobBlock(t, src, job), rule) {
			t.Errorf("job %s does not keep the default-branch rule", job)
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

// actionlint checks self-hosted labels against this list; the stress host job
// names msa2-server, which no tier did before.
func TestActionlintKnowsTheStressHosts(t *testing.T) {
	cfg, err := os.ReadFile(".github/actionlint.yaml")
	if err != nil {
		t.Fatal(err)
	}
	labels := regexp.MustCompile(`(?s)labels:\n((?:\s+- \S+\n)+)`).FindStringSubmatch(string(cfg))
	if labels == nil {
		t.Fatal("no self-hosted-runner labels in .github/actionlint.yaml")
	}
	for _, want := range []string{"celeris-cluster", "msr1", "msa2-server"} {
		if !strings.Contains(labels[1], "- "+want+"\n") {
			t.Errorf("actionlint does not know the label %s", want)
		}
	}
}

// The guard tells a github dispatch (which never joins matrix-tier-cluster)
// from one that may by the run's title; the title's format is set here, and
// the guard's pattern must match what it produces for target github.
func TestStressWorkflowRunNameIsWhatTheGuardReads(t *testing.T) {
	src := readStressWorkflow(t)
	if !strings.Contains(src, "format('Celeris Stress [{0}/{1}] {2}', inputs.target, inputs.mode, inputs.celeris_ref)") {
		t.Error("run-name no longer titles a dispatch 'Celeris Stress [<target>/<mode>] <ref>'")
	}
	guard, err := os.ReadFile("tools/stresstally/guard.go")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(guard), "regexp.MustCompile(`^Celeris Stress \\[github/`)") {
		t.Error("the guard's github-title pattern is not the run-name's github form")
	}
}
