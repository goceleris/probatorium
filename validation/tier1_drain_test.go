package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// drainFleet returns as soon as the fleet is done, without cancelling;
// past its bound it cancels the requests and still waits for every walker.
func TestDrainFleet(t *testing.T) {
	t.Run("returns_when_the_fleet_is_done", func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(1)
		go func() { time.Sleep(10 * time.Millisecond); wg.Done() }()
		var cancelled atomic.Bool
		drainFleet(&wg, time.Hour, func() { cancelled.Store(true) })
		if cancelled.Load() {
			t.Fatal("drainFleet cancelled a fleet that finished inside its bound")
		}
	})
	t.Run("cancels_past_the_bound_then_waits", func(t *testing.T) {
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		var wg sync.WaitGroup
		var returned atomic.Bool
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-ctx.Done() // a walker that only stops when its request is cancelled
			time.Sleep(10 * time.Millisecond)
			returned.Store(true)
		}()
		drainFleet(&wg, 10*time.Millisecond, cancel)
		if ctx.Err() == nil {
			t.Fatal("drainFleet did not cancel a fleet still running past its bound")
		}
		if !returned.Load() {
			t.Fatal("drainFleet returned before the cancelled walker did")
		}
	})
}

// A 401 that arrives after the phase's stop must not start a re-login: the
// walker is draining, and a login there would be one more request inside a
// drain sized for the one already in flight.
func TestRunMarkovWalker_NoReloginAfterStop(t *testing.T) {
	stop, endPhase := context.WithCancel(context.Background())
	defer endPhase()
	var logins, walks atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" {
			logins.Add(1)
			return
		}
		walks.Add(1)
		endPhase() // the phase ends while this request is in flight
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	// The request context only bounds the test: a walker that ignored stop
	// would otherwise walk forever.
	ctx, cancel := context.WithTimeout(context.Background(), tier1TestBudget)
	defer cancel()
	var tally tier1Tally
	runMarkovWalker(ctx, stop, &http.Client{Timeout: time.Second}, srv.URL, minimalMatrix(t), 0xa11ce, &tally)
	// The 401 after stop must have happened, or the test proves nothing.
	if w := walks.Load(); w != 1 {
		t.Fatalf("%d walk requests, want exactly the one in flight when the phase ended", w)
	}
	if n, re := logins.Load(), tally.walkerRelogins.Load(); n != 1 || re != 0 {
		t.Errorf("logins %d, re-logins %d: want the walker's first login only, and no re-login after stop", n, re)
	}
}
