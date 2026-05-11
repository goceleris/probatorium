package validation

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestShrinkFailingSeed_MissingBinsLogsAndReturns(t *testing.T) {
	dir := t.TempDir()
	err := shrinkFailingSeed(context.Background(), dir, 0xc0ffee, shrinkCfg{
		// ReplayBin + RefappBin intentionally empty.
		OriginalDuration: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("shrinkFailingSeed: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "shrink_log.txt"))
	if err != nil {
		t.Fatalf("shrink_log.txt missing: %v", err)
	}
	if !strings.Contains(string(body), "ReplayBin or RefappBin unset") {
		t.Errorf("log should mention missing bins, got %q", body)
	}
}

func TestShrinkFailingSeed_DurationBelowFloor(t *testing.T) {
	dir := t.TempDir()
	err := shrinkFailingSeed(context.Background(), dir, 1, shrinkCfg{
		ReplayBin:        "/usr/bin/true",
		RefappBin:        "/usr/bin/true",
		OriginalDuration: 100 * time.Millisecond,
		MinDuration:      500 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("shrinkFailingSeed: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "shrink_log.txt"))
	if err != nil {
		t.Fatalf("shrink_log.txt missing: %v", err)
	}
	if !strings.Contains(string(body), "original=100ms") {
		t.Errorf("log should reference original duration, got %q", body)
	}
}

func TestPortFromAddr(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1:8080": "8080",
		"localhost:1234": "1234",
		"[::1]:80":       "80",
		"noport":         "noport",
	}
	for in, want := range cases {
		if got := portFromAddr(in); got != want {
			t.Errorf("portFromAddr(%q): got %q, want %q", in, got, want)
		}
	}
}

func TestWriteJSONIndent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "sub", "nested.json")
	if err := writeJSONIndent(path, map[string]any{"seed": "0xc0ffee", "ok": true}); err != nil {
		t.Fatalf("writeJSONIndent: %v", err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if doc["seed"] != "0xc0ffee" {
		t.Errorf("seed: got %v, want 0xc0ffee", doc["seed"])
	}
	// Indented JSON has a newline after each member; lazy sanity check.
	if !strings.Contains(string(body), "\n  ") {
		t.Errorf("output not indented: %q", body)
	}
}
