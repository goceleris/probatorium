package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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

// writePartial persists the cells completed so far, after every cell.
//
// The document is written to the same path the final write uses, so there is
// exactly one artefact and every reader already understands it. The only
// difference from the end-of-run document is that FinishedAt keeps moving and
// the run-wide property totals are recomputed each time -- both cheap for a
// 24-48 cell matrix, and the alternative (a second, partial-only file) would
// give the gate two sources of truth to disagree about.
func writePartial(cfg Config, startedAt time.Time, cells []report.ValidationCellResult) error {
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
	doc := report.Document{
		SchemaVersion: report.SchemaVersion,
		HostArchPair:  cfg.Target + "-" + cfg.Arch,
		Validation: &report.ValidationResults{
			StartedAt:        startedAt,
			FinishedAt:       time.Now().UTC(),
			PropertiesPassed: passed,
			PropertiesFailed: failed,
			FailureSummaries: summaries,
			Cells:            cells,
		},
	}
	return writeJSON(filepath.Join(cfg.OutDir, "validate-results.json"), doc)
}

// completedMatrixCells reads a previous run's validate-results.json and
// returns the "<refapp>/<engine>" pairs that already produced a verdict.
//
// A cell counts only if it actually ran: RequestsSent > 0, or it recorded a
// property verdict. A cell present but empty is what an interrupted run
// leaves behind, and skipping those is how a resume would quietly hand the
// gate a matrix with holes in it.
func completedMatrixCells(dir string) (map[string]struct{}, error) {
	done := make(map[string]struct{})
	b, err := os.ReadFile(filepath.Join(dir, "validate-results.json"))
	if err != nil {
		return nil, err
	}
	var doc report.Document
	if err := json.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("parse validate-results.json: %w", err)
	}
	if doc.Validation == nil {
		return done, nil
	}
	for _, c := range doc.Validation.Cells {
		if !matrixCellIsFinal(c) {
			continue
		}
		done[c.Refapp+"/"+c.Engine] = struct{}{}
	}
	return done, nil
}

// matrixCellIsFinal reports whether a recorded cell represents work that need
// not be repeated. Deliberately conservative: re-running a cell costs one
// per-cell budget, while wrongly skipping one leaves the absolute gate
// judging a matrix that is quietly short a cell.
func matrixCellIsFinal(c report.ValidationCellResult) bool {
	if c.Tier1 != nil && c.Tier1.RequestsSent > 0 {
		return true
	}
	return c.PropertiesPassed+c.PropertiesFailed > 0
}

// dropCompletedMatrixCells filters plan down to the cells not already final
// in dir. A missing or unreadable previous document is not an error -- that
// is the ordinary "nothing was rescued" case, and refusing to start would be
// worse than running the full matrix.
func dropCompletedMatrixCells(plan []matrixCell, dir string) ([]matrixCell, int) {
	done, err := completedMatrixCells(dir)
	if err != nil {
		return plan, 0
	}
	out := plan[:0:0]
	skipped := 0
	for _, mc := range plan {
		if _, ok := done[mc.Refapp+"/"+mc.Engine]; ok {
			skipped++
			continue
		}
		out = append(out, mc)
	}
	return out, skipped
}
