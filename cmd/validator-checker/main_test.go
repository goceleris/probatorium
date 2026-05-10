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
}

func TestSelectPredicates_TierFilter(t *testing.T) {
	specs := selectPredicates("core")
	if len(specs) == 0 {
		t.Fatal("expected core specs")
	}
	for _, s := range specs {
		if s.Tier != "core" {
			t.Errorf("non-core %s tier=%s", s.ID, s.Tier)
		}
	}
}

func TestSelectPredicates_MultiTier(t *testing.T) {
	specs := selectPredicates("core,middleware")
	saw := map[string]bool{}
	for _, s := range specs {
		saw[s.Tier] = true
	}
	if !saw["core"] || !saw["middleware"] {
		t.Fatalf("expected both tiers, saw %v", saw)
	}
}

func TestSelectPredicates_Empty(t *testing.T) {
	if len(selectPredicates("")) == 0 {
		t.Fatal("empty filter should return all")
	}
}
