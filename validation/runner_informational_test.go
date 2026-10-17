package validation

import (
	"testing"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/checker"
)

func TestInformationalSummary(t *testing.T) {
	if informationalSummary(nil) != nil {
		t.Fatal("no twin tripped: want nil")
	}
	got := informationalSummary(map[string]checker.ShadowTally{"I-MEM-1": {Label: "l", Violations: 3, AbsolvedEvaluations: 3, Absolved: true, FirstMessage: "m"}})
	want := report.PropertyInformational{Label: "l", Violations: 3, AbsolvedEvaluations: 3, Absolved: true, FirstMessage: "m"}
	if len(got) != 1 || got["I-MEM-1"] != want {
		t.Fatalf("got %+v", got)
	}
}
