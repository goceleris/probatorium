package validation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/checker"
	"github.com/goceleris/probatorium/validation/properties"
)

// The loop declares I-RACE on the refapp's race_build flag and feeds the
// liveness scan's count on every tick: a -race refapp whose scan saw a
// report violates; a plain refapp stays not-instrumented however high the
// count (its zero would be a lie either way).
func TestRunPropertyLoop_IRACEDeclaredOnRaceBuildAndFedLive(t *testing.T) {
	run := func(raceBuild bool, reports int64) checker.Tally {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"goroutines": 40, "celeris.race_build": %v, "memstats": {"HeapInuse": 4194304, "HeapAlloc": 3000000}}`, raceBuild)
		}))
		defer srv.Close()
		var n atomic.Int64
		n.Store(reports)
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		return runPropertyLoop(ctx, propertyLoopConfig{
			MetricsURL:  srv.URL + "/debug/vars",
			Interval:    20 * time.Millisecond,
			Specs:       []properties.Spec{properties.IRACE},
			RaceReports: n.Load,
		})
	}
	if tally := run(true, 0); slices.Contains(tally.NotInstrumented, "I-RACE") || tally.PerPredicate["I-RACE"] != 0 {
		t.Errorf("race build, no reports: must be instrumented and clean, got not_instrumented=%v per=%v", tally.NotInstrumented, tally.PerPredicate)
	}
	if tally := run(true, 2); tally.PerPredicate["I-RACE"] < 1 {
		t.Errorf("race build with reports must violate, got %v", tally.PerPredicate)
	}
	if tally := run(false, 2); !slices.Contains(tally.NotInstrumented, "I-RACE") {
		t.Errorf("plain build must stay not-instrumented, got %v", tally.NotInstrumented)
	}
}
