//go:build mage

package main

import (
	"errors"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// VALIDATE_TARGET=both runs the matrix twice, once per architecture, and the
// sequential path used to stop at the first architecture that failed. The
// two hosts are independent -- separate validator processes, separate run
// directories, separate results -- so arm64 dying tells you nothing about
// amd64 except that it was never measured.
//
// probatorium#359: the cluster window is the scarce resource. Half of it
// spent proving a failure we already have is half a window wasted, and the
// arch-parity gate (ValidateDiff) needs BOTH sides to say anything at all.
func TestRunValidateTargetsRunsEveryTargetEvenWhenOneFails(t *testing.T) {
	var ran []string
	boom := errors.New("ansible: async job failed")
	err := runValidateTargets([]string{"msa2-server", "msr1"}, func(target string) error {
		ran = append(ran, target)
		if target == "msa2-server" {
			return boom
		}
		return nil
	})
	if want := []string{"msa2-server", "msr1"}; !slices.Equal(ran, want) {
		t.Errorf("ran %v, want %v: a failed target must not cancel the other arch", ran, want)
	}
	if err == nil {
		t.Fatal("runValidateTargets returned nil after a target failed")
	}
	if !strings.Contains(err.Error(), "msa2-server") {
		t.Errorf("error %q does not name the target that failed", err)
	}
	if !errors.Is(err, boom) {
		t.Errorf("error %q does not wrap the underlying failure", err)
	}
}

// Both failing must report both, not whichever finished first.
func TestRunValidateTargetsReportsEveryFailedTarget(t *testing.T) {
	err := runValidateTargets([]string{"msa2-server", "msr1"}, func(target string) error {
		return errors.New("boom on " + target)
	})
	if err == nil {
		t.Fatal("runValidateTargets returned nil with both targets failing")
	}
	for _, want := range []string{"msa2-server", "msr1"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not name %s", err, want)
		}
	}
}

// The clean path stays clean: every target run, no error.
func TestRunValidateTargetsCleanRun(t *testing.T) {
	var ran []string
	if err := runValidateTargets([]string{"msa2-server", "msr1"}, func(target string) error {
		ran = append(ran, target)
		return nil
	}); err != nil {
		t.Fatalf("clean run returned %v", err)
	}
	if len(ran) != 2 {
		t.Errorf("ran %v, want both targets", ran)
	}
}

// The matrix failure caps are read by the validator from ITS OWN
// environment, on the remote host. Setting them in a workflow only puts
// them in the GitHub-Actions process; ansible does not forward them. So a
// cap set in a workflow and not threaded is inert, and inert in the worst
// way: the run looks configured and behaves as if it were not.
func TestMatrixCapExtraVars(t *testing.T) {
	env := map[string]string{"PROBATORIUM_MATRIX_MAX_FAILED_CELLS": "8"}
	got := matrixCapExtraVars(func(k string) string { return env[k] })
	want := []string{"--extra-vars", "probatorium_matrix_max_failed_cells=8"}
	if !slices.Equal(got, want) {
		t.Errorf("matrixCapExtraVars = %v, want %v", got, want)
	}

	env["PROBATORIUM_MATRIX_MAX_CONSECUTIVE_FAILURES"] = "4"
	got = matrixCapExtraVars(func(k string) string { return env[k] })
	if len(got) != 4 {
		t.Errorf("both caps set produced %v, want two extra-vars", got)
	}

	// Unset means "leave the validator's own defaults alone" — not an
	// empty extra-var, which would override them with nothing.
	if got := matrixCapExtraVars(func(string) string { return "" }); got != nil {
		t.Errorf("unset caps produced %v, want nothing", got)
	}
}

// Every hop of that chain must spell the cap the same way, because a
// rename that misses one hop fails silently.
func TestMatrixCapReachesTheValidator(t *testing.T) {
	// hop 1 -> 2: the playbook arguments must actually carry the caps.
	// A text assertion, because the failure it guards is a deletion: with
	// the call site gone, matrixCapExtraVars is still correct, still
	// tested, still compiled -- and never invoked.
	mage, err := os.ReadFile("mage_validate.go")
	if err != nil {
		t.Fatalf("read mage_validate.go: %v", err)
	}
	if !strings.Contains(string(mage), "append(args, matrixCapExtraVars(os.Getenv)...)") {
		t.Error("runValidatePlaybook does not add the cap extra-vars to the ansible argv: " +
			"the caps stop at this process")
	}

	playbook, err := os.ReadFile("ansible/validate.yml")
	if err != nil {
		t.Fatalf("read playbook: %v", err)
	}
	for _, name := range matrixCapNames {
		extraVar := strings.ToLower(name)
		// hop 2 -> 3: the playbook must consume the extra-var this
		// process emits...
		if !strings.Contains(string(playbook), extraVar+" | default('')") {
			t.Errorf("ansible/validate.yml never reads extra-var %q: the cap stops at the playbook", extraVar)
		}
		// ... and re-export it under the name the validator reads.
		if !strings.Contains(string(playbook), name+": \"{{ "+extraVar) {
			t.Errorf("ansible/validate.yml never exports %s to the validator's environment", name)
		}
	}
	// And the validator must actually read that spelling. cmd/validator
	// is a separate package, so this is the one hop a compiler cannot
	// check for us.
	src, err := os.ReadFile("cmd/validator/matrix_run.go")
	if err != nil {
		t.Fatalf("read validator: %v", err)
	}
	for _, name := range matrixCapNames {
		if !strings.Contains(string(src), `"`+name+`"`) {
			t.Errorf("the validator does not read %s; the chain ends one hop short", name)
		}
	}
}

// The weekend soak is the tier where the default cap is expensive: half of
// 32 cells at a 45-minute budget is 12 hours of a 24-hour window, and a run
// that has lost 16 cells cannot satisfy VALIDATE_GATE_EXPECT_CELLS anyway.
// Eight leaves ~18 hours of runway to fix the cause and re-dispatch inside
// the same weekend.
func TestWeekendSoakCapsFailedCells(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/matrix-weekend-tier.yml")
	if err != nil {
		t.Fatalf("read workflow: %v", err)
	}
	m := regexp.MustCompile(`PROBATORIUM_MATRIX_MAX_FAILED_CELLS: "(\d+)"`).FindStringSubmatch(string(b))
	if m == nil {
		t.Fatal("the weekend soak does not cap failed cells; it falls back to half the plan, which is 12 hours of its window")
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n <= 0 {
		t.Fatalf("cap %q is not a positive count", m[1])
	}
	// Asserted as a bound, not an equality: tuning it tighter is a
	// judgement call, loosening it past 8 gives back the hours this
	// exists to save.
	if n > 8 {
		t.Errorf("weekend soak cap is %d failed cells; at ~45 min per cell that is %d hours of the 24-hour window", n, n*45/60)
	}
}
