package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// A timing shard is one observation: one go test process whose test binary
// ran once through obs.sh. A shard that ran go test with no observation, with
// two, or with one missing what the pre-registered analysis reads (the wall
// time, the binary's sha256, and under pmu=required the instruction count) is
// not a complete observation, whatever its tests did.
func TestTimingShardNeedsOneCompleteObservation(t *testing.T) {
	tp, err := planDispatch(timingInputs())
	if err != nil {
		t.Fatal(err)
	}
	good := map[string]string{"target": "cluster", "host": "msr1", "mode": "timing", "cpus_asked": "4", "pmu_asked": "optional",
		"memlock_limit": "unlimited", "memlock_in_force": "unlimited:unlimited", "memlock": "unlimited", "timeout": "2m"}
	required := maps2(good, map[string]string{"pmu_asked": "required"})
	one := obsLine(1, "A", 1, "abc", 5e6)
	counted := strings.Replace(one, "instructions_u= cycles_u= task_clock_ms=", "instructions_u=123456 cycles_u=234567 task_clock_ms=5.00", 1)
	for name, x := range map[string]struct {
		pmu, body, exit string
		header          map[string]string
		status          string
	}{
		"one observation":         {"optional", body("PASS") + one, "0", good, statusComplete},
		"no observation":          {"optional", body("PASS"), "0", good, statusUnparsed},
		"two observations":        {"optional", body("PASS") + one + one, "0", good, statusUnparsed},
		"no wall time":            {"optional", body("PASS") + strings.Replace(one, "wall_ns=5000000 ", "wall_ns= ", 1), "0", good, statusUnparsed},
		"no binary":               {"optional", body("PASS") + strings.Replace(one, "binary_sha256=abc ", "binary_sha256= ", 1), "0", good, statusUnparsed},
		"pmu required, counted":   {"required", body("PASS") + counted, "0", required, statusComplete},
		"pmu required, no counts": {"required", body("PASS") + one, "0", required, statusUnparsed},
		"refused":                 {"optional", "stress-refused: the host did not get quiet in 10 minutes\n", "refused", good, statusWrongShape},
	} {
		t.Run(name, func(t *testing.T) {
			c := tp.Cases[0]
			c.Shards, c.Arches, c.PMU = 1, []string{"arm64"}, x.pmu
			dir := t.TempDir()
			writeShard(t, dir, c, "arm64", 1, x.body, x.exit, x.header)
			s := judgeCase(c, dir, testSHA).Shards[0]
			if s.Status != x.status {
				t.Errorf("status %s, want %s: reasons %v, notes %v", s.Status, x.status, s.Reasons, s.Notes)
			}
			if x.status == statusUnparsed && !hasReason(s.Reasons, "observation") {
				t.Errorf("reasons %v do not name the observation", s.Reasons)
			}
		})
	}
}

// writeTimingLogs writes every shard of a timing plan on both hosts, each
// PASSing with one observation of the binary bin(arch, arm, block).
func writeTimingLogs(t *testing.T, p Plan, shas map[string]string, bin func(arch, arm string, block int) string) string {
	t.Helper()
	dir := t.TempDir()
	for _, arch := range []string{"x86", "arm64"} {
		for i, tok := range p.Sequence(42) {
			parts := strings.Split(tok, ":")
			arm := parts[0]
			block, _ := strconv.Atoi(parts[1])
			var c Case
			for _, x := range p.Cases {
				if x.Name == arm {
					c = x
				}
			}
			ov := map[string]string{"celeris_sha": shas[arm], "target": "cluster", "host": clusterHosts[arch], "mode": "timing",
				"cpus_asked": "4", "pmu_asked": "optional", "memlock_in_force": "unlimited:unlimited", "memlock_limit": "unlimited",
				"memlock": "unlimited", "shuffle": parts[2], "cpuset": "2,3,4,5"}
			writeShard(t, dir, c, arch, block, body("PASS")+obsLine(i+1, arm, block, bin(arch, arm, block), 1e6*float64(10+i)), "0", ov)
		}
	}
	return dir
}

