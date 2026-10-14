//go:build mage

package main

import (
	"strings"
	"testing"
)

// The checker's side of debugvars.ParseFaults' overlap rule: a
// VALIDATE_FAULT_CONTROL spec whose holds overlap is refused before any
// cell is judged, because a dump taken in the overlap would be credited to
// both paths (every hold has the same waiter and holder frames).
func TestFaultControlPathsRefusesOverlappingHolds(t *testing.T) {
	for _, bad := range []string{"/ws:8s@30s,/:40s@35s", "/:40s@30s,/ws:8s@40s"} {
		if _, err := faultControlPaths(bad); err == nil || !strings.Contains(err.Error(), "overlap") {
			t.Errorf("%q: got %v, want an overlap error", bad, err)
		}
	}
	for _, bad := range []string{"/ws:8s", "/ws:x@30s", "/ws:8s@-1s"} {
		if _, err := faultControlPaths(bad); err == nil {
			t.Errorf("%q: malformed entry accepted", bad)
		}
	}
	got, err := faultControlPaths("/ws:8s@30s,/:40s@60s")
	if err != nil || len(got) != 2 || got[0] != "/ws" || got[1] != "/" {
		t.Fatalf("the queued cluster spec: %v %v, want [/ws /]", got, err)
	}
}
