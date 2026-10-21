package validation

import (
	"sync"
	"time"

	"github.com/goceleris/probatorium/report"
)

// eventSpan keeps the earliest and the latest event of one rare kind (an
// h2c_hang, a ws_handshake_fail) with its instant, cause and error
// (schema 5.23, probatorium#481). STUB: records nothing yet.
type eventSpan struct {
	mu sync.Mutex
}

// mark files one event that happened at at.
func (s *eventSpan) mark(at time.Time, st report.EventStamp) {}

// snapshot is the span so far, nil when no event was marked.
func (s *eventSpan) snapshot() *report.EventSpan { return nil }
