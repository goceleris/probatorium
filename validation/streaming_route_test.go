package validation

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/remote"
)

// streamingRouteServer stands up an httptest server that answers 200
// everywhere EXCEPT the streaming endpoints, which get the status the
// caller names. Models the 7-of-8 refapps that ship no /ws and no
// /events at all (404) versus the one that does.
func streamingRouteServer(t *testing.T, streamStatus int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/ws", "/events":
			w.WriteHeader(streamStatus)
		default:
			w.WriteHeader(200)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// driveAgainst runs a full Tier 1 fan-out against srv at the given
// concurrency and returns the snapshot. The cell ends when done holds on
// the live tally, not after a fixed duration -- see tier1_until_test.go
// for why every fixed duration in this package became a flake.
func driveAgainst(t *testing.T, srv *httptest.Server, concurrency int, done func(tier1TallySnapshot) bool) tier1TallySnapshot {
	t.Helper()
	s, err := runTier1Until(t, tier1Config{
		Driver:      remote.NewLocal("/bin/sh"),
		RefappArgs:  readyThenIdle(srv.URL),
		BaseURL:     srv.URL,
		Matrix:      minimalMatrix(t),
		Seed:        42,
		Concurrency: concurrency,
	}, done)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	return s
}

// streamingSlicesFired is the condition for the tests that assert the WS
// and SSE walkers DID run.
func streamingSlicesFired(s tier1TallySnapshot) bool {
	return s.WSTorture.Sent >= 1 && s.SSEKill.Sent >= 1
}

// routesProbed is the condition for the tests that assert only on the
// pre-flight route verdict: the probe runs once, before the slices, so
// the verdict being recorded is the whole event being waited for.
func routesProbed(s tier1TallySnapshot) bool {
	return s.Tier1Summary().WSTorture["ws_route_probed"] == 1 &&
		s.Tier1Summary().SSEKill["sse_route_probed"] == 1
}

// TestDriveTier1_StreamingWalkersSkipAbsentRoutes is the coverage-hole
// regression: the v1.5.11 soak fired 1,999,114 WS upgrades and
// 1,499,314 SSE GETs at refapps that expose neither endpoint, so 87.5%
// of the streaming budget bought nothing but 404s and the ws_*/sse_*
// zeros were a statement about routing, not robustness. A refapp that
// 404s the endpoint must not be walked at all.
func TestDriveTier1_StreamingWalkersSkipAbsentRoutes(t *testing.T) {
	srv := streamingRouteServer(t, http.StatusNotFound)
	// A "must not be walked" claim needs the cell to run long enough for
	// the walkers to have fired had they been scheduled -- the WS torture
	// slice ticks at 150 ms, SSE kill at 200 ms -- so this is a floor on
	// serving time, not a ceiling on the whole test.
	s := driveAgainst(t, srv, 20, workedFor(800*time.Millisecond))

	if s.WSTorture.Sent != 0 {
		t.Errorf("ws torture fired %d times at a 404 /ws — walker must not run on a refapp without the route (absent=%d)",
			s.WSTorture.Sent, s.WSTorture.EndpointAbsent)
	}
	if s.SSEKill.Sent != 0 {
		t.Errorf("sse kill fired %d times at a 404 /events — walker must not run on a refapp without the route (absent=%d)",
			s.SSEKill.Sent, s.SSEKill.EndpointAbsent)
	}
}

// TestDriveTier1_StreamingWalkersRunOnPresentRoutes is the other half:
// a refapp that answers the endpoint with anything but 404 still gets
// tortured. 400 (bad handshake) is what celeris's WS middleware
// replies to a probe that isn't a real upgrade, so "present" must not
// be confused with "handshake succeeded".
func TestDriveTier1_StreamingWalkersRunOnPresentRoutes(t *testing.T) {
	srv := streamingRouteServer(t, http.StatusBadRequest)
	s := driveAgainst(t, srv, 20, streamingSlicesFired)

	if s.WSTorture.Sent < 1 {
		t.Errorf("ws torture didn't fire at a present /ws — Sent=%d", s.WSTorture.Sent)
	}
	if s.SSEKill.Sent < 1 {
		t.Errorf("sse kill didn't fire at a present /events — Sent=%d", s.SSEKill.Sent)
	}
}

// TestTier1Summary_CarriesStreamingRouteCoverage asserts the per-cell
// document says WHETHER each streaming endpoint exists on this refapp.
// Without it a cell of ws_* zeros is indistinguishable from a cell
// that was never walked — which is how a near-total absent rate stayed
// buried across 48 cells.
func TestTier1Summary_CarriesStreamingRouteCoverage(t *testing.T) {
	absent := driveAgainst(t, streamingRouteServer(t, http.StatusNotFound), 20, routesProbed).Tier1Summary()
	if got := absent.WSTorture["ws_route_probed"]; got != 1 {
		t.Errorf("ws_route_probed on a 404 refapp: got %d, want 1", got)
	}
	if got := absent.WSTorture["ws_route_present"]; got != 0 {
		t.Errorf("ws_route_present on a 404 refapp: got %d, want 0", got)
	}
	if got := absent.SSEKill["sse_route_probed"]; got != 1 {
		t.Errorf("sse_route_probed on a 404 refapp: got %d, want 1", got)
	}
	if got := absent.SSEKill["sse_route_present"]; got != 0 {
		t.Errorf("sse_route_present on a 404 refapp: got %d, want 0", got)
	}

	present := driveAgainst(t, streamingRouteServer(t, http.StatusBadRequest), 20, routesProbed).Tier1Summary()
	if got := present.WSTorture["ws_route_present"]; got != 1 {
		t.Errorf("ws_route_present on a refapp that answers /ws: got %d, want 1", got)
	}
	if got := present.SSEKill["sse_route_present"]; got != 1 {
		t.Errorf("sse_route_present on a refapp that answers /events: got %d, want 1", got)
	}
}

// TestDriveTier1_StreamingProbeSkippedBelowThreshold keeps the smoke
// path honest: below streamingWalkerMinConcurrency the slices are
// dormant, so nothing is probed and the cell must not claim to know
// whether the refapp routes /ws.
func TestDriveTier1_StreamingProbeSkippedBelowThreshold(t *testing.T) {
	// Below the threshold nothing is probed, so there is no event to wait
	// for -- only the absence of one. Give the cell a floor of serving
	// time and then assert it stayed silent.
	s := driveAgainst(t, streamingRouteServer(t, http.StatusNotFound), 1, workedFor(400*time.Millisecond)).Tier1Summary()
	if got := s.WSTorture["ws_route_probed"]; got != 0 {
		t.Errorf("ws_route_probed with the slice dormant: got %d, want 0", got)
	}
	if got := s.SSEKill["sse_route_probed"]; got != 0 {
		t.Errorf("sse_route_probed with the slice dormant: got %d, want 0", got)
	}
}
