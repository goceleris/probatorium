package validation

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/remote"
)

// runShortTier1 drives one short Tier 1 cell against a loopback server with the
// given fleet size and Markov period, for at least d of serving, and returns its
// final snapshot.
func runShortTier1(t *testing.T, concurrency int, period time.Duration, d time.Duration) tier1TallySnapshot {
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
		MarkovMinPeriod: period,
	}, workedFor(d))
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	return s
}

// arrivalServer answers every request 200 and records when each non-login
// request arrived. The first one is held for hold (a slow response).
type arrivalServer struct {
	*httptest.Server
	mu   sync.Mutex
	at   []time.Time
	hold time.Duration
}

func newArrivalServer(t *testing.T, hold time.Duration) *arrivalServer {
	t.Helper()
	a := &arrivalServer{hold: hold}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/login" {
			a.mu.Lock()
			a.at = append(a.at, time.Now())
			first := len(a.at) == 1
			a.mu.Unlock()
			if first && a.hold > 0 {
				time.Sleep(a.hold)
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(a.Close)
	return a
}

func (a *arrivalServer) arrivals() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.at...)
}

// walkFor runs one Markov walker against srv for window with the given period.
func walkFor(t *testing.T, srvURL string, window, period time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), window)
	defer cancel()
	var tally tier1Tally
	runMarkovWalker(ctx, ctx, &http.Client{Timeout: 5 * time.Second}, srvURL, minimalMatrix(t), 0xa11ce, period, &tally)
}

// Without a period the walker is the closed loop it always was: against a
// loopback server it sends as fast as the server answers.
func TestMarkovWalkerWithoutPeriodIsClosedLoop(t *testing.T) {
	a := newArrivalServer(t, 0)
	walkFor(t, a.URL, 500*time.Millisecond, 0)
	// ~17k on an idle laptop, ~5.7k under -race; even a 1 ms period would cap it
	// at 500.
	if got := len(a.arrivals()); got < 700 {
		t.Fatalf("a closed-loop walker sent %d requests in 500ms against a loopback server, want >= 700: the walker is paced when it must not be", got)
	}
}

// The period bounds the START rate. The upper bound is exact (starts are at
// least a period apart, plus the first), so it holds on any box; the lower
// bound is loose for a loaded one. Two periods, so a pacing that is twice or
// half the configured one fails one of them.
func TestMarkovWalkerStartsAreAtLeastOnePeriodApart(t *testing.T) {
	for _, tc := range []struct {
		period   time.Duration
		min, max int
	}{
		{50 * time.Millisecond, 12, 21}, // 1 s / 50 ms = 20; twice the period gives 10, half gives 40
		{100 * time.Millisecond, 6, 11}, // 1 s / 100 ms = 10; twice gives 5, half gives 20
	} {
		a := newArrivalServer(t, 0)
		walkFor(t, a.URL, time.Second, tc.period)
		at := a.arrivals()
		if len(at) < tc.min || len(at) > tc.max {
			t.Errorf("period %v: %d requests in 1s, want %d..%d", tc.period, len(at), tc.min, tc.max)
		}
		for i := 1; i < len(at); i++ {
			// Server-side arrival times jitter a little; 80% leaves room for it
			// and still fails a pacing at half the period.
			if gap := at[i].Sub(at[i-1]); gap < tc.period*8/10 {
				t.Errorf("period %v: requests %d and %d arrived %v apart", tc.period, i-1, i, gap)
			}
		}
	}
}

// Open loop: a slow response makes the next start come at once but never earlier
// than a period after the last start, and nothing is caught up afterwards -- no
// burst of the starts that were missed while the response was pending.
func TestMarkovWalkerDoesNotCatchUpAfterASlowResponse(t *testing.T) {
	const period = 50 * time.Millisecond
	a := newArrivalServer(t, 400*time.Millisecond) // eight periods
	walkFor(t, a.URL, 1200*time.Millisecond, period)
	at := a.arrivals()
	if len(at) < 6 {
		t.Fatalf("only %d requests: the walker did not resume after the slow response", len(at))
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < period*8/10 {
			t.Errorf("requests %d and %d arrived %v apart after a slow response: a catch-up burst", i-1, i, gap)
		}
	}
}

