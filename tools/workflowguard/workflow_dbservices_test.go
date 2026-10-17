package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Every tier that drives the matrix runs the driver_* refapps, and those
// need postgres, redis and memcached. Both halves are required: the deploy
// half pulls the images, the validate half starts the containers and passes
// the validator's -seed-services flag. Every dbservices task is gated on
// VALIDATE_DBSERVICES, so a tier missing it does not fail — it SKIPS, and
// its driver cells report "no requests and no property verdicts".
//
// The PR tier was the only tier setting neither, and nothing said so. Twelve
// of its thirty-two cells could never run, from the day the gate was written
// (probatorium#372). A tier that silently cannot exercise a third of its
// matrix is the same defect as a gate whose label does not exist: the check
// reports on less than it appears to, and only a count reveals it.
func TestEveryMatrixTierProvisionsTheDriverServices(t *testing.T) {
	// Tiers that drive the matrix. A new one must be added here, which is
	// the point: the decision to skip the driver refapps should be explicit.
	tiers := []string{
		"matrix-pr-tier.yml",
		"matrix-nightly-tier.yml",
		"matrix-weekend-tier.yml",
		"matrix-race-tier.yml",
		"matrix-checkptr-tier.yml",
	}
	for _, name := range tiers {
		path := filepath.Join(".github", "workflows", name)
		b, err := os.ReadFile(path)
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		src := string(b)
		if !strings.Contains(src, "VALIDATE_MATRIX") {
			continue // not a matrix-driving tier after all
		}
		for _, key := range []string{"DEPLOY_NEEDS_DBSERVICES", "VALIDATE_DBSERVICES"} {
			re := regexp.MustCompile(`(?m)^\s*` + key + `:\s*"?1"?\s*$`)
			if !re.MatchString(src) {
				t.Errorf("%s drives the matrix but does not set %s to 1: its driver_* cells "+
					"will SKIP their services and report no requests and no property verdicts, "+
					"without the tier failing on it (probatorium#372)", name, key)
			}
		}
	}
}

// The gating is what makes the omission silent, so pin that too: if the
// dbservices tasks ever stop being gated on VALIDATE_DBSERVICES, the test
// above is guarding a variable nobody reads.
func TestDbservicesTasksAreGatedOnTheVariableTheTiersSet(t *testing.T) {
	b, err := os.ReadFile(filepath.Join("ansible", "validate.yml"))
	if err != nil {
		t.Skipf("validate.yml not readable: %v", err)
	}
	if !strings.Contains(string(b), "validate_dbservices") {
		t.Error("ansible/validate.yml no longer mentions validate_dbservices: " +
			"the workflow variables asserted above may now be read by nothing")
	}
}
