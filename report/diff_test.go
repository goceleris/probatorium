package report

import (
	"strings"
	"testing"
)

func TestDiffValidation_NoTallies_NoDivergences(t *testing.T) {
	a := &ValidationResults{}
	b := &ValidationResults{}
	d := DiffValidation(a, b, "amd64", "arm64")
	if len(d) != 0 {
		t.Errorf("want no divergences, got %d: %+v", len(d), d)
	}
}

func TestDiffValidation_BothCleanIsNoDivergence(t *testing.T) {
	clean := &ValidationResults{
		Tier1: &Tier1Summary{
			Adversarial: map[string]int64{"adv_wrong_accepted": 0, "adv_sent": 100},
			H2CChurn:    map[string]int64{"h2c_crashed": 0, "h2c_sent": 50},
			WSTorture:   map[string]int64{"ws_accepted_bad_frame": 0, "ws_sent": 10},
			SSEKill:     map[string]int64{"sse_handshake_fail": 0, "sse_sent": 5},
		},
		Tier3: &Tier3Summary{SeedsAttempted: 144, SeedsPassed: 144, SeedsFailed: 0},
	}
	d := DiffValidation(clean, clean, "amd64", "arm64")
	if len(d) != 0 {
		t.Errorf("want no divergences for clean run, got %+v", d)
	}
}

func TestDiffValidation_BothNonZeroIsNotDivergence(t *testing.T) {
	// Both archs hit the same bug — that's a bug, but NOT a
	// cross-arch divergence. The per-arch predicate-tier check
	// surfaces this; DiffValidation only flags zero-asymmetric cases.
	a := &ValidationResults{Tier1: &Tier1Summary{
		Adversarial: map[string]int64{"adv_wrong_accepted": 5},
	}}
	b := &ValidationResults{Tier1: &Tier1Summary{
		Adversarial: map[string]int64{"adv_wrong_accepted": 7},
	}}
	d := DiffValidation(a, b, "amd64", "arm64")
	if len(d) != 0 {
		t.Errorf("want no divergence when both archs hit same bug, got %+v", d)
	}
}

func TestDiffValidation_ZeroAsymmetricAdversarial(t *testing.T) {
	// amd64 saw 3 wrong-accepted, arm64 saw 0. Arch-only bug.
	a := &ValidationResults{Tier1: &Tier1Summary{
		Adversarial: map[string]int64{"adv_wrong_accepted": 3},
	}}
	b := &ValidationResults{Tier1: &Tier1Summary{
		Adversarial: map[string]int64{"adv_wrong_accepted": 0},
	}}
	d := DiffValidation(a, b, "msa2-server-amd64", "msr1-arm64")
	if len(d) != 1 {
		t.Fatalf("want 1 divergence, got %d: %+v", len(d), d)
	}
	if d[0].Slice != "adversarial" || d[0].Counter != "adv_wrong_accepted" {
		t.Errorf("wrong divergence shape: %+v", d[0])
	}
	if d[0].ValA != 3 || d[0].ValB != 0 {
		t.Errorf("wrong values: ValA=%d ValB=%d", d[0].ValA, d[0].ValB)
	}
	if d[0].Severity != SeverityHigh {
		t.Errorf("wrong severity: got %q, want %q", d[0].Severity, SeverityHigh)
	}
	if d[0].HostA != "msa2-server-amd64" || d[0].HostB != "msr1-arm64" {
		t.Errorf("hosts not propagated: %+v", d[0])
	}
}

func TestDiffValidation_Tier3SeedsFailedDivergence(t *testing.T) {
	a := &ValidationResults{Tier3: &Tier3Summary{SeedsAttempted: 100, SeedsFailed: 2}}
	b := &ValidationResults{Tier3: &Tier3Summary{SeedsAttempted: 100, SeedsFailed: 0}}
	d := DiffValidation(a, b, "amd64", "arm64")
	if len(d) != 1 {
		t.Fatalf("want 1 divergence, got %d", len(d))
	}
	if d[0].Slice != "tier_3" || d[0].Counter != "seeds_failed" {
		t.Errorf("wrong divergence: %+v", d[0])
	}
	if d[0].Severity != SeverityHigh {
		t.Errorf("seeds_failed must be HIGH severity, got %q", d[0].Severity)
	}
}

