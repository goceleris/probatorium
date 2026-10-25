//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
)

// Host-sampler targets (probatorium#354).
//
// The weekend soak 34736980002 died at 20 h 05 m of 24 h with "The
// self-hosted runner lost communication with the server" and left nothing
// behind that could tell memory pressure from a reboot from a network event.
// These two targets bracket a long run with a strictly read-only per-host
// sampler so the next loss has an approach, not just a cliff.
//
// See ops/host-sampler/celeris-host-sampler for what it records and why, and
// ansible/host-sampler.yml for why nothing here can disturb a run.

const samplerPlaybook = "host-sampler.yml"

// samplerMarginSeconds is how long the sampler outlives the measurement
// window it was started for. The soak's own step can overrun its nominal
// duration (per-cell budgets round up, and the gate steps follow), and a
// sampler that stopped before the run did would miss the one minute that
// matters. Two hours is comfortably past any observed overrun and still far
// short of anything that would leave a sampler running into the next tier.
const samplerMarginSeconds = 7200

// samplerDurationSeconds resolves how long the sampler should live.
//
//	SAMPLER_DURATION   explicit override (Go duration, e.g. "26h")
//	SOAK_DURATION      the soak window; the sampler gets it plus the margin
//	VALIDATE_DURATION  likewise for a plain validate run
//
// Defaults to 24h + margin, matching the weekend tier.
func samplerDurationSeconds() (int, error) {
	if v := os.Getenv("SAMPLER_DURATION"); v != "" {
		s, err := durationSeconds(v)
		if err != nil {
			return 0, fmt.Errorf("SAMPLER_DURATION %q: %w", v, err)
		}
		return s, nil
	}
	window := os.Getenv("SOAK_DURATION")
	if window == "" {
		window = os.Getenv("VALIDATE_DURATION")
	}
	if window == "" {
		window = "24h"
	}
	s, err := durationSeconds(window)
	if err != nil {
		return 0, fmt.Errorf("sampler window %q: %w", window, err)
	}
	return s + samplerMarginSeconds, nil
}

// samplerTag labels every line the sampler writes, so a series that outlives
// several runs can still be cut into the run that produced it.
func samplerTag() string {
	if v := os.Getenv("SAMPLER_TAG"); v != "" {
		return v
	}
	if v := os.Getenv("GITHUB_RUN_ID"); v != "" {
		return "run-" + v
	}
	return "local"
}

// Bounds of the two cadences. SAMPLER_INTERVAL is the full sample of the shell
// sampler (about 24 ms of CPU each: ansible/host-sampler.yml); SAMPLER_FINE_INTERVAL
// is the lean series of the python sampler (ops/host-sampler/celeris-host-sampler-fine),
// which is what can see a 1-3 s event. The fine floor is 0.05 s (20 Hz): below it
// the sampler's own wake-ups become the load. The ceiling is 10 s so a stop
// request, a file the sampler polls every tick, lands inside the 30 s the stop
// task waits. Both are used in an ansible --extra-vars string, which splits on
// spaces, so only digits and one decimal point reach it.
const (
	samplerMaxInterval = 3600
	samplerFineMin     = 0.05
	samplerFineMax     = 10.0
)

var (
	samplerIntRE  = regexp.MustCompile(`^[0-9]{1,4}$`)
	samplerFineRE = regexp.MustCompile(`^[0-9]{1,2}(\.[0-9]{1,3})?$`)
	// The tag reaches an ansible --extra-vars string and a shell grep: letters,
	// digits, dot, dash and underscore only (weekend-123, bench-123, run-123, local).
	samplerTagRE = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)
)

// samplerTagChecked is samplerTag, refused when it holds anything else.
func samplerTagChecked() (string, error) {
	t := samplerTag()
	if !samplerTagRE.MatchString(t) {
		return "", fmt.Errorf("SAMPLER_TAG must be 1 to 64 of [A-Za-z0-9._-] (got %q)", t)
	}
	return t, nil
}

