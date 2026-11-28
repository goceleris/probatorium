package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"slices"
	"strings"
	"testing"
)

// The bare-metal idle cell (.github/workflows/idle-cell-baremetal.yml and its cluster half, tools/idlecell). The reviewer's
// question: can anything in it touch, queue behind or cancel a cluster run that is already going (a release benchmark tier)?
// These tests pin the answer as text (the job graph) and as behaviour (the plan job's script, run for real).

const (
	idleCellWorkflow = ".github/workflows/idle-cell-baremetal.yml"
	idleCellCluster  = ".github/workflows/idle-cell-baremetal-cluster.yml"
)

func idleCellSrc(t *testing.T, file string) string {
	t.Helper()
	b, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	return withoutComments(string(b))
}

func TestIdleCellOnlyRunsWhenDispatched(t *testing.T) {
	caller := idleCellSrc(t, idleCellWorkflow)
	called := idleCellSrc(t, idleCellCluster)
	on := func(src string) []string {
		blk := regexp.MustCompile(`(?ms)^on:\n(.*?)^\S`).FindStringSubmatch(src + "\nX")
		if blk == nil {
			t.Fatal("no on: block")
		}
		var trig []string
		for _, m := range regexp.MustCompile(`(?m)^  ([a-z_]+):`).FindAllStringSubmatch(blk[1], -1) {
			trig = append(trig, m[1])
		}
		return trig
	}
	if got := on(caller); !slices.Equal(got, []string{"workflow_dispatch"}) {
		t.Errorf("the idle cell's triggers = %v, want only workflow_dispatch", got)
	}
	if got := on(called); !slices.Equal(got, []string{"workflow_call"}) {
		t.Errorf("the cluster half's triggers = %v, want only workflow_call", got)
	}
	for _, src := range []string{caller, called} {
		if strings.Contains(src, "schedule:") || strings.Contains(src, "cron:") {
			t.Error("a schedule")
		}
	}
}

// The group is entered by ONE job, behind two guards, and by nothing else.
func TestIdleCellEntersTheGroupOnlyBehindTheGuards(t *testing.T) {
	caller := idleCellSrc(t, idleCellWorkflow)
	called := idleCellSrc(t, idleCellCluster)

	if strings.Contains(called, "concurrency:") {
		t.Error("the cluster half declares a concurrency group: a second hold of matrix-tier-cluster would wait on the caller forever")
	}
	if regexp.MustCompile(`(?m)^concurrency:`).MatchString(caller) {
		t.Error("the caller declares a WORKFLOW-level concurrency group: it would enter at dispatch, before any guard looked")
	}
	if n := strings.Count(caller, "group: matrix-tier-cluster"); n != 1 {
		t.Errorf("the caller names the group %d times, want exactly once (the cluster job)", n)
	}
	if strings.Count(caller, "concurrency:") != 1 {
		t.Error("another job of the caller declares a concurrency group")
	}
	cluster := jobSection(t, caller, "cluster")
	if !strings.Contains(cluster, "group: matrix-tier-cluster") || !strings.Contains(cluster, "cancel-in-progress: false") {
		t.Errorf("the cluster job does not hold matrix-tier-cluster with cancel-in-progress: false:\n%s", cluster)
	}
	for _, want := range []string{
		"needs: [plan, pre-guard, build, cluster-guard]",
		"needs.cluster-guard.result == 'success'",
		"needs.cluster-guard.outputs.attempt == github.run_attempt",
		"needs.plan.outputs.probe != 'true'",
		"uses: ./.github/workflows/idle-cell-baremetal-cluster.yml",
	} {
		if !strings.Contains(cluster, want) {
			t.Errorf("the cluster job lacks %q", want)
		}
	}
	// guard before any host job: pre-guard -> build -> cluster-guard -> cluster
	if !strings.Contains(jobSection(t, caller, "build"), "needs: [plan, pre-guard]") {
		t.Error("the build does not need pre-guard: a refused dispatch would still build")
	}
	if !strings.Contains(jobSection(t, caller, "cluster-guard"), "needs: [plan, pre-guard, build]") {
		t.Error("cluster-guard does not follow the build")
	}
	for _, g := range []string{"pre-guard", "cluster-guard"} {
		if !strings.Contains(jobSection(t, caller, g), `guard -idle -dir`) {
			t.Errorf("%s does not run `stresstally guard -idle`", g)
		}
	}
	// only the pre-guard of a probe may swallow a refusal; the guard that gates the cluster job never does
	if strings.Contains(jobSection(t, caller, "cluster-guard"), "set +e") {
		t.Error("cluster-guard swallows the guard's exit code")
	}
	// no self-hosted runner, no secret and no cluster action outside the cluster half
	if strings.Contains(caller, "self-hosted") || strings.Contains(caller, "cluster-runner-up") || strings.Contains(caller, "CLUSTER_SSH_KEY: ${{ secrets") && !strings.Contains(cluster, "CLUSTER_SSH_KEY") {
		t.Error("the caller reaches the cluster outside its cluster job")
	}
	if strings.Count(called, "self-hosted") != 1 {
		t.Error("the cluster half has more than one self-hosted job")
	}
	// the guard-probe never has a cluster job
	if !strings.Contains(jobSection(t, caller, "build"), "needs.plan.outputs.probe != 'true'") {
		t.Error("the build runs in guard-probe mode")
	}
	// every cluster job's ceiling
	for _, j := range []string{"plan", "pre-guard", "build", "cluster-guard", "collect", "verdict"} {
		if !strings.Contains(jobSection(t, caller, j), "timeout-minutes:") {
			t.Errorf("job %s has no timeout", j)
		}
	}
	for _, j := range []string{"bootstrap", "host", "teardown"} {
		if !strings.Contains(jobSection(t, called, j), "timeout-minutes:") {
			t.Errorf("cluster job %s has no timeout", j)
		}
	}
	// teardown always runs, after the host
	if !strings.Contains(jobSection(t, called, "teardown"), "if: always()") {
		t.Error("teardown is not always()")
	}
	// the stages are separate steps, each uploading when it ends
	for _, s := range []string{"a", "b", "c"} {
		if !strings.Contains(called, "host-run.sh stage "+s) || !strings.Contains(called, "idlecell-raw-${{ matrix.host }}-stage-"+s+"-${{ github.run_id }}") {
			t.Errorf("stage %s is not a step with its own upload", s)
		}
		if !strings.Contains(called, "contains(format(',{0},', inputs.stages), ',"+s+",')") {
			t.Errorf("stage %s is not conditional on the stages input", s)
		}
	}
}

