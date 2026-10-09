package main

import (
	"regexp"
	"strings"
	"testing"
)

// Text-level guards for the teardown verdict, the cancel-path SUT stop and the
// pre-tier host gate (probatorium#473). Each property is hand-edited YAML that
// fails silently: the job still goes green, you find out when a bench starts on
// a dirty host or a cancelled run leaves its SUT behind.

var tierWorkflows = []string{
	".github/workflows/benchmark-tier.yml",
	".github/workflows/matrix-pr-tier.yml",
	".github/workflows/matrix-nightly-tier.yml",
	".github/workflows/matrix-weekend-tier.yml",
	".github/workflows/matrix-race-tier.yml",
	".github/workflows/matrix-checkptr-tier.yml",
}

// stepBlock returns the text of the step named by its `- name:` prefix, up to
// the next step.
func stepBlock(t *testing.T, src, namePrefix string) string {
	t.Helper()
	i := mustIndex(t, src, "- name: "+namePrefix, "step "+namePrefix)
	rest := src[i+1:]
	if j := regexp.MustCompile(`(?m)^    - name:|^      - name:`).FindStringIndex(rest); j != nil {
		return src[i : i+1+j[0]]
	}
	return src[i:]
}

func TestTeardownWaitsForTheClusterBeforeAnythingElseAndNeverFailsThere(t *testing.T) {
	src := mustRead(t, downAction)
	wait := mustIndex(t, src, "name: Wait for the cluster hosts to answer", "wait step")
	for _, later := range []string{"name: Mint removal token", "name: Collect post-hoc host forensics", "name: Rescue in-flight results", "name: Reclaim cluster host state", "name: Run ansible runner-teardown"} {
		if wait > mustIndex(t, src, later, later) {
			t.Errorf("the reachability wait runs after %q: forensics and the reclamation would run against hosts that were only down for a moment", later)
		}
	}
	block := stepBlock(t, src, "Wait for the cluster hosts to answer")
	if !strings.Contains(block, "if: always()") || !strings.Contains(block, "cluster-reachability.sh wait") {
		t.Errorf("wait step must be `if: always()` and run the script:\n%s", block)
	}
	if strings.Contains(block, "exit 1") {
		t.Error("the wait step must not fail by itself; the verdict step decides")
	}
}

func TestTeardownEndsWithAnAlwaysRunVerdictThatReadsTheReclamationLogs(t *testing.T) {
	src := mustRead(t, downAction)
	verdict := mustIndex(t, src, "- name: Teardown verdict", "verdict step")
	for _, earlier := range []string{"name: Sweep any orphan runner registrations", "name: Run ansible runner-teardown", "name: Reclaim cluster host state", "name: Stop the remote SUT by identity"} {
		if verdict < mustIndex(t, src, earlier, earlier) {
			t.Errorf("the verdict runs before %q and cannot judge it", earlier)
		}
	}
	// It must be the last step, so nothing after it can fail unnoticed.
	if n := strings.Count(src[verdict:], "\n    - name:"); n != 0 {
		t.Errorf("%d step(s) follow the verdict", n)
	}
	block := src[verdict:]
	for _, want := range []string{"if: always()", "cluster-teardown-verdict.sh", "steps.reach.outputs.unreachable", "steps.remtoken.outputs.minted", "LOG_DIR: teardown-logs"} {
		if !strings.Contains(block, want) {
			t.Errorf("verdict step lacks %q:\n%s", want, block)
		}
	}
	// The plays are `ignore_unreachable`: their output is the only record of
	// which hosts they could not reach, so each must be teed where the verdict reads.
	for step, log := range map[string]string{
		"Stop the remote SUT by identity": "teardown-logs/stop-sut.log",
		"Reclaim cluster host state":      "teardown-logs/cleanup.log",
		"Run ansible runner-teardown":     "teardown-logs/runner-teardown.log",
	} {
		if b := stepBlock(t, src, step); !strings.Contains(b, "tee "+log) {
			t.Errorf("step %q does not tee its output to %s:\n%s", step, log, b)
		}
	}
}

func TestVerdictHasNoIgnoringEscapeHatch(t *testing.T) {
	for _, f := range []string{".github/scripts/cluster-teardown-verdict.sh", ".github/scripts/cluster-reachability.sh"} {
		src := stripComments(mustRead(t, f))
		if strings.Contains(src, "|| true") && strings.Contains(src, "exit 0") && !strings.Contains(src, "exit 1") {
			t.Errorf("%s can never fail", f)
		}
	}
	if !strings.Contains(mustRead(t, ".github/scripts/cluster-teardown-verdict.sh"), "::error title=Cluster teardown incomplete::") {
		t.Error("the verdict must annotate with ::error::")
	}
}

// The cancel path: after forensics and rescue (which read what it ends), before
// the purge and the runner removal, and not conditional on success.
func TestCancelPathStopsTheSUTByIdentityBeforeThePurge(t *testing.T) {
	src := mustRead(t, downAction)
	stop := mustIndex(t, src, "name: Stop the remote SUT by identity", "stop step")
	if stop < mustIndex(t, src, "name: Upload rescued results", "rescue upload") {
		t.Error("the SUT is stopped before the forensics and the rescue read what it was doing")
	}
	if stop > mustIndex(t, src, "name: Reclaim cluster host state", "reclaim") || stop > mustIndex(t, src, "name: Run ansible runner-teardown", "runner-teardown") {
		t.Error("the SUT stop must come before the purge and the runner removal")
	}
	block := stepBlock(t, src, "Stop the remote SUT by identity")
	if !strings.Contains(block, "if: always()") || !strings.Contains(block, "ansible/stop-sut.yml") {
		t.Errorf("cancel-path step must be `if: always()` and drive ansible/stop-sut.yml:\n%s", block)
	}
}

