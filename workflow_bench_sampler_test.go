package main

import (
	"encoding/json"
	"os/exec"
	"regexp"
	"strings"
	"testing"
	"time"
)

// The bench tier ran no host sampler. `mage HostSamplerStart` was called by the
// race and weekend tiers only (probatorium#481), so a bench run had no series
// of its own: the forensics artifact fetched whatever the last weekend soak
// left in /var/log/celeris-host-sampler, and the 60 s cadence of that sampler
// could not have seen the 1-3 s stalls of the io512 cell (celeris#1036). The
// bench tier now brackets BenchTier with the sampler (full sample every 60 s)
// and with the fine sampler (SAMPLER_FINE_INTERVAL, default 1 s).
//
// The nightly tier is not in this file on purpose: it runs `mage Validate`,
// not BenchTier, and shares the race tier's path, which has its own pin
// (workflow_race_sampler_test.go).

const benchTier = ".github/workflows/benchmark-tier.yml"

// GOVAB (THROWAWAY ab/gov-ab-20261122, NAMED DEVIATION from the pins of main, never merged): the benchmark step is ONE `run:` step that calls the arm-loop script, which runs
// `mage BenchTier` itself (once for a plain dispatch, once per CPU power arm for a gab dispatch). The pins below keep their intent: one sampler start before the benchmark step,
// the stop after it, the BenchTier step (the one with BENCH_BUDGET and DOCS_TOKEN) free of sampler variables. TestGovabArmLoop* pin the new structure.
const (
	benchStepRun = "bash .github/scripts/gov-ab-arms.sh"
	govabScript  = ".github/scripts/gov-ab-arms.sh"
)

func TestBenchTierBracketsTheRunWithTheSampler(t *testing.T) {
	src := mustRead(t, benchTier)
	deploy := mustIndex(t, src, "run: mage Deploy\n", "explicit mage Deploy step")
	start := mustIndex(t, src, "run: mage HostSamplerStart\n", "sampler start step")
	bench := mustIndex(t, src, "run: "+benchStepRun+"\n", "benchmark step (GOVAB: the arm loop script, which runs mage BenchTier)")
	stop := mustIndex(t, src, "run: mage HostSamplerStop\n", "sampler stop step")
	upload := mustIndex(t, src, "name: Upload results", "results upload step")
	cleanup := mustIndex(t, src, "run: mage Cleanup", "mage Cleanup step")
	if start < deploy {
		t.Errorf("the sampler starts before `mage Deploy` (offsets %d < %d): Deploy would run beside a sampler it does not need to", start, deploy)
	}
	if start > bench {
		t.Errorf("the sampler starts after `mage BenchTier` (offsets %d > %d)", start, bench)
	}
	if stop < bench {
		t.Errorf("the sampler stops before the end of `mage BenchTier` (offsets %d < %d)", stop, bench)
	}
	if stop > upload {
		t.Errorf("the sampler stops after the results upload (offsets %d > %d): its series would miss the artifact", stop, upload)
	}
	// Cleanup purges results_root on the hosts; the series must be fetched
	// before anything is reclaimed.
	if stop > cleanup {
		t.Errorf("the sampler stops after `mage Cleanup` (offsets %d > %d)", stop, cleanup)
	}
	if n := strings.Count(src, "run: mage HostSamplerStart"); n != 1 {
		t.Errorf("%d sampler start steps, want exactly 1", n)
	}
}

// Instrumentation must not be able to fail a multi-day bench, and the stop
// step must run (and fetch) when the bench failed.
func TestBenchSamplerStepsCannotFailTheRun(t *testing.T) {
	src := mustRead(t, benchTier)
	// the stop fetches the compressed series through ansible's slurp (in memory
	// under become): 10 minutes, deliberately; the start stays at 5
	for run, bound := range map[string]string{"mage HostSamplerStart": "5", "mage HostSamplerStop": "10"} {
		blk := stepRunning(src, run)
		if blk == "" {
			t.Fatalf("no step runs %q", run)
		}
		if !strings.Contains(blk, "continue-on-error: true") {
			t.Errorf("%s is not continue-on-error:\n%s", run, blk)
		}
		if !strings.Contains(blk, "timeout-minutes: "+bound+"\n") {
			t.Errorf("%s has no timeout-minutes: %s:\n%s", run, bound, blk)
		}
	}
	if blk := stepRunning(src, "mage HostSamplerStop"); !strings.Contains(blk, "        if: always()\n") {
		t.Errorf("the stop step is skipped when the bench fails:\n%s", blk)
	}
	if blk := stepRunning(src, "mage HostSamplerStart"); strings.Contains(blk, "if: always()") {
		t.Errorf("the start step runs after a failed Deploy:\n%s", blk)
	}
}

