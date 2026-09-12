package debugvars

import (
	"fmt"
	"sync"
	"testing"
	"time"
)

const msNs = int64(time.Millisecond)

// TestConnTableEmptyReportsZero: no open connections must read as zero, not
// as an enormous age. The predicate treats zero as clean, and an empty table
// genuinely is clean -- every accepted connection has been closed.
func TestConnTableEmptyReportsZero(t *testing.T) {
	var ct connTable
	if got := ct.oldestAgeMs(1_000 * msNs); got != 0 {
		t.Fatalf("empty table reported age %d, want 0", got)
	}
	if got := ct.liveConns(); got != 0 {
		t.Fatalf("empty table reported %d live conns, want 0", got)
	}
}

// TestConnTableReportsTheOldestOpenConn is the core behaviour: the age is a
// minimum over stamps, not of the most recent one.
func TestConnTableReportsTheOldestOpenConn(t *testing.T) {
	var ct connTable
	now := int64(1_000_000) * msNs
	ct.open("10.0.0.1:1", now-60_000*msNs) // 60s old
	ct.open("10.0.0.2:2", now-5_000*msNs)  // 5s old
	ct.open("10.0.0.3:3", now-100*msNs)    // fresh

	if got := ct.oldestAgeMs(now); got < 59_000 || got > 61_000 {
		t.Fatalf("oldest age = %dms, want ~60000", got)
	}
	if got := ct.liveConns(); got != 3 {
		t.Fatalf("live conns = %d, want 3", got)
	}
}

// TestConnTableTouchResetsTheStamp: a connection that keeps transacting must
// never age. Without this the busiest connection in the run becomes the
// oldest entry and the predicate fires on healthy traffic.
func TestConnTableTouchResetsTheStamp(t *testing.T) {
	var ct connTable
	now := int64(1_000_000) * msNs
	ct.open("10.0.0.1:1", now-60_000*msNs)
	if got := ct.oldestAgeMs(now); got < 59_000 {
		t.Fatalf("precondition: age = %dms, want ~60000", got)
	}
	ct.touch("10.0.0.1:1", now)
	if got := ct.oldestAgeMs(now); got != 0 {
		t.Fatalf("age after touch = %dms, want 0", got)
	}
}

// TestConnTableTouchIgnoresUnknownAddrs: touching an address with no open
// entry must NOT insert one. An inserted entry would never be closed -- no
// disconnect will ever fire for it -- so it would age forever into a false
// positive that no amount of healthy traffic could clear.
func TestConnTableTouchIgnoresUnknownAddrs(t *testing.T) {
	var ct connTable
	now := int64(1_000_000) * msNs
	ct.touch("10.0.0.9:9", now-90_000*msNs)
	if got := ct.liveConns(); got != 0 {
		t.Fatalf("touch on an unknown addr created %d entries, want 0", got)
	}
	if got := ct.oldestAgeMs(now); got != 0 {
		t.Fatalf("age = %dms after touching an unknown addr, want 0", got)
	}
}

// TestConnTableSurvivesPortReuseInterleaving is the one a plain map delete
// fails.
//
// The epoll engine closes the socket BEFORE invoking the disconnect hook, so
// the kernel can hand the same ephemeral port to a new connection on another
// worker and fire ITS connect hook first. The real ordering is therefore
// open(A), open(B on the same addr), close(A) -- and the entry must survive,
// carrying B's stamp, because B is live. Deleting on the first close would
// under-report, which in a leak detector is a false negative.
func TestConnTableSurvivesPortReuseInterleaving(t *testing.T) {
	var ct connTable
	now := int64(1_000_000) * msNs
	const addr = "10.0.0.1:54321"

	ct.open(addr, now-60_000*msNs) // conn A, old
	ct.open(addr, now)             // conn B reuses the port, before A's close lands
	ct.close(addr)                 // A's disconnect finally arrives

	if got := ct.liveConns(); got != 1 {
		t.Fatalf("live conns = %d after port-reuse interleaving, want 1 (B is still open)", got)
	}
	if got := ct.oldestAgeMs(now); got != 0 {
		t.Fatalf("age = %dms, want 0: the surviving entry is B, which was just stamped", got)
	}
	ct.close(addr) // B closes
	if got := ct.liveConns(); got != 0 {
		t.Fatalf("live conns = %d after both closed, want 0", got)
	}
}

// TestConnTableCloseIsIdempotent: a disconnect with no matching entry must
// not panic or drive the count negative. Engines are allowed to be sloppy
// here; the table must not be.
func TestConnTableCloseIsIdempotent(t *testing.T) {
	var ct connTable
	ct.close("10.0.0.1:1")
	ct.open("10.0.0.1:1", 1000*msNs)
	ct.close("10.0.0.1:1")
	ct.close("10.0.0.1:1")
	if got := ct.liveConns(); got != 0 {
		t.Fatalf("live conns = %d, want 0", got)
	}
}

// TestConnTableIsRaceFree runs the real access pattern concurrently. Connect
// and disconnect fire on the engine event loop while the document handler
// reads; run with -race this is the guard that the sharding is correct.
func TestConnTableIsRaceFree(t *testing.T) {
	var ct connTable
	var writers, reader sync.WaitGroup
	stop := make(chan struct{})

	for w := range 8 {
		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := range 500 {
				addr := fmt.Sprintf("10.0.%d.%d:%d", w, i%251, 1024+i%4096)
				now := time.Now().UnixNano()
				ct.open(addr, now)
				ct.touch(addr, now)
				ct.close(addr)
			}
		}(w)
	}
	// The reader gets its OWN WaitGroup. Putting it in the same one as the
	// writers deadlocks: it exits on `stop`, `stop` closes after the wait,
	// and the wait includes the reader. (Written that way first; the test
	// hung for ten minutes before I read it back.)
	reader.Add(1)
	go func() {
		defer reader.Done()
		for {
			select {
			case <-stop:
				return
			default:
				_ = ct.oldestAgeMs(time.Now().UnixNano())
				_ = ct.liveConns()
			}
		}
	}()
	writers.Wait()
	close(stop)
	reader.Wait()

	if got := ct.liveConns(); got != 0 {
		t.Fatalf("live conns = %d after every open was matched by a close, want 0", got)
	}
}
