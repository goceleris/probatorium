package report

import (
	"strings"
	"testing"
)

// notJudged returns a clean cell whose property loop ran but reached no
// verdict on ids. byDesign is the subset the cell was structurally too
// short to judge (a 150 s nightly cell can never fit I-MEM-1's 5 min
// warm-up plus its 10 min window).
func notJudged(r, e, a string, ids, byDesign []string) ValidationCellResult {
	c := cleanCell(r, e, a)
	c.PropertiesNotJudged = ids
	c.PropertiesNotJudgedByDesign = byDesign
	return c
}

// The v1.5.11 nightly shape, with the cells made long enough to judge:
// every cell reports I-MEM-1 as not judged and NONE of them says the cell
// was too short. The run therefore carries no opinion at all on heap
// growth while the gate header prints require_properties=true. On an
// ABSOLUTE zero-signal gate that silence is a finding, not a pass
// (probatorium#299).
func TestGate_SilentOracleInEveryCellIsACoverageFailure(t *testing.T) {
	cells := []ValidationCellResult{
		notJudged("a", "std", "amd64", []string{"I-MEM-1"}, nil),
		notJudged("a", "epoll", "amd64", []string{"I-MEM-1"}, nil),
		notJudged("a", "iouring", "amd64", []string{"I-MEM-1"}, nil),
	}
	v := Gate(cells, nil, GateOptions{RequireTier3: true, RequireProperties: true, RequireCoverage: true})
	if len(v) != 1 || v[0].Field != "properties.coverage.I-MEM-1" {
		t.Fatalf("want one coverage violation on I-MEM-1, got %v", v)
	}
	if v[0].Value != 3 {
		t.Fatalf("the violation must count the cells that never judged it, got %d", v[0].Value)
	}
	if v[0].Refapp != "*" || v[0].Engine != "*" || v[0].Arch != "*" {
		t.Fatalf("a coverage gap is run-wide, not a property of one cell: %+v", v[0])
	}
	if !strings.Contains(v[0].Why, "no verdict") {
		t.Fatalf("Why must say the predicate reached no verdict, got %q", v[0].Why)
	}
	// Without the switch the run gates exactly as it did before: a
	// pre-5.8 document carries no by-design list, so every short-cell
	// oracle would read as a gap.
	if v := Gate(cells, nil, GateOptions{RequireTier3: true, RequireProperties: true}); len(v) != 0 {
		t.Fatalf("coverage must only be gated under RequireCoverage, got %v", v)
	}
}

// A 150 s nightly cell CANNOT judge a slope predicate: the gap is a
// property of the tier, not a defect of the run. It is reported (silence
// must be visible) but it does not fail the gate -- "do not simply make
// nightlies fail".
func TestGate_ShortCellCoverageGapIsStructural(t *testing.T) {
	ids := []string{"I-MEM-1", "I-MEM-3", "I-MEM-4"}
	cells := []ValidationCellResult{
		notJudged("a", "std", "amd64", ids, ids),
		notJudged("a", "epoll", "arm64", ids, ids),
	}
	if v := Gate(cells, nil, GateOptions{RequireTier3: true, RequireProperties: true, RequireCoverage: true}); len(v) != 0 {
		t.Fatalf("a cell too short to judge a slope must not fail the gate, got %v", v)
	}
	gaps := Coverage(cells)
	if len(gaps) != 3 {
		t.Fatalf("want the three slope oracles reported as gaps, got %+v", gaps)
	}
	for _, g := range gaps {
		if !g.Structural() {
			t.Fatalf("%s: every cell was too short, so the gap is structural: %+v", g.ID, g)
		}
		if g.Cells != 2 || g.ByDesign != 2 {
			t.Fatalf("%s: want 2 cells, 2 by design, got %+v", g.ID, g)
		}
	}
	if gaps[0].ID != "I-MEM-1" || gaps[2].ID != "I-MEM-4" {
		t.Fatalf("gaps must be sorted by ID: %+v", gaps)
	}
}

