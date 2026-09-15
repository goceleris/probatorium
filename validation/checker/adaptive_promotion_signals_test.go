package checker

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// The adaptive controller promotes epoll → io_uring on two signals it derives
// itself: conns/worker (ActiveConnections / Workers) and bytes/req (the
// per-interval delta of (BytesRead+BytesWritten) / RequestCount, suppressing a
// switch above a large-payload threshold). Nightly 34876253223 reported seven
// adaptive cells with adaptive_switches == 0 and carried NOTHING that could
// say why: the artifact recorded the outcome and none of the input. These
// tests cover the two ends of the wire that closes that gap -- the debugvars
// document publishing the four raw counters, and the tally reducing them to
// the two signals the gate prints next to the violation.

func TestParseDebugVarsReadsTheAdaptiveControllerInputs(t *testing.T) {
	doc := map[string]any{
		"goroutines":                    61,
		"celeris.active_conns":          101,
		"celeris.engine_workers":        2,
		"celeris.engine_requests_total": 48_120,
		"celeris.engine_bytes_read":     9_010_000,
		"celeris.engine_bytes_written":  14_020_000,
	}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var snap properties.Snapshot
	if err := ParseDebugVars(body, &snap); err != nil {
		t.Fatalf("ParseDebugVars: %v", err)
	}
	if snap.EngineWorkers != 2 {
		t.Errorf("EngineWorkers = %d, want 2", snap.EngineWorkers)
	}
	if snap.EngineRequestsTotal != 48_120 {
		t.Errorf("EngineRequestsTotal = %d, want 48120", snap.EngineRequestsTotal)
	}
	if snap.EngineBytesRead != 9_010_000 {
		t.Errorf("EngineBytesRead = %d, want 9010000", snap.EngineBytesRead)
	}
	if snap.EngineBytesWritten != 14_020_000 {
		t.Errorf("EngineBytesWritten = %d, want 14020000", snap.EngineBytesWritten)
	}
}

// A refapp that publishes none of the four keys must leave the fields at zero
// rather than inventing a reading -- this is the negative control for the
// parse, and the reason the gate prints the two signals as measured rather
// than asserting on them.
func TestParseDebugVarsLeavesTheAdaptiveInputsZeroWhenAbsent(t *testing.T) {
	body := []byte(`{"goroutines":61,"celeris.active_conns":101}`)
	var snap properties.Snapshot
	if err := ParseDebugVars(body, &snap); err != nil {
		t.Fatalf("ParseDebugVars: %v", err)
	}
	if snap.EngineWorkers != 0 || snap.EngineRequestsTotal != 0 ||
		snap.EngineBytesRead != 0 || snap.EngineBytesWritten != 0 {
		t.Fatalf("absent keys produced a reading: %+v", snap)
	}
}

func TestTallyReducesTheAdaptiveInputsToTheControllerSignals(t *testing.T) {
	e := NewEvaluator(nil)
	base := time.Unix(1_700_000_000, 0)
	// Sample 0 is warm-up: the engine is up, workers are known, and bytes
	// have already moved on the wire -- handshakes and in-flight request
	// bodies -- but no request has COMPLETED, so RequestCount is still
	// zero. Those bytes belong to no request yet. Taking the base here
	// instead of at the first request-bearing sample charges them to the
	// whole cell and pulls the mean below the truth (940 instead of 1000
	// for this series), which is exactly the direction that would make a
	// link-bound workload look small-payload.
	samples := []properties.Snapshot{
		{ActiveConns: 4, EngineWorkers: 2, EngineBytesRead: 300_000},
		{ActiveConns: 60, EngineWorkers: 2, EngineRequestsTotal: 1_000, EngineBytesRead: 400_000, EngineBytesWritten: 600_000},
		{ActiveConns: 101, EngineWorkers: 2, EngineRequestsTotal: 3_000, EngineBytesRead: 1_200_000, EngineBytesWritten: 1_800_000},
		{ActiveConns: 80, EngineWorkers: 2, EngineRequestsTotal: 5_000, EngineBytesRead: 2_000_000, EngineBytesWritten: 3_000_000},
	}
	for i, s := range samples {
		s.TS = base.Add(time.Duration(i) * time.Second).Unix()
		e.Observe(s, base.Add(time.Duration(i)*time.Second))
	}
	got := e.Tally()
	// Peak is sample 2: 101 conns over 2 workers.
	if want := 50.5; math.Abs(got.PeakConnsPerWorker-want) > 1e-9 {
		t.Errorf("PeakConnsPerWorker = %v, want %v", got.PeakConnsPerWorker, want)
	}
	// Mean is measured from the first request-bearing sample:
	// (5,000,000 - 1,000,000) bytes over (5,000 - 1,000) requests = 1000.
	if want := 1000.0; math.Abs(got.MeanBytesPerReq-want) > 1e-9 {
		t.Errorf("MeanBytesPerReq = %v, want %v", got.MeanBytesPerReq, want)
	}
}

// The control for the reduction: a cell whose refapp publishes no engine
// metrics must report both signals as zero, so a gate line reading
// "peak 0.0 conns/worker, mean 0 bytes/req" is unmistakably "not measured"
// and cannot be misread as "no load was offered".
func TestTallyLeavesTheControllerSignalsZeroWithoutEngineMetrics(t *testing.T) {
	e := NewEvaluator(nil)
	base := time.Unix(1_700_000_000, 0)
	for i := range 4 {
		e.Observe(properties.Snapshot{
			TS: base.Add(time.Duration(i) * time.Second).Unix(), ActiveConns: 101,
		}, base.Add(time.Duration(i)*time.Second))
	}
	got := e.Tally()
	if got.PeakConnsPerWorker != 0 || got.MeanBytesPerReq != 0 {
		t.Fatalf("signals invented without engine metrics: peak=%v mean=%v",
			got.PeakConnsPerWorker, got.MeanBytesPerReq)
	}
}
