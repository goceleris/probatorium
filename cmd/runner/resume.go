package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"

	"github.com/goceleris/probatorium/interleave"
	"github.com/goceleris/probatorium/report"
)

// cellIsFinal reports whether a per-cell status is a verdict worth keeping,
// so a resumed run should not spend a measurement window reproducing it.
//
// The distinction that matters is dnf. markCellsInterrupted writes a per-cell
// JSON for every cell a drain or a signal cut short, classified dnf -- so
// "the file exists" is exactly the WRONG completeness test: it would skip
// precisely the cells the resume exists to run. dnf means the cell never
// produced a measurement (dial, port, crash, timeout, interrupted), and it
// must be re-run.
//
// not_applicable is final in the other direction: the adapter genuinely does
// not serve that route or protocol, and another window will not change it.
// suspect kept its samples -- the flag is about integrity, not absence. The
// empty string is legacy-OK, which every reader already treats as OK.
func cellIsFinal(status string) bool {
	switch report.CellStatus(status) {
	case report.CellDNF:
		return false
	case report.CellOK, report.CellSuspect, report.CellNotApplicable, "":
		return true
	default:
		// An unrecognised status is treated as NOT final. Re-running a cell
		// costs a measurement window; wrongly skipping one puts a hole in
		// the matrix that the gate reads as a missing cell.
		return false
	}
}

// completedCells scans a results tree and returns the cells that already
// reached a final verdict, keyed "<runIdx>/<scenario>/<server>".
//
// The layout mirrors executeCell: <dir>/run<N>/<scenario>/<server>.json.
// A file that cannot be read or parsed is treated as NOT final -- a resume
// that silently skipped an unreadable cell would quietly shrink the matrix.
func completedCells(dir string) (map[string]struct{}, error) {
	done := make(map[string]struct{})
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	for _, runDir := range entries {
		if !runDir.IsDir() {
			continue
		}
		var runIdx int
		if _, err := fmt.Sscanf(runDir.Name(), "run%d", &runIdx); err != nil {
			continue
		}
		scPath := filepath.Join(dir, runDir.Name())
		scenarios, err := os.ReadDir(scPath)
		if err != nil {
			continue
		}
		for _, sc := range scenarios {
			if !sc.IsDir() {
				continue
			}
			files, err := os.ReadDir(filepath.Join(scPath, sc.Name()))
			if err != nil {
				continue
			}
			for _, f := range files {
				if f.IsDir() || filepath.Ext(f.Name()) != ".json" {
					continue
				}
				b, err := os.ReadFile(filepath.Join(scPath, sc.Name(), f.Name()))
				if err != nil {
					continue
				}
				var cr cellResultFile
				if err := json.Unmarshal(b, &cr); err != nil {
					continue
				}
				if !cellIsFinal(cr.Status) {
					continue
				}
				done[cellKey(cr.RunIdx, cr.ScenarioName, cr.ServerName)] = struct{}{}
			}
		}
	}
	return done, nil
}

// cellKey identifies one scheduled cell. Run index is part of the key
// because the matrix is interleaved: the same scenario/server pair recurs
// once per run, and each occurrence is a separate measurement.
func cellKey(runIdx int, scenario, server string) string {
	return fmt.Sprintf("%d/%s/%s", runIdx, scenario, server)
}

// dropCompletedCells removes from schedule every cell that already reached a
// final verdict in dir, preserving the interleaved order of the remainder.
//
// Intended usage is IN PLACE -- pass the same path as -out -- so results
// accumulate into one directory across however many interruptions it takes.
// Resuming into a fresh -out works, but then no single directory holds the
// whole matrix, and a third interruption leaves three partial trees with no
// source of truth.
//
// A missing directory is not an error: it is the ordinary "nothing was
// rescued" case, and a resume that refused to start because there was
// nothing to resume from would be worse than simply running the full matrix.
func dropCompletedCells(schedule []interleave.Cell, dir string) ([]interleave.Cell, error) {
	done, err := completedCells(dir)
	if err != nil {
		if os.IsNotExist(err) {
			return schedule, nil
		}
		return nil, err
	}
	out := schedule[:0:0]
	for _, c := range schedule {
		if _, ok := done[cellKey(c.RunIdx, c.Scenario.Name(), c.Server.Name())]; ok {
			continue
		}
		out = append(out, c)
	}
	return out, nil
}
