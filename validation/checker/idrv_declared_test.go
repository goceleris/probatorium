package checker

import (
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// I-DRV reads the driver refapps' own read-after-write tally. Every refapp
// publishes the four keys, so a refapp that never talks to a store has
// structurally-zero counters: without a declaration that must read as not
// instrumented, never as a pass.
func TestIDRVIsNotInstrumentedWithoutADeclaration(t *testing.T) {
	e := NewEvaluator(SelectPredicates("driver"))
	now := time.Unix(1_700_000_000, 0)
	e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 10}, now)
	if ni := e.Tally().NotInstrumented; !slices.Contains(ni, "I-DRV") {
		t.Fatalf("I-DRV must be not-instrumented in a refapp that declared nothing, got %v", ni)
	}
}

// A driver refapp declares it, publishes its tally, and a miss is a
// violation carrying the counts.
func TestIDRVFiresOnADeclaredMiss(t *testing.T) {
	e := NewEvaluator(SelectPredicates("driver"))
	now := time.Unix(1_700_000_000, 0)
	clean := properties.Snapshot{
		TS: now.Unix(), GoroutineCount: 10, InstrumentedProperties: "I-DRV",
		DriverWritesIssued: 100, DriverReadsIssued: 100, DriverReadHits: 100,
	}
	e.Observe(clean, now)
	tally := e.Tally()
	if slices.Contains(tally.NotInstrumented, "I-DRV") || tally.PerPredicate["I-DRV"] != 0 {
		t.Fatalf("declared clean tally must count as instrumented and pass, got not_instrumented=%v per=%v", tally.NotInstrumented, tally.PerPredicate)
	}
	miss := clean
	miss.TS++
	miss.DriverWritesIssued, miss.DriverReadsIssued, miss.DriverReadMisses = 101, 101, 1
	e.Observe(miss, now.Add(time.Second))
	tally = e.Tally()
	if tally.PerPredicate["I-DRV"] != 1 {
		t.Fatalf("one miss must violate once, got %v", tally.PerPredicate)
	}
	if msg := tally.FailureSummaries["I-DRV"]; msg == "" {
		t.Fatal("violation must carry a message")
	}
}

// The parser reads the four keys the refapps publish.
func TestParseDebugVars_DriverCounters(t *testing.T) {
	var snap properties.Snapshot
	err := ParseDebugVars([]byte(`{"goroutines": 1, "celeris.driver_writes_issued": 7, "celeris.driver_reads_issued": 7,
		"celeris.driver_read_hits": 6, "celeris.driver_read_misses": 1, "celeris.instrumented_properties": "I-DRV"}`), &snap)
	if err != nil {
		t.Fatal(err)
	}
	if snap.DriverWritesIssued != 7 || snap.DriverReadsIssued != 7 || snap.DriverReadHits != 6 || snap.DriverReadMisses != 1 {
		t.Fatalf("driver counters not parsed: %+v", snap)
	}
}
