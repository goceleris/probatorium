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

// TestCommentedScheduleWarnsThatRestoringItStartsAFullRun: a scheduled run has no
// inputs, so the profile falls back to `full` and the arch policy to `all`
// (about 102h every week), not the headline the cron's own comment describes.
func TestCommentedScheduleWarnsThatRestoringItStartsAFullRun(t *testing.T) {
	b, err := os.ReadFile(filepath.Join(".github", "workflows", "benchmark-tier.yml"))
	if err != nil {
		t.Fatal(err)
	}
	i := strings.Index(string(b), "#schedule:")
	if i < 0 {
		t.Skip("the schedule is restored: delete this test")
	}
	head := string(b)[:i]
	if !strings.Contains(head, "no inputs") || !strings.Contains(head, "`full`") || !strings.Contains(head, "~102h") {
		t.Errorf("the note above the commented-out schedule does not say that restoring it starts a ~102h full run:\n%s", head[max(0, len(head)-700):])
	}
}
