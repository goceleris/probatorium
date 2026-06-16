package scenarios

import (
	"sort"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// expectedRegistry lists every scenario registered by the static.go and
// concurrency.go init()s. Chain and driver scenarios are wired in other
// files and are deliberately excluded here — this test guards the slice
// we own, not the ones we don't.
var expectedRegistry = []string{
	// static H1 (8)
	"churn-close",
	"get-json",
	"get-json-1k",
	"get-json-64k",
	"get-simple",
	"post-1m",
	"post-4k",
	"post-64k",

	// static H2-prior-knowledge (4) — exercise h2c-noupg and other
	// HTTP2C-capable cells that the H1 variants skip.
	"get-json-64k-h2",
	"get-json-h2",
	"post-4k-h2",
	"post-64k-h2",

	// concurrency (4)
	"auto-mix-111",
	"get-json-1c",
	"get-simple-1024c",
	"get-simple-128c",
}

func TestRegistryContainsExpectedScenarios(t *testing.T) {
	t.Parallel()
	names := make(map[string]bool)
	for _, s := range Registry() {
		names[s.Name()] = true
	}
	for _, want := range expectedRegistry {
		if !names[want] {
			t.Errorf("scenario %q missing from Registry()", want)
		}
	}
	var got []string
	for _, s := range Registry() {
		switch s.Category() {
		case CategoryStatic, CategoryConcurrency:
			got = append(got, s.Name())
		}
	}
	sort.Strings(got)

	want := append([]string(nil), expectedRegistry...)
	sort.Strings(want)

	if len(got) != len(want) {
		t.Fatalf("static+concurrency registry size = %d, want %d\n got=%v\nwant=%v",
			len(got), len(want), got, want)
	}
	for i := range got {
		if got[i] != want[i] {
			t.Errorf("static+concurrency registry[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWorkloadConfigIsSane(t *testing.T) {
	t.Parallel()
	const target = "http://127.0.0.1:8080"
	for _, name := range expectedRegistry {
		name := name
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := findScenario(t, name)
			cfg := s.Workload(target)
			if cfg.URL == "" {
				t.Errorf("Workload(%q).URL is empty", target)
			}
			if cfg.Connections <= 0 {
				t.Errorf("Workload(%q).Connections = %d, want > 0", target, cfg.Connections)
			}
			if cfg.Duration != 0 {
				t.Errorf("Workload(%q).Duration = %s, want 0 (runner fills it)", target, cfg.Duration)
			}
			if cfg.Warmup != 0 {
				t.Errorf("Workload(%q).Warmup = %s, want 0 (runner fills it)", target, cfg.Warmup)
			}
		})
	}
}

func TestStaticPOSTBodiesExactSize(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		want int
	}{
		{"post-4k", 4 * 1024},
		{"post-64k", 64 * 1024},
		{"post-1m", 1024 * 1024},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := findScenario(t, tc.name)
			stat, ok := s.(*StaticScenario)
			if !ok {
				t.Fatalf("scenario %q is %T, want *StaticScenario", tc.name, s)
			}
			if got := len(stat.Body); got != tc.want {
				t.Errorf("POST body size for %q = %d, want %d", tc.name, got, tc.want)
			}
			cfg := stat.Workload("http://x")
			if got := len(cfg.Body); got != tc.want {
				t.Errorf("Workload.Body size for %q = %d, want %d", tc.name, got, tc.want)
			}
		})
	}
}

func TestStaticScenariosRequireHTTP1(t *testing.T) {
	t.Parallel()
	h1Only := servers.FeatureSet{HTTP1: true}
	h2cOnly := servers.FeatureSet{HTTP2C: true}
	for _, name := range []string{
		"churn-close", "get-json", "get-json-1k", "get-json-64k",
		"get-simple", "post-1m", "post-4k", "post-64k",
	} {
		s := findScenario(t, name)
		if !s.Applicable(h1Only) {
			t.Errorf("static scenario %q unexpectedly skipped for HTTP1-only server", name)
		}
		if s.Applicable(h2cOnly) {
			t.Errorf("static scenario %q applicable to H2C-only server (would record 0 RPS)", name)
		}
	}
}

func TestChurnCloseUsesConnectionClose(t *testing.T) {
	t.Parallel()
	s := findScenario(t, "churn-close")
	cfg := s.Workload("http://x")
	if !cfg.DisableKeepAlive {
		t.Errorf("churn-close: DisableKeepAlive = false, want true")
	}
	if cfg.Connections != 32 {
		t.Errorf("churn-close: Connections = %d, want 32", cfg.Connections)
	}
	// churn-close (DisableKeepAlive=true) must cap loadgen's PoolSize=1
	// so the bench only opens Workers (64) concurrent dials, not
	// Workers × PoolSize (1024). The 1024-dial burst overwhelms
	// single-listener SUTs (Zig std.http, axum's default accept loop,
	// etc.) and manifests as "i/o timeout" on the Nth dial. v3.8
	// smoke test caught this on zig_zap / churn-close.
	if cfg.PoolSize != 1 {
		t.Errorf("churn-close: PoolSize = %d, want 1 (cap to keep dial burst under the kernel accept-backlog limit)", cfg.PoolSize)
	}
}

