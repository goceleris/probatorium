package validation

import (
	"sync"
	"time"

	"github.com/goceleris/probatorium/report"
)

// eventSpan keeps the earliest and the latest event of one rare kind (an
// h2c_hang, a ws_handshake_fail) with its instant, cause and error
// (schema 5.23, probatorium#481). Walkers run concurrently, so a mark can
// arrive carrying an instant earlier than one already filed: first and last
// are the true minimum and maximum by instant, not by arrival. Marking is on
// the failure path only, so a mutex costs a routine run nothing.
type eventSpan struct {
	mu              sync.Mutex
	n               int64
	first, last     report.EventStamp
	firstAt, lastAt time.Time
}

// mark files one event that happened at at.
func (s *eventSpan) mark(at time.Time, st report.EventStamp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.n++
	if s.n == 1 || at.Before(s.firstAt) {
		s.firstAt, s.first = at, st
	}
	if s.n == 1 || !at.Before(s.lastAt) {
		s.lastAt, s.last = at, st
	}
}

// snapshot is the span so far, nil when no event was marked.
func (s *eventSpan) snapshot() *report.EventSpan {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.n == 0 {
		return nil
	}
	return &report.EventSpan{Count: s.n, First: s.first, Last: s.last}
}
