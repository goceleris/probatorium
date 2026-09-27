package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The behaviour the cluster target changes, written against the dispatch
// path as the workflow drives it (IN_* in the environment, the outputs as
// JSON), so each test states the change without depending on how it is built.

// planEnv is a dispatch's environment: the workflow's plan step with every
// input set, over a branch other than the default one.
func planEnv(t *testing.T, in map[string]string) (map[string]string, string) {
	t.Helper()
	out := filepath.Join(t.TempDir(), "out")
	if err := os.WriteFile(out, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"STRESS_EVENT": "workflow_dispatch", "STRESS_RUN_ID": "42", "GITHUB_OUTPUT": out,
		"STRESS_REF": "refs/heads/stress/runs", "STRESS_DEFAULT_BRANCH": "main", "STRESS_PROBATORIUM_SHA": testProbatoriumSHA,
		"IN_CELERIS_REF": testSHA, "IN_PACKAGES": "./engine/iouring", "IN_RUN": "^TestA$", "IN_COUNT": "2",
		"IN_SHARDS": "3", "IN_ARCHES": "both", "IN_MEMLOCK": "default", "IN_RACE": "false", "IN_TIMEOUT": "5m", "IN_EXTRA": "",
		"IN_TARGET": "github", "IN_MODE": "stress", "IN_TIMING": "",
	}
	for k, v := range in {
		env[k] = v
	}
	return env, out
}

// runPlan runs cmdPlan and returns its exit status, its log and its outputs.
func runPlan(t *testing.T, in map[string]string) (int, string, map[string]string) {
	t.Helper()
	env, out := planEnv(t, in)
	var log strings.Builder
	code := cmdPlan(&log, func(k string) string { return env[k] })
	b, _ := os.ReadFile(out)
	outputs := map[string]string{}
	for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if k, v, ok := strings.Cut(l, "="); ok {
			outputs[k] = v
		}
	}
	return code, log.String(), outputs
}

// matrixOf decodes the matrix output generically.
func matrixOf(t *testing.T, outputs map[string]string) []map[string]any {
	t.Helper()
	var m struct {
		Include []map[string]any `json:"include"`
	}
	if err := json.Unmarshal([]byte(outputs["matrix"]), &m); err != nil {
		t.Fatalf("matrix %q: %v", outputs["matrix"], err)
	}
	return m.Include
}

// memlock "default" is the target's own shape: celeris CI's 8 MiB on GitHub
// (so a github dispatch that does not name a memlock runs exactly as before),
// the host's unlimited on the cluster. The plan records the resolved value.
func TestMemlockDefaultIsTheTargetsShape(t *testing.T) {
	in := goodInputs()
	in.Memlock = "default"
	p, err := planDispatch(in)
	if err != nil {
		t.Fatalf("memlock default refused on github: %v", err)
	}
	if got := p.Cases[0].Memlock; got != "8m" {
		t.Errorf("github memlock default resolved to %q, want 8m", got)
	}
	code, log, outputs := runPlan(t, map[string]string{"IN_TARGET": "cluster"})
	if code != 0 {
		t.Fatalf("cluster dispatch refused: %s", log)
	}
	for _, e := range matrixOf(t, outputs) {
		if e["memlock"] != "unlimited" || e["memlock_limit"] != "unlimited" {
			t.Errorf("cluster entry %v: memlock default must be unlimited", e)
		}
	}
}

// target: cluster plans one job per arch on the bare-metal host of that arch,
// running every shard of the arch in sequence, one process each.
func TestClusterTargetPlansOneJobPerHost(t *testing.T) {
	code, log, outputs := runPlan(t, map[string]string{"IN_TARGET": "cluster"})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, log)
	}
	es := matrixOf(t, outputs)
	if len(es) != 2 {
		t.Fatalf("%d matrix entries, want one per arch: %v", len(es), es)
	}
	hosts := map[string]string{}
	for _, e := range es {
		arch, _ := e["arch"].(string)
		host, _ := e["host"].(string)
		hosts[arch] = host
		seq, _ := e["sequence"].(string)
		if want := "stress:1:42001 stress:2:42002 stress:3:42003"; seq != want {
			t.Errorf("%s sequence %q, want %q", arch, seq, want)
		}
	}
	if hosts["x86"] != "msa2-server" || hosts["arm64"] != "msr1" {
		t.Errorf("hosts %v, want x86 on msa2-server and arm64 on msr1", hosts)
	}
	if outputs["target"] != "cluster" || outputs["mode"] != "stress" {
		t.Errorf("outputs target %q mode %q", outputs["target"], outputs["mode"])
	}
}

