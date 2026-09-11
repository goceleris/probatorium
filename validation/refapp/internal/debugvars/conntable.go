package debugvars

import (
	"hash/fnv"
	"sync"
)

// Per-connection last-byte table, the data source for I-CONN-1 ("every
// accepted conn closes within 30s of its last observed byte").
//
// The predicate has existed since the suite was written and has never run:
// nothing populated Snapshot.OldestOpenConnLastByteAgeMs, so it evaluated
// 0 > threshold on every tick and passed vacuously in all 48 cells.
//
// WHY THIS LIVES IN THE REFAPP AND NOT IN THE ENGINE
//
// celeris already keeps a per-connection stamp — cs.lastActivity in both
// native engines — and exposing it would be the cheaper source: one compare
// inside a sweep that already reads the field. It would also be the wrong
// source, for three reasons that all point the same way.
//
//   - It is blind to the bug this predicate names. Both engines derive
//     timeouts by walking their live-conn slice, so a minimum over that
//     slice only sees connections the engine still remembers. A connection
//     the engine has FORGOTTEN — dropped from the live set but never closed
//     — is exactly the phantom-socket class I-CONN-1 exists to catch, and
//     the adaptive transplant paths drop connections with no hook at all.
//     Such a connection appears in no live set and would read perfectly
//     clean. This table still holds it, because no disconnect ever fired.
//   - Adoption restamps. A transplant sets lastActivity to now, erasing a
//     pre-existing stall from the engine's own clock.
//   - The two engines do not mean the same thing by it. io_uring stamps on
//     accept, on receive and on send completion; epoll has no write-side
//     stamp at all. The same outbound-only stream would look idle on one
//     engine and busy on the other.
//
// And it buys nothing on the std engine, which has no connection table for
// celeris to read.

// connShards is the stripe count. Connect and disconnect run on the engine
// event loop, which celeris documents as "must be fast — blocks the event
// loop", so the table must not become a single contended mutex at a few
// thousand connections per second.
const connShards = 64

type connEntry struct {
	// n is a reference count, not decoration. The epoll engine closes the
	// socket BEFORE invoking the disconnect hook, so the kernel is free to
	// hand the same ephemeral port to a new connection on another worker
	// and fire ITS connect hook first. The ordering 1 -> 2 -> 1 is normal
	// and must not delete a live entry; a plain map delete here would drop
	// the new connection's stamp and make the table under-report, which is
	// a false negative in a leak detector.
	n       int
	stampNs int64
}

type connTable struct {
	shards [connShards]struct {
		mu sync.Mutex
		m  map[string]*connEntry
	}
}

func (t *connTable) shard(addr string) *struct {
	mu sync.Mutex
	m  map[string]*connEntry
} {
	h := fnv.New32a()
	_, _ = h.Write([]byte(addr))
	return &t.shards[h.Sum32()%connShards]
}

// open records a newly accepted connection, or increments an existing
// entry when the peer address is being reused (see connEntry.n).
func (t *connTable) open(addr string, nowNs int64) {
	s := t.shard(addr)
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.m == nil {
		s.m = make(map[string]*connEntry)
	}
	if e, ok := s.m[addr]; ok {
		e.n++
		e.stampNs = nowNs
		return
	}
	s.m[addr] = &connEntry{n: 1, stampNs: nowNs}
}

// touch refreshes the last-byte stamp. Unknown addresses are ignored
// rather than inserted: an entry with no matching open would never be
// closed, and would age forever into a false positive.
func (t *connTable) touch(addr string, nowNs int64) {
	s := t.shard(addr)
	s.mu.Lock()
	defer s.mu.Unlock()
	if e, ok := s.m[addr]; ok {
		e.stampNs = nowNs
	}
}

// close drops one reference, removing the entry at zero.
func (t *connTable) close(addr string) {
	s := t.shard(addr)
	s.mu.Lock()
	defer s.mu.Unlock()
	e, ok := s.m[addr]
	if !ok {
		return
	}
	if e.n--; e.n <= 0 {
		delete(s.m, addr)
	}
}

// oldestAgeMs is the age in milliseconds of the least recently stamped open
// connection, or 0 when none are open. Zero is the "nothing to report"
// value the predicate treats as clean, which is correct: an empty table
// means every accepted connection has been closed.
func (t *connTable) oldestAgeMs(nowNs int64) int64 {
	var oldest int64
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		for _, e := range s.m {
			if oldest == 0 || e.stampNs < oldest {
				oldest = e.stampNs
			}
		}
		s.mu.Unlock()
	}
	if oldest == 0 {
		return 0
	}
	if age := (nowNs - oldest) / 1e6; age > 0 {
		return age
	}
	return 0
}

// liveConns is the number of entries currently held, exported for the
// document so a zero age can be read as "no connections" rather than
// "instrumentation broken".
func (t *connTable) liveConns() int64 {
	var n int64
	for i := range t.shards {
		s := &t.shards[i]
		s.mu.Lock()
		n += int64(len(s.m))
		s.mu.Unlock()
	}
	return n
}
