package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/markov"
	"github.com/goceleris/probatorium/validation/remote"
)

// designedPanicWork is how long the stand-in refapp's /api/error handler
// runs before it panics. Long against every other route, so at the instant
// a phase ends nearly every Markov walker is inside /api/error -- the
// state the soak's arm64 cells were in at their cuts, made the common case
// instead of a matter of luck.
const designedPanicWork = 40 * time.Millisecond

// The I-PANIC false positive of soak 36433207097 (probatorium#465), in the
// shape of its reproduction: the real observability corpus, a fleet ended
// while its walkers are inside /api/error, and a server that counts the
// designed panic before it looks at the request's context, the way
// celeris's recovery middleware logs "panic recovered" before its
// c.Context().Err() check. The walker counts requests_panic_expected only
// when the 500 arrives, so a request the phase end abandons is a panic the
// server counted and the walker never netted: I-PANIC's excess, +1 for the
// rest of the cell, judged in the idle window that follows.
//
// Each phase of the burst, idle, load, idle schedule must end gracefully:
// every designed panic the server counted inside a phase was netted by the
// walker before that phase's idle window opened, nothing was cut, and no
// walker request reached the server inside a window.
func TestDriveTier1_PhaseEndNetsEveryDesignedPanic(t *testing.T) {
	saved := [3]time.Duration{idleBurstDuration, idleWindowDuration, idleTailDuration}
	// The load phase ends at the cell deadline minus the tail, so this
	// tail gives it about four seconds; the cell ends when window 2
	// opens, never at the deadline.
	idleBurstDuration, idleWindowDuration, idleTailDuration =
		600*time.Millisecond, 300*time.Millisecond, tier1TestBudget-4*time.Second
	t.Cleanup(func() { idleBurstDuration, idleWindowDuration, idleTailDuration = saved[0], saved[1], saved[2] })

	m, err := markov.LoadMatrixFile("markov/observability.yaml")
	if err != nil {
		t.Fatalf("load observability corpus: %v", err)
	}

	type designed struct{ arrived, counted time.Time }
	var (
		mu       sync.Mutex
		panics   []designed  // every /api/error the server counted
		arrivals []time.Time // every Markov request the server received
	)
	recovery := func(w http.ResponseWriter, r *http.Request, arrived time.Time) {
		if rec := recover(); rec != nil {
			// Counted first, as celeris's recovery does: the walker may
			// already have given up on this request.
			mu.Lock()
			panics = append(panics, designed{arrived, time.Now()})
			mu.Unlock()
			if r.Context().Err() != nil {
				return
			}
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		now := time.Now()
		switch r.URL.Path {
		case "/ws", "/events":
			// Absent, so the streaming slices stay dormant.
			w.WriteHeader(http.StatusNotFound)
			return
		case "/", "/healthz":
			// The adversarial and conformance slices and the
			// responsiveness probe: not Markov traffic.
			w.WriteHeader(http.StatusOK)
			return
		}
		mu.Lock()
		arrivals = append(arrivals, now)
		mu.Unlock()
		switch r.URL.Path {
		case "/api/error":
			defer recovery(w, r, now)
			time.Sleep(designedPanicWork)
			panic("designed")
		case "/api/slow":
			time.Sleep(5 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), tier1TestBudget)
	defer cancel()
	var (
		expected  atomic.Pointer[func() int64]
		windowAt  = map[int]time.Time{}
		loadAt    time.Time
		netted    = map[int]int64{} // requests_panic_expected when window n opened
		sequence  []int
		lastValue = -1
	)
	cfg := tier1Config{
		Driver:         remote.NewLocal("/bin/sh"),
		RefappArgs:     readyThenIdle(srv.URL),
		BaseURL:        srv.URL,
		Matrix:         m,
		Seed:           465,
		Concurrency:    8, // 7 Markov walkers + 1 adversarial
		ReadyTimeout:   tier1TestReadyTimeout,
		RequestTimeout: 2 * time.Second,
		IdleWindows:    true,
		OnLiveTally:    func(f func() int64) { expected.Store(&f) },
		OnIdleWindowChange: func(w int) {
			// Inline on the schedule's goroutine: for a window, the fleet
			// before it has already joined.
			now := time.Now()
			sequence = append(sequence, w)
			if w == 0 && lastValue == 1 {
				loadAt = now
			}
			lastValue = w
			if w > 0 {
				windowAt[w] = now
				netted[w] = (*expected.Load())()
			}
			if w == 2 {
				cancel()
			}
		},
	}
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	// Close waits for every handler, so a request the walker abandoned has
	// been counted by the time the books are read.
	srv.Close()
	if len(sequence) != 4 || windowAt[1].IsZero() || windowAt[2].IsZero() || loadAt.IsZero() {
		t.Fatalf("schedule must run burst, idle, load, idle; windows announced %v", sequence)
	}

	mu.Lock()
	defer mu.Unlock()
	type phase struct {
		name       string
		from, to   time.Time
		counted    int64 // designed panics the server counted for requests that arrived in the phase
		netted     int64 // designed panics the walker netted by the time the next window opened
		lastArrive time.Time
	}
	phases := []phase{
		{name: "burst", to: windowAt[1], netted: netted[1]},
		{name: "load", from: loadAt, to: windowAt[2], netted: netted[2] - netted[1]},
	}
	for i := range phases {
		p := &phases[i]
		for _, d := range panics {
			if !d.arrived.Before(p.from) && d.arrived.Before(p.to) {
				p.counted++
			}
		}
		for _, a := range arrivals {
			if !a.Before(p.from) && a.Before(p.to) && a.After(p.lastArrive) {
				p.lastArrive = a
			}
		}
	}
	for _, p := range phases {
		// The trigger has to have happened: a designed panic still running
		// when the phase's last request arrived, i.e. one in flight as the
		// phase was told to stop. Without one this test proves nothing.
		inFlight := 0
		for _, d := range panics {
			if !d.arrived.After(p.lastArrive) && d.counted.After(p.lastArrive) && !d.arrived.Before(p.from) {
				inFlight++
			}
		}
		t.Logf("%s phase: server counted %d designed panics, walker netted %d; %d in flight at its end",
			p.name, p.counted, p.netted, inFlight)
		if p.counted == 0 || inFlight == 0 {
			t.Fatalf("%s phase: %d designed panics, %d in flight at its end; the phase end was never exercised", p.name, p.counted, inFlight)
		}
		if p.counted != p.netted {
			t.Errorf("%s phase: the server counted %d designed panics and the walker netted %d before the window opened: I-PANIC's excess is %d (probatorium#465)",
				p.name, p.counted, p.netted, p.counted-p.netted)
		}
	}
	var inWindow int
	for _, a := range arrivals {
		if (!a.Before(windowAt[1]) && a.Before(loadAt)) || !a.Before(windowAt[2]) {
			inWindow++
		}
	}
	if inWindow != 0 {
		t.Errorf("%d Markov requests reached the server inside an idle window: a walker kept going after its phase ended", inWindow)
	}
	if s.RequestsCutAtDeadline != 0 {
		t.Errorf("requests_cut_at_deadline=%d: a phase end cut requests instead of draining them", s.RequestsCutAtDeadline)
	}
	if got := int64(len(panics)); got != s.RequestsPanicExpected {
		t.Errorf("cell total: server counted %d designed panics, walker netted %d", got, s.RequestsPanicExpected)
	}
	if s.Requests5xx != 0 || s.RequestsError != 0 {
		t.Errorf("requests_5xx=%d requests_error=%d; the stand-in refapp answers every route", s.Requests5xx, s.RequestsError)
	}
}