// The cluster runs code on bare metal as a user with sudo: only commits a
// celeris maintainer pushed. A pull ref (anyone's fork) is refused there,
// though it stays allowed on GitHub-hosted runners.
// (memlock 8m throughout, so this isolates the ref rule from the memlock
// default: the first failing-first run on the old code failed here on
// memlock "default" before it reached the ref.)
func TestClusterTargetRefusesPullRefs(t *testing.T) {
	if code, log, _ := runPlan(t, map[string]string{"IN_CELERIS_REF": "refs/pull/674/head", "IN_MEMLOCK": "8m"}); code != 0 {
		t.Fatalf("github refused a pull ref: %s", log)
	}
	code, log, _ := runPlan(t, map[string]string{"IN_TARGET": "cluster", "IN_CELERIS_REF": "refs/pull/674/head", "IN_MEMLOCK": "8m"})
	if code != 2 || !strings.Contains(log, "refs/pull/674/head") {
		t.Errorf("cluster accepted a pull ref: exit %d\n%s", code, log)
	}
}

// Timing needs the cluster's exclusive hosts; on GitHub it is refused, and
// the refusal names that rule (memlock 8m, so the memlock default plays no
// part; the old code accepted this dispatch outright).
func TestTimingModeNeedsTheCluster(t *testing.T) {
	code, log, _ := runPlan(t, map[string]string{"IN_MODE": "timing", "IN_SHARDS": "2", "IN_MEMLOCK": "8m"})
	if code != 2 || !strings.Contains(log, "mode timing needs target cluster") {
		t.Errorf("timing on github accepted: exit %d\n%s", code, log)
	}
}

// A timing plan runs one observation per arm per block, the arms in a
// counterbalanced order (AB then BA), both arms of a block on one -shuffle seed.
func TestTimingPlanCounterbalancesTheArms(t *testing.T) {
	code, log, outputs := runPlan(t, map[string]string{
		"IN_TARGET": "cluster", "IN_MODE": "timing", "IN_SHARDS": "4",
		"IN_CELERIS_REF": "A=" + testSHA + " B=" + branchSHA, "IN_TIMING": "cpus=4",
	})
	if code != 0 {
		t.Fatalf("exit %d: %s", code, log)
	}
	want := "A:1:42001 B:1:42001 B:2:42002 A:2:42002 A:3:42003 B:3:42003 B:4:42004 A:4:42004"
	for _, e := range matrixOf(t, outputs) {
		if e["sequence"] != want {
			t.Errorf("%v sequence %q, want %q", e["arch"], e["sequence"], want)
		}
	}
}

// Two runs that differ in where or how they ran are not a comparison of two
// commits: a GitHub arm against a cluster arm has two causes.
func TestCompareRefusesAPairThatDiffersInTarget(t *testing.T) {
	base := armReport(t, testSHA, "3", "2", nil, nil)
	branch := armReport(t, branchSHA, "3", "2", nil, nil)
	path := filepath.Join(branch, "report.json")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var r map[string]any
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	r["cases"].([]any)[0].(map[string]any)["config"].(map[string]any)["target"] = "cluster"
	b, _ = json.Marshal(r)
	if err := os.WriteFile(path, b, 0o644); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code := cmdCompare([]string{base, branch}, &out); code != 2 || !strings.Contains(out.String(), "target") {
		t.Errorf("a github arm and a cluster arm were compared: exit %d\n%s", code, out.String())
	}
}
