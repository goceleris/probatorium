//go:build mage

package main

import (
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// TestPublishIntegrityCountsRatedPasses: the integrity summary mage Publish
// prints says how many rated passes were lost or under-delivered and names the
// cells whose headline is not their top planned pass. Informational (no
// violation): the validity rule is still open on #441.
func TestPublishIntegrityCountsRatedPasses(t *testing.T) {
	ok := func(f, target, ratio float64) report.RatedPassResult {
		return report.RatedPassResult{RatedPassRecord: report.RatedPassRecord{Fraction: f, TargetRPS: target,
			P99: time.Millisecond, Status: report.RatedPassOK, AchievedRPS: ratio * target, AchievedRatio: ratio}}
	}
	lost := func(f, target float64, st string) report.RatedPassResult {
		return report.RatedPassResult{RatedPassRecord: report.RatedPassRecord{Fraction: f, TargetRPS: target, Status: st}}
	}
	doc := &report.Document{Benchmarks: []report.ServerResult{{
		Name:                   "celeris-epoll-h1-sync",
		SaturationModeRPS:      map[string]float64{"get-json": 400000, "driver-pg-write": 100000},
		RatedHeadlineTargetRPS: map[string]int{"get-json": 200000, "driver-pg-write": 90000},
		RatedPasses: map[string][]report.RatedPassResult{
			// the 0.9 pass was lost: headline is the 0.5 pass's
			"get-json": {ok(0.25, 100000, 1.0), ok(0.5, 200000, 0.9), lost(0.9, 360000, report.RatedPassSkipped)},
			// all four kept, one of them short
			"driver-pg-write": {ok(0.25, 25000, 1.0), ok(0.5, 50000, 1.0), ok(0.75, 75000, 1.0), ok(0.9, 90000, 0.97)},
		},
	}}}
	pi := collectPublishIntegrity(t.TempDir(), doc)
	if pi.RatedPasses["ok"] != 6 || pi.RatedPasses["skipped"] != 1 || pi.RatedPasses["not_run"] != 0 {
		t.Errorf("RatedPasses = %v, want ok=6 skipped=1", pi.RatedPasses)
	}
	// 0.9 achieved 90% and 0.97: two kept passes under 98%.
	if pi.RatedBelowTarget != 2 {
		t.Errorf("RatedBelowTarget = %d, want 2", pi.RatedBelowTarget)
	}
	if len(pi.RatedHeadlineLow) != 1 || pi.RatedHeadlineLow[0] != "celeris-epoll-h1-sync/get-json" {
		t.Errorf("RatedHeadlineLow = %v, want only get-json", pi.RatedHeadlineLow)
	}
	if v := pi.violations(); len(v) != 0 {
		t.Errorf("lost rated passes must not refuse the publish (informational): %v", v)
	}
	out := renderPublishIntegrity(pi)
	for _, want := range []string{"rated passes: 7 total", "skipped=1", "below 98% of target=2", "RATED HEADLINE  celeris-epoll-h1-sync/get-json"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}

// TestPublishIntegrityIsQuietWithoutRatedPasses: a run with no rated record
// (rated mode off, or a pre-5.24 document) prints no rated line.
func TestPublishIntegrityIsQuietWithoutRatedPasses(t *testing.T) {
	doc := &report.Document{Benchmarks: []report.ServerResult{{Name: "x", SaturationModeRPS: map[string]float64{"get-json": 1}}}}
	if out := renderPublishIntegrity(collectPublishIntegrity(t.TempDir(), doc)); strings.Contains(out, "rated") {
		t.Errorf("a run without rated passes prints a rated line:\n%s", out)
	}
}

// TestPublishIntegrityDoesNotCallALegacyRunClean: a document merged from runs
// that predate 5.24 (a re-merge of bench 38431303162) has records without a
// status. Skipped is 0 there because the old runner never recorded a dropped
// pass, so the census counts them as legacy and says so, instead of as ok.
// An ok pass cut short by a cancelled run is counted too.
func TestPublishIntegrityDoesNotCallALegacyRunClean(t *testing.T) {
	legacy := report.RatedPassResult{RatedPassRecord: report.RatedPassRecord{TargetRPS: 100000, P99: time.Millisecond}}
	cut := report.RatedPassResult{RatedPassRecord: report.RatedPassRecord{Fraction: 0.5, TargetRPS: 200000, P99: time.Millisecond,
		Status: report.RatedPassOK, Reason: report.RatedReasonInterrupted, AchievedRatio: 1}}
	doc := &report.Document{Benchmarks: []report.ServerResult{{
		Name: "gin-h1", SaturationModeRPS: map[string]float64{"get-json": 1, "post-4k": 1},
		RatedPasses: map[string][]report.RatedPassResult{"get-json": {legacy, legacy}, "post-4k": {cut}},
	}}}
	pi := collectPublishIntegrity(t.TempDir(), doc)
	if pi.RatedLegacy != 2 || pi.RatedPasses["ok"] != 1 || pi.RatedInterruptedKept != 1 {
		t.Errorf("legacy %d ok %d interrupted-kept %d, want 2/1/1", pi.RatedLegacy, pi.RatedPasses["ok"], pi.RatedInterruptedKept)
	}
	out := renderPublishIntegrity(pi)
	for _, want := range []string{"legacy=2", "ok but interrupted=1", "RATED LEGACY"} {
		if !strings.Contains(out, want) {
			t.Errorf("summary lacks %q:\n%s", want, out)
		}
	}
}