// One cell long enough to judge and silent anyway is a gap even when
// every other cell in the matrix was too short: the oracle had its
// chance in that cell and said nothing.
func TestGate_OneJudgeableCellMakesTheGapReal(t *testing.T) {
	cells := []ValidationCellResult{
		notJudged("a", "std", "amd64", []string{"I-MEM-3"}, []string{"I-MEM-3"}),
		notJudged("a", "epoll", "amd64", []string{"I-MEM-3"}, nil),
	}
	gaps := Coverage(cells)
	if len(gaps) != 1 || gaps[0].Cells != 2 || gaps[0].ByDesign != 1 || gaps[0].Structural() {
		t.Fatalf("want a non-structural I-MEM-3 gap over 2 cells, got %+v", gaps)
	}
	v := Gate(cells, nil, GateOptions{RequireTier3: true, RequireProperties: true, RequireCoverage: true})
	if len(v) != 1 || v[0].Field != "properties.coverage.I-MEM-3" {
		t.Fatalf("want one coverage violation on I-MEM-3, got %v", v)
	}
	if !strings.Contains(v[0].Why, "1 of them") {
		t.Fatalf("Why must say how many cells had the time, got %q", v[0].Why)
	}
}

// A predicate that reached a verdict in ANY cell is covered: the matrix
// has an opinion on it, even if most cells were too short to form one.
// Order must not matter -- the cell that judged it is evidence whether
// it is walked before or after the cells that did not.
func TestCoverage_JudgedInOneCellIsNoGap(t *testing.T) {
	silent := notJudged("a", "std", "amd64", []string{"I-MEM-1"}, []string{"I-MEM-1"})
	judged := cleanCell("a", "epoll", "amd64") // judged everything it was given
	for _, cells := range [][]ValidationCellResult{{silent, judged}, {judged, silent}} {
		if gaps := Coverage(cells); len(gaps) != 0 {
			t.Fatalf("a predicate judged somewhere is not a coverage gap, got %+v", gaps)
		}
		if v := Gate(cells, nil, GateOptions{RequireTier3: true, RequireProperties: true, RequireCoverage: true}); len(v) != 0 {
			t.Fatalf("gate must pass, got %v", v)
		}
	}
}

// A cell whose loop never ran (ssh driver, or an unreachable
// /debug/vars) reports empty lists. It must not be read as "this cell
// judged the predicate" -- that would silently erase the gap the other
// cells reported.
func TestCoverage_CellsWithoutALoopAreNotEvidence(t *testing.T) {
	skipped := cleanCell("a", "epoll", "amd64")
	skipped.Tier1.PropertyEvaluations = 0
	skipped.Tier1.PropertyLoopSkipped = "ssh driver: remote /debug/vars is loopback-only"
	cells := []ValidationCellResult{
		notJudged("a", "std", "amd64", []string{"I-MEM-1"}, nil),
		skipped,
	}
	gaps := Coverage(cells)
	if len(gaps) != 1 || gaps[0].ID != "I-MEM-1" || gaps[0].Cells != 1 {
		t.Fatalf("want the gap from the one cell that ran a loop, got %+v", gaps)
	}
	// A run of nothing but skipped loops has no coverage opinion at all.
	if gaps := Coverage([]ValidationCellResult{skipped}); len(gaps) != 0 {
		t.Fatalf("a run whose loops never ran reports no gaps, got %+v", gaps)
	}
}

// A predicate with no data source in this deployment is #297's subject
// (vacuous instrumentation), not a verdict-coverage gap: a cell that
// lists it as not instrumented is neither evidence that it was judged
// nor a cell that failed to judge it.
func TestCoverage_NotInstrumentedIsNotJudged(t *testing.T) {
	c := notJudged("a", "std", "amd64", []string{"I-MEM-4"}, []string{"I-MEM-4"})
	other := cleanCell("a", "epoll", "amd64")
	other.PropertiesNotInstrumented = []string{"I-MEM-4"}
	gaps := Coverage([]ValidationCellResult{c, other})
	if len(gaps) != 1 || gaps[0].ID != "I-MEM-4" || gaps[0].Cells != 1 || gaps[0].ByDesign != 1 {
		t.Fatalf("an uninstrumented cell must neither cover nor accuse: %+v", gaps)
	}
}
