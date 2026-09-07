package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// A 5xx from a state the corpus marks `expect: 5xx` is tallied as expected,
// never as an unexpected 5xx; an unexpected 5xx carrying the refapp
// invariant marker is additionally surfaced as an invariant hit.
func TestDoMarkovRequest_ExpectedFiveXXAndInvariantHits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/designed":
			http.Error(w, `{"error":"Internal Server Error"}`, 500)
		case "/invariant":
			w.WriteHeader(500)
			_, _ = w.Write([]byte(`{"err":"read-after-write mismatch","x-invariant":"I-DRV-1","x-invariant-h":"high"}`))
		default:
			w.WriteHeader(500)
			_, _ = w.Write([]byte("insert: boom"))
		}
	}))
	defer srv.Close()
	hc := srv.Client()
	var tally tier1Tally
	doMarkovRequest(context.Background(), hc, "GET", srv.URL+"/designed", true, false, &tally)
	doMarkovRequest(context.Background(), hc, "GET", srv.URL+"/invariant", false, false, &tally)
	doMarkovRequest(context.Background(), hc, "GET", srv.URL+"/plain", false, false, &tally)
	s := tally.snapshot()
	if s.Requests5xxExpected != 1 || s.Requests5xx != 2 || s.InvariantHits != 1 {
		t.Fatalf("expected=%d unexpected=%d invariant=%d; want 1/2/1", s.Requests5xxExpected, s.Requests5xx, s.InvariantHits)
	}
	if s.RequestsSent != 3 || s.Requests2xx != 0 {
		t.Fatalf("sent=%d 2xx=%d", s.RequestsSent, s.Requests2xx)
	}
	sum := s.Tier1Summary()
	if sum.Requests5xxExpected != 1 || sum.InvariantHits != 1 || sum.Requests5xx != 2 {
		t.Fatalf("summary not projected: %+v", sum)
	}
}

// A request whose run context has already expired must be counted as cut at
// the deadline, not as a transport error.
func TestDoMarkovRequest_DeadlineCutIsNotAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var tally tier1Tally
	doMarkovRequest(ctx, srv.Client(), "GET", srv.URL, false, false, &tally)
	s := tally.snapshot()
	if s.RequestsError != 0 || s.RequestsCutAtDeadline != 1 {
		t.Fatalf("error=%d cut=%d; want 0/1", s.RequestsError, s.RequestsCutAtDeadline)
	}
	if s.Tier1Summary().RequestsCutAtDeadline != 1 {
		t.Fatal("cut-at-deadline not projected into Tier1Summary")
	}
}

// `expect: panic` states count their designed 5xx into BOTH
// requests_5xx_expected and requests_panic_expected, so the property loop can
// net designed panics out of I-PANIC.
func TestDoMarkovRequest_ExpectPanicCounts(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()
	var tally tier1Tally
	doMarkovRequest(context.Background(), srv.Client(), "GET", srv.URL+"/panic", true, true, &tally)
	doMarkovRequest(context.Background(), srv.Client(), "GET", srv.URL+"/designed", true, false, &tally)
	if got := tally.requests5xxExpected.Load(); got != 2 {
		t.Fatalf("requests_5xx_expected=%d want 2", got)
	}
	if got := tally.requestsPanicExpected.Load(); got != 1 {
		t.Fatalf("requests_panic_expected=%d want 1", got)
	}
	if got := tally.requests5xx.Load(); got != 0 {
		t.Fatalf("unexpected 5xx=%d want 0", got)
	}
}
