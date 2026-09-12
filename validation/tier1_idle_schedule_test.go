package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/remote"
)

// With IdleWindows the tier runs burst, idle, load, idle: the published
// window goes 0 → 1 → 0 → 2, and no walker request lands inside a window
// (the responsiveness probe's /healthz is the only traffic).
func TestDriveTier1_IdleWindowsBurstIdleLoadIdle(t *testing.T) {
	saved := [3]time.Duration{idleBurstDuration, idleWindowDuration, idleTailDuration}
	idleBurstDuration, idleWindowDuration, idleTailDuration = 300*time.Millisecond, 300*time.Millisecond, 300*time.Millisecond
	t.Cleanup(func() { idleBurstDuration, idleWindowDuration, idleTailDuration = saved[0], saved[1], saved[2] })

	type stamp struct {
		at     time.Time
		window int
	}
	var window atomic.Pointer[func() int]
	current := func() int {
		if f := window.Load(); f != nil {
			return (*f)()
		}
		return 0
	}
	var mu sync.Mutex
	var requests []stamp
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			mu.Lock()
			requests = append(requests, stamp{time.Now(), current()})
			mu.Unlock()
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// Sample the published window and record every transition.
	var transitions []stamp
	stop := make(chan struct{})
	var sampler sync.WaitGroup
	sampler.Add(1)
	go func() {
		defer sampler.Done()
		last := -1
		for {
			select {
			case <-stop:
				return
			case <-time.After(2 * time.Millisecond):
			}
			if v := current(); v != last {
				mu.Lock()
				transitions = append(transitions, stamp{time.Now(), v})
				mu.Unlock()
				last = v
			}
		}
	}()

	cfg := tier1Config{
		Driver:         remote.NewLocal("/bin/sh"),
		RefappArgs:     []string{"-c", `echo "ready addr=` + srv.URL + `"; sleep 10`},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    2,
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
		IdleWindows:    true,
		OnIdleWindow:   func(f func() int) { window.Store(&f) },
	}
	// burst 0.3 s, window 1 0.3 s, load until deadline-0.3 s, window 2 to the end.
	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	close(stop)
	sampler.Wait()
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.RequestsSent < 1 {
		t.Fatalf("no requests sent: %+v", s)
	}

	mu.Lock()
	defer mu.Unlock()
	seq := make([]int, 0, len(transitions))
	for _, tr := range transitions {
		seq = append(seq, tr.window)
	}
	want := []int{0, 1, 0, 2}
	if len(seq) != len(want) {
		t.Fatalf("window sequence must be %v, got %v", want, seq)
	}
	for i := range want {
		if seq[i] != want[i] {
			t.Fatalf("window sequence must be %v, got %v", want, seq)
		}
	}
	// A request the server is still reading when the burst fleet is
	// cancelled may be stamped a few ms into window 1; anything later is
	// load inside a window, which the design forbids.
	const grace = 50 * time.Millisecond
	startOf := func(w int) time.Time {
		for _, tr := range transitions {
			if tr.window == w {
				return tr.at
			}
		}
		return time.Time{}
	}
	perWindow := map[int]int{}
	for _, r := range requests {
		perWindow[r.window]++
		if r.window > 0 && r.at.Sub(startOf(r.window)) > grace {
			t.Errorf("walker request landed %s into idle window %d", r.at.Sub(startOf(r.window)), r.window)
		}
	}
	if perWindow[0] == 0 {
		t.Errorf("no walker requests under load: %v", perWindow)
	}
	t.Logf("requests per window: %v; transitions: %d", perWindow, len(transitions))
}

// Without IdleWindows nothing changes: the window stays 0 for the whole
// run and the walkers run to the deadline.
func TestDriveTier1_NoIdleWindowsByDefault(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	var window atomic.Pointer[func() int]
	cfg := tier1Config{
		Driver:         remote.NewLocal("/bin/sh"),
		RefappArgs:     []string{"-c", `echo "ready addr=` + srv.URL + `"; sleep 10`},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    2,
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
		OnIdleWindow:   func(f func() int) { window.Store(&f) },
	}
	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := driveTier1(ctx, cfg); err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	f := window.Load()
	if f == nil {
		t.Fatal("OnIdleWindow must publish the accessor even when the tier never idles")
	}
	if (*f)() != 0 {
		t.Fatalf("window must stay 0 without IdleWindows, got %d", (*f)())
	}
}
