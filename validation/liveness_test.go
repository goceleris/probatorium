package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestWatchResponsivenessDetectsHang is the I-HANG regression guard (the
// celeris#311 class): a server that is ALIVE but stops answering — accepts the
// connection and never responds — must be caught. The crash oracles never fire
// here (the process is up); only the health probe timing out catches it.
func TestWatchResponsivenessDetectsHang(t *testing.T) {
	restore := tightenHangProbe()
	defer restore()

	// /healthz blocks forever -> every probe times out (server accepted us but
	// never answered), exactly the wedge state.
	block := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		<-block
	}))
	defer srv.Close()
	defer close(block)

	l := &livenessTally{}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	hung := make(chan struct{})
	go watchResponsiveness(ctx, srv.URL, l, func() { close(hung) })

	select {
	case <-hung:
		if s := l.snapshot(); !s.Hung || s.HangFails < hangThreshold {
			t.Fatalf("onHang fired but tally wrong: %+v", s)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("watchResponsiveness did not detect an unresponsive server")
	}
}

// TestWatchResponsivenessHealthyServerNeverHangs confirms no false positive: a
// responsive server (and a fast-failing / crashed one — connection refused is
// NOT a timeout) must never trip I-HANG.
func TestWatchResponsivenessHealthyServerNeverHangs(t *testing.T) {
	restore := tightenHangProbe()
	defer restore()

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	l := &livenessTally{}
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	hung := false
	done := make(chan struct{})
	go func() { watchResponsiveness(ctx, srv.URL, l, func() { hung = true }); close(done) }()
	<-done
	if hung || l.snapshot().Hung {
		t.Fatal("healthy server falsely flagged as hung")
	}
}

// tightenHangProbe shrinks the probe cadence so the hang test runs in well
// under a second, and returns a restore func.
func tightenHangProbe() func() {
	pi, pt, th := hangProbeInterval, hangProbeTimeout, hangThreshold
	hangProbeInterval = 20 * time.Millisecond
	hangProbeTimeout = 60 * time.Millisecond
	hangThreshold = 3
	return func() { hangProbeInterval, hangProbeTimeout, hangThreshold = pi, pt, th }
}
