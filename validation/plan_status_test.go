package validation

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestNew_MissingOpenAPISpecIsHardError: New's contract says it
// "returns an error if any required artifact (markov YAML, corpus,
// OpenAPI) is missing", but the OpenAPI path was never opened — a
// caller could name a spec that does not exist (every cluster cell
// did: validation/spec/ is not staged under bench_root) and the run
// proceeded, recording the fictional path in plan.json.
func TestNew_MissingOpenAPISpecIsHardError(t *testing.T) {
	cfg := Default()
	cfg.OpenAPIPath = filepath.Join(t.TempDir(), "no-such-refapp.openapi.yaml")
	if _, err := New(cfg); err == nil {
		t.Fatal("New accepted an OpenAPI path that does not exist; want a hard error")
	}
}

// TestNew_PresentOpenAPISpecAccepted guards the other direction: a
// spec that IS there must load cleanly.
func TestNew_PresentOpenAPISpecAccepted(t *testing.T) {
	cfg := Default()
	cfg.OpenAPIPath = "spec/auth_session_ratelimit.openapi.yaml"
	if _, err := New(cfg); err != nil {
		t.Fatalf("New rejected the shipped spec: %v", err)
	}
}

// TestPlan_RESTlerTierDeclaresItIsDisabled: Tier 2 is scaffolding —
// runTierRESTler parks on ctx and generates no traffic at all — yet
// the plan advertised it as "RESTler-style stateful fuzzing over the
// OpenAPI 3.1 spec" on a "rolling, max N windows" cadence. Every
// nightly plan.json therefore read as covered while nothing ran. The
// plan must say the tier is disabled, and why.
func TestPlan_RESTlerTierDeclaresItIsDisabled(t *testing.T) {
	cfg := Default()
	cfg.Duration = 24 * time.Hour // long enough for 3 whole 8h windows
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var restler *TierPlan
	for i := range o.Plan().Tiers {
		if o.Plan().Tiers[i].Tier == TierRESTler {
			restler = &o.Plan().Tiers[i]
		}
	}
	if restler == nil {
		t.Fatal("no tier-2 entry in the plan")
	}
	if restler.Enabled {
		t.Error("tier-2 claims to be enabled, but runTierRESTler only parks on ctx")
	}
	if restler.Status == "" {
		t.Error("tier-2 is disabled but the plan gives no reason")
	}

	var buf bytes.Buffer
	PrintPlan(&buf, o.Plan())
	if !strings.Contains(buf.String(), "DISABLED") {
		t.Errorf("printed plan does not mark the disabled tier:\n%s", buf.String())
	}
}

// TestPlan_AbsentSpecIsExplicit: with no spec resolved for the refapp
// the plan must SAY so rather than printing an empty path that reads
// like a formatting artifact.
func TestPlan_AbsentSpecIsExplicit(t *testing.T) {
	o, err := New(Default()) // Default carries no OpenAPIPath
	if err != nil {
		t.Fatalf("new: %v", err)
	}
	var buf bytes.Buffer
	PrintPlan(&buf, o.Plan())
	if !strings.Contains(buf.String(), "openapi=<none>") {
		t.Errorf("absent spec not called out in the plan:\n%s", buf.String())
	}
}