// The reap and the gate end processes / read hosts; the playbooks that carry
// them are the only part a Go-only run never loads, so pin what they must say.
func TestSUTIdentityIsSetAtLaunchAndReapedEverywhereItMatters(t *testing.T) {
	cell := mustRead(t, "ansible/tasks/run_bench_cell.yml")
	launch := stepBlockYAML(t, cell, "start competitor server in background")
	if !strings.Contains(launch, "PROBATORIUM_SUT_RUN") || !strings.Contains(launch, "GITHUB_RUN_ID") {
		t.Errorf("the SUT launch does not tag its environment with the run id:\n%s", launch)
	}
	// The tag must NOT be merged into cell_sut_env: that mapping is echoed into
	// server.log and results.json, and a per-run id there makes every run differ.
	if regexp.MustCompile(`cell_sut_env:.*PROBATORIUM_SUT_RUN`).MatchString(launch) {
		t.Error("the run tag is part of cell_sut_env and would be recorded in results.json")
	}
	if !strings.Contains(cell, "tasks/reap_bench_sut.yml") {
		t.Error("the per-cell stop does not run the identity reap")
	}
	if !regexp.MustCompile(`(?s)reap_bench_sut\.yml.{0,400}reap_sut_match: tag`).MatchString(cell) {
		t.Error("the per-cell reap runs mid-bench and must match by run tag ONLY (reap_sut_match: tag); a cwd/binary match could take a helper that sits in bench_root")
	}
	if !strings.Contains(mustRead(t, "ansible/cleanup.yml"), "tasks/reap_bench_sut.yml") {
		t.Error("cleanup.yml does not run the identity reap")
	}
	cl := mustRead(t, "ansible/cleanup.yml")
	if mustIndex(t, cl, "tasks/reap_bench_sut.yml", "reap") > mustIndex(t, cl, "- name: Drop bench staging dir", "bench_root removal") {
		t.Error("cleanup.yml removes bench_root before the reap: a cwd/exe match needs the paths to still resolve")
	}
	stop := mustRead(t, "ansible/stop-sut.yml")
	for _, want := range []string{"ignore_unreachable: true", "reap_sut_strict: true", "tasks/reap_bench_sut.yml", "ConnectTimeout=10"} {
		if !strings.Contains(stop, want) {
			t.Errorf("stop-sut.yml lacks %q", want)
		}
	}
}

// stepBlockYAML: a task in an ansible tasks file, by a substring of its name.
func stepBlockYAML(t *testing.T, src, nameFragment string) string {
	t.Helper()
	i := mustIndex(t, src, nameFragment, "task "+nameFragment)
	start := strings.LastIndex(src[:i], "\n- name:")
	if start < 0 {
		start = 0
	}
	end := strings.Index(src[i:], "\n- name:")
	if end < 0 {
		return src[start:]
	}
	return src[start : i+end]
}

// ---- the pre-tier host gate ----

func TestEveryTierRunsTheHostGateBeforeAnythingIsStartedOnTheCluster(t *testing.T) {
	for _, wf := range tierWorkflows {
		src := mustRead(t, wf)
		gate := mustIndex(t, src, "run: mage HostGate", wf+" host gate")
		deploy := mustIndex(t, src, "run: mage Deploy", wf+" deploy")
		if gate > deploy {
			t.Errorf("%s: the host gate runs after mage Deploy; the tier's own fixtures would be reported as leftovers", wf)
		}
		if i := strings.Index(src, "run: mage HostSamplerStart"); i >= 0 && gate > i {
			t.Errorf("%s: the host gate runs after the sampler start", wf)
		}
		block := stepBlock(t, src, "mage HostGate")
		if strings.Contains(block, "continue-on-error") || strings.Contains(block, "if: always()") {
			t.Errorf("%s: the gate must fail the job, not be advisory:\n%s", wf, block)
		}
	}
	// A narrow tier gates only the hosts it uses.
	if !strings.Contains(stepBlock(t, mustRead(t, ".github/workflows/matrix-pr-tier.yml"), "mage HostGate"), "HOSTGATE_TARGET: msa2-server") {
		t.Error("the PR tier runs on msa2-server alone; gating msr1 would hold it up for a host it does not use")
	}
}

// Same hard constraint as monitoring: the gate only reports. A kill added to
// it would make a leftover into a casualty of the next tier (and the leftover
// may be somebody's).
func TestGateFilesCannotDisturbACluster(t *testing.T) {
	for _, path := range []string{
		"hostgate/collect.sh",
		"hostgate/hostgate.go",
		"mage_hostgate.go",
		"ansible/host-gate.yml",
		"ansible/tasks/host_gate.yml",
	} {
		src := stripComments(mustRead(t, path))
		if strings.HasSuffix(path, ".go") {
			for _, bad := range []string{"syscall.Kill", "Process.Kill", "Process.Signal", `"os/signal"`} {
				if strings.Contains(src, bad) {
					t.Errorf("%s uses %s; the gate reports and never acts", path, bad)
				}
			}
			continue
		}
		if m := commandPosition.FindString(src); m != "" {
			t.Errorf("%s invokes %q as a command; the host gate is report-only", path, strings.TrimSpace(m))
		}
		if m := destructiveSystemctl.FindString(src); m != "" {
			t.Errorf("%s: %q changes unit state", path, m)
		}
		for _, re := range otherDestructive {
			if m := re.FindString(src); m != "" {
				t.Errorf("%s: %q can destroy state on a cluster host", path, m)
			}
		}
	}
}