func TestDiffValidation_MultipleDivergencesSortedBySeverity(t *testing.T) {
	a := &ValidationResults{Tier1: &Tier1Summary{
		// One HIGH severity (h2c_crashed), one MED (adv_hang).
		H2CChurn:    map[string]int64{"h2c_crashed": 2},
		Adversarial: map[string]int64{"adv_hang_until_timeout": 4},
	}}
	b := &ValidationResults{Tier1: &Tier1Summary{
		H2CChurn:    map[string]int64{"h2c_crashed": 0},
		Adversarial: map[string]int64{"adv_hang_until_timeout": 0},
	}}
	d := DiffValidation(a, b, "amd64", "arm64")
	if len(d) != 2 {
		t.Fatalf("want 2 divergences, got %d", len(d))
	}
	// HIGH severity must come first.
	if d[0].Severity != SeverityHigh {
		t.Errorf("first divergence should be HIGH, got %s: %+v", d[0].Severity, d[0])
	}
	if d[1].Severity != SeverityMed {
		t.Errorf("second divergence should be MED, got %s: %+v", d[1].Severity, d[1])
	}
}

func TestDiffValidation_OrderIndependence(t *testing.T) {
	// DiffValidation(a, b) and DiffValidation(b, a) should find the
	// SAME set of divergences (just with HostA/HostB swapped and
	// ValA/ValB swapped). Validate both cases produce the same count.
	a := &ValidationResults{Tier1: &Tier1Summary{
		Adversarial: map[string]int64{"adv_wrong_accepted": 5},
	}}
	b := &ValidationResults{Tier1: &Tier1Summary{
		Adversarial: map[string]int64{"adv_wrong_accepted": 0},
	}}
	d1 := DiffValidation(a, b, "x", "y")
	d2 := DiffValidation(b, a, "y", "x")
	if len(d1) != len(d2) {
		t.Errorf("order-dependent count: %d vs %d", len(d1), len(d2))
	}
	if d1[0].ValA != d2[0].ValB || d1[0].ValB != d2[0].ValA {
		t.Errorf("values not consistent across swap: %+v / %+v", d1, d2)
	}
}

func TestFormatDivergences_EmptyReportsOK(t *testing.T) {
	got := FormatDivergences(nil, "amd64", "arm64")
	if !strings.Contains(got, "OK") {
		t.Errorf("empty divergences should report OK, got %q", got)
	}
}

func TestFormatDivergences_RendersTable(t *testing.T) {
	d := []Divergence{
		{
			Kind:     KindZeroAsymmetric,
			Slice:    "adversarial",
			Counter:  "adv_wrong_accepted",
			ValA:     3,
			ValB:     0,
			Severity: SeverityHigh,
		},
	}
	got := FormatDivergences(d, "amd64", "arm64")
	for _, want := range []string{
		"amd64",
		"arm64",
		"adversarial",
		"adv_wrong_accepted",
		"HIGH",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("report missing %q, got:\n%s", want, got)
		}
	}
}

func TestIsZeroAsymmetric(t *testing.T) {
	cases := []struct {
		a, b int64
		want bool
	}{
		{0, 0, false},
		{1, 1, false},
		{1, 2, false},
		{0, 1, true},
		{1, 0, true},
		{0, 999, true},
	}
	for _, tc := range cases {
		if got := isZeroAsymmetric(tc.a, tc.b); got != tc.want {
			t.Errorf("isZeroAsymmetric(%d, %d): got %v, want %v",
				tc.a, tc.b, got, tc.want)
		}
	}
}

func TestDiffCells_EmptyOrSingleReturnsNil(t *testing.T) {
	if got := DiffCells(nil); got != nil {
		t.Errorf("nil cells: got %d, want nil", len(got))
	}
	one := []ValidationCellResult{{Refapp: "x", Engine: "iouring", Arch: "amd64"}}
	if got := DiffCells(one); got != nil {
		t.Errorf("single cell: got %d, want nil", len(got))
	}
}

