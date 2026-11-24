package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The release CPU governor (record, set performance, restore, compare): the steps that bracket the
// measured step in benchmark-tier.yml and matrix-race-tier.yml, .github/scripts/cpu-governor.sh.
// What these tests pin is the guarantee that matters for the data: the measured steps are the same
// single, untouched steps as before, the governor is set BEFORE the sampler and the bench start
// with no way to continue past a failed set, and the restore runs on every path, before the
// results are uploaded and before anything is reclaimed.

const (
	cpuGovSetup = "bash .github/scripts/cpu-governor.sh set"
	cpuGovBack  = "bash .github/scripts/cpu-governor.sh restore"
)

// stepBlock returns the text of the step whose `- name:` line starts with name, up to the next step.
func stepBlock(t *testing.T, src, name string) string {
	t.Helper()
	i := strings.Index(src, "- name: "+name)
	if i < 0 {
		t.Fatalf("no step named %q", name)
	}
	rest := src[i+len("- name: "):]
	// the next step, or the comment block that precedes it (a step's own comments are indented deeper)
	for _, sep := range []string{"\n      - ", "\n      #"} {
		if j := strings.Index(rest, sep); j >= 0 {
			rest = rest[:j]
		}
	}
	return rest
}

func TestBenchTierSetsTheGovernorBeforeTheBenchAndRestoresAfterIt(t *testing.T) {
	src := mustRead(t, benchTier)
	deploy := mustIndex(t, src, "run: mage Deploy\n", "mage Deploy step")
	set := mustIndex(t, src, cpuGovSetup, "governor set step")
	start := mustIndex(t, src, "run: mage HostSamplerStart", "sampler start step")
	bench := mustIndex(t, src, "run: mage BenchTier\n", "mage BenchTier step")
	back := mustIndex(t, src, cpuGovBack, "governor restore step")
	stop := mustIndex(t, src, "run: mage HostSamplerStop\n", "sampler stop step")
	upload := mustIndex(t, src, "name: Upload results", "results upload step")
	cleanup := mustIndex(t, src, "run: mage Cleanup", "mage Cleanup step")
	if !(deploy < set && set < start && start < bench) {
		t.Errorf("the governor must be set after Deploy and before the sampler and the bench: offsets deploy %d set %d sampler %d bench %d", deploy, set, start, bench)
	}
	if !(bench < back && back < stop && stop < upload && upload < cleanup) {
		t.Errorf("the restore must follow the bench and precede the sampler stop, the upload (the records travel with the results) and Cleanup: bench %d restore %d stop %d upload %d cleanup %d", bench, back, stop, upload, cleanup)
	}
	for _, n := range []string{cpuGovSetup, cpuGovBack} {
		if c := strings.Count(src, n); c != 1 {
			t.Errorf("%q occurs %d times, want 1", n, c)
		}
	}
	if n := strings.Count(src, "run: mage BenchTier\n"); n != 1 {
		t.Errorf("%d `run: mage BenchTier` steps, want exactly 1: the measured step must stay a single, unwrapped step", n)
	}
}

func TestBenchTierGovernorStepsFailClosedAndRestoreAlways(t *testing.T) {
	src := mustRead(t, benchTier)
	set := stepBlock(t, src, "CPU governor (record, set performance, read back)")
	back := stepBlock(t, src, "CPU governor (restore, record, compare with the original)")
	if strings.Contains(set, "continue-on-error") || strings.Contains(set, "if:") {
		t.Errorf("the set step must neither continue on error nor be conditional (a failed set must stop the run before anything is measured):\n%s", set)
	}
	if !strings.Contains(back, "if: always()") {
		t.Errorf("the restore step must run `if: always()` (failed bench, cancel, timeout):\n%s", back)
	}
	if strings.Contains(back, "continue-on-error") {
		t.Errorf("the restore step must be able to fail the job (NOT-RESTORED / NOT-EQUAL):\n%s", back)
	}
	for name, blk := range map[string]string{"set": set, "restore": back} {
		if !strings.Contains(blk, "CPU_GOVERNOR: ${{ github.event.inputs.governor || 'performance' }}") {
			t.Errorf("the %s step does not take the governor input with the performance default:\n%s", name, blk)
		}
	}
	// the measured step must not read the input or the records
	bench := stepBlock(t, src, "mage BenchTier (curated matrix, amd64, single pass, publish run-1)")
	for _, bad := range []string{"governor", "GOVERNOR", "cpu-power", "cpu_power"} {
		if strings.Contains(bench, bad) {
			t.Errorf("the mage BenchTier step mentions %q: the governor must stay outside the measured step", bad)
		}
	}
}

