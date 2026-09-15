package report

import (
	"strings"
	"testing"
)

// An adaptive cell that never promoted validated epoll under another name.
// With ExpectAdaptiveSwitch the gate fails it by name; cells on the other
// engines, and adaptive cells that did promote, are untouched.
func TestGate_ExpectAdaptiveSwitchFailsAnAdaptiveCellThatNeverPromoted(t *testing.T) {
	stayed := cleanCell("kitchen_sink", "adaptive", "arm64")
	stayed.Tier1.AdaptiveSwitches = 0
	promoted := cleanCell("kitchen_sink", "adaptive", "amd64")
	promoted.Tier1.AdaptiveSwitches = 1
	epoll := cleanCell("kitchen_sink", "epoll", "arm64")
	epoll.Tier1.AdaptiveSwitches = 0

	base := GateOptions{RequireTier3: true, RequireProperties: true}
	if v := Gate([]ValidationCellResult{stayed, promoted, epoll}, nil, base); len(v) != 0 {
		t.Fatalf("without the expectation the switch count is not gated, got %v", v)
	}

	opts := base
	opts.ExpectAdaptiveSwitch = true
	v := Gate([]ValidationCellResult{stayed, promoted, epoll}, nil, opts)
	if len(v) != 1 {
		t.Fatalf("want exactly the never-promoted adaptive cell, got %v", v)
	}
	if v[0].Field != "tier_1.adaptive_switches" || v[0].Engine != "adaptive" || v[0].Arch != "arm64" {
		t.Fatalf("violation must name the field and the cell, got %+v", v[0])
	}
	if !strings.Contains(v[0].Why, "VALIDATE_GATE_EXPECT_ADAPTIVE_SWITCH") {
		t.Fatalf("Why must name the knob, got %q", v[0].Why)
	}

	// A cell whose loop never ran has no sample to judge.
	stayed.Tier1.PropertyEvaluations = 0
	stayed.Tier1.PropertyLoopSkipped = "ssh driver: remote /debug/vars is loopback-only"
	if v := Gate([]ValidationCellResult{stayed}, nil, opts); len(v) != 0 {
		t.Fatalf("a cell without a property loop cannot vote, got %v", v)
	}
}

// The violation is only actionable with the controller's own two inputs
// beside it. Nightly 34876253223 reported seven cells at
// adaptive_switches == 0 with no way to tell apart a harness that offered
// too little load, a controller that deliberately suppressed a link-bound
// workload, and a celeris defect -- the three have different owners and the
// artifact named none of them.
func TestGate_AdaptiveSwitchViolationCarriesTheMeasuredLoad(t *testing.T) {
	stayed := cleanCell("driver_postgres", "adaptive", "arm64")
	stayed.Tier1.AdaptiveSwitches = 0
	stayed.Tier1.PeakConnsPerWorker = 50.5
	stayed.Tier1.MeanBytesPerReq = 11_400

	opts := GateOptions{RequireTier3: true, RequireProperties: true, ExpectAdaptiveSwitch: true}
	v := Gate([]ValidationCellResult{stayed}, nil, opts)
	if len(v) != 1 {
		t.Fatalf("want one violation, got %v", v)
	}
	for _, want := range []string{"50.5 conns/worker", "11400 bytes/req"} {
		if !strings.Contains(v[0].Why, want) {
			t.Errorf("Why must carry %q, got %q", want, v[0].Why)
		}
	}
}

// The control for that: a cell whose refapp published no engine metrics must
// read as zero, which is a visibly different sentence from a measured load.
// Reporting 0.0/0 as though it were a reading would invite exactly the wrong
// conclusion -- "the load never arrived" -- from a cell that simply was not
// instrumented.
func TestGate_AdaptiveSwitchViolationShowsZeroWhenNothingWasMeasured(t *testing.T) {
	stayed := cleanCell("driver_postgres", "adaptive", "arm64")
	stayed.Tier1.AdaptiveSwitches = 0

	opts := GateOptions{RequireTier3: true, RequireProperties: true, ExpectAdaptiveSwitch: true}
	v := Gate([]ValidationCellResult{stayed}, nil, opts)
	if len(v) != 1 {
		t.Fatalf("want one violation, got %v", v)
	}
	if !strings.Contains(v[0].Why, "peak 0.0 conns/worker, mean 0 bytes/req") {
		t.Errorf("an unmeasured cell must say so, got %q", v[0].Why)
	}
}
