package main

import (
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestParseMCCap(t *testing.T) {
	for _, tc := range []struct {
		in      string
		want    int
		wantErr bool
	}{{"", 0, false}, {"16", 16, false}, {"1", 1, false}, {"128", 128, false}, {"0", 0, true}, {"-1", 0, true}, {"abc", 0, true}, {"16 ", 0, true}} {
		got, err := parseMCCap(func(string) string { return tc.in })
		if (err != nil) != tc.wantErr || (err == nil && got != tc.want) {
			t.Errorf("parseMCCap(%q) = %d, %v; want %d, err %v", tc.in, got, err, tc.want, tc.wantErr)
		}
	}
}

// With a cap of n at most n callers are between mcAcquire and mcRelease at once, and n do get in; with no cap (nil semaphore) nothing blocks.
func TestMCSemaphoreBoundsInFlight(t *testing.T) {
	old := mcSem
	defer func() { mcSem = old }()
	const n, workers = 3, 40
	mcSem = make(chan struct{}, n)
	var cur, max atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			mcAcquire()
			c := cur.Add(1)
			for {
				m := max.Load()
				if c <= m || max.CompareAndSwap(m, c) {
					break
				}
			}
			time.Sleep(2 * time.Millisecond)
			cur.Add(-1)
			mcRelease()
		}()
	}
	wg.Wait()
	if max.Load() != n {
		t.Errorf("max in flight %d, want exactly the cap %d", max.Load(), n)
	}
	mcSem = nil
	done := make(chan struct{})
	go func() {
		for i := 0; i < 1000; i++ {
			mcAcquire()
		}
		for i := 0; i < 1000; i++ {
			mcRelease()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("a nil semaphore blocked")
	}
}
