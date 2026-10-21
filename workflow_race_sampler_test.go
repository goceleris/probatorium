package main

import (
	"regexp"
	"strconv"
	"strings"
	"testing"
)

// probatorium#481. The host sampler (probatorium#354) ran only in the weekend
// soak. A race-tier run started none, so the series on a cluster host and its
// current-run.txt were the last weekend's: race run 39560945814's forensics
// (collected 2026-10-21) read tag=weekend-38441116109, whose deadline was
// 2026-10-16 19:26Z. The sampler is read-only and self-terminating; the race
// tier now brackets its run with it like the weekend does.

func TestRaceTierBracketsTheRunWithTheSampler(t *testing.T) {
	src := mustRead(t, raceTier)
	start := mustIndex(t, src, "run: mage HostSamplerStart", "sampler start step")
	validate := mustIndex(t, src, "run: mage Validate\n", "mage Validate step")
	diff := mustIndex(t, src, "run: mage ValidateDiff", "mage ValidateDiff step")
	stop := mustIndex(t, src, "run: mage HostSamplerStop", "sampler stop step")
	upload := mustIndex(t, src, "name: Upload results", "results upload step")
	if start > validate {
		t.Errorf("the sampler starts after `mage Validate` (offsets %d > %d)", start, validate)
	}
	if stop < diff {
		t.Errorf("the sampler stops before the end of the run (offsets %d < %d)", stop, diff)
	}
	if stop > upload {
		t.Errorf("the sampler stops after the results upload (offsets %d > %d): its series would miss the artifact", stop, upload)
	}
}

// Instrumentation must not be able to fail a gate run, and the stop step must
// run on the path where the gate failed.
func TestRaceSamplerStepsCannotFailTheRun(t *testing.T) {
	src := mustRead(t, raceTier)
	for _, run := range []string{"mage HostSamplerStart", "mage HostSamplerStop"} {
		blk := stepRunning(src, run)
		if blk == "" {
			t.Fatalf("no step runs %q", run)
		}
		if !strings.Contains(blk, "continue-on-error: true") {
			t.Errorf("%s is not continue-on-error:\n%s", run, blk)
		}
	}
	if blk := stepRunning(src, "mage HostSamplerStop"); !strings.Contains(blk, "if: always()") {
		t.Errorf("the stop step is skipped when the gate fails:\n%s", blk)
	}
}

// The sampler's lifetime is explicit and ends inside the job. Left to
// VALIDATE_DURATION it is that plus a 2 h margin (mage_sampler.go), about
// 200 minutes for the default 80m run, longer than the matrix job's limit: a
// killed job would leave a sampler writing into the next tier. The tag cuts a
// series that outlives a run into the run that made it.
func TestRaceSamplerIsBoundedByTheJobAndTaggedForThisRun(t *testing.T) {
	src := mustRead(t, raceTier)
	blk := stepRunning(src, "mage HostSamplerStart")
	m := regexp.MustCompile(`SAMPLER_DURATION: (\d+)m\n`).FindStringSubmatch(blk)
	if m == nil {
		t.Fatalf("the start step does not set SAMPLER_DURATION in minutes:\n%s", blk)
	}
	sampler, _ := strconv.Atoi(m[1])
	job := raceJobTimeoutMinutes(t, src, "name: matrix validate (race tier)")
	if sampler+10 > job {
		t.Errorf("SAMPLER_DURATION %dm leaves under 10 minutes inside the job's %dm limit: a sampler started a few minutes into the job would outlive it", sampler, job)
	}
	if sampler < 90 {
		t.Errorf("SAMPLER_DURATION %dm does not cover the default 80m run plus its deploy", sampler)
	}
	if !strings.Contains(blk, "SAMPLER_TAG: race-${{ github.run_id }}") {
		t.Errorf("the start step does not tag the series with this run:\n%s", blk)
	}
	if strings.Contains(blk, "weekend-") {
		t.Errorf("the race sampler carries the weekend's tag:\n%s", blk)
	}
}

func raceJobTimeoutMinutes(t *testing.T, src, jobName string) int {
	t.Helper()
	i := strings.Index(src, jobName)
	if i < 0 {
		t.Fatalf("no job %q", jobName)
	}
	m := regexp.MustCompile(`timeout-minutes: (\d+)`).FindStringSubmatch(src[i:])
	if m == nil {
		t.Fatalf("job %q has no timeout-minutes", jobName)
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// A stuck ansible call must not hold the job: both steps are bounded.
func TestRaceSamplerStepsAreTimeBounded(t *testing.T) {
	src := mustRead(t, raceTier)
	for _, run := range []string{"mage HostSamplerStart", "mage HostSamplerStop"} {
		if blk := stepRunning(src, run); !strings.Contains(blk, "timeout-minutes: 5\n") {
			t.Errorf("%s has no timeout-minutes: 5:\n%s", run, blk)
		}
	}
}
