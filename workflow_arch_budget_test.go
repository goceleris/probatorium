package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"slices"
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
	workflowBudgetIndex  = regexp.MustCompile(`BENCH_BUDGET: \$\{\{ fromJSON\('\{[^']*\}'\)(\[.*\]) \}\}`)
	workflowTimeoutIndex = regexp.MustCompile(`timeout-minutes: \$\{\{ fromJSON\('\{[^']*\}'\)(\[.*\]) \}\}`)
)

// GitHub-hosted-runner-free jobs on a self-hosted runner stop at 5 days.
const selfHostedJobLimit = 5 * 24 * 60 // minutes

// The policy key a dispatch indexes the two tables with, and the profile and
// baseline keys of the budget table. Single-arch targets have their own key:
// they take no lock, and a default that is a strict policy must not send them
// to the strict policy's (much larger) entries.
const (
	wantPolicyIndex   = `[(github.event.inputs.target || 'both') != 'both' && 'single' || github.event.inputs.arch_exclusive || 'all']`
	wantBaselineIndex = `[github.event.inputs.baseline == 'true' && 'true' || 'false']`
	// the validation read of 2026-10-21 (docs/ARCH-EXCLUSION-VALIDATION.md,
	// "Verdict"): P6 failed on arm64, so the default arch policy is `all`.
	wantDefaultPolicy = "all"
	// Deploy (builds, bootstrap) + Cleanup + the docs push on top of BENCH_BUDGET.
	jobOverhead = 6 * time.Hour
)

var tablePolicies = []string{report.ExclusionHeavy, report.ExclusionAll, report.ExclusionOff, "single"}

// TestWorkflowDefaultPolicyIsTheMageDefault: the workflow picks its budget and
// timeout for the policy a dispatch leaves empty, and mage picks the policy it
// runs. If they disagree the budget gate checks one policy and the run uses
// another. The default is `all` (the verdict of 2026-10-21), so the two
// fallbacks of the workflow's tables, the workflow input text and
// resolveArchExclusion's empty case move together; this test is what notices a
// missed one.
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
		if string(m[1]) != wantDefaultPolicy {
			t.Errorf("workflow table fallback is policy %q, the default policy is %q", m[1], wantDefaultPolicy)
		}
		want := "case \"\":\n\t\treturn report.Exclusion" + strings.ToUpper(string(m[1][:1])) + string(m[1][1:]) + ", nil"
		if !strings.Contains(string(mage), want) {
			t.Errorf("workflow falls back to policy %q, resolveArchExclusion's empty case does not return it", m[1])
		}
	}
	// the input text says which policy an empty value is.
	in := regexp.MustCompile(`(?m)^      arch_exclusive:\n        description: "([^"]*)"`).FindSubmatch(src)
	if in == nil {
		t.Fatal("no arch_exclusive input description")
	}
	if !strings.Contains(string(in[1]), "'"+wantDefaultPolicy+"' (DEFAULT when empty)") {
		t.Errorf("the arch_exclusive input text does not name %q as the default when empty:\n%s", wantDefaultPolicy, in[1])
	}
}

// readWorkflowTables returns BENCH_BUDGET (profile -> baseline -> policy key ->
// duration) and timeout-minutes (policy key -> minutes).
func readWorkflowTables(t *testing.T) (budgets map[string]map[string]map[string]string, timeouts map[string]int) {
	t.Helper()
	src, err := os.ReadFile(filepath.Join(".github", "workflows", "benchmark-tier.yml"))
	if err != nil {
		t.Fatal(err)
	}
	m := workflowBudgetTable.FindSubmatch(src)
	if m == nil {
		t.Fatalf("BENCH_BUDGET is not a fromJSON table: a single number per profile cannot admit both `heavy` and `all`")
	}
	if err := json.Unmarshal(m[1], &budgets); err != nil {
		t.Fatalf("BENCH_BUDGET must be a table of profile, baseline ('false'/'true') and arch policy: %v", err)
	}
	m = workflowTimeoutTable.FindSubmatch(src)
	if m == nil {
		t.Fatalf("the benchmark job's timeout-minutes is not a fromJSON table indexed by arch policy: a 6600 minute ceiling cannot hold the full profile under `all`")
	}
	if err := json.Unmarshal(m[1], &timeouts); err != nil {
		t.Fatalf("timeout-minutes table: %v", err)
	}
	return budgets, timeouts
}

