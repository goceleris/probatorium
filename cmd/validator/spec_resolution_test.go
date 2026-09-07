package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestResolveRefappSpec_PerRefapp: the matrix picks a refapp's own
// spec the same way it already picks that refapp's Markov yaml —
// <spec-dir>/<slug>.openapi.yaml.
func TestResolveRefappSpec_PerRefapp(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "kitchen_sink.openapi.yaml")
	if err := os.WriteFile(want, []byte("openapi: 3.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, absent := resolveRefappSpec(dir, "kitchen_sink")
	if got != want {
		t.Errorf("path = %q, want %q", got, want)
	}
	if absent != "" {
		t.Errorf("absent reason = %q, want none", absent)
	}
}

// TestResolveRefappSpec_MissingNeverFallsBack is the actual defect:
// the old code handed every cell the one spec that happens to exist.
// A refapp with no spec of its own must come back with NO spec and a
// reason, never with another refapp's API description.
func TestResolveRefappSpec_MissingNeverFallsBack(t *testing.T) {
	dir := t.TempDir()
	other := filepath.Join(dir, "auth_session_ratelimit.openapi.yaml")
	if err := os.WriteFile(other, []byte("openapi: 3.1.0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, absent := resolveRefappSpec(dir, "driver_postgres")
	if got != "" {
		t.Errorf("path = %q, want empty — a foreign spec is worse than none", got)
	}
	if !strings.Contains(absent, "driver_postgres") {
		t.Errorf("absent reason %q does not name the refapp", absent)
	}
}

// TestCheckMatrixSpecFlag_RejectsOneSpecForManyRefapps: -openapi names
// a single API description. Pointing it at a multi-refapp matrix is
// the silent-fallback bug spelled out on the command line, so it fails
// before the first cell burns cluster time.
func TestCheckMatrixSpecFlag_RejectsOneSpecForManyRefapps(t *testing.T) {
	plan := []matrixCell{
		{Refapp: "auth_session_ratelimit", Engine: "std"},
		{Refapp: "kitchen_sink", Engine: "std"},
	}
	err := checkMatrixSpecFlag("validation/spec/auth_session_ratelimit.openapi.yaml", plan)
	if err == nil {
		t.Fatal("explicit -openapi accepted for a 2-refapp matrix; want an error")
	}
	if !strings.Contains(err.Error(), "-spec-dir") {
		t.Errorf("error does not point at the fix: %v", err)
	}
	if err := checkMatrixSpecFlag("", plan); err != nil {
		t.Errorf("auto-resolved spec dir must be fine for many refapps: %v", err)
	}
	single := []matrixCell{{Refapp: "auth_session_ratelimit", Engine: "std"}}
	if err := checkMatrixSpecFlag("validation/spec/auth_session_ratelimit.openapi.yaml", single); err != nil {
		t.Errorf("explicit -openapi with one refapp must be allowed: %v", err)
	}
}
