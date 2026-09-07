package report

import (
	"strings"
	"testing"
)

// cellWithStreams builds a cell whose tier-1 block carries the
// streaming route-coverage keys the validator now emits.
func cellWithStreams(refapp, engine string, wsProbed, wsPresent, wsSent, wsReached, wsAbsent int64) ValidationCellResult {
	return ValidationCellResult{
		Refapp: refapp,
		Engine: engine,
		Arch:   "amd64",
		Tier1: &Tier1Summary{
			WSTorture: map[string]int64{
				"ws_route_probed":    wsProbed,
				"ws_route_present":   wsPresent,
				"ws_sent":            wsSent,
				"ws_upgraded":        wsReached,
				"ws_endpoint_absent": wsAbsent,
			},
			SSEKill: map[string]int64{},
		},
	}
}

// TestStreamingCoverage_SeparatesRoutedFromAbsent is the reporting
// half of the coverage hole: across the v1.5.11 soak, 87.5% of WS
// upgrades and 96.5% of SSE GETs hit refapps with no such endpoint,
// and the aggregate buried that under one big ws_upgraded total. The
// coverage view has to attribute reach to the refapps that actually
// route the endpoint.
func TestStreamingCoverage_SeparatesRoutedFromAbsent(t *testing.T) {
	cells := []ValidationCellResult{
		cellWithStreams("auth_session_ratelimit", "std", 1, 1, 400, 380, 0),
		cellWithStreams("auth_session_ratelimit", "epoll", 1, 1, 200, 190, 0),
		cellWithStreams("kitchen_sink", "std", 1, 0, 0, 0, 0),
		cellWithStreams("kitchen_sink", "epoll", 1, 0, 0, 0, 0),
	}
	cov := StreamingCoverage(cells)
	if len(cov) != 2 {
		t.Fatalf("expected one row per refapp, got %d", len(cov))
	}
	if cov[0].Refapp != "auth_session_ratelimit" {
		t.Errorf("rows not sorted by refapp: %+v", cov)
	}
	auth := cov[0]
	if auth.Cells != 2 || auth.WS.PresentCells != 2 {
		t.Errorf("auth row: cells=%d ws present=%d, want 2/2", auth.Cells, auth.WS.PresentCells)
	}
	if auth.WS.Sent != 600 || auth.WS.Reached != 570 {
		t.Errorf("auth row: ws sent=%d reached=%d, want 600/570", auth.WS.Sent, auth.WS.Reached)
	}
	ks := cov[1]
	if ks.WS.PresentCells != 0 || ks.WS.Sent != 0 {
		t.Errorf("kitchen_sink row: present=%d sent=%d, want 0/0", ks.WS.PresentCells, ks.WS.Sent)
	}
	if !ks.WS.RouteAbsent() {
		t.Error("kitchen_sink probed every cell and found no /ws — want RouteAbsent")
	}
}

// TestStreamingCoverage_UnprobedIsNotAbsent: a cell whose streaming
// slices were dormant (smoke concurrency) never asked. That is
// "unknown", not "the refapp has no endpoint".
func TestStreamingCoverage_UnprobedIsNotAbsent(t *testing.T) {
	cov := StreamingCoverage([]ValidationCellResult{
		cellWithStreams("observability", "std", 0, 0, 0, 0, 0),
	})
	if len(cov) != 1 {
		t.Fatalf("got %d rows", len(cov))
	}
	if cov[0].WS.RouteAbsent() {
		t.Error("an unprobed cell must not read as a missing route")
	}
	if cov[0].WS.ProbedCells != 0 {
		t.Errorf("ProbedCells = %d, want 0", cov[0].WS.ProbedCells)
	}
}

// TestFormatStreamingCoverage_NamesTheAbsentRefapps: the whole point
// is that a near-total absent rate is READABLE in the run log.
func TestFormatStreamingCoverage_NamesTheAbsentRefapps(t *testing.T) {
	out := FormatStreamingCoverage(StreamingCoverage([]ValidationCellResult{
		cellWithStreams("auth_session_ratelimit", "std", 1, 1, 400, 380, 0),
		cellWithStreams("kitchen_sink", "std", 1, 0, 0, 0, 0),
	}))
	for _, want := range []string{"auth_session_ratelimit", "kitchen_sink", "no route"} {
		if !strings.Contains(out, want) {
			t.Errorf("coverage table missing %q:\n%s", want, out)
		}
	}
}
