package main

import (
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

// Without VALIDATE_DURATION the sampler's lifetime defaults to 24 h plus the
// margin (samplerDurationSeconds), and it would run on into the next tier.
// The tag is what cuts a series that outlives a run into the run that made it.
func TestRaceSamplerIsSizedAndTaggedForThisRun(t *testing.T) {
	blk := stepRunning(mustRead(t, raceTier), "mage HostSamplerStart")
	if !strings.Contains(blk, "VALIDATE_DURATION: ${{ inputs.duration || '80m' }}") {
		t.Errorf("the start step does not size the sampler to the run's duration:\n%s", blk)
	}
	if !strings.Contains(blk, "SAMPLER_TAG: race-${{ github.run_id }}") {
		t.Errorf("the start step does not tag the series with this run:\n%s", blk)
	}
	if strings.Contains(blk, "weekend-") {
		t.Errorf("the race sampler carries the weekend's tag:\n%s", blk)
	}
}
