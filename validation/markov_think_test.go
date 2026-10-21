package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/remote"
)

// runShortTier1 drives one short Tier 1 cell against a loopback server with the
// given fleet size and Markov pause, and returns its final snapshot.
func runShortTier1(t *testing.T, concurrency int, think time.Duration) tier1TallySnapshot {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(200) }))
	defer srv.Close()
	s, err := runTier1Until(t, tier1Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      readyThenIdle(srv.URL),
		BaseURL:         srv.URL,
		Matrix:          minimalMatrix(t),
		Seed:            42,
		Concurrency:     concurrency,
		MarkovThinkTime: think,
	}, sentAtLeast(20))
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	return s
}

// countingServer answers every request 200 and counts the ones that are not the
// login.
func countingServer(t *testing.T) (*httptest.Server, *atomic.Int64) {
	t.Helper()
	var n atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			n.Add(1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(srv.Close)
	return srv, &n
}

// requestsIn runs one Markov walker against a counting server for window and
// returns how many requests it sent.
func requestsIn(t *testing.T, window, think time.Duration) int64 {
	t.Helper()
	srv, n := countingServer(t)
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	var tally tier1Tally
	runMarkovWalker(ctx, ctx, &http.Client{Timeout: time.Second}, srv.URL, minimalMatrix(t), 0xa11ce, think, &tally)
	return n.Load()
}

// Without a think time the walker is the closed loop it always was: against a
// loopback server it sends as fast as the server answers.
func TestMarkovWalkerWithoutThinkTimeIsClosedLoop(t *testing.T) {
	// ~17k on an idle laptop, a few thousand under -race; even a 1 ms pause
	// would cap it at 500.
	if got := requestsIn(t, 500*time.Millisecond, 0); got < 700 {
		t.Fatalf("a closed-loop walker sent %d requests in 500ms against a loopback server, want >= 700: the walker is paced when it must not be", got)
	}
}

// A think time spaces the requests: 50 ms for 500 ms is about 10 requests, and
// the bound is loose in both directions so a loaded box does not flake.
func TestMarkovWalkerThinkTimePacesRequests(t *testing.T) {
	got := requestsIn(t, 500*time.Millisecond, 50*time.Millisecond)
	if got < 3 || got > 14 {
		t.Fatalf("a walker thinking 50ms sent %d requests in 500ms, want about 10 (3..14)", got)
	}
}

// The pause ends the moment the phase does: a walker mid-think when stop fires
// returns at once, so the drain (runPhase) is not lengthened by the pause and a
// shaped cell ends when an unshaped one would.
func TestMarkovWalkerThinkTimeEndsWithStop(t *testing.T) {
	srv, _ := countingServer(t)
	stop, endPhase := context.WithCancel(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var tally tier1Tally
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMarkovWalker(ctx, stop, &http.Client{Timeout: time.Second}, srv.URL, minimalMatrix(t), 0xa11ce, time.Hour, &tally)
	}()
	time.Sleep(200 * time.Millisecond) // inside the first pause
	endPhase()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the walker's pause ignored stop: it was still thinking 3s after the phase ended")
	}
}

// And with the request context: a cut (the deadline) ends the pause too.
func TestMarkovWalkerThinkTimeEndsWithContext(t *testing.T) {
	srv, _ := countingServer(t)
	ctx, cancel := context.WithCancel(context.Background())
	var tally tier1Tally
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMarkovWalker(ctx, context.Background(), &http.Client{Timeout: time.Second}, srv.URL, minimalMatrix(t), 0xa11ce, time.Hour, &tally)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the walker's pause ignored the request context")
	}
}

// The cell records the shaping it ran under, with the counts it kept, so a
// reader can see it without the source. A cell with no think time records
// nothing (omitted), which keeps every other cell's document what it was.
func TestTier1SummaryRecordsTheLoadShaping(t *testing.T) {
	var plain tier1TallySnapshot
	if got := plain.Tier1Summary().LoadShaping; got != nil {
		t.Fatalf("an unshaped cell carries load_shaping: %+v", got)
	}
	var shaped tier1TallySnapshot
	shaped.LoadShaping = &report.LoadShaping{Reason: "race build", Walkers: 110, MarkovWalkers: 67, MarkovThinkMs: 4}
	got := shaped.Tier1Summary().LoadShaping
	if got == nil || got.MarkovThinkMs != 4 || got.Walkers != 110 || got.MarkovWalkers != 67 || got.Reason == "" {
		t.Fatalf("load_shaping not projected: %+v", got)
	}
	got.Walkers = 1
	if shaped.LoadShaping.Walkers != 110 {
		t.Error("the projected record aliases the snapshot's")
	}
}

// driveTier1 itself stamps the record from the config it ran with: the count of
// walkers it started, the Markov part of them, and the pause.
func TestDriveTier1StampsTheLoadShaping(t *testing.T) {
	for _, tc := range []struct {
		think      time.Duration
		wantRecord bool
	}{{0, false}, {4 * time.Millisecond, true}} {
		snap := runShortTier1(t, 12, tc.think)
		if (snap.LoadShaping != nil) != tc.wantRecord {
			t.Fatalf("think %v: load_shaping present=%t, want %t", tc.think, snap.LoadShaping != nil, tc.wantRecord)
		}
		if !tc.wantRecord {
			continue
		}
		ls := snap.LoadShaping
		// 12 walkers: 2 adversarial and 1 h2c leave 9 for the Markov mix, less
		// one each for ws and sse when the route probe finds them.
		if ls.Walkers != 12 || ls.MarkovWalkers < 7 || ls.MarkovWalkers > 9 || ls.MarkovThinkMs != 4 || ls.Reason == "" {
			t.Errorf("load_shaping = %+v, want walkers 12, markov_walkers 7..9, markov_think_ms 4", ls)
		}
	}
}

// The orchestrator hands Config.MarkovThinkTime to Tier 1 and the record reaches
// the cell's snapshot: a field that is configured but never plumbed would shape
// nothing and say nothing. (Mutant: the Config field not copied to tier1Config.)
func TestOrchestratorPlumbsMarkovThinkTimeToTier1(t *testing.T) {
	for _, think := range []time.Duration{0, 4 * time.Millisecond} {
		cfg := Default()
		cfg.Duration = 2 * time.Second
		cfg.OutDir = t.TempDir()
		fake := filepath.Join(t.TempDir(), "fakerefapp")
		if err := os.WriteFile(fake, []byte("#!/bin/sh\necho 'ready addr=127.0.0.1:1'\nexec sleep 60\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		cfg.CelerisBin = fake
		cfg.MarkovPath = "markov/auth_session_ratelimit.yaml"
		cfg.ConcurrencyFloor = 12
		cfg.MarkovThinkTime = think
		o, err := New(cfg)
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		violations := make(chan Incident, 64)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		o.runTierProperty(ctx, violations)
		cancel()
		if !o.tier1Ran {
			t.Fatalf("premise: Tier 1 did not complete (think %v)", think)
		}
		got := o.tier1Snapshot.Tier1Summary().LoadShaping
		switch {
		case think == 0 && got != nil:
			t.Errorf("no think time configured but the cell records %+v", got)
		case think > 0 && (got == nil || got.MarkovThinkMs != 4 || got.Walkers != 12):
			t.Errorf("think %v configured: record %+v, want markov_think_ms 4 and walkers 12", think, got)
		}
	}
}
