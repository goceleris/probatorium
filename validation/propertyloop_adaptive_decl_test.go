package validation

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/checker"
	"github.com/goceleris/probatorium/validation/properties"
)

// The loop declares I-ENG-ADAPTIVE from the engine name the refapp reports
// (celeris publishes engine.Type.String(), "adaptive"), and only there.
func TestRunPropertyLoop_DeclaresIENGAdaptiveOnlyOnTheAdaptiveEngine(t *testing.T) {
	run := func(engine string) checker.Tally {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"goroutines": 40, "celeris.engine": %q, "celeris.adaptive_switches": 0,
				"memstats": {"HeapInuse": 4194304, "HeapAlloc": 3000000}}`, engine)
		}))
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		return runPropertyLoop(ctx, propertyLoopConfig{
			MetricsURL: srv.URL + "/debug/vars",
			Interval:   20 * time.Millisecond,
			Specs:      []properties.Spec{properties.IENGAdaptive},
		})
	}
	if ni := run("adaptive").NotInstrumented; slices.Contains(ni, "I-ENG-ADAPTIVE") {
		t.Errorf("an adaptive cell must declare I-ENG-ADAPTIVE, got not_instrumented=%v", ni)
	}
	for _, e := range []string{"io_uring", "epoll", "std"} {
		if ni := run(e).NotInstrumented; !slices.Contains(ni, "I-ENG-ADAPTIVE") {
			t.Errorf("%s never moves the switch counter; must stay not-instrumented, got %v", e, ni)
		}
	}
}
