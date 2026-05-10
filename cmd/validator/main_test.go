package main

import (
	"bytes"
	"testing"
	"time"
)

func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := ParseArgs(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Duration <= 0 {
		t.Fatalf("default duration must be positive: %s", cfg.Duration)
	}
	if cfg.CelerisListenAddr == "" {
		t.Fatal("default listen addr empty")
	}
	if cfg.PropertyTier == "" {
		t.Fatal("default property tier empty")
	}
}

func TestParseArgs_Override(t *testing.T) {
	cfg, err := ParseArgs([]string{
		"-target", "msa2-server",
		"-duration", "10m",
		"-arch", "amd64",
		"-dry-run",
	}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.Target != "msa2-server" {
		t.Errorf("target=%q", cfg.Target)
	}
	if cfg.Duration != 10*time.Minute {
		t.Errorf("duration=%s", cfg.Duration)
	}
	if cfg.Arch != "amd64" {
		t.Errorf("arch=%q", cfg.Arch)
	}
	if !cfg.DryRun {
		t.Error("dry-run not set")
	}
}

// TestRun_DryRunExitsCleanly is the wave 6 acceptance: validator
// -target=... -duration=10m -dry-run must exit 0.
func TestRun_DryRunExitsCleanly(t *testing.T) {
	dir := t.TempDir()
	cfg := DefaultConfig()
	cfg.Target = "msa2-server"
	cfg.Duration = 10 * time.Minute
	cfg.DryRun = true
	cfg.OutDir = dir
	// The default Markov / OpenAPI paths are relative to the repo
	// root; tests run from cmd/validator, so override to repo-relative.
	cfg.MarkovPath = "../../validation/markov/auth_session_ratelimit.yaml"
	cfg.OpenAPIPath = "../../validation/spec/auth_session_ratelimit.openapi.yaml"
	if err := run(cfg); err != nil {
		t.Fatalf("dry run: %v", err)
	}
}
