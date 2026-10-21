package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// probatorium#481, celeris#871. The race tier judges liveness and records the
// latency of a SLOW_CLASS ws_echo timeout; every other tier keeps failing on
// it because its timing budget is real. The tolerance reaches the validator
// and the gate only through the race tier's own plumbing:
//
//	validate.yml  -race-tier  only when validate_race == '1'   (the validator)
//	ValidateGate  VALIDATE_GATE_TOLERATE_WS_ECHO_SLOW=1        (the gate)

const (
	slowGateEnv   = "VALIDATE_GATE_TOLERATE_WS_ECHO_SLOW"
	validatorFlag = "-race-tier"
)

func TestOnlyTheRaceTierGateToleratesASlowWSEchoTimeout(t *testing.T) {
	gate := stepRunning(readRepoFile(t, raceTier), "mage ValidateGate")
	if !strings.Contains(gate, slowGateEnv+`: "1"`) {
		t.Errorf("the race tier's ValidateGate step does not set %s=1:\n%s", slowGateEnv, gate)
	}
	files, err := filepath.Glob(".github/workflows/*.yml")
	if err != nil || len(files) == 0 {
		t.Fatalf("no workflows: %v", err)
	}
	for _, f := range files {
		if f == raceTier {
			continue
		}
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), slowGateEnv) || strings.Contains(string(b), "WS_ECHO_SLOW") {
			t.Errorf("%s carries the race tier's ws_echo tolerance", f)
		}
	}
}

// The validator gets -race-tier from the playbook, under the same condition
// that points it at the -race refapp set, and nowhere else.
func TestValidatorRaceTierFlagIsConditionalOnValidateRace(t *testing.T) {
	pb := readRepoFile(t, "ansible/validate.yml")
	if n := strings.Count(pb, validatorFlag); n != 1 {
		t.Fatalf("ansible/validate.yml names %s %d times, want exactly 1", validatorFlag, n)
	}
	i := strings.Index(pb, validatorFlag)
	// The flag sits inside the {% if validate_race ... == '1' %} ... {% endif %}
	// that opens right before it on the same line.
	line := pb[strings.LastIndex(pb[:i], "\n")+1:]
	line, _, _ = strings.Cut(line, "\n")
	open := strings.LastIndex(line[:strings.Index(line, validatorFlag)], "{% if")
	if open < 0 || !strings.Contains(line[open:strings.Index(line, validatorFlag)], "validate_race | default('') == '1'") {
		t.Errorf("-race-tier is not guarded by validate_race == '1':\n%s", line)
	}
	for _, f := range []string{"mage_validate.go", "mage_tier.go", "mage_cluster.go"} {
		if strings.Contains(readRepoFile(t, f), validatorFlag) {
			t.Errorf("%s passes %s to a validator", f, validatorFlag)
		}
	}
}

func TestGateReadsTheToleranceAndPrintsTheSlowLines(t *testing.T) {
	src := readRepoFile(t, "mage_gate.go")
	for _, want := range []string{slowGateEnv, "ToleratesWSEchoSlow", "report.WSEchoSlowLines("} {
		if !strings.Contains(src, want) {
			t.Errorf("mage_gate.go lacks %s", want)
		}
	}
}