// TestWorkflowTablesAreIndexedByTheRightKeys: BENCH_BUDGET is indexed by
// profile, then by the baseline switch (the same predicate BENCH_BASELINE
// uses, so the two cannot disagree), then by the policy key; timeout-minutes
// by the policy key. A single-arch target indexes `single`, never a policy:
// with `all` as the default, an empty arch_exclusive on a single-arch dispatch
// must not land on the strict entries.
func TestWorkflowTablesAreIndexedByTheRightKeys(t *testing.T) {
	src, err := os.ReadFile(filepath.Join(".github", "workflows", "benchmark-tier.yml"))
	if err != nil {
		t.Fatal(err)
	}
	b := workflowBudgetIndex.FindSubmatch(src)
	if b == nil {
		t.Fatal("cannot read the BENCH_BUDGET index expression")
	}
	if want := `[github.event.inputs.profile || 'full']` + wantBaselineIndex + wantPolicyIndex; string(b[1]) != want {
		t.Errorf("BENCH_BUDGET index:\n got %s\nwant %s", b[1], want)
	}
	tm := workflowTimeoutIndex.FindSubmatch(src)
	if tm == nil {
		t.Fatal("cannot read the timeout-minutes index expression")
	}
	if string(tm[1]) != wantPolicyIndex {
		t.Errorf("timeout-minutes index:\n got %s\nwant %s", tm[1], wantPolicyIndex)
	}
	// the baseline key is BENCH_BASELINE's own predicate.
	if !strings.Contains(string(src), "BENCH_BASELINE: ${{ github.event.inputs.baseline == 'true' && '1' || '' }}") {
		t.Error("BENCH_BASELINE's predicate changed: the budget table's baseline index must follow it")
	}
	budgets, timeouts := readWorkflowTables(t)
	if _, ok := timeouts["single"]; !ok {
		t.Error("timeout-minutes has no `single` entry")
	}
	for _, profile := range []string{"fast", "headline", "full"} {
		for _, bl := range []string{"false", "true"} {
			// a single-arch dispatch takes no lock: its entry is the no-lock one.
			if s, o := budgets[profile][bl]["single"], budgets[profile][bl][report.ExclusionOff]; s == "" || s != o {
				t.Errorf("%s baseline=%s: `single` budget %q must be the no-lock (`off`) budget %q", profile, bl, s, o)
			}
		}
	}
}

