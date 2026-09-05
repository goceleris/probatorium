package validation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/checker"
	"github.com/goceleris/probatorium/validation/properties"
)

// fakeDebugVars serves the refapp /debug/vars shape with a settable
// panic count, standing in for a real refapp.
func fakeDebugVars(t *testing.T, panics *atomic.Int64) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/debug/vars" {
			w.WriteHeader(404)
			return
		}
		fmt.Fprintf(w, `{"goroutines": 40, "celeris.accepted_conn_total": 10, "celeris.closed_conn_total": 9,
			"celeris.active_conns": 1, "celeris.panic_count": %d, "memstats": {"HeapInuse": 4194304, "HeapAlloc": 3000000}}`,
			panics.Load())
	}))
	t.Cleanup(srv.Close)
	return srv
}

// The loop polls, sets the baseline from the first sample, and surfaces
// the FIRST violation of a predicate as an Incident while counting every
// subsequent one in the tally -- the contract the orchestrator and the
// gate rely on.
func TestRunPropertyLoop_FiresIncidentOnceAndCountsEveryViolation(t *testing.T) {
	var panics atomic.Int64
	srv := fakeDebugVars(t, &panics)
	violations := make(chan Incident, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	go func() {
		time.Sleep(150 * time.Millisecond)
		panics.Store(1)
	}()
	snapPath := filepath.Join(t.TempDir(), "properties_tally.json")
	tally := runPropertyLoop(ctx, propertyLoopConfig{
		MetricsURL:   srv.URL + "/debug/vars",
		PID:          os.Getpid(),
		Interval:     10 * time.Millisecond,
		Specs:        checker.SelectPredicates("core"),
		Violations:   violations,
		SnapshotPath: snapPath,
	})
	if tally.Samples < 10 || tally.PollErrors != 0 {
		t.Fatalf("samples=%d poll_errors=%d", tally.Samples, tally.PollErrors)
	}
	if tally.BaselineGoroutines != 40 || tally.FirstHeapInuse != 4194304 {
		t.Fatalf("baseline not taken from the first sample: %+v", tally)
	}
	if tally.Failed() != 1 || tally.ViolationIDs[0] != properties.IPANIC.ID {
		t.Fatalf("want exactly I-PANIC failed, got %v", tally.ViolationIDs)
	}
	if tally.Violations < 2 {
		t.Fatalf("a persisting violation must be counted on every sample, got %d", tally.Violations)
	}
	if tally.Passed() == 0 {
		t.Fatal("instrumented predicates that never failed must count as passed")
	}
	if !strings.Contains(tally.FailureSummaries["I-PANIC"], "I-PANIC") {
		t.Fatalf("failure summary: %+v", tally.FailureSummaries)
	}
	select {
	case inc := <-violations:
		if inc.PredicateID != properties.IPANIC.ID || inc.Tier != TierProperty {
			t.Fatalf("incident: %+v", inc)
		}
		if inc.Snapshot.PanicCount != 1 || inc.RefappPID != os.Getpid() {
			t.Fatalf("incident must carry the offending snapshot and pid: %+v", inc)
		}
	default:
		t.Fatal("no incident emitted for the first I-PANIC violation")
	}
	if len(violations) != 0 {
		t.Fatal("only the FIRST violation of a predicate may be emitted as an incident")
	}
	if runtime.GOOS == "linux" && tally.LastRSS == 0 {
		t.Fatal("RSS must be sampled from /proc on linux")
	}
	if _, err := os.Stat(snapPath); err != nil {
		t.Fatalf("properties_tally.json not written on exit: %v", err)
	}
}

// An unreachable /debug/vars must show up as poll errors with zero
// samples -- never as a healthy all-zero process.
func TestRunPropertyLoop_DeadEndpointIsVisible(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	tally := runPropertyLoop(ctx, propertyLoopConfig{
		MetricsURL: srv.URL + "/debug/vars",
		Interval:   10 * time.Millisecond,
		Specs:      checker.SelectPredicates("core,middleware"),
	})
	if tally.Samples != 0 || tally.Evaluations != 0 || tally.PollErrors == 0 {
		t.Fatalf("dead endpoint: %+v", tally)
	}
	if tally.Passed() != 0 || tally.Failed() != 0 {
		t.Fatalf("nothing was evaluated, nothing may pass: passed=%d failed=%d", tally.Passed(), tally.Failed())
	}
}

// The property tally must reach report.Tier1Summary (the matrix cell
// projection) -- the field-completeness test cannot see a nested struct.
func TestTier1Summary_ProjectsPropertyTally(t *testing.T) {
	s := tier1TallySnapshot{Properties: checker.Tally{
		Samples: 100, PollErrors: 2, Evaluations: 1400, Skips: 300, Violations: 30,
		ViolationIDs: []string{"I-MEM-1", "I-MEM-3"},
	}}
	sum := s.Tier1Summary()
	if sum.PropertyEvaluations != 1400 || sum.PropertyViolations != 30 || sum.PropertyPollErrors != 2 || sum.PropertySkips != 300 {
		t.Fatalf("scalars not projected: %+v", sum)
	}
	if sum.PropertyLoopSkipped != "" {
		t.Fatalf("a loop that ran must not read as skipped: %q", sum.PropertyLoopSkipped)
	}
	skipped := tier1TallySnapshot{Properties: checker.Tally{SkippedReason: propertyLoopSkippedSSH}}.Tier1Summary()
	if skipped.PropertyLoopSkipped != propertyLoopSkippedSSH || skipped.PropertyEvaluations != 0 {
		t.Fatalf("skipped loop not projected: %+v", skipped)
	}
	if len(sum.PropertyViolationIDs) != 2 || sum.PropertyViolationIDs[1] != "I-MEM-3" {
		t.Fatalf("ids not projected: %+v", sum.PropertyViolationIDs)
	}
}

// Single-cell document: properties_passed/failed and the soak leak
// fields are filled from the loop's tally.
func TestWriteValidateResults_PropertiesAndSoakLeak(t *testing.T) {
	cfg := Default()
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/usr/bin/true"
	cfg.SoakMode = true
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	o.tier1Ran = true
	o.tier1Snapshot = tier1TallySnapshot{
		RequestsSent: 10, Requests2xx: 10,
		Properties: checker.Tally{
			Samples: 900, Evaluations: 2700, Violations: 12,
			Predicates:       []string{"I-MEM-1", "I-MEM-3", "I-PANIC"},
			ViolationIDs:     []string{"I-MEM-3"},
			FailureSummaries: map[string]string{"I-MEM-3": "I-MEM-3 violated: goroutine slope 3.00/s"},
			FirstHeapInuse:   100_000_000, LastHeapInuse: 350_000_000,
		},
	}
	if err := o.writeValidateResults(time.Now().Add(-time.Minute)); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(filepath.Join(cfg.OutDir, "validate-results.json"))
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{
		`"properties_passed": 2`,
		`"properties_failed": 1`,
		`"I-MEM-3": "I-MEM-3 violated: goroutine slope 3.00/s"`,
		`"property_evaluations": 2700`,
		`"property_violations": 12`,
		`"property_violation_ids": [`,
		`"goroutine_leak_detected": true`,
		`"heap_growth_mb": 250`,
	} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("missing %q in document:\n%s", want, raw)
		}
	}
}
