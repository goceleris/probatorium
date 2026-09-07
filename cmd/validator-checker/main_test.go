package main

import (
	"bytes"
	"testing"
)

func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := ParseArgs(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsURL == "" {
		t.Fatal("default metrics url empty")
	}
	if cfg.Interval <= 0 {
		t.Fatalf("interval=%s", cfg.Interval)
	}
	if cfg.ValidationSocketPath == "" {
		t.Fatal("default validation socket path empty")
	}
	if cfg.PID != 0 {
		t.Fatalf("default pid=%d want 0 (RSS unsampled)", cfg.PID)
	}
}

func TestParseArgs_PID(t *testing.T) {
	cfg, err := ParseArgs([]string{"-pid=4242", "-property-tier=core"}, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.PID != 4242 || cfg.PropertyTier != "core" {
		t.Fatalf("cfg=%+v", cfg)
	}
}
