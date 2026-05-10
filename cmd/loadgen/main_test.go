package main

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/goceleris/loadgen"
)

func TestParseArgs_DefaultsMatchLoadgenLibrary(t *testing.T) {
	cfg, err := ParseArgs(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	lib := loadgen.DefaultConfig()
	if cfg.Connections != lib.Connections {
		t.Errorf("Connections: got %d, want %d (library default)", cfg.Connections, lib.Connections)
	}
	if cfg.Workers != lib.Workers {
		t.Errorf("Workers: got %d, want %d (library default)", cfg.Workers, lib.Workers)
	}
	if cfg.Duration != 30*time.Second {
		t.Errorf("Duration: got %s, want 30s", cfg.Duration)
	}
	if cfg.Warmup != 5*time.Second {
		t.Errorf("Warmup: got %s, want 5s", cfg.Warmup)
	}
	if cfg.Rate != 0 {
		t.Errorf("Rate: got %d, want 0 (saturation)", cfg.Rate)
	}
}

func TestParseArgs_FlagOverrides(t *testing.T) {
	args := []string{
		"-target", "http://example.test:1234/x",
		"-method", "POST",
		"-duration", "5s",
		"-warmup", "1s",
		"-connections", "8",
		"-workers", "4",
		"-rate", "100",
		"-http2",
		"-h2c-upgrade",
		"-out", "/tmp/probtest-loadgen.json",
	}
	cfg, err := ParseArgs(args, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Target != "http://example.test:1234/x" {
		t.Errorf("Target=%q", cfg.Target)
	}
	if cfg.Method != "POST" {
		t.Errorf("Method=%q", cfg.Method)
	}
	if cfg.Duration != 5*time.Second {
		t.Errorf("Duration=%s", cfg.Duration)
	}
	if cfg.Warmup != time.Second {
		t.Errorf("Warmup=%s", cfg.Warmup)
	}
	if cfg.Connections != 8 {
		t.Errorf("Connections=%d", cfg.Connections)
	}
	if cfg.Workers != 4 {
		t.Errorf("Workers=%d", cfg.Workers)
	}
	if cfg.Rate != 100 {
		t.Errorf("Rate=%d", cfg.Rate)
	}
	if !cfg.HTTP2 {
		t.Error("HTTP2 not set")
	}
	if !cfg.H2CUpgrade {
		t.Error("H2CUpgrade not set")
	}
	if cfg.Out != "/tmp/probtest-loadgen.json" {
		t.Errorf("Out=%q", cfg.Out)
	}
}

func TestParseArgs_RejectsUnknownFlag(t *testing.T) {
	_, err := ParseArgs([]string{"-no-such-flag"}, &bytes.Buffer{})
	if err == nil {
		t.Fatal("expected parse error on unknown flag")
	}
}

func TestRun_RejectsRatedMode(t *testing.T) {
	// Rated mode is reserved for loadgen v2; the CLI must refuse with
	// a clear message instead of silently faking saturation numbers.
	cfg := DefaultConfig()
	cfg.Rate = 50
	err := run(cfg)
	if err == nil {
		t.Fatal("expected error for -rate >0")
	}
}

func TestEmit_WritesJSONToFile(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "result.json")
	res := &loadgen.Result{Requests: 42, Errors: 0}
	if err := emit(res, out); err != nil {
		t.Fatalf("emit: %v", err)
	}
	body, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var parsed loadgen.Result
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Requests != 42 {
		t.Errorf("Requests round-trip: got %d, want 42", parsed.Requests)
	}
}

func TestEmit_TrailingNewlineForJqFriendliness(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "result.json")
	if err := emit(&loadgen.Result{}, out); err != nil {
		t.Fatalf("emit: %v", err)
	}
	body, _ := os.ReadFile(out)
	if len(body) == 0 || body[len(body)-1] != '\n' {
		t.Error("output missing trailing newline (breaks `jq` / line-orientated tools)")
	}
}
