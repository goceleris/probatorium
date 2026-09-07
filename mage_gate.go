//go:build mage

package main

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"

	"github.com/goceleris/probatorium/report"
)

// ValidateGate applies the ABSOLUTE zero-signal gate to the most recent run's
// validate-results.json on every host.
//
// It fails on ANY nonzero true-signal counter in ANY cell: unexpected 5xx,
// transport errors, invariant hits, h2c hangs/crashes, adversarial
// wrong-accepts or hangs, WebSocket bad frames / hangs / handshake failures,
// SSE handshake failures / early server closes, tier-3 seed failures, dead
// cells, missing cells, a tier that never ran, and soak leak indicators.
//
// ValidateDiff is RELATIVE (cross-engine, cross-arch): a defect present on
// every engine agrees with itself and passes it. This gate exists because that
// happened -- nightlies ran green for weeks over real bugs (probatorium#274).
//
//	VALIDATE_GATE_EXPECT_CELLS=48        fail if fewer cells reported (0 = no check)
//	VALIDATE_GATE_REQUIRE_TIER3=1        fail a cell whose tier 3 never ran (default 1)
//	VALIDATE_GATE_REQUIRE_SOAK=1         fail a cell that carries no soak summary (soak runs)
//	VALIDATE_GATE_REQUIRE_PROPERTIES=1   fail a cell whose property loop never evaluated a
//	                                     predicate, i.e. /debug/vars was unreachable. Unset
//	                                     defaults to 1 when every document is schema >= 5.6
//	                                     (emitted by a validator that has the in-process
//	                                     loop) and to 0 for older results, which carry no
//	                                     property fields at all and would fail every cell.
//	                                     A cell whose tier_1.property_loop_skipped names a
//	                                     reason (ssh driver) is waived either way.
//	VALIDATE_GATE_REQUIRE_COVERAGE=1     fail the run for a predicate that reached no
//	                                     verdict in ANY cell while at least one cell was
//	                                     long enough to judge it. Unset defaults to 1 when
//	                                     every document is schema >= 5.8 (the first that
//	                                     records which silences were structural) and to 0
//	                                     for older results, whose short-cell oracles would
//	                                     all read as gaps.
//
// The not-judged set is PRINTED whatever the switches say, on PASS and on
// FAIL. A 150 s nightly cell cannot judge I-MEM-1, I-MEM-3 or I-MEM-4 -- the
// slope oracles need 5 min of warm-up plus a 10 min window -- so a nightly
// PASS carries no opinion at all on memory growth, and the header's
// require_properties=true reads as though the whole property suite had been
// satisfied (probatorium#299). That is worth saying out loud; it is not
// worth failing the nightly for, since the cells are short by design.
//
// Property predicate violations (tier_1.property_violations, naming the
// I-* IDs) are always gated, whether the run recorded them (the default,
// VALIDATE_PROPERTY_HARD_FAIL unset) or hard-failed the cell on them.
func ValidateGate() error {
	paths, err := latestRunValidateResults()
	if err != nil {
		return err
	}
	var cells []report.ValidationCellResult
	soaks := map[string]*report.SoakSummary{}
	// propertyAware: every document was emitted by a validator that runs
	// the in-process property loop (schema >= 5.6). Older documents have
	// no property fields, so their property_evaluations decode as 0 and
	// RequireProperties would fail every cell of a run that never had a
	// loop to begin with.
	//
	// coverageAware: every document also records WHICH not-judged
	// predicates the cell was structurally too short to judge (schema
	// >= 5.8). Without that list every short-cell oracle reads as a
	// coverage gap, so the check defaults off for older documents.
	propertyAware := true
	coverageAware := true
	for _, p := range paths {
		doc, err := loadValidateDoc(p)
		if err != nil {
			return fmt.Errorf("load %s: %w", p, err)
		}
		if !report.SchemaAtLeast(doc.SchemaVersion, "5.6") {
			propertyAware = false
		}
		if !report.SchemaAtLeast(doc.SchemaVersion, "5.8") {
			coverageAware = false
		}
		host := filepath.Base(filepath.Dir(p))
		cs := doc.Validation.Cells
		if len(cs) == 0 && (doc.Validation.Tier1 != nil || doc.Validation.Tier3 != nil) {
			// Single-cell (non-matrix) run: gate the top-level tallies as one cell.
			cs = []report.ValidationCellResult{{Refapp: "(single)", Engine: "(single)", Arch: host,
				Tier1: doc.Validation.Tier1, Tier3: doc.Validation.Tier3}}
		}
		cells = append(cells, cs...)
		if doc.Soak != nil {
			soaks[host] = doc.Soak
		}
	}
	requireProps := propertyAware
	switch os.Getenv("VALIDATE_GATE_REQUIRE_PROPERTIES") {
	case "1":
		requireProps = true
	case "0":
		requireProps = false
	}
	requireCoverage := coverageAware
	switch os.Getenv("VALIDATE_GATE_REQUIRE_COVERAGE") {
	case "1":
		requireCoverage = true
	case "0":
		requireCoverage = false
	}
	opts := report.GateOptions{
		ExpectedCells:     gateEnvInt("VALIDATE_GATE_EXPECT_CELLS", 0),
		RequireTier3:      os.Getenv("VALIDATE_GATE_REQUIRE_TIER3") != "0",
		RequireSoak:       os.Getenv("VALIDATE_GATE_REQUIRE_SOAK") == "1",
		RequireProperties: requireProps,
		RequireCoverage:   requireCoverage,
	}
	cellSoaks := 0
	var propEvals, propViol int64
	for _, c := range cells {
		if c.Soak != nil {
			cellSoaks++
		}
		if c.Tier1 != nil {
			propEvals += c.Tier1.PropertyEvaluations
			propViol += c.Tier1.PropertyViolations
		}
	}
	fmt.Printf("ValidateGate: %d cell(s) from %d host file(s); expect_cells=%d require_tier3=%v require_soak=%v require_properties=%v (schema>=5.6: %v) require_coverage=%v (schema>=5.8: %v) soak_summaries=%d (cells) + %d (hosts) property_evaluations=%d property_violations=%d\n",
		len(cells), len(paths), opts.ExpectedCells, opts.RequireTier3, opts.RequireSoak, opts.RequireProperties, propertyAware, opts.RequireCoverage, coverageAware, cellSoaks, len(soaks), propEvals, propViol)
	printCoverage(report.Coverage(cells), coverageAware)
	viol := report.Gate(cells, soaks, opts)
	if len(viol) == 0 {
		fmt.Println("ValidateGate: PASS -- every gated signal is zero in every cell.")
		return nil
	}
	fmt.Println("ValidateGate: FAIL")
	fmt.Printf("  %-24s %-8s %-12s %-44s %10s  %s\n", "refapp", "engine", "arch", "field", "value", "meaning")
	for _, v := range viol {
		fmt.Printf("  %-24s %-8s %-12s %-44s %10d  %s\n", v.Refapp, v.Engine, v.Arch, v.Field, v.Value, v.Why)
	}
	return fmt.Errorf("ValidateGate: %d violation(s) -- a nonzero true-signal counter is a failure, not a note", len(viol))
}

