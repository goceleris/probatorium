package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// The cluster tiers adjudicate their own artifact: mage Validate produces
// the cells, mage ValidateGate and mage ValidateDiff judge them. Gating the
// judges on `if: success()` means a run that found something never gets
// judged -- which is exactly backwards, because the runs worth judging are
// the ones that found something.
//
// probatorium#359 / nightly 34818888908: eight of sixty-four cells failed,
// mage Validate exited non-zero, and BOTH gates were skipped. The artifact
// held 64 complete cell directories and nothing in the run said which cells
// were bad; the answer had to be dug out of validator.stderr by hand.
//
// A skipped step is not a failed step, so running the judges anyway cannot
// turn a red run green: the failed Validate step already fails the job.
var adjudicationTiers = map[string]int{
	".github/workflows/matrix-nightly-tier.yml":  2, // ValidateGate + ValidateDiff
	".github/workflows/matrix-checkptr-tier.yml": 2,
	".github/workflows/matrix-race-tier.yml":     2,
	".github/workflows/matrix-weekend-tier.yml":  2,
	".github/workflows/matrix-pr-tier.yml":       1, // ValidateDiff only
}

var stepSplit = regexp.MustCompile(`(?m)^      - (?:name|uses|run):`)

// adjudicationSteps returns the step blocks that run one of the judges.
func adjudicationSteps(src string) []string {
	var steps []string
	idx := stepSplit.FindAllStringIndex(src, -1)
	for i, loc := range idx {
		end := len(src)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		block := src[loc[0]:end]
		if strings.Contains(block, "run: mage ValidateGate") || strings.Contains(block, "run: mage ValidateDiff") {
			steps = append(steps, block)
		}
	}
	return steps
}

var stepIfRe = regexp.MustCompile(`(?m)^        if: (.*)$`)

func TestValidateGatesJudgeTheArtifactEvenWhenValidateFailed(t *testing.T) {
	for wf, wantSteps := range adjudicationTiers {
		b, err := os.ReadFile(wf)
		if err != nil {
			t.Fatalf("read %s: %v", wf, err)
		}
		steps := adjudicationSteps(string(b))
		// Without this the whole test passes for free the day a step is
		// renamed and the parser stops matching anything.
		if len(steps) != wantSteps {
			t.Fatalf("%s: found %d ValidateGate/ValidateDiff step(s), want %d", wf, len(steps), wantSteps)
		}
		for _, step := range steps {
			m := stepIfRe.FindStringSubmatch(step)
			if m == nil {
				t.Errorf("%s: adjudication step has no `if:` — it defaults to success() and is skipped "+
					"exactly when the run had something to judge:\n%s", wf, step)
				continue
			}
			cond := strings.TrimSpace(m[1])
			if !strings.Contains(cond, "cancelled()") && cond != "always()" {
				t.Errorf("%s: adjudication step runs on `if: %s`; it must survive a failed "+
					"mage Validate (use `!cancelled()`), or a run that found bad cells is never judged:\n%s",
					wf, cond, step)
			}
		}
	}
}
