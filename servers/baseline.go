package servers

import (
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// Baseline columns: the celeris v1.5.11 RELEASE benched in the same run as
// the celeris main under test (maintainer decision U3: the release
// criterion is "same-run interleaved bench vs the v1.5.x baseline", and
// the comparator is the v1.5.11 tag).
//
// They are registry members like any other column, but OPT-IN: while
// BaselineEnvVar is unset they are invisible to every "all" expansion
// (Names, AdaptersSorted, hence the "*/*" grid, the budget pins, the
// default competitor set and the DEPLOY_COMPETITORS=all build set), so a
// routine run costs and publishes exactly what it did before they landed.
// A dispatch that wants them sets BENCH_BASELINE=1 (the benchmark-tier
// workflow's `baseline` input) and gets baselineEngines x the celeris
// scenario grid on top of it; see budget.BaselineRealizedCells for the
// cost. A column named explicitly in a CSV still needs the switch, so a
// typo never silently benches a different set.
//
// Naming. The column slug is "baseline-celeris-v1511-<engine>" and it must
// NOT start with "celeris": goceleris/docs (src/lib/results/taxonomy.ts)
// classes every id that starts with "celeris", and every category
// "celeris", as a Celeris column, so a "celeris-v1511-*" slug would be
// counted among the Celeris results of a published run instead of shown
// as the comparator. The "baseline-" prefix also keeps the usual
// "*/celeris-*" cell globs on the main columns only. The observer must
// still scrape the baseline's /debug/vars exactly like its twin's, so
// ansible/tasks/run_bench_cell.yml names the prefix in its scrape rule
// (^(baseline-)?celeris; baseline_test.go reads that rule).
// Framework is "celeris-baseline" with FrameworkVersion "v1.5.11" (and
// Category "celeris-baseline"), NOT "celeris": the report builders stamp
// the benched celeris build's version on every Framework=="celeris" row
// (mage_bench.go clusterServerMeta, cmd/runner serverMetaFromRegistry), which
// would publish the baseline under the main sha. Engine and Capabilities
// are copied from the main column of the same engine, so the scheduler
// gates the same scenario set for both (featureSetFor reads Engine).

// BaselineEnvVar is the switch that makes the baseline columns part of the
// default expansion. "1" or "true" turns it on.
const BaselineEnvVar = "BENCH_BASELINE"

// BaselineModule is the servers/<dir> module the baseline columns run
// from: celeris pinned at BaselineCelerisVersion.
const BaselineModule = "servers/celeris_v1511"

// BaselineCelerisVersion is the celeris release the baseline module pins.
// servers/baseline_test.go checks it against the module's go.mod and go.sum.
const BaselineCelerisVersion = "v1.5.11"

// baselineEngines are the -engine values (and main-column suffixes) with a
// baseline column: epoll, iouring and adaptive in every protocol flavour
// the main celeris columns have. celeris-std-h1 is left out: the std
// engine is net/http based and carries no loop-engine claim.
var baselineEngines = []string{
	"epoll-h1-sync",
	"epoll-h1-async",
	"epoll-auto+upg-async",
	"iouring-h1-sync",
	"iouring-h1-async",
	"iouring-auto+upg-async",
	"adaptive-h1-async",
	"adaptive-auto+upg-async",
}

// BaselineName is the column slug of the baseline for a celeris engine.
func BaselineName(engine string) string { return BaselinePrefix + engine }

// BaselinePrefix starts every baseline column slug.
const BaselinePrefix = "baseline-celeris-v1511-"

func init() {
	for _, e := range baselineEngines {
		main, ok := Registry["celeris-"+e]
		if !ok {
			panic("servers: baseline engine " + e + " has no main celeris column")
		}
		name := BaselineName(e)
		if _, dup := Registry[name]; dup {
			panic("servers: duplicate baseline column " + name)
		}
		Registry[name] = Adapter{
			Name:             name,
			Category:         "celeris-baseline",
			Language:         main.Language,
			Framework:        "celeris-baseline",
			FrameworkVersion: BaselineCelerisVersion,
			Engine:           main.Engine,
			Bin:              GoBinary{ModuleDir: BaselineModule},
			Capabilities:     main.Capabilities,
			Baseline:         true,
		}
	}
}

// BaselineEnabled reports whether the baseline columns are part of the
// default expansion (BENCH_BASELINE=1|true).
func BaselineEnabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv(BaselineEnvVar))) {
	case "1", "true":
		return true
	}
	return false
}

// BaselineNames returns every baseline column slug, sorted, whether or not
// the switch is on.
func BaselineNames() []string {
	var out []string
	for n, a := range Registry {
		if a.Baseline {
			out = append(out, n)
		}
	}
	sort.Strings(out)
	return out
}

// BaselineModules returns the module directory names (the last element of
// GoBinary.ModuleDir, as mage Deploy names them) the baseline columns run
// from, sorted and unique.
func BaselineModules() []string {
	seen := map[string]bool{}
	for _, a := range Registry {
		if !a.Baseline {
			continue
		}
		if gb, ok := a.Bin.(GoBinary); ok {
			seen[filepath.Base(gb.ModuleDir)] = true
		}
	}
	out := make([]string, 0, len(seen))
	for n := range seen {
		out = append(out, n)
	}
	sort.Strings(out)
	return out
}

// visible reports whether an adapter takes part in the default expansion.
func (a Adapter) visible() bool { return !a.Baseline || BaselineEnabled() }

// InterleaveBaseline reorders a column list so that every baseline column
// whose celeris twin is also in the list sits directly beside it, and
// returns the list otherwise unchanged. mage Bench runs the columns in list
// order (ansible/bench.yml loops competitor_set as given, one column block
// after another), and the list is sorted by name, which would put the eight
// baseline columns about fifteen hours before their twins: same run, but the
// host would drift between the two arms of every pair. Beside each other the
// two are one column block apart (about 1.7 h projected). The pair's
// orientation alternates in sorted order (twin first, baseline first, ...),
// so neither arm is always the second to run.
func InterleaveBaseline(names []string) []string {
	present := make(map[string]bool, len(names))
	for _, n := range names {
		present[n] = true
	}
	twinOf := func(n string) (string, bool) {
		a, ok := Registry[n]
		if !ok || !a.Baseline {
			return "", false
		}
		t := "celeris-" + a.Engine
		return t, present[t]
	}
	beside := map[string]string{} // twin -> its baseline
	order := map[string]int{}     // twin -> index among the paired baselines, in list order
	for _, n := range names {
		if t, ok := twinOf(n); ok {
			order[t] = len(beside)
			beside[t] = n
		}
	}
	out := make([]string, 0, len(names))
	for _, n := range names {
		if _, ok := twinOf(n); ok {
			continue // placed with its twin
		}
		b, paired := beside[n]
		switch {
		case !paired:
			out = append(out, n)
		case order[n]%2 == 0:
			out = append(out, n, b)
		default:
			out = append(out, b, n)
		}
	}
	return out
}
