package main

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// probatorium#481. The race tier takes two experiment arms as dispatch
// inputs. Both default to empty, which must change nothing; a value must
// survive every hop to the refapp (workflow env -> mage extra-var ->
// validate.yml environment -> validator / refapp) or the dispatch measures an
// arm that was never applied and reads as a null result.

const raceTier = ".github/workflows/matrix-race-tier.yml"

func TestRaceTierArmInputsDefaultToOff(t *testing.T) {
	src := readRepoFile(t, raceTier)
	for _, name := range []string{"refapp_cpus", "refapp_workload_hint"} {
		in := dispatchInput(src, name)
		if in == "" {
			t.Errorf("%s: no workflow_dispatch input %s", raceTier, name)
			continue
		}
		if !strings.Contains(in, "type: string") {
			t.Errorf("%s is not a string input:\n%s", name, in)
		}
		if !strings.Contains(in, `default: ""`) {
			t.Errorf("%s does not default to empty; a routine dispatch would take the arm:\n%s", name, in)
		}
	}
}

func TestRaceTierArmEnvReachesTheValidateStepAndTheGate(t *testing.T) {
	src := readRepoFile(t, raceTier)
	step := stepRunning(src, "mage Validate")
	if step == "" {
		t.Fatal("no step runs mage Validate")
	}
	for _, want := range []string{
		"PROBATORIUM_REFAPP_CPUS: ${{ inputs.refapp_cpus }}",
		"PROBATORIUM_REFAPP_WORKLOAD_HINT: ${{ inputs.refapp_workload_hint }}",
	} {
		if !strings.Contains(step, want) {
			t.Errorf("the mage Validate step does not set %s", want)
		}
	}
	// Arm B starts adaptive on io_uring: there is no switch for the gate to
	// expect, and the default run still expects it.
	gate := stepRunning(src, "mage ValidateGate")
	if !strings.Contains(gate, `VALIDATE_GATE_EXPECT_ADAPTIVE_SWITCH: ${{ inputs.refapp_workload_hint != 'high' && '1' || '' }}`) {
		t.Errorf("the gate's switch expectation is not conditional on arm B:\n%s", gate)
	}
}

// The other cluster tiers do not carry the arms.
func TestOnlyTheRaceTierCarriesTheArms(t *testing.T) {
	for wf := range stallCaptureTiers {
		if wf == raceTier {
			continue
		}
		if src := readRepoFile(t, wf); strings.Contains(src, "REFAPP_CPUS") || strings.Contains(src, "refapp_workload_hint") {
			t.Errorf("%s carries an experiment arm", wf)
		}
	}
}

func TestRefappArmsReachTheClusterValidator(t *testing.T) {
	mage := readRepoFile(t, "mage_validate.go")
	playbook := readRepoFile(t, "ansible/validate.yml")
	if !strings.Contains(mage, "refappArmExtraVars(os.Getenv)") {
		t.Error("runValidatePlaybook does not build the arm extra-vars")
	}
	for _, h := range []struct{ env, extraVar string }{
		{"PROBATORIUM_REFAPP_CPUS", "probatorium_refapp_cpus"},
		{"PROBATORIUM_REFAPP_WORKLOAD_HINT", "probatorium_refapp_workload_hint"},
	} {
		if !strings.Contains(playbook, h.env+`: "{{ `+h.extraVar+` | default('') }}"`) {
			t.Errorf("ansible/validate.yml does not export %s from extra-var %s", h.env, h.extraVar)
		}
		if !strings.Contains(mage, `"`+h.extraVar+`"`) {
			t.Errorf("mage_validate.go does not name extra-var %s", h.extraVar)
		}
	}
	if !strings.Contains(readRepoFile(t, "validation/refapparms/refapparms.go"), `"PROBATORIUM_REFAPP_CPUS"`) ||
		!strings.Contains(readRepoFile(t, "validation/refapparms/refapparms.go"), `"PROBATORIUM_REFAPP_WORKLOAD_HINT"`) {
		t.Error("refapparms no longer spells the two variables the playbook exports")
	}
}

// earlyArmCheck extracts the setup job's first step script and runs it under
// sh with the two inputs as the step's environment.
func earlyArmCheck(t *testing.T, cpus, hint string) (string, error) {
	t.Helper()
	src := readRepoFile(t, raceTier)
	_, after, ok := strings.Cut(src, "      - name: Refuse a malformed experiment arm before any host is touched\n")
	if !ok {
		t.Fatal("the setup job has no early arm check")
	}
	_, body, ok := strings.Cut(after, "        run: |\n")
	if !ok {
		t.Fatal("the early arm check has no run block")
	}
	var script []string
	for _, line := range strings.Split(body, "\n") {
		if !strings.HasPrefix(line, "          ") {
			break
		}
		script = append(script, strings.TrimPrefix(line, "          "))
	}
	cmd := exec.Command("sh", "-c", strings.Join(script, "\n"))
	cmd.Env = append(os.Environ(), "REFAPP_CPUS="+cpus, "REFAPP_WORKLOAD_HINT="+hint)
	out, err := cmd.CombinedOutput()
	return string(out), err
}

func TestEarlyArmCheckMatchesTheValidator(t *testing.T) {
	for _, tc := range []struct{ cpus, hint string }{
		{"", ""}, {"2-5", ""}, {"0,2,4", "high"}, {"0-3,8", "low"}, {"", "high"}, {"4,0-1", ""},
	} {
		if out, err := earlyArmCheck(t, tc.cpus, tc.hint); err != nil {
			t.Errorf("cpus=%q hint=%q refused: %v\n%s", tc.cpus, tc.hint, err, out)
		}
	}
	// Negative controls: each of these is also refused by refapparms.
	for _, tc := range []struct{ cpus, hint string }{
		{"3", ""}, {"3-3", ""}, {"5-2", ""}, {"2-", ""}, {"a,b", ""}, {"2;id", ""}, {"1,,2", ""},
		{"", "HIGH"}, {"", "iouring"}, {"2-3", "x"},
		// grep matches line by line: a first line that is valid must not carry a second.
		{"2-3\nfoo;bar", ""}, {"0-1\n2-3", ""},
	} {
		if out, err := earlyArmCheck(t, tc.cpus, tc.hint); err == nil {
			t.Errorf("cpus=%q hint=%q accepted:\n%s", tc.cpus, tc.hint, out)
		}
	}
}
