package report

import (
	"strings"
	"testing"
)

// docTree builds a generic results tree shaped like a canonical Document:
// benchmarks[].latency_at_slo[scenario][slo-ms] = max-sustained-RPS. Numbers
// are float64 (the encoding/json default) so the gate's numeric coercion path
// is exercised exactly as it would be on a real results.json.
func docTree(server, scenario string, slo map[string]float64) any {
	inner := map[string]any{}
	for ms, rps := range slo {
		inner[ms] = rps
	}
	return map[string]any{
		"benchmarks": []any{
			map[string]any{
				"name":           server,
				"latency_at_slo": map[string]any{scenario: inner},
			},
		},
	}
}

// TestFlattenLatencyAtSLOFindsLeaves confirms the walk surfaces every
// latency_at_slo leaf under the typed Document, keyed by a stable path, and
// that non-latency_at_slo numerics (rps_median) are NOT collected.
func TestFlattenLatencyAtSLOFindsLeaves(t *testing.T) {
	tree := map[string]any{
		"benchmarks": []any{
			map[string]any{
				"name":       "celeris",
				"rps_median": 999999.0, // must be ignored
				"latency_at_slo": map[string]any{
					"get-json": map[string]any{"100": 10000.0, "500": 12000.0},
				},
			},
		},
	}
	flat := FlattenLatencyAtSLO(tree)
	if len(flat) != 2 {
		t.Fatalf("expected 2 latency_at_slo leaves, got %d: %v", len(flat), flat)
	}
	var found100 bool
	for k, v := range flat {
		if strings.HasSuffix(k, "/100") {
			found100 = true
			if v != 10000 {
				t.Fatalf("path %s = %v, want 10000", k, v)
			}
		}
		if v == 999999 {
			t.Fatalf("rps_median leaked into latency_at_slo flatten: %s", k)
		}
	}
	if !found100 {
		t.Fatalf("no /100 leaf in %v", flat)
	}
}

// TestDiffLatencyAtSLORegression is the required synthetic-regression test:
// a baseline cell at 10000 RPS-at-SLO dropping to 9000 (-10%) under a 5%
// threshold must flag a regression, and the rendered report must mark it
// with " !!". latency_at_slo is bigger-is-better, so the drop is the
// regression direction.
func TestDiffLatencyAtSLORegression(t *testing.T) {
	base := docTree("celeris", "get-json", map[string]float64{"100": 10000})
	curr := docTree("celeris", "get-json", map[string]float64{"100": 9000})

	regs, regressed := DiffLatencyAtSLO(base, curr, 0.05)
	if !regressed {
		t.Fatalf("expected regression for -10%% drop under 5%% threshold")
	}
	rep := RenderRegressionReport(regs, 0.05)
	if !strings.Contains(rep, " !!") {
		t.Fatalf("report should mark the regression with ' !!':\n%s", rep)
	}
}

// TestDiffLatencyAtSLOControl confirms a within-threshold drop and an
// improvement do NOT flag, so the gate is not a hair-trigger.
func TestDiffLatencyAtSLOControl(t *testing.T) {
	base := docTree("celeris", "get-json", map[string]float64{"100": 10000})

	// -4% is within the 5% threshold: not a regression.
	if _, regressed := DiffLatencyAtSLO(base,
		docTree("celeris", "get-json", map[string]float64{"100": 9600}), 0.05); regressed {
		t.Fatalf("-4%% drop should be within the 5%% threshold")
	}
	// +10% is an improvement (bigger is better): never a regression.
	if _, regressed := DiffLatencyAtSLO(base,
		docTree("celeris", "get-json", map[string]float64{"100": 11000}), 0.05); regressed {
		t.Fatalf("an improvement must not flag a regression")
	}
}

// TestDiffLatencyAtSLOMissing confirms a baseline cell that vanishes in the
// current run flags a regression — a dropped measurement is treated as worse,
// not silently passed.
func TestDiffLatencyAtSLOMissing(t *testing.T) {
	base := docTree("celeris", "get-json", map[string]float64{"100": 10000})
	curr := docTree("celeris", "other-scenario", map[string]float64{"100": 10000})

	regs, regressed := DiffLatencyAtSLO(base, curr, 0.05)
	if !regressed {
		t.Fatalf("a baseline cell absent from current must regress")
	}
	rep := RenderRegressionReport(regs, 0.05)
	if !strings.Contains(rep, "missing") {
		t.Fatalf("report should name the missing cell:\n%s", rep)
	}
}

// TestDiffLatencyAtSLONoBaselineSignal confirms that when neither side carries
// any latency_at_slo (rated mode never ran), the gate sees nothing and passes —
// so turning rated OFF cannot itself trip the gate.
func TestDiffLatencyAtSLONoBaselineSignal(t *testing.T) {
	empty := map[string]any{"benchmarks": []any{
		map[string]any{"name": "celeris", "rps_median": 100000.0},
	}}
	if regs, regressed := DiffLatencyAtSLO(empty, empty, 0.05); regressed || len(regs) != 0 {
		t.Fatalf("no latency_at_slo signal must produce no regressions, got %d regs", len(regs))
	}
}