// The wait ends the moment the phase does: a walker waiting for its first start
// when stop fires returns at once, so a shaped cell ends when an unshaped one
// would.
func TestMarkovWalkerWaitEndsWithStop(t *testing.T) {
	a := newArrivalServer(t, 0)
	stop, endPhase := context.WithCancel(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var tally tier1Tally
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMarkovWalker(ctx, stop, &http.Client{Timeout: time.Second}, a.URL, minimalMatrix(t), 0xa11ce, time.Hour, &tally)
	}()
	time.Sleep(200 * time.Millisecond)
	endPhase()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the walker's wait ignored stop: it was still waiting 3s after the phase ended")
	}
}

// And with the request context: a cut (the deadline) ends the wait too.
func TestMarkovWalkerWaitEndsWithContext(t *testing.T) {
	a := newArrivalServer(t, 0)
	ctx, cancel := context.WithCancel(context.Background())
	var tally tier1Tally
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMarkovWalker(ctx, context.Background(), &http.Client{Timeout: time.Second}, a.URL, minimalMatrix(t), 0xa11ce, time.Hour, &tally)
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the walker's wait ignored the request context")
	}
}

// The cell records the shaping it ran under, with the counts it kept, so a
// reader can see it without the source. A cell with no period records nothing
// (omitted), which keeps every other cell's document what it was.
func TestTier1SummaryRecordsTheLoadShaping(t *testing.T) {
	var plain tier1TallySnapshot
	if got := plain.Tier1Summary().LoadShaping; got != nil {
		t.Fatalf("an unshaped cell carries load_shaping: %+v", got)
	}
	var shaped tier1TallySnapshot
	shaped.LoadShaping = &report.LoadShaping{Reason: "race build", Walkers: 110, MarkovWalkers: 67, MarkovMinPeriodUs: 6500}
	got := shaped.Tier1Summary().LoadShaping
	if got == nil || got.MarkovMinPeriodUs != 6500 || got.Walkers != 110 || got.MarkovWalkers != 67 || got.Reason != "race build" {
		t.Fatalf("load_shaping not projected: %+v", got)
	}
	got.Walkers = 1
	if shaped.LoadShaping.Walkers != 110 {
		t.Error("the projected record aliases the snapshot's")
	}
}

// driveTier1 itself stamps the record from the config it ran with: the count of
// walkers it started, the Markov part of them, and the period, whatever value it
// is (not just the production one).
func TestDriveTier1StampsTheLoadShaping(t *testing.T) {
	for _, tc := range []struct {
		period     time.Duration
		wantRecord bool
		wantUs     int64
	}{{0, false, 0}, {7 * time.Millisecond, true, 7000}, {6500 * time.Microsecond, true, 6500}} {
		snap := runShortTier1(t, 12, tc.period, 300*time.Millisecond)
		if (snap.LoadShaping != nil) != tc.wantRecord {
			t.Fatalf("period %v: load_shaping present=%t, want %t", tc.period, snap.LoadShaping != nil, tc.wantRecord)
		}
		if !tc.wantRecord {
			continue
		}
		ls := snap.LoadShaping
		// 12 walkers: 2 adversarial and 1 h2c leave 9 for the Markov mix, less
		// one each for ws and sse when the route probe finds them.
		if ls.Walkers != 12 || ls.MarkovWalkers < 7 || ls.MarkovWalkers > 9 || ls.MarkovMinPeriodUs != tc.wantUs || ls.Reason == "" {
			t.Errorf("load_shaping = %+v, want walkers 12, markov_walkers 7..9, markov_min_period_us %d", ls, tc.wantUs)
		}
	}
}

