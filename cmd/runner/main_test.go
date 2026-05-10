package main

import (
	"bytes"
	"testing"
	"time"
)

func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := ParseArgs(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if cfg.Runs != 5 {
		t.Errorf("Runs default = %d, want 5", cfg.Runs)
	}
	if cfg.Duration != 120*time.Second {
		t.Errorf("Duration default = %s, want 2m", cfg.Duration)
	}
	if cfg.Warmup != 30*time.Second {
		t.Errorf("Warmup default = %s, want 30s", cfg.Warmup)
	}
	if cfg.Services != "local" {
		t.Errorf("Services default = %q, want local", cfg.Services)
	}
}

func TestParseArgs_OverrideAll(t *testing.T) {
	args := []string{
		"-runs", "3",
		"-duration", "10s",
		"-warmup", "1s",
		"-cells", "get-simple/*",
		"-out", "/tmp/x",
		"-services", "none",
		"-fail-fast",
		"-fd-trace",
		"-seed", "42",
		"-dry-run",
	}
	cfg, err := ParseArgs(args, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("ParseArgs: %v", err)
	}
	if cfg.Runs != 3 {
		t.Errorf("Runs = %d, want 3", cfg.Runs)
	}
	if cfg.Cells != "get-simple/*" {
		t.Errorf("Cells = %q", cfg.Cells)
	}
	if cfg.Out != "/tmp/x" {
		t.Errorf("Out = %q", cfg.Out)
	}
	if cfg.Services != "none" {
		t.Errorf("Services = %q", cfg.Services)
	}
	if !cfg.FailFast || !cfg.FDTrace || !cfg.DryRun {
		t.Errorf("flags not set: fail-fast=%v fd-trace=%v dry-run=%v",
			cfg.FailFast, cfg.FDTrace, cfg.DryRun)
	}
	if cfg.Seed != 42 {
		t.Errorf("Seed = %d", cfg.Seed)
	}
}
