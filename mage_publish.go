//go:build mage

package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// Publish targets. Publish dispatches the latest bench results.json
// to goceleris/docs via repository_dispatch — the docs repo's
// GitHub Action picks up the payload and rebuilds the bench page
// from the canonical v5.0 schema. PublishValidate does the same for
// the most recent Validate run's validate-results.json (different
// event_type so the docs workflow can fan to a different panel).
// BenchAndValidate composes the gate that release branches actually
// run end-to-end.

// Publish reads the latest results/<...>-bench-<ver>/results.json and
// POSTs it to goceleris/docs as a repository_dispatch event of type
// "celeris-bench". The docs repo's workflow consumes the payload to
// regenerate the bench dashboard.
//
// Auth: DOCS_TOKEN env var if set, otherwise `gh auth token` — same
// behaviour as gh's own commands so a logged-in dev needs zero extra
// setup.
//
// Env knobs:
//
//	PUBLISH_VERSION=        override go.mod auto-detect (rare; the
//	                        bench run that produced results.json
//	                        already encodes the version, but a
//	                        manual republish may want to relabel).
//	DOCS_TOKEN=             GitHub token with repo scope on
//	                        goceleris/docs. Falls back to gh CLI.
func Publish() error {
	version := os.Getenv("PUBLISH_VERSION")
	if version == "" {
		v, err := celerisVersion()
		if err != nil {
			return err
		}
		version = v
	}

	resultsPath, err := latestBenchResults(version)
	if err != nil {
		// Fall back to the most recent run regardless of version
		// when no version-specific match exists. The user's intent
		// when running Publish without a fresh same-version bench
		// is "ship what I have."
		resultsPath, err = latestBenchResults("")
		if err != nil {
			return fmt.Errorf("no bench results to publish: %w", err)
		}
	}

	data, err := os.ReadFile(resultsPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", resultsPath, err)
	}
	var resultsDoc any
	if err := json.Unmarshal(data, &resultsDoc); err != nil {
		return fmt.Errorf("parse %s: %w", resultsPath, err)
	}

	token, err := resolveDocsToken()
	if err != nil {
		return err
	}

	// repository_dispatch payload shape — `event_type` is the key the
	// docs workflow filters on; `client_payload` carries arbitrary
	// JSON. We pass the full results.json plus a version tag so the
	// workflow doesn't have to re-derive it from the payload tree.
	payload := map[string]any{
		"event_type": "celeris-bench",
		"client_payload": map[string]any{
			"version": version,
			"results": resultsDoc,
			"source":  resultsPath,
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	fmt.Printf("Publishing %s (%s) → goceleris/docs...\n", resultsPath, version)
	cmd := exec.Command("gh", "api",
		"-X", "POST",
		"/repos/goceleris/docs/dispatches",
		"-H", "Accept: application/vnd.github+json",
		"-H", "Authorization: token "+token,
		"--input", "-",
	)
	cmd.Stdin = bytes.NewReader(payloadJSON)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api dispatch: %w", err)
	}
	fmt.Println("Published.")
	return nil
}

// PublishValidate reads the latest results/<ts>-validate-<ver>/
// validate-results.json and POSTs it to goceleris/docs as a
// repository_dispatch event of type "celeris-validate". The docs
// workflow consumes the payload to regenerate the validation panel
// (tier-1 workload mix counts, tier-3 seed-corpus pass/fail rate).
//
// Same auth contract as Publish: DOCS_TOKEN env or `gh auth token`.
func PublishValidate() error {
	version := os.Getenv("PUBLISH_VERSION")
	if version == "" {
		v, err := celerisVersion()
		if err != nil {
			return err
		}
		version = v
	}

	resultsPath, err := latestValidateResults(version)
	if err != nil {
		resultsPath, err = latestValidateResults("")
		if err != nil {
			return fmt.Errorf("no validate results to publish: %w", err)
		}
	}

	data, err := os.ReadFile(resultsPath)
	if err != nil {
		return fmt.Errorf("read %s: %w", resultsPath, err)
	}
	var doc any
	if err := json.Unmarshal(data, &doc); err != nil {
		return fmt.Errorf("parse %s: %w", resultsPath, err)
	}

	token, err := resolveDocsToken()
	if err != nil {
		return err
	}

	payload := map[string]any{
		"event_type": "celeris-validate",
		"client_payload": map[string]any{
			"version": version,
			"results": doc,
			"source":  resultsPath,
		},
	}
	payloadJSON, err := json.Marshal(payload)
	if err != nil {
		return err
	}

	fmt.Printf("Publishing %s (%s) → goceleris/docs (validate)...\n", resultsPath, version)
	cmd := exec.Command("gh", "api",
		"-X", "POST",
		"/repos/goceleris/docs/dispatches",
		"-H", "Accept: application/vnd.github+json",
		"-H", "Authorization: token "+token,
		"--input", "-",
	)
	cmd.Stdin = bytes.NewReader(payloadJSON)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("gh api dispatch: %w", err)
	}
	fmt.Println("Published.")
	return nil
}

// BenchAndValidate is the release-gate composition: Validate first
// (long-running invariant + property suite), then on success a fresh
// Bench, then Publish + PublishValidate. Failure at any step
// short-circuits — a release that can't pass Validate has no
// business shipping a bench number, and a publish that runs without
// a fresh bench is misleading.
//
// Reuses every BENCH_*, VALIDATE_*, CELERIS_VERSION, CLUSTER_USE_LAN,
// and DOCS_TOKEN env knob from the underlying targets — set them
// once in the caller's shell and they flow through.
func BenchAndValidate() error {
	fmt.Println("=== BenchAndValidate: Validate ===")
	if err := Validate(); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	// Cross-arch divergence check runs only when BENCH_TARGET=both
	// (otherwise there's only one side to diff). Best-effort: a
	// missing second-arch run leaves <2 validate-results.json files
	// under results/, and ValidateDiff returns "need at least two"
	// — we log that as a soft skip rather than failing the gate.
	if os.Getenv("BENCH_TARGET") == "both" {
		fmt.Println("\n=== BenchAndValidate: ValidateDiff ===")
		if err := ValidateDiff(); err != nil {
			return fmt.Errorf("validate-diff: %w", err)
		}
	} else {
		fmt.Println("\n=== BenchAndValidate: ValidateDiff (skipped — BENCH_TARGET != both) ===")
	}
	fmt.Println("\n=== BenchAndValidate: Bench ===")
	if err := Bench(); err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	fmt.Println("\n=== BenchAndValidate: Publish ===")
	if err := Publish(); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	// PublishValidate is best-effort: docs panel for the validation
	// tier is informational, not gating. A missing validate-results.json
	// (e.g. an old run that pre-dates the v5 emitter) shouldn't fail
	// the whole release-gate.
	fmt.Println("\n=== BenchAndValidate: PublishValidate ===")
	if err := PublishValidate(); err != nil {
		fmt.Printf("warning: PublishValidate failed (best-effort): %v\n", err)
	}
	fmt.Println("\n=== BenchAndValidate: complete ===")
	return nil
}

// resolveDocsToken returns the token used for the docs dispatch.
// DOCS_TOKEN env wins; falls back to `gh auth token`. We never log
// the token (just stamp whether it came from env or CLI) so a stray
// CI log dump doesn't leak credentials.
func resolveDocsToken() (string, error) {
	if t := os.Getenv("DOCS_TOKEN"); t != "" {
		return t, nil
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token (set DOCS_TOKEN env or run `gh auth login`): %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("gh auth token returned empty (set DOCS_TOKEN env or run `gh auth login`)")
	}
	return tok, nil
}
