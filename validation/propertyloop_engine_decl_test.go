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

// The loop declares I-ENG-IOURING only when BOTH facts are in the document:
// the refapp is a -tags=validation build AND the engine is io_uring.
func TestRunPropertyLoop_DeclaresIENGIOURingOnlyForValidationBuildOnIOUring(t *testing.T) {
	run := func(engine string, validationBuild bool) checker.Tally {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_, _ = fmt.Fprintf(w, `{"goroutines": 40, "celeris.engine": %q, "celeris.validation_build": %v,
				"celeris.iouring_sqe_corruptions": 0, "memstats": {"HeapInuse": 4194304, "HeapAlloc": 3000000}}`, engine, validationBuild)
		}))
		defer srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
		defer cancel()
		return runPropertyLoop(ctx, propertyLoopConfig{
			MetricsURL: srv.URL + "/debug/vars",
			Interval:   20 * time.Millisecond,
			Specs:      []properties.Spec{properties.IENGIOURing},
		})
	}
	if ni := run("iouring", true).NotInstrumented; slices.Contains(ni, "I-ENG-IOURING") {
		t.Errorf("validation build on io_uring must declare I-ENG-IOURING, got not_instrumented=%v", ni)
	}
	if ni := run("epoll", true).NotInstrumented; !slices.Contains(ni, "I-ENG-IOURING") {
		t.Errorf("a validation build on epoll has no ring to check; must stay not-instrumented, got %v", ni)
	}
	if ni := run("iouring", false).NotInstrumented; !slices.Contains(ni, "I-ENG-IOURING") {
		t.Errorf("a plain build on io_uring has no checker; must stay not-instrumented, got %v", ni)
	}
}
