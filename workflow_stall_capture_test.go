package main

import (
	"os"
	"regexp"
	"strings"
	"testing"
)

// celeris#588 / probatorium#412 review round 3. The stall capture -- the
// in-stall dossiers and the refapp's debug side listener -- is on in a run
// with PROBATORIUM_REFAPP_FAULT set or PROBATORIUM_STALL_CAPTURE=1
// (validation/liveness.go stallCaptureEnabled), and off in every other
// run. The validator reads both from ITS OWN environment on the cluster
// host, where a variable set on the workflow or on mage does not arrive
// unless it is carried: workflow env -> mage extra-var -> validate.yml
// environment (probatorium#359). Only REFAPP_FAULT made those hops, so a
// cluster run could switch the capture on only by injecting a fault, and no
// routine soak could carry the side-listener attribution that celeris#588's
// release criterion asks of a natural event.

// stallCaptureHops is each knob's spelling at every hop.
var stallCaptureHops = []struct{ env, extraVar, helper string }{
	{"PROBATORIUM_REFAPP_FAULT", "probatorium_refapp_fault", "refappFaultExtraVars"},
	{"PROBATORIUM_STALL_CAPTURE", "probatorium_stall_capture", "stallCaptureExtraVars"},
}

func readRepoFile(t *testing.T, p string) string {
	t.Helper()
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("read %s: %v", p, err)
	}
	return string(b)
}

func TestStallCaptureKnobsReachTheClusterValidator(t *testing.T) {
	mage := readRepoFile(t, "mage_validate.go")
	playbook := readRepoFile(t, "ansible/validate.yml")
	validator := readRepoFile(t, "validation/liveness.go")
	for _, h := range stallCaptureHops {
		// mage -> ansible: the playbook argv carries the extra-var. A text
		// assertion, because the failure it guards is a missing call site.
		if !strings.Contains(mage, "append(args, "+h.helper+"(os.Getenv)...)") {
			t.Errorf("runValidatePlaybook does not add %s's extra-var (%s) to the ansible argv: %s stops at the mage process",
				h.env, h.helper, h.env)
		}
		// ansible -> the validator's environment, under the name it reads.
		if !strings.Contains(playbook, h.env+`: "{{ `+h.extraVar+` | default('') }}"`) {
			t.Errorf("ansible/validate.yml does not export %s from extra-var %s: the validator on the cluster host never sees it",
				h.env, h.extraVar)
		}
		// and the validator reads that spelling.
		if !strings.Contains(validator, `"`+h.env+`"`) {
			t.Errorf("validation/liveness.go does not read %s; the chain ends one hop short", h.env)
		}
	}
}

// stallCaptureTiers are the cluster tiers that run the validator, and the
// step that runs it.
var stallCaptureTiers = map[string]string{
	".github/workflows/matrix-nightly-tier.yml":  "mage Validate",
	".github/workflows/matrix-weekend-tier.yml":  "mage Soak",
	".github/workflows/matrix-checkptr-tier.yml": "mage Validate",
	".github/workflows/matrix-race-tier.yml":     "mage Validate",
}

// dispatchInput returns the block of workflow_dispatch input name: its key
// line and every deeper-indented line after it; "" when absent.
func dispatchInput(src, name string) string {
	m := regexp.MustCompile(`(?m)^      ` + regexp.QuoteMeta(name) + `:\n((?:        .*\n|\s*\n)*)`).FindStringSubmatch(src)
	if m == nil {
		return ""
	}
	return m[1]
}

// stepRunning returns the step block whose run line is exactly run.
func stepRunning(src, run string) string {
	idx := stepSplit.FindAllStringIndex(src, -1)
	for i, loc := range idx {
		end := len(src)
		if i+1 < len(idx) {
			end = idx[i+1][0]
		}
		block := src[loc[0]:end]
		if strings.Contains(block, "        run: "+run+"\n") {
			return block
		}
	}
	return ""
}

// Every cluster tier that runs the validator can be DISPATCHED with the
// stall capture on (the release soak is the one celeris#588 needs), and it
// is off unless the dispatch asks: a boolean input defaulting to false, so
// a scheduled run -- which has no inputs -- leaves the variable empty.
func TestEveryClusterTierCanDispatchTheStallCapture(t *testing.T) {
	const wantEnv = `PROBATORIUM_STALL_CAPTURE: ${{ inputs.stall_capture && '1' || '' }}`
	for wf, run := range stallCaptureTiers {
		src := readRepoFile(t, wf)
		in := dispatchInput(src, "stall_capture")
		if in == "" {
			t.Errorf("%s: no workflow_dispatch input stall_capture; the tier cannot turn the capture on without injecting a fault", wf)
		} else {
			if !strings.Contains(in, "type: boolean") {
				t.Errorf("%s: stall_capture is not a boolean input:\n%s", wf, in)
			}
			if !strings.Contains(in, "default: false") {
				t.Errorf("%s: stall_capture does not default to false; a routine dispatch would take the capture:\n%s", wf, in)
			}
		}
		step := stepRunning(src, run)
		if step == "" {
			t.Fatalf("%s: no step runs %q; the parser no longer matches the workflow", wf, run)
		}
		if !strings.Contains(step, wantEnv) {
			t.Errorf("%s: the %q step does not set %s", wf, run, wantEnv)
		}
	}
}
