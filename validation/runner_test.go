package validation

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// osReadFile is an alias for os.ReadFile; kept in a single place so
// adding a wrapper for synthetic testing later only changes one line.
func osReadFile(p string) ([]byte, error) { return os.ReadFile(p) }

func TestPlan_NonEmptyAfterNew(t *testing.T) {
	cfg := Default()
	cfg.Duration = time.Hour
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	p := o.Plan()
	if len(p.Properties) == 0 {
		t.Fatal("expected predicates in plan")
	}
	if len(p.Tiers) != 3 {
		t.Fatalf("expected 3 tiers, got %d", len(p.Tiers))
	}
	if p.CorpusSize == 0 {
		t.Fatal("expected corpus seeded from InitialSeeds")
	}
}

func TestPlan_TierFilter(t *testing.T) {
	cfg := Default()
	cfg.PropertyTier = "core"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	for _, s := range o.Plan().Properties {
		if s.Tier != "core" {
			t.Errorf("non-core predicate %q tier=%s", s.ID, s.Tier)
		}
	}
}

func TestPlan_MultiTierFilter(t *testing.T) {
	cfg := Default()
	cfg.PropertyTier = "core,middleware"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	saw := map[string]bool{}
	for _, s := range o.Plan().Properties {
		saw[s.Tier] = true
	}
	if !saw["core"] || !saw["middleware"] {
		t.Fatalf("expected core + middleware, got %v", saw)
	}
	if saw["engine"] {
		t.Fatal("did not expect engine tier")
	}
}

func TestPrintPlan_Renders(t *testing.T) {
	o, _ := New(Default())
	var buf bytes.Buffer
	PrintPlan(&buf, o.Plan())
	out := buf.String()
	for _, frag := range []string{"validator plan", "tier-1-property", "tier-2-restler", "tier-3-replay", "I-CONN-1"} {
		if !strings.Contains(out, frag) {
			t.Errorf("expected substring %q in plan output", frag)
		}
	}
}

func TestRun_DryRunWritesPlan(t *testing.T) {
	dir := t.TempDir()
	cfg := Default()
	cfg.DryRun = true
	cfg.Duration = time.Second
	cfg.OutDir = dir
	o, _ := New(cfg)
	if err := o.Run(context.Background()); err != nil {
		t.Fatalf("dry run: %v", err)
	}
	// plan.json must exist after dry-run.
	if _, err := readFile(filepath.Join(dir, "plan.json")); err != nil {
		t.Fatalf("plan.json missing: %v", err)
	}
}

func TestReplayPlan_Deterministic(t *testing.T) {
	a := ReplayPlan(0xc0ffee, time.Hour, 100, 8080)
	b := ReplayPlan(0xc0ffee, time.Hour, 100, 8080)
	if a.Seed != b.Seed || a.MarkovSteps != b.MarkovSteps || a.StartupJitter != b.StartupJitter {
		t.Fatalf("non-deterministic: %+v vs %+v", a, b)
	}
	if len(a.Schedule) != len(b.Schedule) {
		t.Fatalf("schedule len differs")
	}
}

func TestReplayPlan_DifferentSeeds(t *testing.T) {
	a := ReplayPlan(1, time.Hour, 100, 8080)
	b := ReplayPlan(2, time.Hour, 100, 8080)
	if a.MarkovSteps == b.MarkovSteps && a.StartupJitter == b.StartupJitter {
		t.Fatal("expected different seeds to diverge")
	}
}

func TestPrintReplayPlan_Renders(t *testing.T) {
	rs := ReplayPlan(0x1, time.Hour, 100, 8080)
	var buf bytes.Buffer
	PrintReplayPlan(&buf, rs)
	out := buf.String()
	if !strings.Contains(out, "0x1") || !strings.Contains(out, "markov_steps") {
		t.Fatalf("unexpected plan render: %q", out)
	}
}

func TestSplitComma(t *testing.T) {
	cases := []struct {
		in   string
		want []string
	}{
		{"", nil},
		{"a", []string{"a"}},
		{"a,b", []string{"a", "b"}},
		{"a, b ,c", []string{"a", "b", "c"}},
		{",,", nil},
	}
	for _, tc := range cases {
		got := splitComma(tc.in)
		if !equalStringSlices(got, tc.want) {
			t.Errorf("splitComma(%q) = %v; want %v", tc.in, got, tc.want)
		}
	}
}

func equalStringSlices(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// readFile is a tiny wrapper used only by tests.
func readFile(p string) ([]byte, error) { return osReadFile(p) }

// TestRunTierProperty_EmptyCelerisBinIsNoop verifies the orchestrator
// short-circuits Tier 1 when no refapp binary is configured — the
// unit-test default. Without this guard every test that calls Run()
// would fail trying to exec an empty path.
func TestRunTierProperty_EmptyCelerisBinIsNoop(t *testing.T) {
	cfg := Default()
	cfg.Duration = 50 * time.Millisecond
	cfg.OutDir = t.TempDir()
	// CelerisBin left empty: Tier 1 should park.
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	violations := make(chan Incident, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	// Drain runs synchronously — Tier 1 must return on ctx.Done().
	o.runTierProperty(ctx, violations)
	if len(violations) != 0 {
		t.Errorf("unexpected violation in empty-bin mode: %+v", <-violations)
	}
}

// TestRunTierProperty_NilMatrixIsNoop verifies Tier 1 parks (not
// errors) when CelerisBin is set but the matrix didn't load — the
// orchestrator should never run un-replayable traffic.
func TestRunTierProperty_NilMatrixIsNoop(t *testing.T) {
	cfg := Default()
	cfg.Duration = 50 * time.Millisecond
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/usr/bin/true" // present but matrix missing
	// MarkovPath empty -> o.matrix stays nil.
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	violations := make(chan Incident, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	o.runTierProperty(ctx, violations)
	if len(violations) != 0 {
		t.Errorf("nil matrix should park silently, not emit a violation")
	}
}

// TestRunTierProperty_BinaryNotFoundEmitsIncident verifies Tier 1
// surfaces driver Start failures as synthetic incidents (not silent
// noops) so the orchestrator's incident pipeline captures the cause.
func TestRunTierProperty_BinaryNotFoundEmitsIncident(t *testing.T) {
	cfg := Default()
	cfg.Duration = time.Second
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/this/path/does/not/exist/probatorium-test"
	cfg.MarkovPath = "markov/auth_session_ratelimit.yaml"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	violations := make(chan Incident, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	o.runTierProperty(ctx, violations)
	select {
	case inc := <-violations:
		if inc.PredicateID != "T1-DRIVE" {
			t.Errorf("PredicateID: got %q, want T1-DRIVE", inc.PredicateID)
		}
		if inc.Tier != TierProperty {
			t.Errorf("Tier: got %v, want TierProperty", inc.Tier)
		}
		if !strings.Contains(strings.ToLower(inc.Message), "no such file") &&
			!strings.Contains(strings.ToLower(inc.Message), "not found") {
			t.Errorf("Message should mention missing binary, got %q", inc.Message)
		}
	default:
		t.Fatal("expected incident on missing binary, got none")
	}
}
