package debugvars

import (
	"sync"
	"sync/atomic"
	"testing"
)

// A poll that interleaves with DriverRead's two adds must never publish
// hits > reads: the checker scores that pair as a validator-internal
// violation (it did, twice in 18 driver cells of the first full-harness
// nightly). Hammer the document from one goroutine while others record
// hits, and check every published pair.
func TestDriverDocument_HitsNeverExceedReadsUnderConcurrentPolls(t *testing.T) {
	dv := New()
	var stop atomic.Bool
	var writers sync.WaitGroup
	for i := 0; i < 4; i++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for !stop.Load() {
				dv.DriverWrite()
				dv.DriverRead(true)
			}
		}()
	}
	torn := 0
	for i := 0; i < 20_000; i++ {
		doc := dv.Document()
		if doc["celeris.driver_read_hits"].(int64) > doc["celeris.driver_reads_issued"].(int64) {
			torn++
		}
	}
	stop.Store(true)
	writers.Wait()
	if torn != 0 {
		t.Fatalf("%d of 20000 polls published hits > reads_issued", torn)
	}
}
