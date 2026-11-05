//go:build mage

package main

import (
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/c1-harness-h0h8-20261105, not for merge). H8, the dispatcher half of the attribution validators' "exact launched env" rule: every path that can put a key into the
// SUT's environment besides the column's own env is closed or empty by default, and the one open path (the workflow's sut_env input -> BENCH_SUT_ENV -> bench_sut_env) is refused at
// mage start for the arm columns.
func firstWithPrefix(t *testing.T, p string) string {
	t.Helper()
	var best string
	for name := range servers.Registry {
		if strings.HasPrefix(name, p) && (best == "" || name < best) {
			best = name
		}
	}
	if best == "" {
		t.Fatalf("no %s column in the registry", p)
	}
	return best
}

func TestSUTEnvIsRefusedForTheAttributionColumnsOnly(t *testing.T) {
	for _, want := range []string{"c1arm-", "c2x86-", "pinp38-"} { // the protected set is stated here, not derived from the code under test
		if !slices.Contains(armColumnPrefixes, want) {
			t.Errorf("armColumnPrefixes lacks %q", want)
		}
		if err := refuseSUTEnvOnArmColumns([]string{firstWithPrefix(t, want)}, map[string]string{"A": "1"}); err == nil {
			t.Errorf("%s columns are not protected", want)
		}
	}
	env := map[string]string{"CELERIS_IOURING_FIXED_FILES": "1"}
	plain := "celeris-iouring-h1-async"
	for _, p := range armColumnPrefixes {
		arm := firstWithPrefix(t, p)
		if err := refuseSUTEnvOnArmColumns([]string{plain, arm}, env); err == nil || !strings.Contains(err.Error(), arm) || !strings.Contains(err.Error(), "CELERIS_IOURING_FIXED_FILES=1") {
			t.Errorf("%s: sut_env with %s: err = %v, want a refusal naming the column and the key", p, arm, err)
		}
		if err := refuseSUTEnvOnArmColumns([]string{arm}, nil); err != nil {
			t.Errorf("%s: an empty sut_env must pass: %v", p, err)
		}
		if err := refuseSUTEnvOnArmColumns([]string{arm}, map[string]string{}); err != nil {
			t.Errorf("%s: an empty sut_env must pass: %v", p, err)
		}
	}
	// the celeris#585 A/B (a dispatch-wide override on ordinary columns) stays available
	if err := refuseSUTEnvOnArmColumns([]string{plain}, map[string]string{"CELERIS_IOURING_SEND_ZC": "off"}); err != nil {
		t.Errorf("sut_env on non-arm columns must still work: %v", err)
	}
}

// The busy-poll knob may only come from a registry column env: in a dispatch-wide sut_env it would turn the default arm (and the 1a pinp38- main arm) into bp-on, on ANY column.
func TestSUTEnvNeverCarriesTheBusyPollKnob(t *testing.T) {
	for _, slugs := range [][]string{{"celeris-iouring-h1-async"}, nil, {firstWithPrefix(t, "pinp38-")}} {
		err := refuseSUTEnvOnArmColumns(slugs, map[string]string{busyPollKnobEnv: "50us"})
		if err == nil || !strings.Contains(err.Error(), busyPollKnobEnv) {
			t.Errorf("slugs %v: sut_env with the knob: err = %v, want a refusal naming %s", slugs, err, busyPollKnobEnv)
		}
	}
	if busyPollKnobEnv != "PROBATORIUM_SOCKET_BUSY_POLL" {
		t.Error("the knob name changed: review servers/celeris/busypoll_knob.go and the validators")
	}
}