// The series is tagged with this run, never with the weekend's or the race
// tier's tag, so a series that outlives a run can be cut into the run that
// made it.
func TestBenchSamplerIsTaggedForThisRun(t *testing.T) {
	src := mustRead(t, benchTier)
	// the stop names the same run AND attempt (a re-run keeps the run id: attempt 2 must not
	// fetch attempt 1's series when its Deploy fails): the fine series is fetched only if its
	// fine-run.txt carries the tag, so a failed Deploy (no start) fetches nothing
	for _, run := range []string{"mage HostSamplerStart", "mage HostSamplerStop"} {
		blk := stepRunning(src, run)
		if !strings.Contains(blk, "SAMPLER_TAG: bench-${{ github.run_id }}-${{ github.run_attempt }}\n") {
			t.Errorf("%s does not tag the series with this run:\n%s", run, blk)
		}
		for _, other := range []string{"weekend-", "race-"} {
			if strings.Contains(blk, other) {
				t.Errorf("the bench sampler carries another tier's tag %q:\n%s", other, blk)
			}
		}
	}
}

// Only the bench tier launches the 60 s shell sampler under SCHED_IDLE.
func TestBenchSamplerLaunchesTheShellSamplerAtLowPriority(t *testing.T) {
	if blk := stepRunning(mustRead(t, benchTier), "mage HostSamplerStart"); !strings.Contains(blk, "SAMPLER_LOW_PRIORITY: \"1\"\n") {
		t.Errorf("the bench start step does not set SAMPLER_LOW_PRIORITY=1:\n%s", blk)
	}
	for _, wf := range []string{raceTier, weekendWorkflow} {
		if strings.Contains(mustRead(t, wf), "SAMPLER_LOW_PRIORITY") {
			t.Errorf("%s sets SAMPLER_LOW_PRIORITY: the race and weekend starts are meant to stay what they were", wf)
		}
	}
}

var (
	samplerDurationTable = regexp.MustCompile(`SAMPLER_DURATION: \$\{\{ fromJSON\('(\{[^']*\})'\)(\[.*\]) \}\}\n`)
	samplerFineInput     = regexp.MustCompile(`SAMPLER_FINE_INTERVAL: \$\{\{ github\.event\.inputs\.sampler_interval \|\| '1' \}\}\n`)
)

// SAMPLER_DURATION is the sampler's own hard deadline: the run's end if the
// stop step runs, a bound if it cannot (the runner died). It is keyed like the
// job's timeout-minutes, and for every key it covers the largest budget the
// workflow admits for that key (plus 3 h for the work between the start step
// and the end of the run) and ends inside the job ceiling. A key the
// sampler cuts short loses the tail of the series, not the run; a key that
// outlives the ceiling leaves a sampler writing on the hosts into the next tier.
func TestBenchSamplerIsBoundedByTheBudgetAndTheJob(t *testing.T) {
	src := mustRead(t, benchTier)
	blk := stepRunning(src, "mage HostSamplerStart")
	m := samplerDurationTable.FindStringSubmatch(blk)
	if m == nil {
		t.Fatalf("SAMPLER_DURATION is not a fromJSON table in the start step:\n%s", blk)
	}
	if want := wantPolicyIndex; m[2] != want {
		t.Errorf("SAMPLER_DURATION index:\n got %s\nwant %s", m[2], want)
	}
	var durations map[string]string
	if err := json.Unmarshal([]byte(m[1]), &durations); err != nil {
		t.Fatalf("SAMPLER_DURATION table: %v", err)
	}
	budgets, timeouts := readWorkflowTables(t)
	if len(durations) != len(timeouts) {
		t.Errorf("SAMPLER_DURATION has keys %v, the job timeout has %v", benchSamplerKeys(durations), benchSamplerKeys(timeouts))
	}
	for key, ceiling := range timeouts {
		got, ok := durations[key]
		if !ok {
			t.Errorf("SAMPLER_DURATION has no entry for policy key %q", key)
			continue
		}
		d, err := time.ParseDuration(got)
		if err != nil || !strings.HasSuffix(got, "m") {
			t.Errorf("SAMPLER_DURATION[%s] = %q: want whole minutes", key, got)
			continue
		}
		var largest time.Duration
		for profile, byBaseline := range budgets {
			for baseline, byKey := range byBaseline {
				// full + baseline + all is refused by the setup job.
				if profile == "full" && baseline == "true" && key == "all" {
					continue
				}
				b, err := time.ParseDuration(byKey[key])
				if err != nil {
					t.Fatalf("budget %s/%s/%s: %v", profile, baseline, key, err)
				}
				largest = max(largest, b)
			}
		}
		if d < largest+3*time.Hour {
			t.Errorf("SAMPLER_DURATION[%s] = %s does not cover the largest admissible budget %s plus 3h: the tail of a long run would have no series", key, d, largest)
		}
		if d+10*time.Minute > time.Duration(ceiling)*time.Minute {
			t.Errorf("SAMPLER_DURATION[%s] = %s leaves under 10 minutes inside the job's %dm ceiling: a killed job would leave a sampler running into the next tier", key, d, ceiling)
		}
	}
}

