package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/goceleris/probatorium/report"
)

// drainFile is the sentinel ops/power/cluster-power-event writes when apcupsd
// reports mains loss. It lives on tmpfs (/run) so a stale one cannot survive a
// reboot and silently drain the next run at its first cell.
func drainFile() string {
	if p := os.Getenv("PROBATORIUM_DRAIN_FILE"); p != "" {
		return p
	}
	return "/run/celeris/drain-requested"
}

// drainRequested reports whether a graceful stop has been asked for. Anything
// other than "the file is there" means no drain: a validator must never
// abandon a 24 h soak because /run was briefly unreadable.
func drainRequested() bool {
	_, err := os.Stat(drainFile())
	return err == nil
}

// resumeMerge is an interrupted run's contribution to this run's document.
//
// A resumed run has to produce a WHOLE-soak verdict or it produces nothing
// useful: the weekend tier gates on VALIDATE_GATE_EXPECT_CELLS=64, and until
// probatorium#376 a resume filtered the plan and never merged the results, so
// a perfect resume of the last ten cells handed the gate a ten-cell document
// and failed it. Merging means the document asserts cells this process did
// not measure, which is exactly the kind of claim that has to be auditable --
// hence Inherited carrying ResumedFrom per cell and [report.ResumeProvenance]
// carrying the split.
type resumeMerge struct {
	// From is the resume directory, verbatim as passed on the command line.
	From string
	// Inherited are the prior run's final cells, in plan order, each already
	// stamped with ResumedFrom.
	Inherited []report.ValidationCellResult
	// NotInherited explains every prior entry that was deliberately left
	// behind: "<refapp>/<engine>: <why>".
	NotInherited []string
	// PriorStartedAt / PriorFinishedAt are the earlier run's own window.
	PriorStartedAt  time.Time
	PriorFinishedAt time.Time
}

// provenance renders the run-level record. ran is the number of cells THIS
// run measured, so InheritedCells+RanCells is the document's cell count and a
// reader can check it against len(cells) without trusting either number.
func (r *resumeMerge) provenance(ran int) *report.ResumeProvenance {
	if r == nil {
		return nil
	}
	return &report.ResumeProvenance{
		From:            r.From,
		PriorStartedAt:  r.PriorStartedAt,
		PriorFinishedAt: r.PriorFinishedAt,
		InheritedCells:  len(r.Inherited),
		RanCells:        ran,
		NotInherited:    r.NotInherited,
	}
}

// cells returns the inherited cells, or nil for a non-resume run. Safe on a
// nil receiver so the caller does not branch.
func (r *resumeMerge) cells() []report.ValidationCellResult {
	if r == nil {
		return nil
	}
	return r.Inherited
}

// loadResumeDoc reads the interrupted run's document.
//
// Every failure here is LOUD, and that reversal is the point of
// probatorium#376. The previous behaviour -- a missing or unparseable
// document read as "nothing was rescued", plan returned intact -- turns three
// separate accidents into the same silent outcome: the teardown had already
// purged the results, the `auto` locator selected the empty directory it had
// just created, or the path simply does not exist on THIS host of a two-arch
// run. In all three the run then re-runs the full matrix while printing
// "resume: 0 of 64 cells already final" and, hours later, reports success.
// An operator who asked to resume a lost 20 h soak gets no signal at all that
// the resume did not happen.
func loadResumeDoc(dir string) (*report.Document, error) {
	path := filepath.Join(dir, "validate-results.json")
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w (nothing to resume from: the directory may have been "+
			"purged by the teardown, or may be the empty one this run just created)", path, err)
	}
	var doc report.Document
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse %s (%d bytes): %w (a document truncated by the kill that "+
			"ended the previous run cannot be resumed from -- it is not known what it recorded)",
			path, len(b), err)
	}
	if doc.Validation == nil {
		return nil, fmt.Errorf("%s has no validation block: it is not a validate run's document", path)
	}
	return &doc, nil
}

// matrixCellIsFinal reports whether a recorded cell represents work that need
// not be repeated. Deliberately conservative: re-running a cell costs one
// per-cell budget, while wrongly skipping one leaves the absolute gate
// judging a matrix that is quietly short a cell -- and, since #376, would
// also make the merged document inherit a cell that never produced a verdict
// as though it had passed.
func matrixCellIsFinal(c report.ValidationCellResult) bool {
	if c.Tier1 != nil && c.Tier1.RequestsSent > 0 {
		return true
	}
	return c.PropertiesPassed+c.PropertiesFailed > 0
}

