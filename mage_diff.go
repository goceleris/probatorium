//go:build mage

package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/goceleris/probatorium/report"
)

// ValidateDiff compares the two most recent validate-results.json
// documents in the local results/ directory and reports any
// cross-arch divergences (counters that should be zero on every
// arch but came back zero on one side and non-zero on the other).
//
// Typical use: after `mage Validate BENCH_TARGET=both` finishes,
// run `mage ValidateDiff` to see whether amd64 + arm64 agreed on
// the predicate-violation invariants.
//
// Returns a non-nil error when any HIGH severity divergence is
// found — CI uses this as a hard gate. MED / LOW divergences are
// reported but don't fail the target; they get re-examined as
// trajectory in soak runs.
//
// Env knobs:
//
//	VALIDATE_DIFF_STRICT=1      treat MED divergences as failure too
//	VALIDATE_DIFF_HOSTS=a,b     override host labels (default: from
//	                            host_arch_pair field in each doc)
func ValidateDiff() error {
	paths, err := twoLatestValidateResults()
	if err != nil {
		return err
	}
	docA, err := loadValidateDoc(paths[0])
	if err != nil {
		return fmt.Errorf("load %s: %w", paths[0], err)
	}
	docB, err := loadValidateDoc(paths[1])
	if err != nil {
		return fmt.Errorf("load %s: %w", paths[1], err)
	}
	hostA, hostB := docA.HostArchPair, docB.HostArchPair
	if hostA == "" {
		hostA = filepath.Base(filepath.Dir(paths[0]))
	}
	if hostB == "" {
		hostB = filepath.Base(filepath.Dir(paths[1]))
	}
	if override := os.Getenv("VALIDATE_DIFF_HOSTS"); override != "" {
		parts := strings.SplitN(override, ",", 2)
		if len(parts) == 2 {
			hostA, hostB = parts[0], parts[1]
		}
	}

	divs := report.DiffValidation(docA.Validation, docB.Validation, hostA, hostB)
	textReport := report.FormatDivergences(divs, hostA, hostB)
	fmt.Println(textReport)

	// Persist the diff alongside the validate-results.json files so a
	// CI workflow can upload it as an artifact and a postmortem reader
	// has the divergence list without having to re-run the diff.
	// Best-effort: a write failure shouldn't mask a real divergence.
	if err := persistDivergenceReport(paths, divs, textReport, hostA, hostB); err != nil {
		fmt.Fprintf(os.Stderr, "warning: persist diff report: %v\n", err)
	}

	strict := os.Getenv("VALIDATE_DIFF_STRICT") == "1"
	for _, d := range divs {
		if d.Severity == report.SeverityHigh {
			return fmt.Errorf("HIGH severity cross-arch divergence: %s/%s", d.Slice, d.Counter)
		}
		if strict && d.Severity == report.SeverityMed {
			return fmt.Errorf("MED severity cross-arch divergence (strict mode): %s/%s", d.Slice, d.Counter)
		}
	}
	return nil
}

// persistDivergenceReport writes the divergence findings to a
// validate-diff/ subdir under the SHARED parent of the two compared
// validate-results.json paths. When both paths are siblings under the
// same results/<ts>-validate-<ver>/ run (the production case), this
// lands the diff inside the same run dir so postmortem readers see
// it alongside the inputs. When the paths diverge (manual re-run
// against an older run), the report lands under results/.
//
// Both diff.txt and diff.json are written so dashboards can consume
// either form.
func persistDivergenceReport(srcPaths []string, divs []report.Divergence,
	textReport, hostA, hostB string,
) error {
	parent := commonResultsParent(srcPaths)
	if parent == "" {
		parent = "results"
	}
	outDir := filepath.Join(parent, "validate-diff")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(outDir, "diff.txt"),
		[]byte(textReport+"\n"), 0o644); err != nil {
		return err
	}
	payload := map[string]any{
		"host_a":       hostA,
		"host_b":       hostB,
		"source_a":     srcPaths[0],
		"source_b":     srcPaths[1],
		"divergences":  divs,
		"divergence_n": len(divs),
	}
	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(filepath.Join(outDir, "diff.json"), data, 0o644)
}

// commonResultsParent returns the shallowest common ancestor of two
// paths that's under results/ — typically results/<ts>-validate-<ver>/
// in production runs where both inputs live in the same run.
//
// Returns "" if the paths don't share a results/<run>/ parent.
func commonResultsParent(paths []string) string {
	if len(paths) < 2 {
		return ""
	}
	a := filepath.Dir(filepath.Dir(paths[0])) // strip the <host-refapp>/<file> tail
	b := filepath.Dir(filepath.Dir(paths[1]))
	if a == b {
		return a
	}
	return ""
}

// twoLatestValidateResults walks results/ and returns the two most
// recent validate-results.json paths (sorted by mtime descending).
// Returns an error if fewer than two are present — a diff needs
// two sides.
func twoLatestValidateResults() ([]string, error) {
	entries, err := os.ReadDir("results")
	if err != nil {
		return nil, err
	}
	type fileInfo struct {
		path  string
		mtime int64
	}
	var found []fileInfo
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runDir := filepath.Join("results", e.Name())
		// Validate runs have a nested layout (see latestValidateResults
		// in mage_bench.go); each host-refapp subdir holds its own
		// validate-results.json. We want them all as candidates so the
		// "two most recent" cross-host comparison picks one per host.
		subs, err := os.ReadDir(runDir)
		if err != nil {
			continue
		}
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			p := filepath.Join(runDir, sub.Name(), "validate-results.json")
			st, err := os.Stat(p)
			if err != nil {
				continue
			}
			found = append(found, fileInfo{path: p, mtime: st.ModTime().UnixNano()})
		}
	}
	if len(found) < 2 {
		return nil, fmt.Errorf("need at least two validate-results.json under results/ to diff, found %d", len(found))
	}
	sort.Slice(found, func(i, j int) bool {
		return found[i].mtime > found[j].mtime
	})
	return []string{found[0].path, found[1].path}, nil
}

// loadValidateDoc reads + parses a validate-results.json into a
// report.Document. Returns an error if the file is missing or the
// schema_version field isn't v5+ (older runs predate the v5 emit
// and aren't comparable).
func loadValidateDoc(path string) (*report.Document, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if doc.SchemaVersion == "" {
		return nil, fmt.Errorf("%s: missing schema_version (run is from pre-v5 emitter; cannot diff)", path)
	}
	if doc.Validation == nil {
		return nil, fmt.Errorf("%s: missing validation_results", path)
	}
	return &doc, nil
}