// The slugs checked are the ones the bench will run: an explicit competitors CSV narrows the cells glob (so the celeris#585 A/B on one column still works with a broad glob),
// and a broad glob with no narrowing includes every registered column, the attribution columns among them.
func TestScopeSlugsFollowsTheBenchColumnDerivation(t *testing.T) {
	arm := firstWithPrefix(t, "c1arm-")
	env := map[string]string{"CELERIS_IOURING_SEND_ZC": "off"}
	broad, err := cellsGlobServers("get-simple-512c/*")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(broad, arm) {
		t.Fatalf("the broad cells glob does not include %s: the test premise is wrong", arm)
	}
	if err := refuseSUTEnvOnArmColumns(scopeSlugs("celeris-iouring-h1-async", broad), env); err != nil {
		t.Errorf("an explicit single competitor must narrow the glob: %v", err)
	}
	if err := refuseSUTEnvOnArmColumns(scopeSlugs("all", broad), env); err == nil {
		t.Error("a broad glob with competitors=all includes the arm columns and must be refused")
	}
	if err := refuseSUTEnvOnArmColumns(scopeSlugs("", nil), env); err == nil {
		t.Error("cells '*' (nil slugs) means the full registry and must be refused")
	}
	narrow, err := cellsGlobServers("get-simple-512c/c1arm-*-io512")
	if err != nil {
		t.Fatal(err)
	}
	if err := refuseSUTEnvOnArmColumns(scopeSlugs("", narrow), env); err == nil {
		t.Error("the arm glob itself must be refused")
	}
}

// The workflow reaches the SUT's environment through exactly one dispatch input, and its default is empty; no other workflow or script of the repo sets it.
func TestSUTEnvInputDefaultsToEmptyAndIsTheOnlyPath(t *testing.T) {
	b, err := os.ReadFile(".github/workflows/benchmark-tier.yml")
	if err != nil {
		t.Fatal(err)
	}
	wf := string(b)
	in := regexp.MustCompile(`(?m)^      sut_env:\n(?:        .*\n)+`).FindString(wf)
	if in == "" {
		t.Fatal("benchmark-tier.yml has no sut_env input block")
	}
	if !regexp.MustCompile(`(?m)^        default: ""\s*$`).MatchString(in) || !regexp.MustCompile(`(?m)^        required: false\s*$`).MatchString(in) {
		t.Errorf("the sut_env input must be optional with default \"\":\n%s", in)
	}
	if got := regexp.MustCompile(`(?m)^\s*BENCH_SUT_ENV: .*$`).FindAllString(wf, -1); len(got) != 1 || strings.TrimSpace(got[0]) != "BENCH_SUT_ENV: ${{ github.event.inputs.sut_env || '' }}" {
		t.Errorf("BENCH_SUT_ENV must be set once, from the sut_env input only: %q", got)
	}
	ents, err := os.ReadDir(".github/workflows")
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range ents {
		if e.Name() == "benchmark-tier.yml" {
			continue
		}
		b, err := os.ReadFile(".github/workflows/" + e.Name())
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(b), "BENCH_SUT_ENV") || strings.Contains(string(b), "sut_env") || strings.Contains(string(b), "bench_sut_env") {
			t.Errorf("%s reaches the SUT environment (sut_env / BENCH_SUT_ENV): only benchmark-tier.yml may", e.Name())
		}
	}
	// the playbook's base env carries no key an arm column could be confused with
	pb, err := os.ReadFile("ansible/tasks/run_bench_cell.yml")
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(pb), "PROBATORIUM_SOCKET_BUSY_POLL") || strings.Contains(string(pb), "CELERIS_IOURING_FIXED_FILES") {
		t.Error("run_bench_cell.yml sets a busy-poll or fixed-files knob itself")
	}
}

// The refusal is wired into Bench itself, right after the cells glob is resolved and BEFORE the auto-deploy (a refused dispatch must have touched nothing); resolveBenchColumns has
// one caller, so nothing else can bench the columns.
func TestBenchRefusesSUTEnvForAttributionColumnsBeforeAnythingRuns(t *testing.T) {
	b, err := os.ReadFile("mage_bench.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	g, f := strings.Index(src, "cellsGlobServers(cells)"), strings.Index(src, "refuseSUTEnvOnArmColumns(scopeSlugs(competitors, cellsSlugs), sutEnv)")
	d, w := strings.Index(src, "Deploy(); err != nil"), strings.Index(src, "bench-vars.json")
	if g < 0 || f < g || d < f || w < f {
		t.Errorf("Bench must call refuseSUTEnvOnArmColumns after cellsGlobServers and before the auto-deploy and bench-vars.json (offsets glob %d refuse %d deploy %d vars %d)", g, f, d, w)
	}
	if strings.Count(src, "refuseSUTEnvOnArmColumns(") != 1 {
		t.Error("the refusal must be called exactly once, in Bench")
	}
}
