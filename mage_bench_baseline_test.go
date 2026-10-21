//go:build mage

package main

import (
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/servers"
)

// TestBaselineColumnsOnlyJoinTheDefaultSweepWithTheSwitch: "all" expands to
// the registry minus the opt-in baseline columns; with BENCH_BASELINE=1 it
// also holds the eight baseline-celeris-v1511-* columns, each staged from
// competitors/celeris_v1511 and launched with the same -engine value as its
// main twin.
func TestBaselineColumnsOnlyJoinTheDefaultSweepWithTheSwitch(t *testing.T) {
	slug := func(cols []benchColumn) (out []string) {
		for _, c := range cols {
			out = append(out, c.Slug)
		}
		return out
	}
	t.Setenv(servers.BaselineEnvVar, "")
	off, err := resolveBenchColumns("all")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range servers.BaselineNames() {
		if slices.Contains(slug(off), n) {
			t.Errorf("%s in the default column set", n)
		}
	}

	t.Setenv(servers.BaselineEnvVar, "1")
	on, err := resolveBenchColumns("all")
	if err != nil {
		t.Fatal(err)
	}
	if len(on) != len(off)+len(servers.BaselineNames()) {
		t.Errorf("columns with the switch: %d, without %d, baseline %d", len(on), len(off), len(servers.BaselineNames()))
	}
	for _, c := range on {
		if !servers.Registry[c.Slug].Baseline {
			continue
		}
		twin := servers.Registry[strings.Replace(c.Slug, servers.BaselinePrefix, "celeris-", 1)]
		if c.Bin != "celeris_v1511" || c.Engine != twin.Engine || c.Engine == "" {
			t.Errorf("%s: bin %q engine %q, want celeris_v1511 and the twin's %q", c.Slug, c.Bin, c.Engine, twin.Engine)
		}
	}
}

// TestBaselineColumnNamedWithoutTheSwitchIsRefused: a CSV naming a baseline
// column while the switch is off fails loudly (with the hint), it neither
// silently drops the column nor benches it behind the workflow's back; with
// the switch on the same CSV resolves to exactly those columns.
func TestBaselineColumnNamedWithoutTheSwitchIsRefused(t *testing.T) {
	name := servers.BaselineName("epoll-h1-async")
	t.Setenv(servers.BaselineEnvVar, "")
	_, err := resolveBenchColumns(name + ",celeris-epoll-h1-async")
	if err == nil || !strings.Contains(err.Error(), servers.BaselineEnvVar) || !strings.Contains(err.Error(), "baseline=true") {
		t.Fatalf("want an error naming %s and the workflow input, got %v", servers.BaselineEnvVar, err)
	}
	t.Setenv(servers.BaselineEnvVar, "true")
	cols, err := resolveBenchColumns(name + ",celeris-epoll-h1-async")
	if err != nil || len(cols) != 2 {
		t.Fatalf("with the switch: %v, %v", cols, err)
	}
	if cols[0].Slug != "celeris-epoll-h1-async" || cols[1].Slug != name { // the twin first, the baseline beside it
		t.Errorf("columns = %+v", cols)
	}
}

// TestBaselineCellsGlobDerivation: the profile glob derives the baseline
// columns only with the switch, and a glob that names them derives them.
func TestBaselineCellsGlobDerivation(t *testing.T) {
	cells := budget.CellsGlob(budget.Full())
	t.Setenv(servers.BaselineEnvVar, "")
	off, err := cellsGlobServers(cells)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(off, servers.Names()) {
		t.Fatalf("default derivation drifted from the registry")
	}
	for _, n := range servers.BaselineNames() {
		if slices.Contains(off, n) {
			t.Errorf("%s derived with the switch off", n)
		}
	}
	t.Setenv(servers.BaselineEnvVar, "1")
	on, err := cellsGlobServers(cells)
	if err != nil {
		t.Fatal(err)
	}
	if len(on) != len(off)+len(servers.BaselineNames()) {
		t.Errorf("derived %d columns with the switch, %d without", len(on), len(off))
	}
	got, err := cellsGlobServers("get-simple/" + servers.BaselinePrefix + "*")
	if err != nil || len(got) != len(servers.BaselineNames()) {
		t.Errorf("glob baseline-celeris-v1511-*: %v, %v", got, err)
	}
}