func summarizeTiming(t *testing.T, p Plan, shas map[string]string, logs string) (int, string, string) {
	t.Helper()
	pj, _ := json.Marshal(p)
	var cs []string
	for _, a := range p.Arms {
		cs = append(cs, a.Name+"="+shas[a.Name])
	}
	env := map[string]string{"STRESS_PLAN": string(pj), "STRESS_CELERIS_SHA": shas[p.Arms[0].Name], "STRESS_CASE_SHAS": strings.Join(cs, " ")}
	out := t.TempDir()
	var log strings.Builder
	code := cmdSummarize([]string{"-logs", logs, "-out", out}, &log, func(k string) string { return env[k] })
	md, _ := os.ReadFile(filepath.Join(out, "summary.md"))
	return code, log.String(), string(md)
}

// The binaries are the timing's witness that an arm difference is a code
// difference: every observation of an arm on one host ran the same bytes,
// and two arms of one commit (an A/A control) ran the same bytes as each
// other on each host. The summary checks both and says what it found; the
// two arches' binaries always differ.
func TestTimingSummaryChecksTheBinaries(t *testing.T) {
	in := timingInputs()
	in.CelerisRef = "A=" + testSHA + " B=" + testSHA
	p, err := planDispatch(in)
	if err != nil {
		t.Fatal(err)
	}
	p.ProbatoriumSHA = testProbatoriumSHA
	aa := map[string]string{"A": testSHA, "B": testSHA}

	perArch := func(arch, _ string, _ int) string { return "bin-" + arch }
	code, log, md := summarizeTiming(t, p, aa, writeTimingLogs(t, p, aa, perArch))
	if code != 0 {
		t.Errorf("an A/A timing with one binary per arch: exit %d\n%s", code, log)
	}
	for _, want := range []string{"A and B test the same commit and ran the same binary on x86 (`bin-x86`)",
		"A and B test the same commit and ran the same binary on arm64 (`bin-arm64`)"} {
		if !strings.Contains(md, want) {
			t.Errorf("summary.md lacks the witness %q:\n%s", want, md)
		}
	}

	perArm := func(arch, arm string, _ int) string {
		if arch == "x86" {
			return "bin-x86-" + arm
		}
		return "bin-arm64"
	}
	code, log, md = summarizeTiming(t, p, aa, writeTimingLogs(t, p, aa, perArm))
	if code != 1 || !strings.Contains(log, "aa-binaries-differ") || !strings.Contains(md, "different binaries on x86") {
		t.Errorf("A/A arms that ran different binaries on x86: exit %d, want 1 naming aa-binaries-differ\n%s\n%s", code, log, md)
	}

	drift := func(arch, arm string, block int) string {
		if arch == "arm64" && arm == "A" && block == 2 {
			return "bin-other"
		}
		return "bin-" + arch
	}
	code, log, _ = summarizeTiming(t, p, aa, writeTimingLogs(t, p, aa, drift))
	if code != 1 || !strings.Contains(log, "binary-drift") {
		t.Errorf("an arm that ran two binaries on arm64: exit %d, want 1 naming binary-drift\n%s", code, log)
	}

	// Two arms of different commits may of course run different binaries.
	ab := map[string]string{"A": testSHA, "B": branchSHA}
	in.CelerisRef = "A=" + testSHA + " B=" + branchSHA
	q, err := planDispatch(in)
	if err != nil {
		t.Fatal(err)
	}
	q.ProbatoriumSHA = testProbatoriumSHA
	if code, log, _ := summarizeTiming(t, q, ab, writeTimingLogs(t, q, ab, perArm)); code != 0 {
		t.Errorf("A/B arms with their own binaries: exit %d\n%s", code, log)
	}
}
