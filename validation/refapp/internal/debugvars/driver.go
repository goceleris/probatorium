package debugvars

import "sync/atomic"

// driver carries the read-after-write oracle behind I-DRV. The driver
// refapps (driver_redis, driver_postgres, driver_memcached) already read
// every write back inside the handler and answer 500 with x-invariant
// I-DRV-1 on a mismatch; until now that answer was only visible as one
// more 5xx in the walker's histogram, and the predicate that exists for
// exactly this read counters nothing ever wrote to. Counting here lets
// the property loop judge I-DRV on the refapp's own evidence.
//
// A read is "issued" only when it CHECKED a write: a plain GET with no
// expectation cannot miss and is not counted. A read that errored is not
// counted either -- it returned nothing to compare, and whether the
// store was reachable is the 5xx oracle's question, not this one's.
type driver struct {
	writes, reads, hits, misses atomic.Int64
}

// DriverWrite records one write the handler will read back.
func (v *Vars) DriverWrite() { v.drv.writes.Add(1) }

// DriverRead records the read-back of a write: hit when it returned the
// value just written, miss when it returned something else. reads is
// bumped first: driverDocument relies on reads >= hits holding at every
// instant (see there).
func (v *Vars) DriverRead(hit bool) {
	v.drv.reads.Add(1)
	if hit {
		v.drv.hits.Add(1)
	} else {
		v.drv.misses.Add(1)
	}
}

// driverDocument adds the I-DRV keys to doc. Present in every refapp's
// document (the checker reads a fixed shape); only a refapp that declared
// I-DRV makes the zeros mean anything.
//
// Load order is load-bearing. The predicate's own sanity check is
// hits <= reads, and the counters are four independent atomics, so a
// poll that lands between a DriverRead's two adds sees a torn pair.
// DriverRead bumps reads BEFORE hits, so at every instant reads >= hits;
// reading hits (and misses) BEFORE reads therefore yields
// hits_loaded <= reads_at_that_instant <= reads_loaded, and the pair is
// consistent whatever interleaves. The first full-harness nightly read
// them the other way round and two of 18 driver cells reported
// hits(380748) > reads_issued(380747): a 1-in-400k torn poll, scored as
// a violation.
func (v *Vars) driverDocument(doc map[string]any) {
	misses := v.drv.misses.Load()
	hits := v.drv.hits.Load()
	reads := v.drv.reads.Load()
	writes := v.drv.writes.Load()
	doc["celeris.driver_writes_issued"] = writes
	doc["celeris.driver_reads_issued"] = reads
	doc["celeris.driver_read_hits"] = hits
	doc["celeris.driver_read_misses"] = misses
}