// samplerLowPriority resolves SAMPLER_LOW_PRIORITY: "" (unset) or "1". With 1 the
// 60 s shell sampler is launched at nice 19 under SCHED_IDLE (chrt -i 0), which
// the bench tier sets and the race and weekend tiers do not: their start is
// byte-for-byte what it was.
func samplerLowPriority() (bool, error) {
	switch v := os.Getenv("SAMPLER_LOW_PRIORITY"); v {
	case "":
		return false, nil
	case "1":
		return true, nil
	default:
		return false, fmt.Errorf("SAMPLER_LOW_PRIORITY must be empty or 1 (got %q)", v)
	}
}

// samplerInterval resolves SAMPLER_INTERVAL, the full sample's cadence: a
// positive integer of seconds, at most an hour. Unset is 60, which the race and
// weekend tiers rely on.
func samplerInterval() (string, error) {
	v := envOrDefault("SAMPLER_INTERVAL", "60")
	n, err := strconv.Atoi(v)
	if !samplerIntRE.MatchString(v) || err != nil || n < 1 || n > samplerMaxInterval {
		return "", fmt.Errorf("SAMPLER_INTERVAL must be an integer of 1 to %d seconds (got %q)", samplerMaxInterval, v)
	}
	return strconv.Itoa(n), nil
}

// samplerFineInterval resolves SAMPLER_FINE_INTERVAL: empty (the default: no
// fine series, the sampler is exactly the 60 s one) or seconds from 0.05 to 10
// with at most three decimals. The result is normalised ("1.0" -> "1").
func samplerFineInterval() (string, error) {
	v := os.Getenv("SAMPLER_FINE_INTERVAL")
	if v == "" {
		return "", nil
	}
	f, err := strconv.ParseFloat(v, 64)
	if !samplerFineRE.MatchString(v) || err != nil || f < samplerFineMin || f > samplerFineMax {
		return "", fmt.Errorf("SAMPLER_FINE_INTERVAL must be %g to %g seconds with at most 3 decimals (got %q)", samplerFineMin, samplerFineMax, v)
	}
	return strconv.FormatFloat(f, 'f', -1, 64), nil
}

// samplerStartVars is the --extra-vars of a start. The fine cadence is passed
// only when asked for, so a start without it is what it always was.
func samplerStartVars() ([]string, error) {
	secs, err := samplerDurationSeconds()
	if err != nil {
		return nil, err
	}
	interval, err := samplerInterval()
	if err != nil {
		return nil, err
	}
	fine, err := samplerFineInterval()
	if err != nil {
		return nil, err
	}
	tag, err := samplerTagChecked()
	if err != nil {
		return nil, err
	}
	low, err := samplerLowPriority()
	if err != nil {
		return nil, err
	}
	vars := []string{
		"sampler_action=start",
		"sampler_interval=" + interval,
		"sampler_duration_seconds=" + strconv.Itoa(secs),
		"sampler_tag=" + tag,
	}
	if fine != "" {
		vars = append(vars, "sampler_fine_interval="+fine)
	}
	if low {
		vars = append(vars, "sampler_low_priority=true")
	}
	return vars, nil
}

// samplerPlaybookArgs is the ansible-playbook command line for the sampler
// playbook. A bench scoped to one arch (BENCH_TARGET=msr1|msa2-server, a
// job-level variable of the bench tier) is limited to the hosts it uses, like
// Deploy and Bench: without it a target=msr1 bench would open SSH to
// msa2-server, which may be down for unrelated work, and the best-effort
// sampler would stall on it (mage_helpers.go clusterLimitForTarget). The race
// and weekend tiers set no BENCH_TARGET and keep every host.
func samplerPlaybookArgs(extra ...string) []string {
	args := []string{"-i", "inventory.yml", samplerPlaybook}
	if limit := clusterLimitForTarget(os.Getenv("BENCH_TARGET")); limit != "" {
		args = append(args, "--limit", limit)
	}
	for _, e := range extra {
		args = append(args, "--extra-vars", e)
	}
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		args = append(args, "--extra-vars", "use_lan=true")
	}
	return args
}