func benchSamplerKeys[V any](m map[string]V) []string {
	var ks []string
	for k := range m {
		ks = append(ks, k)
	}
	return ks
}

// The cadence: a dispatch input, default 1 s, also the default of a scheduled
// run (which has no inputs, so the expression carries the fallback). The full
// sample stays at the sampler's own 60 s: the bench step does not set
// SAMPLER_INTERVAL.
func TestBenchSamplerCadenceIsAnInputDefaultingToOneSecond(t *testing.T) {
	src := mustRead(t, benchTier)
	blk := stepRunning(src, "mage HostSamplerStart")
	if !samplerFineInput.MatchString(blk) {
		t.Errorf("the start step does not set SAMPLER_FINE_INTERVAL from the sampler_interval input with a default of 1:\n%s", blk)
	}
	if strings.Contains(blk, "SAMPLER_INTERVAL") {
		t.Errorf("the bench step changes the full sampler's 60 s cadence:\n%s", blk)
	}
	in := regexp.MustCompile(`(?m)^      sampler_interval:\n        description: "([^"]*)"\n        required: false\n        type: string\n        default: "1"\n`).FindStringSubmatch(src)
	if in == nil {
		t.Fatal("no `sampler_interval` dispatch input (string, default \"1\")")
	}
	for _, want := range []string{"0.05", "1 s", "% of one CPU at 1 s", "% at 0.05 s (20 Hz)", "SCHED_IDLE", "65 h", "per-minute record"} {
		if !strings.Contains(in[1], want) {
			t.Errorf("the sampler_interval input text lacks %q (it must state the load it adds):\n%s", want, in[1])
		}
	}
}

// The value is validated in the setup job, before any runner is provisioned,
// by the same `check` allowlist as the other inputs. The allowlist is a subset
// of what mage accepts (mage_sampler.go: 0.05 to 10 s).
func TestBenchSamplerIntervalIsValidatedBeforeProvisioning(t *testing.T) {
	script := validateStep(t)
	if !strings.Contains(script, "IN_SAMPLER_INTERVAL") {
		t.Fatal("the validate step does not read the sampler_interval input")
	}
	if !strings.Contains(mustRead(t, benchTier), "IN_SAMPLER_INTERVAL: ${{ github.event.inputs.sampler_interval }}") {
		t.Error("IN_SAMPLER_INTERVAL is not the sampler_interval input")
	}
	run := func(v string) error {
		cmd := exec.Command("bash", "-e", "-c", script)
		cmd.Env = []string{"PATH=" + pathEnv(), "IN_SAMPLER_INTERVAL=" + v}
		out, err := cmd.CombinedOutput()
		t.Logf("%q -> %v\n%s", v, err, out)
		return err
	}
	for _, ok := range []string{"", "1", "0.5", "0.2", "0.1", "0.05"} {
		if err := run(ok); err != nil {
			t.Errorf("sampler_interval=%q refused: %v", ok, err)
		}
	}
	for _, bad := range []string{"0", "0.01", "0.049", "2", "10", "60", " 1", "1 ", "1.0", "1s", "abc", "1;x", "off", "-1", "0.050"} {
		if err := run(bad); err == nil {
			t.Errorf("sampler_interval=%q accepted", bad)
		}
	}
}

// BENCH_TARGET is a job-level variable, and the sampler playbook reads it
// (mage_sampler.go) to select the run's hosts only. The start step must not
// shadow it.
func TestBenchSamplerStepsDoNotShadowTheTarget(t *testing.T) {
	src := mustRead(t, benchTier)
	for _, run := range []string{"mage HostSamplerStart", "mage HostSamplerStop"} {
		if blk := stepRunning(src, run); strings.Contains(blk, "BENCH_TARGET") {
			t.Errorf("%s sets BENCH_TARGET itself:\n%s", run, blk)
		}
	}
	if !regexp.MustCompile(`(?m)^      BENCH_TARGET: \$\{\{ github\.event\.inputs\.target \|\| 'both' \}\}$`).MatchString(src) {
		t.Error("BENCH_TARGET is no longer a job-level variable the sampler steps inherit")
	}
}