func TestDiffCells_CrossEngineSameArchDetectsDivergence(t *testing.T) {
	cells := []ValidationCellResult{
		{Refapp: "auth_session_ratelimit", Engine: "iouring", Arch: "amd64",
			Tier1: &Tier1Summary{
				Adversarial: map[string]int64{"adv_wrong_accepted": 5},
			}},
		{Refapp: "auth_session_ratelimit", Engine: "epoll", Arch: "amd64",
			Tier1: &Tier1Summary{
				Adversarial: map[string]int64{"adv_wrong_accepted": 0},
			}},
	}
	divs := DiffCells(cells)
	if len(divs) != 1 {
		t.Fatalf("got %d divergences, want 1: %+v", len(divs), divs)
	}
	// DiffCells sorts cells alphabetically by engine within each
	// (refapp, arch) group, so HostA=epoll, HostB=iouring.
	if divs[0].HostA != "epoll-amd64" || divs[0].HostB != "iouring-amd64" {
		t.Errorf("expected cross-engine labels epoll-amd64 / iouring-amd64 (alpha-sorted), got %s / %s",
			divs[0].HostA, divs[0].HostB)
	}
	if divs[0].Severity != SeverityHigh {
		t.Errorf("severity: got %s, want HIGH", divs[0].Severity)
	}
}

func TestDiffCells_CrossArchSameEngineDetectsDivergence(t *testing.T) {
	cells := []ValidationCellResult{
		{Refapp: "auth_session_ratelimit", Engine: "iouring", Arch: "amd64",
			Tier1: &Tier1Summary{
				H2CChurn: map[string]int64{"h2c_crashed": 3},
			}},
		{Refapp: "auth_session_ratelimit", Engine: "iouring", Arch: "arm64",
			Tier1: &Tier1Summary{
				H2CChurn: map[string]int64{"h2c_crashed": 0},
			}},
	}
	divs := DiffCells(cells)
	if len(divs) != 1 {
		t.Fatalf("got %d divergences, want 1", len(divs))
	}
	if divs[0].HostA != "iouring-amd64" || divs[0].HostB != "iouring-arm64" {
		t.Errorf("expected cross-arch labels, got %s / %s", divs[0].HostA, divs[0].HostB)
	}
}

func TestDiffCells_FullMatrixDetectsAll(t *testing.T) {
	// 2 refapps × 2 engines × 2 arches = 8 cells. All clean EXCEPT one
	// (auth_session_ratelimit, iouring, amd64) which has 1 wrong-
	// accepted adversarial.
	mkCell := func(refapp, engine, arch string, advWrong int64) ValidationCellResult {
		return ValidationCellResult{
			Refapp: refapp, Engine: engine, Arch: arch,
			Tier1: &Tier1Summary{Adversarial: map[string]int64{"adv_wrong_accepted": advWrong}},
		}
	}
	cells := []ValidationCellResult{
		mkCell("auth", "iouring", "amd64", 1),
		mkCell("auth", "iouring", "arm64", 0),
		mkCell("auth", "epoll", "amd64", 0),
		mkCell("auth", "epoll", "arm64", 0),
		mkCell("kitchen", "iouring", "amd64", 0),
		mkCell("kitchen", "iouring", "arm64", 0),
		mkCell("kitchen", "epoll", "amd64", 0),
		mkCell("kitchen", "epoll", "arm64", 0),
	}
	divs := DiffCells(cells)
	// Cross-engine on (auth, amd64): 1 divergence (iouring vs epoll)
	// Cross-arch on (auth, iouring): 1 divergence (amd64 vs arm64)
	// No others (every other cell is 0)
	if len(divs) != 2 {
		t.Fatalf("got %d divergences, want 2: %+v", len(divs), divs)
	}
	for _, d := range divs {
		if d.Severity != SeverityHigh {
			t.Errorf("severity: got %s, want HIGH", d.Severity)
		}
		if d.Counter != "adv_wrong_accepted" {
			t.Errorf("counter: got %s, want adv_wrong_accepted", d.Counter)
		}
	}
}