// printCoverage reports every predicate the property loop ran and never
// reached a verdict on, in ANY cell. It prints on PASS as well as on
// FAIL: "properties_passed=2" over 48 cells looks like a pass, and
// nothing else in the summary says that the three memory oracles never
// judged a single sample (probatorium#299).
func printCoverage(gaps []report.CoverageGap, classified bool) {
	if len(gaps) == 0 {
		return
	}
	fmt.Printf("ValidateGate: property coverage -- %d predicate(s) reached NO verdict in any cell:\n", len(gaps))
	for _, g := range gaps {
		var why string
		switch {
		case !classified:
			// Pre-5.8 documents carry the not-judged list without the
			// by-design one, so "0 too short" means "not recorded", not
			// "had the time". Saying SILENT ORACLE here would be a guess.
			why = "cannot classify -- document predates the by-design list (schema < 5.8)"
		case g.Structural():
			why = "not judgeable in cells this short -- by design, no opinion recorded"
		default:
			why = fmt.Sprintf("SILENT ORACLE: %d cell(s) had the time and judged nothing", g.Cells-g.ByDesign)
		}
		fmt.Printf("  %-14s not judged in %d cell(s) (%d too short)  %s\n", g.ID, g.Cells, g.ByDesign, why)
	}
}

// latestRunValidateResults returns every host's validate-results.json from
// the most recent run directory under results/ (newest by file mtime), sorted.
func latestRunValidateResults() ([]string, error) {
	entries, err := os.ReadDir("results")
	if err != nil {
		return nil, err
	}
	type run struct {
		mtime int64
		files []string
	}
	var runs []run
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		runDir := filepath.Join("results", e.Name())
		subs, err := os.ReadDir(runDir)
		if err != nil {
			continue
		}
		var r run
		for _, sub := range subs {
			if !sub.IsDir() {
				continue
			}
			p := filepath.Join(runDir, sub.Name(), "validate-results.json")
			st, err := os.Stat(p)
			if err != nil {
				continue
			}
			r.files = append(r.files, p)
			if m := st.ModTime().UnixNano(); m > r.mtime {
				r.mtime = m
			}
		}
		if len(r.files) > 0 {
			sort.Strings(r.files)
			runs = append(runs, r)
		}
	}
	if len(runs) == 0 {
		return nil, fmt.Errorf("ValidateGate: no validate-results.json under results/")
	}
	sort.Slice(runs, func(i, j int) bool { return runs[i].mtime > runs[j].mtime })
	return runs[0].files, nil
}

func gateEnvInt(k string, def int) int {
	if v := os.Getenv(k); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			return n
		}
	}
	return def
}