// What the PR claims about the measurement path, pinned: the sampler steps
// are the only ones between Deploy and the upload that do not belong to the
// bench, and the BenchTier step itself is untouched by them (it is still the
// one that carries BENCH_BUDGET and DOCS_TOKEN, and no sampler variable).
func TestBenchTierStepCarriesNoSamplerVariable(t *testing.T) {
	blk := stepRunning(mustRead(t, benchTier), benchStepRun)
	if strings.Contains(blk, "SAMPLER_") {
		t.Errorf("the BenchTier step carries a sampler variable; the sampler must run beside the bench, not inside it:\n%s", blk)
	}
	if !strings.Contains(blk, "BENCH_BUDGET:") {
		t.Errorf("stepRunning(mage BenchTier) no longer finds the BenchTier step:\n%s", blk)
	}
}

func pathEnv() string {
	p, _ := exec.LookPath("bash")
	if p == "" {
		return "/usr/bin:/bin"
	}
	return p[:strings.LastIndex(p, "/")] + ":/usr/bin:/bin"
}

// GOVAB: the arm loop is ONE step, the restore is an always-run step right after it (before the sampler stop, the upload and Cleanup), and the script runs `mage BenchTier` from exactly
// two places: the plain dispatch (exec) and the arm loop (under a timeout).
func TestGovabArmLoopStructure(t *testing.T) {
	src := mustRead(t, benchTier)
	if n := strings.Count(src, "run: "+benchStepRun+"\n"); n != 1 {
		t.Fatalf("%d arm-loop steps, want exactly 1", n)
	}
	if strings.Contains(src, "run: mage BenchTier") {
		t.Error("a bare `run: mage BenchTier` step is back: the benchmark must go through the arm-loop script")
	}
	blk := stepRunning(src, benchStepRun)
	if !strings.Contains(blk, "timeout-minutes: ${{ startsWith(github.event.inputs.competitors, 'gab') && 300 || 6600 }}\n") {
		t.Errorf("the arm-loop step has no timeout bounding a hung gab dispatch:\n%s", blk)
	}
	rest := stepRunning(src, benchStepRun+" restore")
	if rest == "" || !strings.Contains(rest, "        if: always()\n") {
		t.Fatalf("no always-run restore step:\n%s", rest)
	}
	if !strings.Contains(rest, "timeout-minutes:") {
		t.Errorf("the restore step has no timeout:\n%s", rest)
	}
	bench := strings.Index(src, "run: "+benchStepRun+"\n")
	restore := strings.Index(src, "run: "+benchStepRun+" restore\n")
	stop := mustIndex(t, src, "run: mage HostSamplerStop\n", "sampler stop step")
	upload := mustIndex(t, src, "name: Upload results", "results upload step")
	cleanup := mustIndex(t, src, "run: mage Cleanup", "mage Cleanup step")
	if !(bench < restore && restore < stop && stop < upload && upload < cleanup) {
		t.Errorf("step order wrong: bench %d restore %d sampler stop %d upload %d cleanup %d", bench, restore, stop, upload, cleanup)
	}
	script := mustRead(t, govabScript)
	if n := strings.Count(script, `"$MAGE" BenchTier`); n != 2 {
		t.Errorf("%d BenchTier call sites in the arm-loop script, want 2 (the plain exec and the arm loop)", n)
	}
	if !strings.Contains(script, "trap on_exit EXIT") || !strings.Contains(script, "trap 'exit 143' TERM") {
		t.Error("the arm-loop script has no restore trap on EXIT / TERM")
	}
	for _, arm := range []string{"as-found", "performance", "performance-c2off"} {
		if !strings.Contains(mustRead(t, ".github/gov-ab-arms.tsv"), arm) {
			t.Errorf("arm %s is in no table line", arm)
		}
	}
}

// The dispatcher (evidence/lanes-20261105/H8/cluster-sequence-v160.sh) refuses any workflow_dispatch field outside its list: the arm loop must be selected by a field it already
// sends, so no new input is declared for it.
func TestGovabArmsAreSelectedByTheCompetitorsValueNotByANewInput(t *testing.T) {
	src := mustRead(t, benchTier)
	if strings.Contains(src, "cpu_power_arms") {
		t.Error("benchmark-tier.yml declares a cpu_power_arms input: the cluster dispatcher would refuse the queue line")
	}
	for _, line := range strings.Split(mustRead(t, ".github/gov-ab-arms.tsv"), "\n") {
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		f := strings.Split(line, "\t")
		if len(f) != 4 || !strings.HasPrefix(f[0], "gab") {
			t.Errorf("table line %q: want 4 TAB fields and a competitors value that starts with gab", line)
		}
	}
}