func TestBenchTierGovernorInputDefaultsToPerformanceAndIsValidated(t *testing.T) {
	src := mustRead(t, benchTier)
	i := mustIndex(t, src, "      governor:\n        description:", "governor input")
	blk := src[i:]
	blk = blk[:strings.Index(blk, "\npermissions:")]
	if !strings.Contains(blk, "default: performance") || !strings.Contains(blk, "required: false") {
		t.Errorf("the governor input must be optional with default performance:\n%s", blk)
	}
	script := validateStep(t)
	if !strings.Contains(script, `check governor "$IN_GOVERNOR" performance keep`) {
		t.Fatal("the validate step does not check the governor input against performance|keep")
	}
	if !strings.Contains(src, "IN_GOVERNOR: ${{ github.event.inputs.governor }}") {
		t.Error("IN_GOVERNOR is not the governor input")
	}
	run := func(v string) error {
		cmd := exec.Command("bash", "-e", "-c", script)
		cmd.Env = []string{"PATH=" + pathEnv(), "IN_GOVERNOR=" + v}
		out, err := cmd.CombinedOutput()
		t.Logf("%q -> %v\n%s", v, err, out)
		return err
	}
	for _, ok := range []string{"", "performance", "keep"} {
		if err := run(ok); err != nil {
			t.Errorf("governor=%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"powersave", "Performance", " keep", "performance ", "schedutil", "ondemand", "on", "1"} {
		if err := run(bad); err == nil {
			t.Errorf("governor=%q accepted", bad)
		}
	}
}

func TestRaceTierBracketsTheGatesWithTheGovernor(t *testing.T) {
	src := mustRead(t, raceTier)
	deploy := mustIndex(t, src, "run: mage Deploy\n", "mage Deploy step")
	set := mustIndex(t, src, cpuGovSetup, "governor set step")
	gate := mustIndex(t, src, "run: mage ValidateGate\n", "mage ValidateGate step")
	diff := mustIndex(t, src, "run: mage ValidateDiff\n", "mage ValidateDiff step")
	back := mustIndex(t, src, cpuGovBack, "governor restore step")
	stop := mustIndex(t, src, "run: mage HostSamplerStop\n", "sampler stop step")
	upload := mustIndex(t, src, "name: Upload results", "results upload step")
	cleanup := mustIndex(t, src, "run: mage Cleanup", "mage Cleanup step")
	if !(deploy < set && set < gate) {
		t.Errorf("the governor must be set after Deploy and before the gates: deploy %d set %d gate %d", deploy, set, gate)
	}
	// ValidateDiff reads every directory under results/: the copy of the records must come after it
	if !(diff < back && back < stop && stop < upload && upload < cleanup) {
		t.Errorf("the restore must follow ValidateDiff and precede the sampler stop, the upload and Cleanup: diff %d restore %d stop %d upload %d cleanup %d", diff, back, stop, upload, cleanup)
	}
	setBlk := stepBlock(t, src, "CPU governor (record, set performance, read back)")
	backBlk := stepBlock(t, src, "CPU governor (restore, record, compare with the original)")
	if strings.Contains(setBlk, "continue-on-error") || strings.Contains(setBlk, "if:") {
		t.Errorf("the set step must fail closed:\n%s", setBlk)
	}
	if !strings.Contains(backBlk, "if: always()") || strings.Contains(backBlk, "continue-on-error") {
		t.Errorf("the restore step must run always() and be able to fail the job:\n%s", backBlk)
	}
	if !strings.Contains(src, "      governor:\n        description: >-") || !strings.Contains(src, "        default: performance\n        type: string") {
		t.Error("the governor input (default performance) is missing")
	}
	if !strings.Contains(src, `case "${GOVERNOR:-}" in`) {
		t.Error("the governor input is not validated in the setup job")
	}
}

// The script is the same one the workflows call; run its end-to-end test here so `go test` covers it
// on a machine with python3 and bash.
func TestCPUGovernorScriptEndToEnd(t *testing.T) {
	if _, err := exec.LookPath("python3"); err != nil {
		t.Skip("python3 not installed")
	}
	if _, err := os.Stat("ansible/tests/test_cpu_governor.sh"); err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("bash", "ansible/tests/test_cpu_governor.sh").CombinedOutput()
	if err != nil {
		t.Fatalf("test_cpu_governor.sh: %v\n%s", err, out)
	}
}
