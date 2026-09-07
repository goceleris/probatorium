package main

import (
	"bytes"
	"testing"
)

// TestDefaultConfig_NoGlobalSpecDefault: OpenAPIPath used to default to
// validation/spec/auth_session_ratelimit.openapi.yaml — ONE refapp's
// spec, carried unchanged into all 48 matrix cells. Seven of eight
// refapps would have been fuzzed against an API description that does
// not describe them, and the path is not even staged on the cluster.
// The spec is now resolved per refapp; there is no global default.
func TestDefaultConfig_NoGlobalSpecDefault(t *testing.T) {
	if got := DefaultConfig().OpenAPIPath; got != "" {
		t.Errorf("OpenAPIPath default = %q; want empty (resolved per refapp from -spec-dir)", got)
	}
	if got := DefaultConfig().SpecDir; got == "" {
		t.Error("SpecDir default is empty; want the per-refapp spec directory")
	}
}

// TestParseArgs_SpecDirBound keeps the new knob on the command line —
// the cluster runs the validator with a staged bench_root, so the spec
// directory has to be overridable without recompiling.
func TestParseArgs_SpecDirBound(t *testing.T) {
	cfg, err := ParseArgs([]string{"-spec-dir", "/tmp/celeris-bench/spec"}, &bytes.Buffer{})
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if cfg.SpecDir != "/tmp/celeris-bench/spec" {
		t.Errorf("SpecDir = %q", cfg.SpecDir)
	}
}