// runSamplerPlaybook is the shared body of the two targets.
func runSamplerPlaybook(extra ...string) error {
	if err := requireAnsible(); err != nil {
		return err
	}
	cmd := exec.Command("ansible-playbook", samplerPlaybookArgs(extra...)...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

// HostSamplerStart launches the read-only host sampler on every cluster host
// for the duration of the run.
//
// Env knobs:
//
//	SAMPLER_DURATION=   explicit lifetime; otherwise SOAK_DURATION + 2h
//	SAMPLER_INTERVAL=60 seconds between full samples (integer, 1 to 3600)
//	SAMPLER_FINE_INTERVAL=  seconds between ticks of the lean fine series
//	                    (0.05 to 10, unset = none); see celeris-host-sampler-fine
//	SAMPLER_TAG=        label recorded in every line (defaults to the run id)
//	CLUSTER_USE_LAN=1   LAN fabric instead of Tailscale
//	SAMPLER_LOW_PRIORITY=1  launch the 60 s sampler at nice 19 under SCHED_IDLE (bench tier)
//	BENCH_TARGET=       msr1 | msa2-server limits the playbook to that host
//	                    and the loadgen (as Deploy does); both or unset = all
//
// Starting it is best-effort by design: the playbook warns and continues when
// a host will not take it. Instrumentation that can fail a 24 h soak is worse
// than no instrumentation.
func HostSamplerStart() error {
	vars, err := samplerStartVars()
	if err != nil {
		return err
	}
	fine, _ := samplerFineInterval()
	interval, _ := samplerInterval()
	secs, _ := samplerDurationSeconds()
	fmt.Printf("\n=== HostSamplerStart ===\n")
	fmt.Printf("  interval:  %ss\n", interval)
	if fine != "" {
		fmt.Printf("  fine:      %ss (lean series, nice 10; see ansible/host-sampler.yml for its cost)\n", fine)
	}
	fmt.Printf("  lifetime:  %ds (self-terminating hard deadline)\n", secs)
	fmt.Printf("  tag:       %s\n", samplerTag())
	if limit := clusterLimitForTarget(os.Getenv("BENCH_TARGET")); limit != "" {
		fmt.Printf("  hosts:     %s (BENCH_TARGET scoped)\n", limit)
	}
	fmt.Println()
	return runSamplerPlaybook(vars...)
}

// samplerStopVars is the --extra-vars of a stop. When the step names its run
// (SAMPLER_TAG set, as the bench tier's stop step does), the fine series is
// fetched only if its fine-run.txt carries that tag: a start that never ran
// (Deploy failed) cannot hand the previous bench's series to this run. A stop
// that names no tag (race, weekend) fetches no fine file at all.
func samplerStopVars(fetchDir string) ([]string, error) {
	vars := []string{"sampler_action=stop", "sampler_fetch_dir=" + fetchDir}
	if os.Getenv("SAMPLER_TAG") != "" {
		tag, err := samplerTagChecked()
		if err != nil {
			return nil, err
		}
		vars = append(vars, "sampler_fetch_tag="+tag)
	}
	return vars, nil
}

// HostSamplerStop asks the sampler to finish and fetches its series into
// results/host-sampler/<host>/ so the run's own artifact carries it.
//
// "Asks" is literal: the playbook creates a sentinel file the sampler polls.
// Nothing is signalled, and if the sampler does not go, its own deadline ends
// it. This is also why the target is safe to run unconditionally with
// `if: always()` — on the path where the runner died it never executes at
// all, and the teardown job's forensics collection is what retrieves the
// series instead.
//
// Env knobs:
//
//	SAMPLER_FETCH_DIR=  where to place the fetched series
//	                    (default results/host-sampler)
//	CLUSTER_USE_LAN=1   LAN fabric instead of Tailscale
//	BENCH_TARGET=       as for HostSamplerStart
//	SAMPLER_TAG=        when set, the fine series is fetched only if it is this run's
func HostSamplerStop() error {
	fetch := envOrDefault("SAMPLER_FETCH_DIR", filepath.Join("results", "host-sampler"))
	abs, err := filepath.Abs(fetch)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	vars, err := samplerStopVars(abs)
	if err != nil {
		return err
	}
	fmt.Printf("\n=== HostSamplerStop ===\n")
	fmt.Printf("  series -> %s\n\n", abs)
	return runSamplerPlaybook(vars...)
}
