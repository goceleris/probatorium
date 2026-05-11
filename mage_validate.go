//go:build mage

package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// titleCase upper-cases the first rune of s. Replaces strings.Title
// (deprecated since Go 1.18) for the small "validate"/"soak" labels
// the validate targets print at start/end of a run.
func titleCase(s string) string {
	if s == "" {
		return s
	}
	return strings.ToUpper(s[:1]) + s[1:]
}

// Validation tier targets. The validation suite drives long-running
// soak tests with property invariants, RESTler-style stateful fuzzing,
// and a deterministic-seed replay harness. After 10 days clean on
// both archs, the celeris release is operationally production-ready.
//
// Each target produces a results dir under
// results/<ts>-<kind>-<celeris-version>/ with raw per-host artifacts
// preserved (no merge step — these are diagnostic, not headline).

// Validate runs the property + invariant suite for VALIDATE_DURATION
// against the configured target(s). Failures land in
// results/<ts>-validate-<ver>/<host>/ with a per-host log + any
// captured failure trace.
//
// Env knobs:
//
//	VALIDATE_DURATION=6h         per-host run duration
//	VALIDATE_TARGET=both         msa2-server | msr1 | both
//	CELERIS_VERSION=             override go.mod auto-detect
//	CLUSTER_USE_LAN=1            LAN fabric instead of Tailscale
func Validate() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	duration := envOrDefault("VALIDATE_DURATION", "6h")
	target := envOrDefault("VALIDATE_TARGET", "both")
	if target != "both" && target != "msa2-server" && target != "msr1" {
		return fmt.Errorf("VALIDATE_TARGET must be msa2-server, msr1, or both (got %q)", target)
	}
	version, err := celerisVersion()
	if err != nil {
		return err
	}
	return runValidatePlaybook(duration, target, version, false)
}

// Soak is Validate with a 24h default duration and the
// validate_soak_mode=1 extra-var flipped on. The soak playbook layers
// extra invariant checks (goroutine count drift, heap growth, FD
// leak) that would be too expensive for a short Validate run.
//
// Env knobs:
//
//	SOAK_DURATION=24h            per-host run duration (overrides
//	                             VALIDATE_DURATION when both set).
//	VALIDATE_TARGET=both         msa2-server | msr1 | both
//	CELERIS_VERSION=             override go.mod auto-detect
func Soak() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	duration := envOrDefault("SOAK_DURATION", "24h")
	target := envOrDefault("VALIDATE_TARGET", "both")
	if target != "both" && target != "msa2-server" && target != "msr1" {
		return fmt.Errorf("VALIDATE_TARGET must be msa2-server, msr1, or both (got %q)", target)
	}
	version, err := celerisVersion()
	if err != nil {
		return err
	}
	return runValidatePlaybook(duration, target, version, true)
}

