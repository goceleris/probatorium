package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/budget"
)

func readWorkflow(t *testing.T, name string) string {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(".github", "workflows", name))
	if err != nil {
		t.Fatalf("%s: %v", name, err)
	}
	return string(b)
}

// TestBenchmarkTierBaselineInputIsOptIn: the benchmark-tier `baseline` input
// defaults to "false", is wired to BENCH_BASELINE at the JOB level (so both
// mage Deploy, which builds servers/celeris_v1511, and mage BenchTier, which
// picks the columns and projects the budget, see it), and no other workflow
// turns the switch on.
func TestBenchmarkTierBaselineInputIsOptIn(t *testing.T) {
	src := readWorkflow(t, "benchmark-tier.yml")
	in := regexp.MustCompile(`(?m)^      baseline:\n        description: .*\n        required: false\n        type: string\n        default: "false"\n`)
	if !in.MatchString(src) {
		t.Error(`benchmark-tier.yml: input baseline must be a string input, not required, default "false"`)
	}
	const env = `      BENCH_BASELINE: ${{ github.event.inputs.baseline == 'true' && '1' || '' }}` + "\n"
	i := strings.Index(src, env)
	if i < 0 {
		t.Fatalf("benchmark-tier.yml: job env lacks %q", strings.TrimSpace(env))
	}
	m := strings.Index(src, "\n  matrix:\n")
	j := strings.Index(src[m+1:], "    steps:") + m + 1
	if m < 0 || i < m || i > j {
		t.Error("BENCH_BASELINE must be set in the matrix job's env block (above steps:), not on one step: Deploy and BenchTier are separate steps")
	}
	entries, err := os.ReadDir(filepath.Join(".github", "workflows"))
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() == "benchmark-tier.yml" {
			continue
		}
		if b := readWorkflow(t, e.Name()); strings.Contains(b, "BENCH_BASELINE") {
			t.Errorf("%s mentions BENCH_BASELINE: only a benchmark-tier dispatch may bench the baseline", e.Name())
		}
	}
}

// TestBenchmarkTierBudgetCoversTheBaselineGrid ties the workflow's
// BENCH_BUDGET expression to the budget model: the budgets of a dispatch
// without the baseline are unchanged (24h fast, 36h headline, 60h full),
// and a baseline dispatch uses budgets that fit the grid it will run
// (budget.WithBaseline) and that the baseline-less ones do not.
func TestBenchmarkTierBudgetCoversTheBaselineGrid(t *testing.T) {
	src := readWorkflow(t, "benchmark-tier.yml")
	line := regexp.MustCompile(`(?m)^\s+BENCH_BUDGET: \$\{\{ (.*) \}\}$`).FindStringSubmatch(src)
	if line == nil {
		t.Fatal("no BENCH_BUDGET expression in benchmark-tier.yml")
	}
	const want = `github.event.inputs.profile == 'fast' && '24h' || (github.event.inputs.profile == 'headline' && (github.event.inputs.baseline == 'true' && '48h' || '36h') || (github.event.inputs.baseline == 'true' && '80h' || '60h'))`
	if line[1] != want {
		t.Fatalf("BENCH_BUDGET expression changed:\n got %s\nwant %s\n(update this test with the new budgets, after checking them against budget.WithBaseline)", line[1], want)
	}
	hl, full := budget.HeadlineWeekly(), budget.Full()
	for _, c := range []struct {
		name        string
		p           budget.Profile
		plain, with time.Duration
	}{
		{"headline", hl, 36 * time.Hour, 48 * time.Hour},
		{"full", full, 60 * time.Hour, 80 * time.Hour},
	} {
		if log, ok := c.p.FitWithin(c.plain); !ok {
			t.Errorf("%s no longer fits the %v of a dispatch without the baseline:\n%s", c.name, c.plain, log)
		}
		if _, ok := c.p.WithBaseline().FitWithin(c.plain); ok {
			t.Errorf("%s with the baseline fits %v: the raised budget would be unnecessary", c.name, c.plain)
		}
		if log, ok := c.p.WithBaseline().FitWithin(c.with); !ok {
			t.Errorf("%s with the baseline does not fit the workflow's %v:\n%s", c.name, c.with, log)
		}
	}
	if _, ok := budget.Fast().WithBaseline().FitWithin(24 * time.Hour); !ok {
		t.Error("fast with the baseline no longer fits 24h")
	}
	// The matrix job's ceiling (timeout-minutes) must leave room for the
	// raised full budget plus Deploy and Cleanup.
	var ceiling time.Duration
	for _, x := range regexp.MustCompile(`(?m)^    timeout-minutes: (\d+)$`).FindAllStringSubmatch(src, -1) {
		d, _ := time.ParseDuration(x[1] + "m")
		ceiling = max(ceiling, d)
	}
	if ceiling <= 80*time.Hour {
		t.Errorf("job timeout %v does not leave room above the 80h baseline budget", ceiling)
	}
}
