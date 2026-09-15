//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
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

// runSamplerPlaybook is the shared body of the two targets.
func runSamplerPlaybook(extra ...string) error {
	if err := requireAnsible(); err != nil {
		return err
	}
	args := []string{"-i", "inventory.yml", samplerPlaybook}
	for _, e := range extra {
		args = append(args, "--extra-vars", e)
	}
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		args = append(args, "--extra-vars", "use_lan=true")
	}
	cmd := exec.Command("ansible-playbook", args...)
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
//	SAMPLER_INTERVAL=60 seconds between samples
//	SAMPLER_TAG=        label recorded in every line (defaults to the run id)
//	CLUSTER_USE_LAN=1   LAN fabric instead of Tailscale
//
// Starting it is best-effort by design: the playbook warns and continues when
// a host will not take it. Instrumentation that can fail a 24 h soak is worse
// than no instrumentation.
func HostSamplerStart() error {
	secs, err := samplerDurationSeconds()
	if err != nil {
		return err
	}
	interval := envOrDefault("SAMPLER_INTERVAL", "60")
	if n, err := strconv.Atoi(interval); err != nil || n < 1 {
		return fmt.Errorf("SAMPLER_INTERVAL must be a positive integer (got %q)", interval)
	}
	tag := samplerTag()
	fmt.Printf("\n=== HostSamplerStart ===\n")
	fmt.Printf("  interval:  %ss\n", interval)
	fmt.Printf("  lifetime:  %ds (self-terminating hard deadline)\n", secs)
	fmt.Printf("  tag:       %s\n\n", tag)
	return runSamplerPlaybook(
		"sampler_action=start",
		"sampler_interval="+interval,
		"sampler_duration_seconds="+strconv.Itoa(secs),
		"sampler_tag="+tag,
	)
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
func HostSamplerStop() error {
	fetch := envOrDefault("SAMPLER_FETCH_DIR", filepath.Join("results", "host-sampler"))
	abs, err := filepath.Abs(fetch)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(abs, 0o755); err != nil {
		return err
	}
	fmt.Printf("\n=== HostSamplerStop ===\n")
	fmt.Printf("  series -> %s\n\n", abs)
	return runSamplerPlaybook(
		"sampler_action=stop",
		"sampler_fetch_dir="+abs,
	)
}
