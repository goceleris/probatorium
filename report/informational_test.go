package report

import (
	"strings"
	"testing"
)

// The informational twin of I-MEM-1 is never read by the gate: a cell on
// which the pre-scatter-floor rule tripped and the gated rule did not passes
// (PropertyViolations is the gated rule's alone), and the output shows the
// trip as absolved.
func TestInformational_absolvedTripDoesNotFailTheGate(t *testing.T) {
	c := cleanCell("observability", "std", "arm64")
	c.Tier1.PropertyInformational = map[string]PropertyInformational{
		"I-MEM-1": {Label: "I-MEM-1 (pre-scatter-floor rule, informational)", Violations: 77, AbsolvedEvaluations: 77, Absolved: true,
			FirstMessage: "I-MEM-1 violated: heap_inuse trough slope 1.8 KB/s exceeds 1.0 KB/s budget"},
	}
	cells := []ValidationCellResult{c, cleanCell("a", "epoll", "amd64")}
	if v := Gate(cells, nil, GateOptions{ExpectedCells: 2, RequireTier3: true}); len(v) != 0 {
		t.Fatalf("an absolved informational trip must not fail the gate, got %v", v)
	}
	lines := strings.Join(InformationalLines(cells), "\n")
	for _, want := range []string{"NOT gating", "1 cell(s)", "1 of them absolved", "observability", "std", "arm64", "I-MEM-1", "77 violation(s)", "ABSOLVED", "trough slope 1.8 KB/s"} {
		if !strings.Contains(lines, want) {
			t.Errorf("informational output lacks %q:\n%s", want, lines)
		}
	}
}

// A trip both rules raise is the gated rule's: the gate fails, and the
// informational line says the gated rule tripped too.
func TestInformational_bothTripFailsTheGate(t *testing.T) {
	c := cleanCell("observability", "std", "arm64")
	c.Tier1.PropertyViolations = 300
	c.Tier1.PropertyViolationIDs = []string{"I-MEM-1"}
	c.Tier1.PropertyInformational = map[string]PropertyInformational{
		"I-MEM-1": {Label: "I-MEM-1 (pre-scatter-floor rule, informational)", Violations: 340, AbsolvedEvaluations: 40, FirstMessage: "I-MEM-1 violated"},
	}
	v := Gate([]ValidationCellResult{c}, nil, GateOptions{ExpectedCells: 1, RequireTier3: true})
	if len(v) != 1 || v[0].Field != "tier_1.property_violations" {
		t.Fatalf("a trip both rules raise must fail the gate on the gated count, got %v", v)
	}
	lines := strings.Join(InformationalLines([]ValidationCellResult{c}), "\n")
	if strings.Contains(lines, "ABSOLVED") || !strings.Contains(lines, "gated rule tripped too, 40 of these") {
		t.Fatalf("informational output wrong for a trip both rules raise:\n%s", lines)
	}
}

// The verdict is the same with and without the informational field, whatever
// it holds.
func TestInformational_isNeverGated(t *testing.T) {
	for _, info := range []map[string]PropertyInformational{
		nil,
		{"I-MEM-1": {Label: "x", Violations: 1 << 40, AbsolvedEvaluations: 1 << 40, Absolved: true}},
	} {
		c := cleanCell("a", "std", "amd64")
		c.Tier1.PropertyInformational = info
		if v := Gate([]ValidationCellResult{c}, nil, GateOptions{ExpectedCells: 1, RequireTier3: true}); len(v) != 0 {
			t.Fatalf("informational %v changed the verdict: %v", info, v)
		}
	}
}

func TestInformational_saysSoWhenNothingTripped(t *testing.T) {
	lines := InformationalLines([]ValidationCellResult{cleanCell("a", "std", "amd64")})
	if len(lines) != 2 || !strings.Contains(lines[0], "NOT gating") || !strings.Contains(lines[1], "trips in no cell") {
		t.Fatalf("unexpected output: %v", lines)
	}
	if got := InformationalLines(nil); len(got) != 2 {
		t.Fatalf("no cells: %v", got)
	}
}
