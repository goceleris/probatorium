package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/report"
)

// The workflow's BENCH_BUDGET and the job's timeout-minutes must admit every
// arch policy the arch_exclusive input accepts (probatorium#442). Until the
// budget was a function of the policy it was one number per profile, set for
// `heavy`: BenchTier's FitWithin gate refused `all` on every profile
// (headline 59h41m against 40h, full 101h55m against 69h, fast 28h against
// 24h), so the strict option the maintainer decision and celeris#892 name as
// the fallback could not be dispatched.

var (
	workflowBudgetTable  = regexp.MustCompile(`BENCH_BUDGET: \$\{\{ fromJSON\('(\{[^']*\})'\)\[`)
	workflowTimeoutTable = regexp.MustCompile(`timeout-minutes: \$\{\{ fromJSON\('(\{[^']*\})'\)\[`)
)

// GitHub-hosted-runner-free jobs on a self-hosted runner stop at 5 days.
const selfHostedJobLimit = 5 * 24 * 60 // minutes

// TestWorkflowDefaultPolicyIsTheMageDefault: the workflow picks its budget and
// timeout for the policy a dispatch leaves empty, and mage picks the policy it
// runs. If they disagree the budget gate checks one policy and the run uses
// another. Changing the default to `all` therefore means this literal, the
// workflow input text and resolveArchExclusion together; this test is what
// notices a missed one.
func TestWorkflowDefaultPolicyIsTheMageDefault(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".github", "workflows", "benchmark-tier.yml"))
	if err != nil {
		t.Fatal(err)
	}
	re := regexp.MustCompile(`github\.event\.inputs\.arch_exclusive \|\| '([a-z]+)'\]`)
	ms := re.FindAllSubmatch(src, -1)
	if len(ms) != 2 {
		t.Fatalf("want the policy fallback in the budget and timeout expressions (2), found %d", len(ms))
	}
	mage, err := os.ReadFile("mage_bench_archexcl.go")
	if err != nil {
		t.Fatal(err)
	}
	for _, m := range ms {
		want := "case \"\":\n\t\treturn report.Exclusion" + strings.ToUpper(string(m[1][:1])) + string(m[1][1:]) + ", nil"
		if !strings.Contains(string(mage), want) {
			t.Errorf("workflow falls back to policy %q, resolveArchExclusion's empty case does not return it", m[1])
		}
	}
}

func readWorkflowTables(t *testing.T) (budgets map[string]map[string]string, timeouts map[string]int) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(".github", "workflows", "benchmark-tier.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m := workflowBudgetTable.FindSubmatch(src)
	if m == nil {
		t.Fatalf("BENCH_BUDGET is not a fromJSON table indexed by profile and arch policy: a single number per profile cannot admit both `heavy` and `all`")
	}
	if err := json.Unmarshal(m[1], &budgets); err != nil {
		t.Fatalf("BENCH_BUDGET table: %v", err)
	}
	// the headline job (the one that runs BenchTier) is the first with the table.
	m = workflowTimeoutTable.FindSubmatch(src)
	if m == nil {
		t.Fatalf("the benchmark job's timeout-minutes is not a fromJSON table indexed by arch policy: a 6600 minute ceiling cannot hold the full profile under `all`")
	}
	if err := json.Unmarshal(m[1], &timeouts); err != nil {
		t.Fatalf("timeout-minutes table: %v", err)
	}
	return budgets, timeouts
}

// TestWorkflowBudgetAdmitsEveryArchPolicy: for every profile and every policy
// the workflow can be dispatched with, the BENCH_BUDGET the workflow passes
// makes BenchTier's FitWithin accept a two-arch run, and the job's
// timeout-minutes holds that budget plus Deploy and Cleanup.
func TestWorkflowBudgetAdmitsEveryArchPolicy(t *testing.T) {
	budgets, timeouts := readWorkflowTables(t)
	const overhead = 6 * time.Hour // Deploy (builds, bootstrap) + Cleanup + the docs push
	margins := map[string]float64{}
	for _, profile := range []string{"fast", "headline", "full"} {
		for _, policy := range []string{report.ExclusionHeavy, report.ExclusionAll, report.ExclusionOff} {
			raw, ok := budgets[profile][policy]
			if !ok {
				t.Errorf("BENCH_BUDGET table has no entry for profile %q policy %q", profile, policy)
				continue
			}
			bud, err := time.ParseDuration(raw)
			if err != nil {
				t.Errorf("profile %s policy %s: budget %q: %v", profile, policy, raw, err)
				continue
			}
			// BenchTier's own construction of the profile it gates.
			p := budget.ForProfile(profile)
			p.Runs, p.Arches, p.ArchExclusion = 1, 2, policy
			log, fits := p.FitWithin(bud)
			if !fits {
				t.Errorf("profile %s policy %s: workflow budget %s does not fit a both-arch run:\n%s", profile, policy, bud, log)
				continue
			}
			margins[profile+"/"+policy] = float64(bud)/float64(p.WallClock()) - 1
			tm, ok := timeouts[policy]
			if !ok {
				t.Errorf("timeout-minutes table has no entry for policy %q", policy)
				continue
			}
			if got := time.Duration(tm) * time.Minute; got < bud+overhead {
				t.Errorf("profile %s policy %s: timeout %s leaves under %s above the budget %s", profile, policy, got, overhead, bud)
			}
			if tm > selfHostedJobLimit {
				t.Errorf("policy %s: timeout %d min is above the 5 day self-hosted job limit (%d min)", policy, tm, selfHostedJobLimit)
			}
		}
	}
	// The pre-existing dispatches keep their budgets (heavy / unset).
	for profile, want := range map[string]string{"fast": "24h", "headline": "40h", "full": "69h"} {
		if got := budgets[profile][report.ExclusionHeavy]; got != want {
			t.Errorf("profile %s policy heavy: budget %s, want %s (the default dispatch must not change)", profile, got, want)
		}
	}
	for k, m := range margins {
		t.Logf("%-14s budget margin over the model's projection: %+.1f%%", k, 100*m)
		// A budget below the projection cannot pass; one under 5% above it
		// leaves nothing for the model's own error (it ran 7% high on
		// bench-full 38431303162, so a negative error is not ruled out).
		if m < 0.05 {
			t.Errorf("%s: margin %.1f%% is under the 5%% floor", k, 100*m)
		}
	}
}