// runValidatePlaybook is the shared body of Validate and Soak. The
// soakMode bool toggles validate_soak_mode=1 in the extra-vars; the
// playbook keys off it to enable the extended invariant set.
//
// target=both expands into two ansible-playbook invocations (one per
// bench_target). Each invocation drives the validator on ONE host
// — single-host validator drives single-host refapp via the local
// Driver — so "both" semantically means "run the soak twice in
// series, once per arch."
func runValidatePlaybook(duration, target, version string, soakMode bool) error {
	ts := time.Now().UTC().Format("20060102-150405")
	kind := "validate"
	if soakMode {
		kind = "soak"
	}
	resultsDir, err := filepath.Abs(filepath.Join("results",
		fmt.Sprintf("%s-%s-%s", ts, kind, version)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return err
	}

	var targets []string
	switch target {
	case "both":
		targets = []string{"msa2-server", "msr1"}
	default:
		targets = []string{target}
	}

	fmt.Printf("\n=== %s ===\n", titleCase(kind))
	fmt.Printf("  targets:      %v\n", targets)
	fmt.Printf("  duration:     %s\n", duration)
	fmt.Printf("  celeris ver:  %s\n", version)
	fmt.Printf("  soak mode:    %t\n", soakMode)
	fmt.Printf("  results:      %s\n\n", resultsDir)

	// Playbook selection:
	//   default     → ansible/validate.yml      (validator + refapp
	//                                            both on bench_target)
	//   PROBATORIUM_VALIDATE_DRIVER=ssh
	//               → ansible/validate-ssh.yml  (validator on
	//                                            msa2-client; SSH
	//                                            into bench_target)
	playbook := validatePlaybook
	if os.Getenv("PROBATORIUM_VALIDATE_DRIVER") == "ssh" {
		playbook = "validate-ssh.yml"
	}
	runOne := func(t string) error {
		args := []string{
			"-i", "inventory.yml",
			playbook,
			"--extra-vars", "bench_target=" + t,
			"--extra-vars", "validate_duration=" + duration,
			"--extra-vars", "celeris_version=" + version,
			"--extra-vars", "results_local_dir=" + resultsDir,
		}
		if soakMode {
			args = append(args, "--extra-vars", "validate_soak_mode=1")
		}
		if os.Getenv("CLUSTER_USE_LAN") == "1" {
			args = append(args, "--extra-vars", "use_lan=true")
		}
		fmt.Printf("\n=== %s on %s (playbook=%s) ===\n", titleCase(kind), t, playbook)
		cmd := exec.Command("ansible-playbook", args...)
		cmd.Dir = ansibleDir
		cmd.Stdout = os.Stdout
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("%s on %s: %w", kind, t, err)
		}
		return nil
	}

	// VALIDATE_PARALLEL=1 fans the targets over their own goroutines
	// so a two-arch soak takes wall-clock N hours instead of 2N. The
	// validator binary on msa2-client multiplexes — one process per
	// target — and each target writes to a distinct validate_run_dir
	// on the remote, so the two streams don't collide.
	//
	// Default (sequential) keeps stdout interleaving readable for
	// dev-iteration runs; PARALLEL is for the 3-day cluster soak.
	if os.Getenv("VALIDATE_PARALLEL") == "1" && len(targets) > 1 {
		fmt.Printf("\n=== %s targets running in parallel (VALIDATE_PARALLEL=1) ===\n", titleCase(kind))
		if err := runHostsParallel(targets, runOne); err != nil {
			return err
		}
	} else {
		for _, t := range targets {
			if err := runOne(t); err != nil {
				return err
			}
		}
	}
	fmt.Printf("\n=== %s complete: %s ===\n", titleCase(kind), resultsDir)
	return nil
}

// Fuzz runs the RESTler-style stateful fuzzer for FUZZ_DURATION.
// Crash artifacts and reduced repro corpora land under
// results/<ts>-fuzz-<ver>/<host>/.
//
// Env knobs:
//
//	FUZZ_DURATION=30m           per-host run duration
//	FUZZ_CORPUS=default         default | aggressive
//	CELERIS_VERSION=            override go.mod auto-detect
//	CLUSTER_USE_LAN=1           LAN fabric instead of Tailscale
func Fuzz() error {
	if err := requireAnsible(); err != nil {
		return err
	}
	duration := envOrDefault("FUZZ_DURATION", "30m")
	corpus := envOrDefault("FUZZ_CORPUS", "default")
	if corpus != "default" && corpus != "aggressive" {
		return fmt.Errorf("FUZZ_CORPUS must be default or aggressive (got %q)", corpus)
	}
	version, err := celerisVersion()
	if err != nil {
		return err
	}

	ts := time.Now().UTC().Format("20060102-150405")
	resultsDir, err := filepath.Abs(filepath.Join("results",
		fmt.Sprintf("%s-fuzz-%s", ts, version)))
	if err != nil {
		return err
	}
	if err := os.MkdirAll(resultsDir, 0o755); err != nil {
		return err
	}

	fmt.Printf("\n=== Fuzz ===\n")
	fmt.Printf("  duration:     %s\n", duration)
	fmt.Printf("  corpus:       %s\n", corpus)
	fmt.Printf("  celeris ver:  %s\n", version)
	fmt.Printf("  results:      %s\n\n", resultsDir)

	args := []string{
		"-i", "inventory.yml",
		fuzzPlaybook,
		"--extra-vars", "fuzz_duration=" + duration,
		"--extra-vars", "fuzz_corpus=" + corpus,
		"--extra-vars", "celeris_version=" + version,
		"--extra-vars", "results_local_dir=" + resultsDir,
	}
	if os.Getenv("CLUSTER_USE_LAN") == "1" {
		args = append(args, "--extra-vars", "use_lan=true")
	}

	cmd := exec.Command("ansible-playbook", args...)
	cmd.Dir = ansibleDir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("fuzz: %w", err)
	}
	fmt.Printf("\n=== Fuzz complete: %s ===\n", resultsDir)
	return nil
}
