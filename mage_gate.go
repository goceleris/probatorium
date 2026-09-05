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
	propertyAware := true
	for _, p := range paths {
		doc, err := loadValidateDoc(p)
		if err != nil {
			return fmt.Errorf("load %s: %w", p, err)
		}
		if !report.SchemaAtLeast(doc.SchemaVersion, "5.6") {
			propertyAware = false
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
	opts := report.GateOptions{
		ExpectedCells:     gateEnvInt("VALIDATE_GATE_EXPECT_CELLS", 0),
		RequireTier3:      os.Getenv("VALIDATE_GATE_REQUIRE_TIER3") != "0",
		RequireSoak:       os.Getenv("VALIDATE_GATE_REQUIRE_SOAK") == "1",
		RequireProperties: requireProps,
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
	fmt.Printf("ValidateGate: %d cell(s) from %d host file(s); expect_cells=%d require_tier3=%v require_soak=%v require_properties=%v (schema>=5.6: %v) soak_summaries=%d (cells) + %d (hosts) property_evaluations=%d property_violations=%d\n",
		len(cells), len(paths), opts.ExpectedCells, opts.RequireTier3, opts.RequireSoak, opts.RequireProperties, propertyAware, cellSoaks, len(soaks), propEvals, propViol)
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