// What the host job may do to a host: nothing but read it.
func TestIdleCellHostScriptsChangeNothing(t *testing.T) {
	files, _ := filepath.Glob("tools/idlecell/*.sh")
	files = append(files, idleCellCluster)
	forbidden := regexp.MustCompile(`(?m)(^|[^a-z_-])(sudo|apt-get|apt|dpkg|systemctl (start|stop|restart|enable|disable)|sysctl +-w|cpupower|modprobe|insmod|rmmod|grubby|update-grub|reboot|shutdown|rm +-|rm +/|git clean|chmod +-R|chown)( |$)`)
	redirect := regexp.MustCompile(`>+ *"?/(sys|proc)/`)
	for _, f := range files {
		src := withoutComments(string(idleCellRead(t, f)))
		for i, line := range strings.Split(src, "\n") {
			if forbidden.MatchString(line) {
				t.Errorf("%s:%d changes or removes something on the host: %s", f, i+1, strings.TrimSpace(line))
			}
			if redirect.MatchString(line) {
				t.Errorf("%s:%d writes under /sys or /proc: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
}

func idleCellRead(t *testing.T, f string) []byte {
	t.Helper()
	b, err := os.ReadFile(f)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// planScript returns the plan job's script.
func idleCellPlanScript(t *testing.T) string {
	t.Helper()
	sec := jobSection(t, string(idleCellRead(t, idleCellWorkflow)), "plan")
	i := strings.Index(sec, "        run: |\n")
	if i < 0 {
		t.Fatal("no run: block in the plan job")
	}
	var out []string
	for _, l := range strings.Split(sec[i+len("        run: |\n"):], "\n") {
		if l != "" && !strings.HasPrefix(l, "          ") {
			break
		}
		out = append(out, strings.TrimPrefix(l, "          "))
	}
	return strings.Join(out, "\n")
}

// The plan job's script, run for real with a fake gh: every refusal, and the outputs of an accepted request.
func TestIdleCellPlanRefusesAMalformedRequest(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash")
	}
	dir := t.TempDir()
	fakeGH := filepath.Join(dir, "gh")
	// gh api repos/goceleris/celeris/commits/main --jq .sha  -> the sha of main; commits/<sha> --jq .sha -> that sha, unless it is the unknown one
	script := "#!/bin/sh\ncase \"$2\" in\n  *commits/main) echo " + strings.Repeat("a", 40) + " ;;\n  *commits/" + strings.Repeat("0", 40) + ") exit 1 ;;\n  *commits/*) echo ok ;;\nesac\n"
	if err := os.WriteFile(fakeGH, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	planFile := filepath.Join(dir, "plan.sh")
	if err := os.WriteFile(planFile, []byte(idleCellPlanScript(t)), 0o644); err != nil {
		t.Fatal(err)
	}
	base := map[string]string{
		"IN_MODE": "run", "IN_HOST": "msr1", "IN_STAGES": "a,b,c", "IN_MAIN": "", "IN_HEAD": strings.Repeat("b", 40), "IN_OLD": strings.Repeat("c", 40),
		"IN_PASSES": "3", "IN_PAIRS": "10", "IN_MAXLOAD": "1.0", "IN_SCPUS": "", "IN_CCPUS": "", "IN_REASON": "bare-metal cells",
		"REF": "refs/heads/main", "DEFAULT_BRANCH": "main",
	}
	run := func(over map[string]string) (string, string, error) {
		env := []string{"PATH=" + dir + ":" + os.Getenv("PATH"), "GH_TOKEN=x", "GITHUB_OUTPUT=" + filepath.Join(t.TempDir(), "out"), "GITHUB_STEP_SUMMARY=/dev/null"}
		outFile := strings.TrimPrefix(env[2], "GITHUB_OUTPUT=")
		for k, v := range base {
			if o, ok := over[k]; ok {
				v = o
			}
			env = append(env, k+"="+v)
		}
		cmd := exec.Command("bash", planFile)
		cmd.Env = env
		b, err := cmd.CombinedOutput()
		out, _ := os.ReadFile(outFile)
		return string(b), string(out), err
	}
	refuse := map[string]map[string]string{
		"a ref but main":                   {"REF": "refs/heads/feat/x"},
		"a wrong mode":                     {"IN_MODE": "bench"},
		"a wrong host":                     {"IN_HOST": "msa2-client"},
		"a wrong stage list":               {"IN_STAGES": "c,a"},
		"passes below the ruling":          {"IN_PASSES": "2"},
		"pairs below the ruling":           {"IN_PAIRS": "9"},
		"a threshold that is not a number": {"IN_MAXLOAD": "high"},
		"a threshold above 4":              {"IN_MAXLOAD": "9"},
		"a CPU list with a command":        {"IN_SCPUS": "0-3;id"},
		"a short sha":                      {"IN_HEAD": "783d0019"},
		"a sha that is not there":          {"IN_OLD": strings.Repeat("0", 40)},
		"an empty reason":                  {"IN_REASON": "  "},
		"braces in the reason":             {"IN_REASON": "x ${{ y }}"},
		"a long reason":                    {"IN_REASON": strings.Repeat("x", 201)},
	}
	for name, over := range refuse {
		if out, _, err := run(over); err == nil {
			t.Errorf("%s was accepted:\n%s", name, out)
		}
	}
	out, outputs, err := run(nil)
	if err != nil {
		t.Fatalf("a good request was refused: %v\n%s", err, out)
	}
	for _, want := range []string{`matrix={"include":[{"host":"msr1","arch":"arm64"}]}`, "hosts=msr1", "stages=a,b,c", "dry=false", "probe=false", "main_sha=" + strings.Repeat("a", 40), "host_timeout=230"} {
		if !strings.Contains(outputs, want) {
			t.Errorf("outputs lack %q:\n%s", want, outputs)
		}
	}
	// a dry-run takes passes and pairs below the ruling, and is always stages a,b; the other host is amd64
	_, outputs, err = run(map[string]string{"IN_MODE": "dry-run", "IN_PASSES": "1", "IN_PAIRS": "1", "IN_HOST": "msa2-server", "IN_STAGES": "c"})
	if err != nil || !strings.Contains(outputs, `"arch":"amd64"`) || !strings.Contains(outputs, "stages=a,b") || !strings.Contains(outputs, "dry=true") || !strings.Contains(outputs, "host_timeout=40") {
		t.Errorf("dry-run: %v\n%s", err, outputs)
	}
	// stages=a,b stops after b: the ceiling does not include stage c
	_, outputs, _ = run(map[string]string{"IN_STAGES": "a,b"})
	if !strings.Contains(outputs, "host_timeout=200") {
		t.Errorf("a,b ceiling:\n%s", outputs)
	}
	// both hosts: two host jobs, one group hold
	_, outputs, err = run(map[string]string{"IN_HOST": "both"})
	if err != nil || !strings.Contains(outputs, `{"host":"msr1","arch":"arm64"},{"host":"msa2-server","arch":"amd64"}`) || !strings.Contains(outputs, "hosts=msr1 msa2-server") {
		t.Errorf("host=both: %v\n%s", err, outputs)
	}
	// a probe needs no sha and builds nothing
	_, outputs, err = run(map[string]string{"IN_MODE": "guard-probe", "IN_HEAD": "", "IN_OLD": ""})
	if err != nil || !strings.Contains(outputs, "probe=true") {
		t.Errorf("guard-probe: %v\n%s", err, outputs)
	}
}

// The reader reproduces the laptop-VM figures of celeris#1485 (SUMMARY.md) and every verdict of the decision rule.
func TestIdleCellReader(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("no python3")
	}
	out, err := exec.Command("python3", "-I", "tools/idlecell/reader_test.py").CombinedOutput()
	if err != nil {
		t.Fatalf("reader_test.py: %v\n%s", err, out)
	}
}
