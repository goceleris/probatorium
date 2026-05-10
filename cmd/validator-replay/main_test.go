package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseSeed_Hex(t *testing.T) {
	cases := map[string]uint64{
		"0x1":            1,
		"0X10":           16,
		"0xdeadbeef":     0xdeadbeef,
		"1":              1,
		"42":             42,
		"18446744073709551615": 18446744073709551615,
	}
	for in, want := range cases {
		got, err := parseSeed(in)
		if err != nil {
			t.Errorf("parseSeed(%q): %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseSeed(%q) = %d; want %d", in, got, want)
		}
	}
}

func TestParseSeed_Empty(t *testing.T) {
	if _, err := parseSeed(""); err == nil {
		t.Fatal("expected error for empty seed")
	}
}

func TestParseArgs_RequiresSeedAtRunTime(t *testing.T) {
	cfg, err := ParseArgs([]string{"-target", "msa2-server"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Seed != "" {
		t.Fatalf("expected empty seed, got %q", cfg.Seed)
	}
	if err := run(cfg); err == nil || !strings.Contains(err.Error(), "seed") {
		t.Fatalf("expected seed-required error, got %v", err)
	}
}

func TestRun_DryRunWithSeed(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Seed = "0x1"
	cfg.Commit = "deadbeef"
	cfg.Target = "msa2-server"
	cfg.DryRun = true
	if err := run(cfg); err != nil {
		t.Fatalf("dry-run: %v", err)
	}
}
