package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// The cluster is reserved for the v1.6.0 release sequence. Every tier shares
// the matrix-tier-cluster concurrency group, which keeps only ONE pending run,
// so a scheduled run does not merely add load: it can evict a queued release
// run or push one behind a 53-to-69-hour benchmark.
//
// Two things have to hold at once, and they pull in opposite directions:
// no cron may fire, and workflow_dispatch must still work, because the
// release sequence drives every one of these tiers by hand. Disabling the
// workflows outright would satisfy the first and break the second.
//
// Delete this test when the schedules are restored. It is meant to fail then.
func TestClusterSchedulesArePausedButDispatchStillWorks(t *testing.T) {
	tiers := []string{
		"matrix-nightly-tier.yml",
		"matrix-weekend-tier.yml",
		"benchmark-tier.yml",
	}
	// A cron line that is live: "    - cron:" with no # before it.
	liveCron := regexp.MustCompile(`(?m)^\s*-\s*cron:`)
	liveSchedule := regexp.MustCompile(`(?m)^\s{2}schedule:`)

	for _, name := range tiers {
		b, err := os.ReadFile(filepath.Join(".github", "workflows", name))
		if err != nil {
			t.Errorf("%s: %v", name, err)
			continue
		}
		src := string(b)
		if liveCron.MatchString(src) || liveSchedule.MatchString(src) {
			t.Errorf("%s still has a live schedule: a cron run can evict a queued "+
				"release run from the matrix-tier-cluster group, which keeps only one pending", name)
		}
		if !strings.Contains(src, "workflow_dispatch:") {
			t.Errorf("%s lost workflow_dispatch: the release sequence drives this tier by hand, "+
				"so pausing the cron must not also take the manual trigger away", name)
		}
	}
}