// TestDeployAllLeavesTheBaselineModuleOut: DEPLOY_COMPETITORS=all builds
// the baseline module only with the switch; naming it builds it either way.
func TestDeployAllLeavesTheBaselineModuleOut(t *testing.T) {
	t.Setenv(servers.BaselineEnvVar, "")
	for _, spec := range []string{"", "all", "go-only"} {
		got, err := selectGoCompetitors(spec)
		if err != nil {
			t.Fatal(err)
		}
		if slices.Contains(got, "celeris_v1511") || !slices.Contains(got, "celeris") {
			t.Errorf("%q without the switch: %v", spec, got)
		}
	}
	got, err := selectGoCompetitors("celeris,celeris_v1511")
	if err != nil || !slices.Equal(got, []string{"celeris", "celeris_v1511"}) {
		t.Errorf("explicit CSV: %v, %v", got, err)
	}
	t.Setenv(servers.BaselineEnvVar, "1")
	got, err = selectGoCompetitors("all")
	if err != nil || !slices.Contains(got, "celeris_v1511") || !slices.Contains(got, "celeris") {
		t.Errorf("all with the switch: %v, %v", got, err)
	}
}

// TestBaselineProjectionIsAppliedByBenchTier: the budget gate projects the
// baseline cells only with the switch.
func TestBaselineProjectionIsAppliedByBenchTier(t *testing.T) {
	p := budget.Full()
	t.Setenv(servers.BaselineEnvVar, "")
	if got := baselineProjection(p); got.Cells != p.Cells || got.RatedCells != p.RatedCells {
		t.Errorf("switch off changed the projection: %d/%d", got.Cells, got.RatedCells)
	}
	t.Setenv(servers.BaselineEnvVar, "1")
	if got := baselineProjection(p); got.Cells != p.Cells+budget.BaselineRealizedCells || got.RatedCells != p.RatedCells+budget.BaselineRatedRealizedCells {
		t.Errorf("switch on: %d/%d", got.Cells, got.RatedCells)
	}
}

// TestBaselinePublishesItsOwnVersion: the published row of a baseline column
// carries framework_version v1.5.11 and a framework that is not "celeris",
// while the main celeris column of the same run keeps the benched build's
// version. (Framework=="celeris" rows are stamped with the run's celeris
// version, which would have published the baseline under the main sha, and
// are held to the framework-under-test integrity checks of mage Publish.)
func TestBaselinePublishesItsOwnVersion(t *testing.T) {
	cells := map[string]*report.CellResult{
		servers.BaselineName("epoll-h1-async") + "|get-simple": {},
		"celeris-epoll-h1-async|get-simple":                    {},
	}
	meta := clusterServerMeta(cells, "v1.5.12-0.20261020184830-d1a105bb12eb", "arm64")
	b := meta[servers.BaselineName("epoll-h1-async")]
	if b.FrameworkVersion != "v1.5.11" || b.Framework == "celeris" || b.Category == "celeris" || b.Engine != "epoll-h1-async" {
		t.Errorf("baseline meta = %+v", b)
	}
	m := meta["celeris-epoll-h1-async"]
	if m.FrameworkVersion != "v1.5.12-0.20261020184830-d1a105bb12eb" || m.Framework != "celeris" {
		t.Errorf("main meta changed: %+v", m)
	}
}

// TestBaselineColumnsRunBesideTheirTwins: the column order mage hands the
// playbook (competitor_set, looped as given) has each baseline directly
// beside its twin, not in the "baseline-" block of the sorted list.
func TestBaselineColumnsRunBesideTheirTwins(t *testing.T) {
	t.Setenv(servers.BaselineEnvVar, "1")
	cols, err := resolveBenchColumns("all")
	if err != nil {
		t.Fatal(err)
	}
	pos := map[string]int{}
	for i, c := range cols {
		pos[c.Slug] = i
	}
	for _, n := range servers.BaselineNames() {
		d := pos[n] - pos["celeris-"+servers.Registry[n].Engine]
		if d != 1 && d != -1 {
			t.Errorf("%s runs %d columns away from its twin", n, d)
		}
	}
	t.Setenv(servers.BaselineEnvVar, "")
	off, err := resolveBenchColumns("all")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.IsSortedFunc(off, func(a, b benchColumn) int { return strings.Compare(a.Slug, b.Slug) }) {
		t.Error("without the switch the columns are no longer in sorted order")
	}
}

// TestBaselineHangGuardIsSizedFromTheRealGrid: the hang-guard sizing runs the
// runner as a subprocess; it must see the baseline columns (it inherits
// BENCH_BASELINE) and give a baseline column its real cell count, not the
// one-scenario floor that would time out every baseline column.
func TestBaselineHangGuardIsSizedFromTheRealGrid(t *testing.T) {
	t.Setenv(servers.BaselineEnvVar, "1")
	auto := servers.BaselineName("epoll-auto+upg-async")
	got, err := benchMaxScenariosPerColumn("*/*", []string{auto})
	if err != nil {
		t.Fatal(err)
	}
	twin, err := benchMaxScenariosPerColumn("*/*", []string{"celeris-epoll-auto+upg-async"})
	if err != nil {
		t.Fatal(err)
	}
	if got != 29 || got != twin {
		t.Errorf("baseline %s sized for %d scenarios, its twin %d, want 29", auto, got, twin)
	}
}