// The record is not a claim the walkers ignore: the same short cell with a
// period sends a small fraction of the requests the closed loop does. Wide
// margins (closed-loop is thousands a second, paced at most ~20 a second per
// walker), so it does not flake under -race on a loaded box.
func TestDriveTier1ReallyPacesTheMarkovFleet(t *testing.T) {
	closed := runShortTier1(t, 12, 0, time.Second)
	paced := runShortTier1(t, 12, 50*time.Millisecond, time.Second)
	if closed.RequestsSent < 500 {
		t.Fatalf("premise: the closed-loop fleet sent only %d requests", closed.RequestsSent)
	}
	if paced.LoadShaping == nil {
		t.Fatal("premise: the paced cell recorded no load_shaping")
	}
	if paced.RequestsSent*5 >= closed.RequestsSent {
		t.Errorf("paced fleet sent %d requests against %d closed-loop: the walkers were not paced (a record that says 50 ms over walkers that ran free)",
			paced.RequestsSent, closed.RequestsSent)
	}
	// And the upper bound the period implies, with generous slack for the
	// fleet's start-up and the final tick: at most 9 walkers, one start per 50 ms
	// each, over well under 5 s.
	if paced.RequestsSent > 9*(5*20+1) {
		t.Errorf("paced fleet sent %d requests, more than 9 walkers at one per 50ms allow in 5s", paced.RequestsSent)
	}
}

func orchestratorCell(t *testing.T, period time.Duration) (*Orchestrator, string) {
	t.Helper()
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
	cfg.MarkovMinPeriod = period
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	violations := make(chan Incident, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o.runTierProperty(ctx, violations)
	if !o.tier1Ran {
		t.Fatalf("premise: Tier 1 did not complete (period %v)", period)
	}
	return o, cfg.OutDir
}

// The orchestrator hands Config.MarkovMinPeriod to Tier 1 and the record reaches
// the cell's snapshot: a field that is configured but never plumbed would shape
// nothing and say nothing. (Mutant: the Config field not copied to tier1Config.)
func TestOrchestratorPlumbsMarkovMinPeriodToTier1(t *testing.T) {
	for _, period := range []time.Duration{0, 7 * time.Millisecond} {
		o, _ := orchestratorCell(t, period)
		got := o.tier1Snapshot.Tier1Summary().LoadShaping
		switch {
		case period == 0 && got != nil:
			t.Errorf("no period configured but the cell records %+v", got)
		case period > 0 && (got == nil || got.MarkovMinPeriodUs != 7000 || got.Walkers != 12):
			t.Errorf("period %v configured: record %+v, want markov_min_period_us 7000 and walkers 12", period, got)
		}
	}
}

// Byte identity of every cell that is not shaped: neither the per-cell sidecar
// (tier1_tally.json), nor the snapshot, nor the cell document's Tier 1 summary
// gains a load_shaping key, null or otherwise. Those are the cells of every
// non-race tier and of the race tier's pure engines, which all run with a zero
// period. The shaped cell carries it in all three.
func TestUnshapedCellsGainNoLoadShapingKeyAnywhere(t *testing.T) {
	for _, period := range []time.Duration{0, 7 * time.Millisecond} {
		o, out := orchestratorCell(t, period)
		want := period > 0
		side, err := os.ReadFile(filepath.Join(out, "tier1_tally.json"))
		if err != nil {
			t.Fatal(err)
		}
		snap, _ := json.Marshal(o.tier1Snapshot)
		sum, _ := json.Marshal(o.tier1Snapshot.Tier1Summary())
		for name, raw := range map[string][]byte{"tier1_tally.json": side, "snapshot": snap, "Tier1Summary": sum} {
			if has := strings.Contains(string(raw), `"load_shaping"`); has != want {
				t.Errorf("period %v: %s has load_shaping=%t, want %t", period, name, has, want)
			}
		}
	}
	// A zero snapshot, the shape of every cell that never set one.
	var zero tier1TallySnapshot
	raw, _ := json.Marshal(zero)
	if strings.Contains(string(raw), "load_shaping") {
		t.Errorf("a zero snapshot serialises load_shaping: %s", raw)
	}
}
