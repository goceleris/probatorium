package validation

import (
	"context"
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