// TestErrorBudgets pins the per-scenario error-ratio ceilings the runner's
// suspect gate keys on (schema v5.4). churn-close carries an explicit 0.5
// budget — refused dials are inherent to churn, but the v3.8 evidence
// (errors 28x–97x requests on EVERY server, published status=ok) must flag
// as suspect. Every other scenario uses the 5% default.
func TestErrorBudgets(t *testing.T) {
	t.Parallel()
	if got := ErrorBudgetFor(findScenario(t, "churn-close")); got != 0.5 {
		t.Errorf("churn-close ErrorBudget = %v, want 0.5", got)
	}
	for _, name := range []string{"get-json", "post-4k", "sse-fanout-1024", "chain-api-post-4k"} {
		if got := ErrorBudgetFor(findScenario(t, name)); got != DefaultErrorBudget {
			t.Errorf("%s ErrorBudget = %v, want DefaultErrorBudget (%v)", name, got, DefaultErrorBudget)
		}
	}
	// A zero/negative declared budget falls back to the default rather
	// than disabling the gate.
	if got := ErrorBudgetFor(&StaticScenario{name: "x"}); got != DefaultErrorBudget {
		t.Errorf("zero-ErrBudget scenario = %v, want DefaultErrorBudget fallback", got)
	}
	// The v3.8 churn-close numbers themselves: ntex ran 12,081,484
	// requests against 290,204,598 errors (ratio 0.960) — over budget.
	ratio := 290204598.0 / (290204598.0 + 12081484.0)
	if budget := ErrorBudgetFor(findScenario(t, "churn-close")); ratio <= budget {
		t.Errorf("v3.8 churn-close ratio %.3f must exceed the 0.5 budget", ratio)
	}
}

func TestAutoMixApplicableGating(t *testing.T) {
	t.Parallel()
	s := findScenario(t, "auto-mix-111")
	checks := []struct {
		name string
		fs   servers.FeatureSet
		want bool
	}{
		{"empty", servers.FeatureSet{}, false},
		{"only-h1", servers.FeatureSet{HTTP1: true}, false},
		{"h1+h2c", servers.FeatureSet{HTTP1: true, HTTP2C: true}, false},
		{"h1+h2c+upgrade", servers.FeatureSet{HTTP1: true, HTTP2C: true, H2CUpgrade: true}, true},
		{"everything", servers.FeatureSet{
			HTTP1: true, HTTP2C: true, Auto: true, H2CUpgrade: true,
			Drivers: true, Middleware: true, AsyncHandlers: true,
		}, true},
	}
	for _, c := range checks {
		if got := s.Applicable(c.fs); got != c.want {
			t.Errorf("auto-mix-111 Applicable(%s) = %v, want %v", c.name, got, c.want)
		}
	}

	cfg := s.Workload("http://x")
	if cfg.Mix == nil {
		t.Fatalf("auto-mix-111: Workload.Mix == nil, want *loadgen.MixRatio{1,1,1}")
	}
	if cfg.Mix.H1 != 1 || cfg.Mix.H2 != 1 || cfg.Mix.Upgrade != 1 {
		t.Errorf("auto-mix-111: Mix = %+v, want {1,1,1}", *cfg.Mix)
	}
	if cfg.HTTP2 {
		t.Errorf("auto-mix-111: HTTP2 = true, must be false when Mix is set")
	}
	if cfg.H2CUpgrade {
		t.Errorf("auto-mix-111: H2CUpgrade = true, must be false when Mix is set")
	}
}

func TestConcurrencyNonAutoMixRequireHTTP1(t *testing.T) {
	t.Parallel()
	h1Only := servers.FeatureSet{HTTP1: true}
	h2cOnly := servers.FeatureSet{HTTP2C: true}
	for _, name := range []string{"get-json-1c", "get-simple-128c", "get-simple-1024c"} {
		s := findScenario(t, name)
		if !s.Applicable(h1Only) {
			t.Errorf("%q: unexpectedly skipped for HTTP1-only server", name)
		}
		if s.Applicable(h2cOnly) {
			t.Errorf("%q: applicable to H2C-only server (would record 0 RPS)", name)
		}
	}
}

func TestStaticH2ScenariosRequireHTTP2C(t *testing.T) {
	t.Parallel()
	h1Only := servers.FeatureSet{HTTP1: true}
	h2cOnly := servers.FeatureSet{HTTP2C: true}
	for _, name := range []string{"get-json-h2", "get-json-64k-h2", "post-4k-h2", "post-64k-h2"} {
		s := findScenario(t, name)
		if s.Applicable(h1Only) {
			t.Errorf("%q: applicable to HTTP1-only server (H2 scenario needs HTTP2C)", name)
		}
		if !s.Applicable(h2cOnly) {
			t.Errorf("%q: unexpectedly skipped for H2C-capable server", name)
		}
	}
}

func TestCategories(t *testing.T) {
	t.Parallel()
	for _, name := range []string{
		"churn-close", "get-json", "get-json-1k", "get-json-64k",
		"get-simple", "post-1m", "post-4k", "post-64k",
		"get-json-h2", "get-json-64k-h2", "post-4k-h2", "post-64k-h2",
	} {
		s := findScenario(t, name)
		if got := s.Category(); got != CategoryStatic {
			t.Errorf("%q: Category() = %q, want %q", name, got, CategoryStatic)
		}
	}
	for _, name := range []string{
		"auto-mix-111", "get-json-1c", "get-simple-128c", "get-simple-1024c",
	} {
		s := findScenario(t, name)
		if got := s.Category(); got != CategoryConcurrency {
			t.Errorf("%q: Category() = %q, want %q", name, got, CategoryConcurrency)
		}
	}
}

// findScenario locates a registered scenario by name; fails the test if
// missing so downstream assertions can assume non-nil.
func findScenario(t *testing.T, name string) Scenario {
	t.Helper()
	for _, s := range Registry() {
		if s.Name() == name {
			return s
		}
	}
	t.Fatalf("scenario %q not registered", name)
	return nil
}
