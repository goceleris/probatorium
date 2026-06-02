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

func TestWriteValidateResults_NeitherTierRan_NoFile(t *testing.T) {
	cfg := Default()
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/usr/bin/true"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Neither tier ran. writeValidateResults should be a no-op:
	// no file created, no error.
	if err := o.writeValidateResults(time.Now()); err != nil {
		t.Fatalf("writeValidateResults: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.OutDir, "validate-results.json")); !os.IsNotExist(err) {
		t.Errorf("expected no validate-results.json, got err=%v", err)
	}
}

func TestWriteValidateResults_Tier1OnlyEmitsDocument(t *testing.T) {
	cfg := Default()
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/usr/bin/true"
	cfg.Target = "msa2-server"
	cfg.Arch = "amd64"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// Synthesise a populated tier1 snapshot — the kind the
	// orchestrator would stash on clean exit.
	o.tier1Snapshot = tier1TallySnapshot{
		RequestsSent: 1000,
		Requests2xx:  900,
		Requests4xx:  50,
		Requests5xx:  10,
		Adversarial:  adversarialSnapshot{Sent: 100, WellRejected: 95, WrongAccepted: 5},
		H2CChurn:     h2cSnapshot{Sent: 50, Declined: 50},
		WSTorture:    wsSnapshot{Sent: 10, ClosedCorrectly: 10},
		SSEKill:      sseSnapshot{Sent: 5, KilledMidStream: 5},
	}
	o.tier1Ran = true

	if err := o.writeValidateResults(time.Now().Add(-time.Hour)); err != nil {
		t.Fatalf("writeValidateResults: %v", err)
	}
	// File must exist + contain the canonical schema_version + the
	// adv/h2c/ws/sse sub-tallies.
	raw, err := osReadFile(filepath.Join(cfg.OutDir, "validate-results.json"))
	if err != nil {
		t.Fatalf("read result: %v", err)
	}
	for _, want := range []string{
		`"schema_version": "5.2"`,
		`"host_arch_pair": "msa2-server-amd64"`,
		`"adv_sent": 100`,
		`"adv_wrong_accepted": 5`,
		`"h2c_sent": 50`,
		`"ws_sent": 10`,
		`"sse_killed_mid_stream": 5`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("missing %q in document, got:\n%s", want, raw)
		}
	}
	// Tier3 should NOT be present (didn't run).
	if bytes.Contains(raw, []byte(`"tier_3"`)) {
		t.Errorf("tier_3 should be absent when Tier 3 didn't run, got:\n%s", raw)
	}
}

func TestWriteValidateResults_BothTiersEmitFull(t *testing.T) {
	cfg := Default()
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/usr/bin/true"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.tier1Ran = true
	o.tier1Snapshot = tier1TallySnapshot{RequestsSent: 1}
	o.tier3Ran = true
	o.tier3Snapshot = tier3TallySnapshot{SeedsAttempted: 42, SeedsPassed: 40, SeedsFailed: 1, SeedsErrored: 1}

	if err := o.writeValidateResults(time.Now()); err != nil {
		t.Fatalf("writeValidateResults: %v", err)
	}
	raw, err := osReadFile(filepath.Join(cfg.OutDir, "validate-results.json"))
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	for _, want := range []string{
		`"seeds_attempted": 42`,
		`"seeds_passed": 40`,
		`"seeds_failed": 1`,
		`"seeds_errored": 1`,
	} {
		if !bytes.Contains(raw, []byte(want)) {
			t.Errorf("missing %q in document, got:\n%s", want, raw)
		}
	}
}

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

func TestOrchestratorBuildDriver_LocalDefault(t *testing.T) {
	cfg := Default()
	cfg.CelerisBin = "/usr/bin/true"
	cfg.OutDir = t.TempDir()
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d, err := o.buildDriver()
	if err != nil {
		t.Fatalf("buildDriver: %v", err)
	}
	if d == nil {
		t.Fatal("nil driver")
	}
	_ = d.Close()
}

func TestOrchestratorBuildDriver_SSHRequiresHostAndUser(t *testing.T) {
	cfg := Default()
	cfg.CelerisBin = "/usr/bin/true"
	cfg.OutDir = t.TempDir()
	cfg.DriverMode = "ssh"
	// Missing DriverSSHUser + DriverSSHHost.
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = o.buildDriver()
	if err == nil {
		t.Fatal("expected error for ssh-without-host-or-user")
	}
}

func TestOrchestratorBuildDriver_SSHWithHostAndUser(t *testing.T) {
	cfg := Default()
	cfg.CelerisBin = "/usr/bin/true"
	cfg.OutDir = t.TempDir()
	cfg.DriverMode = "ssh"
	cfg.DriverSSHUser = "test"
	cfg.DriverSSHHost = "127.0.0.1"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	d, err := o.buildDriver()
	if err != nil {
		t.Fatalf("buildDriver(ssh): %v", err)
	}
	if d == nil {
		t.Fatal("nil ssh driver")
	}
	_ = d.Close()
}

func TestBuildRefappArgs_NoEngine(t *testing.T) {
	got := buildRefappArgs("127.0.0.1:8080", "", "")
	want := []string{"-bind", "127.0.0.1:8080"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestBuildRefappArgs_WithEngine(t *testing.T) {
	got := buildRefappArgs("127.0.0.1:8080", "iouring", "")
	want := []string{"-bind", "127.0.0.1:8080", "-engine", "iouring"}
	if len(got) != len(want) {
		t.Fatalf("got %v, want %v", got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("[%d]: got %q, want %q", i, got[i], want[i])
		}
	}
}

func TestOrchestratorBuildDriver_UnknownMode(t *testing.T) {
	cfg := Default()
	cfg.CelerisBin = "/usr/bin/true"
	cfg.OutDir = t.TempDir()
	cfg.DriverMode = "make-believe"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = o.buildDriver()
	if err == nil {
		t.Fatal("expected error for unknown driver mode")
	}
	if !strings.Contains(err.Error(), "unknown driver mode") {
		t.Errorf("error: %v", err)
	}
}