// planResume splits plan against the interrupted run's document at dir: the
// cells already final there become the merge's Inherited set, the rest stay
// in the returned plan.
//
// arch is this run's GOARCH. A prior cell recorded on a different one is a
// hard error rather than a skip: `resume_from` is a single string handed to
// BOTH hosts of a two-arch run, so pointing an amd64 host at msr1's directory
// is a thing an operator can do by accident, and inheriting arm64 verdicts
// into an amd64 document would put them in front of ValidateDiff's arch-parity
// gate as though the parity had been measured.
func planResume(plan []matrixCell, dir, arch string) ([]matrixCell, *resumeMerge, error) {
	doc, err := loadResumeDoc(dir)
	if err != nil {
		return nil, nil, err
	}
	wanted := make(map[string]int, len(plan))
	for i, mc := range plan {
		wanted[mc.Refapp+"/"+mc.Engine] = i
	}

	merge := &resumeMerge{
		From:            dir,
		PriorStartedAt:  doc.Validation.StartedAt,
		PriorFinishedAt: doc.Validation.FinishedAt,
	}
	// Keyed by plan index so the inherited cells come back in plan order and
	// a prior document that lists the same cell twice contributes it ONCE.
	// The gate counts entries; a merge that can double-count is a merge that
	// can reach 64 without 64 cells having run.
	byPlanIdx := map[int]report.ValidationCellResult{}
	for _, c := range doc.Validation.Cells {
		key := c.Refapp + "/" + c.Engine
		if arch != "" && c.Arch != "" && c.Arch != arch {
			return nil, nil, fmt.Errorf("cell %s in %s was measured on arch %q, but this run is %q; "+
				"a resume directory belongs to one host and must not be handed to the other",
				key, filepath.Join(dir, "validate-results.json"), c.Arch, arch)
		}
		idx, inPlan := wanted[key]
		switch {
		case !matrixCellIsFinal(c):
			// The cell an interrupted run leaves behind: recorded, but with no
			// requests and no property verdict. It is re-run, never inherited.
			merge.NotInherited = append(merge.NotInherited,
				key+": recorded with no requests and no property verdict (mid-flight when the "+
					"previous run ended) -- re-run, not inherited")
		case !inPlan:
			merge.NotInherited = append(merge.NotInherited,
				key+": final in the previous run but not in this run's plan -- dropped, not inherited")
		case containsPlanIdx(byPlanIdx, idx):
			merge.NotInherited = append(merge.NotInherited,
				key+": listed more than once in the previous document -- inherited once")
		default:
			c.ResumedFrom = dir
			byPlanIdx[idx] = c
		}
	}

	idxs := make([]int, 0, len(byPlanIdx))
	for i := range byPlanIdx {
		idxs = append(idxs, i)
	}
	sort.Ints(idxs)
	for _, i := range idxs {
		merge.Inherited = append(merge.Inherited, byPlanIdx[i])
	}

	remaining := plan[:0:0]
	for i, mc := range plan {
		if _, ok := byPlanIdx[i]; ok {
			continue
		}
		remaining = append(remaining, mc)
	}
	sort.Strings(merge.NotInherited)
	return remaining, merge, nil
}

// containsPlanIdx keeps the duplicate branch above readable.
func containsPlanIdx(m map[int]report.ValidationCellResult, i int) bool {
	_, ok := m[i]
	return ok
}

// buildMatrixDoc assembles the document every matrix write emits -- the
// after-every-cell partial and the end-of-run final are the same artefact, so
// there is exactly one shape for a reader to understand and exactly one place
// the resume provenance can be forgotten.
//
// cells is the UNION: the inherited cells first, then the ones this run
// measured. ran is derived rather than passed so the provenance split cannot
// drift from the slice it describes.
func buildMatrixDoc(cfg Config, startedAt time.Time, cells []report.ValidationCellResult,
	resume *resumeMerge,
) report.Document {
	var passed, failed int
	var summaries map[string]string
	for _, c := range cells {
		passed += c.PropertiesPassed
		failed += c.PropertiesFailed
		for id, msg := range c.FailureSummaries {
			if summaries == nil {
				summaries = map[string]string{}
			}
			summaries[c.Refapp+"/"+c.Engine+"/"+id] = msg
		}
	}
	ran := len(cells) - len(resume.cells())
	return report.Document{
		SchemaVersion: report.SchemaVersion,
		HostArchPair:  cfg.Target + "-" + cfg.Arch,
		Validation: &report.ValidationResults{
			StartedAt:        startedAt,
			FinishedAt:       time.Now().UTC(),
			PropertiesPassed: passed,
			PropertiesFailed: failed,
			FailureSummaries: summaries,
			Cells:            cells,
			Resume:           resume.provenance(ran),
		},
	}
}

// writePartial persists the cells completed so far, after every cell.
//
// The document is written to the same path the final write uses, so there is
// exactly one artefact and every reader already understands it. The only
// difference from the end-of-run document is that FinishedAt keeps moving and
// the run-wide property totals are recomputed each time -- both cheap for a
// 24-48 cell matrix, and the alternative (a second, partial-only file) would
// give the gate two sources of truth to disagree about.
//
// On a resumed run the partial is ALSO the merged document from the first
// cell onwards, which is what makes a resume of a resume work: whatever kills
// this run leaves behind a document carrying both the inherited cells and the
// ones measured so far.
func writePartial(cfg Config, startedAt time.Time, cells []report.ValidationCellResult,
	resume *resumeMerge,
) error {
	return writeJSON(filepath.Join(cfg.OutDir, "validate-results.json"),
		buildMatrixDoc(cfg, startedAt, cells, resume))
}