// TestWorkflowBudgetAdmitsEveryArchPolicy: for every profile, baseline switch
// and policy key the workflow can be dispatched with, either the BENCH_BUDGET
// the workflow passes makes BenchTier's FitWithin accept the run and the job's
// timeout-minutes holds that budget plus Deploy and Cleanup, or the run cannot
// be held by any job (projection + Deploy/Cleanup over the timeout of its
// policy), in which case the table says so by an entry that FitWithin refuses
// and the workflow refuses the dispatch at minute 0
// (TestWorkflowRefusesAGridTheJobCannotHold). The set of refused dispatches is
// pinned: a combination entering or leaving it is a deliberate edit.
func TestWorkflowBudgetAdmitsEveryArchPolicy(t *testing.T) {
	budgets, timeouts := readWorkflowTables(t)
	margins := map[string]float64{}
	var refused []string
	for _, profile := range []string{"fast", "headline", "full"} {
		for _, bl := range []string{"false", "true"} {
			for _, policy := range tablePolicies {
				key := profile + "/baseline=" + bl + "/" + policy
				raw, ok := budgets[profile][bl][policy]
				if !ok {
					t.Errorf("BENCH_BUDGET table has no entry for %s", key)
					continue
				}
				bud, err := time.ParseDuration(raw)
				if err != nil {
					t.Errorf("%s: budget %q: %v", key, raw, err)
					continue
				}
				tm, ok := timeouts[policy]
				if !ok {
					t.Errorf("timeout-minutes table has no entry for policy %q", policy)
					continue
				}
				if tm > selfHostedJobLimit {
					t.Errorf("policy %s: timeout %d min is above the 5 day self-hosted job limit (%d min)", policy, tm, selfHostedJobLimit)
				}
				ceiling := time.Duration(tm) * time.Minute
				// BenchTier's own construction of the profile it gates: arches and
				// lock from the target and policy (a single-arch dispatch has one
				// arch and no lock), the baseline columns on top.
				p := budget.ForProfile(profile)
				p.Runs, p.Arches, p.ArchExclusion = 1, 2, policy
				if policy == "single" {
					p.Arches, p.ArchExclusion = 1, ""
				}
				if bl == "true" {
					p = p.WithBaseline()
				}
				log, fits := p.FitWithin(bud)
				if p.WallClock()+jobOverhead > ceiling {
					// no budget can admit it: the job would be killed first.
					refused = append(refused, key)
					if fits {
						t.Errorf("%s: projects %s, which the job cannot hold (timeout %s, overhead %s), but its budget %s admits it", key, p.WallClock().Round(time.Minute), ceiling, jobOverhead, bud)
					}
					if bud+jobOverhead > ceiling {
						t.Errorf("%s: the refusing budget %s plus %s overhead is above the timeout %s", key, bud, jobOverhead, ceiling)
					}
					continue
				}
				if !fits {
					t.Errorf("%s: workflow budget %s does not fit the run:\n%s", key, bud, log)
					continue
				}
				margins[key] = float64(bud)/float64(p.WallClock()) - 1
				if bud+jobOverhead > ceiling {
					t.Errorf("%s: timeout %s leaves under %s above the budget %s", key, ceiling, jobOverhead, bud)
				}
			}
		}
	}
	slices.Sort(refused)
	if want := []string{"full/baseline=true/all"}; !slices.Equal(refused, want) {
		t.Errorf("dispatches no job can hold: %v, want %v (a change here is a decision: update the refusal step and the PR text)", refused, want)
	}
	// The explicit-heavy dispatch keeps the budgets it had before the default moved.
	for profile, want := range map[string]string{"fast": "24h", "headline": "40h", "full": "69h"} {
		if got := budgets[profile]["false"][report.ExclusionHeavy]; got != want {
			t.Errorf("profile %s policy heavy: budget %s, want %s", profile, got, want)
		}
	}
	for k, m := range margins {
		t.Logf("%-32s budget margin over the model's projection: %+.1f%%", k, 100*m)
		// A budget below the projection cannot pass; one under 5% above it
		// leaves nothing for the model's own error (it ran 7% high on
		// bench-full 38431303162, so a negative error is not ruled out).
		if m < 0.05 {
			t.Errorf("%s: margin %.1f%% is under the 5%% floor", k, 100*m)
		}
	}
}

// TestWorkflowRefusesAGridTheJobCannotHold: full + baseline + target=both +
// policy all projects 130h55m, above the 7200 minute job limit, so no
// BENCH_BUDGET admits it. FitWithin would refuse it, but only after mage
// Deploy has built the baseline module: a step before Deploy refuses it at
// minute 0, for exactly the dispatches the table pins as unholdable.
func TestWorkflowRefusesAGridTheJobCannotHold(t *testing.T) {
	src := readWorkflow(t, "benchmark-tier.yml")
	const cond = `if: ${{ (github.event.inputs.target || 'both') == 'both' && (github.event.inputs.arch_exclusive || 'all') == 'all' && github.event.inputs.baseline == 'true' && (github.event.inputs.profile || 'full') == 'full' }}`
	i := strings.Index(src, cond)
	if i < 0 {
		t.Fatalf("no step guarded by\n%s", cond)
	}
	d := strings.Index(src, "- name: mage Deploy")
	if d < 0 || i > d {
		t.Error("the refusal step must come before mage Deploy (which builds the baseline module)")
	}
	rest := src[i:]
	if j := strings.Index(rest, "\n      - name:"); j > 0 {
		rest = rest[:j]
	}
	for _, want := range []string{"::error::", "exit 1", "130h55m"} {
		if !strings.Contains(rest, want) {
			t.Errorf("the refusal step lacks %q:\n%s", want, rest)
		}
	}
}
