package debugvars

import "testing"

// The key is always present and mirrors the build: false under a plain
// `go test`, true under `go test -race` (the toolchain sets the race tag).
func TestRaceBuildKeyInDocument(t *testing.T) {
	doc := New().Document()
	b, ok := doc["celeris.race_build"].(bool)
	if !ok {
		t.Fatalf("celeris.race_build must be a bool, got %#v", doc["celeris.race_build"])
	}
	if b != raceBuild {
		t.Fatalf("celeris.race_build = %v, want the build's own %v", b, raceBuild)
	}
}
