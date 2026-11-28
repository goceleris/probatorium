package validation

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/validation/markov"
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

// arrivalServer answers requests and records, in order, when each arrived and
// whether it was the login. hold delays the FIRST non-login response; delayAll
// delays EVERY non-login response (a slow server); unauth makes the first n
// non-login requests answer 401, which drives the walker's re-login.
//
// The stamps are taken in the handler, after the scheduler has run it, so an
// individual stamp can be late on a loaded box. The tests therefore never rely
// on one gap: they compare sums of several consecutive gaps (spanOK), which one
// late handler cannot distort by more than its own lateness.
type arrivalServer struct {
	*httptest.Server
	mu       sync.Mutex
	ev       []arrival
	hold     time.Duration
	delayAll time.Duration
	unauth   int
	seen     int
}

type arrival struct {
	at    time.Time
	login bool
}

func newArrivalServer(t *testing.T, hold, delayAll time.Duration, unauth int) *arrivalServer {
	t.Helper()
	a := &arrivalServer{hold: hold, delayAll: delayAll, unauth: unauth}
	a.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		login := r.URL.Path == "/login"
		a.mu.Lock()
		a.ev = append(a.ev, arrival{at: time.Now(), login: login})
		var first bool
		var unauthorized bool
		if !login {
			a.seen++
			first = a.seen == 1
			unauthorized = a.seen <= a.unauth
		}
		a.mu.Unlock()
		if !login {
			if first && a.hold > 0 {
				time.Sleep(a.hold)
			}
			if a.delayAll > 0 {
				time.Sleep(a.delayAll)
			}
		}
		if unauthorized {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(a.Close)
	return a
}

// arrivals is the stamps of the non-login requests, in arrival order.
func (a *arrivalServer) arrivals() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	var out []time.Time
	for _, e := range a.ev {
		if !e.login {
			out = append(out, e.at)
		}
	}
	return out
}

// spanOK reports whether every k consecutive gaps of at sum to at least
// frac*k*period. A pacer that keeps starts a period apart passes with room to
// spare (frac 0.6); one that halves the period, makes up lateness in a burst or
// ignores the period fails. One handler stamped late by less than (1-frac)*k
// periods cannot fail it.
func spanOK(at []time.Time, k int, period time.Duration, frac float64) (bool, int, time.Duration) {
	for i := 0; i+k < len(at); i++ {
		if sum := at[i+k].Sub(at[i]); sum < time.Duration(frac*float64(k)*float64(period)) {
			return false, i, sum
		}
	}
	return true, 0, 0
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
	a := newArrivalServer(t, 0, 0, 0)
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
		a := newArrivalServer(t, 0, 0, 0)
		walkFor(t, a.URL, time.Second, tc.period)
		at := a.arrivals()
		if len(at) < tc.min || len(at) > tc.max {
			t.Errorf("period %v: %d requests in 1s, want %d..%d", tc.period, len(at), tc.min, tc.max)
		}
		if ok, i, sum := spanOK(at, 4, tc.period, 0.6); !ok {
			t.Errorf("period %v: requests %d..%d arrived %v apart, under 0.6 x 4 periods", tc.period, i, i+4, sum)
		}
	}
}

// OPEN LOOP: the next start is booked from the ACTUAL START of the last request,
// not from the end of its response. Every response here takes 30 ms against a
// 50 ms period: open loop starts every 50 ms (about 20 in a second), a pause
// after the response (a think time, the round-1 design) every 80 ms (about 12).
// A fast response must not shorten the gap either (the span check, with the
// responses at 0 ms elsewhere in this file). Counts, not stamps: a late handler
// cannot change how many requests the walker started.
func TestMarkovWalkerBooksFromTheStartNotTheResponse(t *testing.T) {
	const period = 50 * time.Millisecond
	a := newArrivalServer(t, 0, 30*time.Millisecond, 0)
	walkFor(t, a.URL, time.Second, period)
	at := a.arrivals()
	if len(at) < 16 || len(at) > 21 {
		t.Fatalf("with 30ms responses and a 50ms period the walker started %d requests in 1s, want 16..21 (open loop ~20; a pause after the response gives ~12)", len(at))
	}
	if ok, i, sum := spanOK(at, 4, period, 0.6); !ok {
		t.Errorf("requests %d..%d arrived %v apart, under 0.6 x 4 periods", i, i+4, sum)
	}
}

// Open loop, slow response: a response slower than the period makes the next
// start come at once but never earlier than a period after the last start, and
// nothing is made up afterwards -- no burst of the starts missed while the
// response was pending. Two stall lengths: eight periods and just two (a short
// stall is the one a bounded catch-up would still make up).
func TestMarkovWalkerDoesNotCatchUpAfterASlowResponse(t *testing.T) {
	const period = 150 * time.Millisecond
	for _, hold := range []time.Duration{1200 * time.Millisecond, 2 * period} {
		a := newArrivalServer(t, hold, 0, 0)
		walkFor(t, a.URL, hold+1500*time.Millisecond, period)
		at := a.arrivals()
		if len(at) < 6 {
			t.Fatalf("hold %v: only %d requests: the walker did not resume after the slow response", hold, len(at))
		}
		// Skip the stalled gap itself: the check is on what follows it. Two
		// consecutive gaps must sum to most of two periods: a start made up for
		// the lateness (bounded catch-up) leaves one gap near zero and the pair
		// at about one period.
		if ok, i, sum := spanOK(at[1:], 2, period, 0.6); !ok {
			t.Errorf("hold %v: requests %d..%d after the stall arrived %v apart: a catch-up burst", hold, i+1, i+3, sum)
		}
	}
}

// The first starts are staggered: each walker's seed gives it its own offset in
// [0, period), so a fleet does not start in lockstep, and none waits a whole
// period for its first request.
func TestStartPacerStaggersTheFirstStart(t *testing.T) {
	const period = 6500 * time.Microsecond
	now := time.Unix(1_000_000, 0)
	distinct := map[time.Duration]bool{}
	var lo, hi time.Duration = period, 0
	for seed := uint64(1); seed <= 64; seed++ {
		p := newStartPacer(period, rand.New(rand.NewPCG(seed, ^seed)), now)
		off := p.next.Sub(now)
		if off < 0 || off >= period {
			t.Fatalf("seed %d: first start offset %v is outside [0, %v)", seed, off, period)
		}
		distinct[off] = true
		lo, hi = min(lo, off), max(hi, off)
	}
	if len(distinct) < 32 || hi-lo < period/2 {
		t.Errorf("64 walkers got %d distinct first-start offsets spanning %v: the fleet starts in lockstep", len(distinct), hi-lo)
	}
}

// A 401 draws a re-login, which is a request start like any other: it waits for
// its own start, a period after the request that drew the 401. (Re-logins are
// about 4% of the starts of the real refapp.)
func TestMarkovWalkerPacesTheReloginAfterA401(t *testing.T) {
	const period = 100 * time.Millisecond
	a := newArrivalServer(t, 0, 0, 6)
	walkFor(t, a.URL, 1800*time.Millisecond, period)
	a.mu.Lock()
	ev := append([]arrival(nil), a.ev...)
	a.mu.Unlock()
	var pairs, spaced int
	for i := 1; i < len(ev); i++ {
		if ev[i].login && !ev[i-1].login {
			pairs++
			if ev[i].at.Sub(ev[i-1].at) >= period/2 {
				spaced++
			}
		}
	}
	if pairs < 4 {
		t.Fatalf("premise: only %d request->re-login pairs seen", pairs)
	}
	// One stamp may be late; an unpaced re-login would fail every pair.
	if spaced < pairs-1 {
		t.Errorf("%d of %d re-logins came within half a period of the request that drew the 401: the re-login is not paced", pairs-spaced, pairs)
	}
}

// A wait ended by stop sends nothing: the request a walker was about to start
// when the phase ended is not sent, so a shaped cell's drain is the one an
// unshaped cell has (probatorium#465).
func TestMarkovWalkerSendsNothingAfterStop(t *testing.T) {
	a := newArrivalServer(t, 0, 0, 0)
	stop, endPhase := context.WithCancel(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var tally tier1Tally
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMarkovWalker(ctx, stop, &http.Client{Timeout: time.Second}, a.URL, minimalMatrix(t), 0xa11ce, 400*time.Millisecond, &tally)
	}()
	// Wait for the first request, then end the phase while the walker waits for
	// its second start (up to 400 ms away).
	for len(a.arrivals()) < 1 {
		select {
		case <-done:
			t.Fatal("walker ended before sending anything")
		case <-time.After(5 * time.Millisecond):
		}
	}
	endPhase()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the walker did not end with the phase")
	}
	if n := len(a.arrivals()); n > 1 {
		t.Errorf("%d requests reached the server: the wait woken by stop still sent one", n)
	}
}

// The wait ends the moment the phase does: a walker waiting for its first start
// when stop fires returns at once, so a shaped cell ends when an unshaped one
// would.
func TestMarkovWalkerWaitEndsWithStop(t *testing.T) {
	a := newArrivalServer(t, 0, 0, 0)
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
	a := newArrivalServer(t, 0, 0, 0)
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
	// The period belongs to the Markov walkers alone: the interval-paced slices
	// run at their own rates in both cells (a period leaking into the adversarial
	// or h2c ticks would starve them). Paced, they get at least what they got
	// beside a closed-loop fleet that competes with them for the CPU.
	if closed.Adversarial.Sent < 10 {
		t.Fatalf("premise: the adversarial slice sent only %d in the closed-loop cell", closed.Adversarial.Sent)
	}
	if paced.Adversarial.Sent*5 < closed.Adversarial.Sent*2 {
		t.Errorf("adversarial slice sent %d paced against %d closed-loop: the period leaked into a non-Markov slice", paced.Adversarial.Sent, closed.Adversarial.Sent)
	}
	if closed.H2CChurn.Sent >= 5 && paced.H2CChurn.Sent*5 < closed.H2CChurn.Sent*2 {
		t.Errorf("h2c slice sent %d paced against %d closed-loop: the period leaked into a non-Markov slice", paced.H2CChurn.Sent, closed.H2CChurn.Sent)
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

// A logout is counted when it is sent, not while it waits for its start: the
// re-login budget (logouts + 10 x logins) is judged against the logouts that
// reached the server. A phase that ends during the wait counts none.
func TestMarkovWalkerCountsALogoutOnlyWhenItIsSent(t *testing.T) {
	const yaml = `login: POST /login
logout: POST /logout
start: bye
states:
  bye:
    request: POST /logout
`
	m, err := markov.LoadMatrix(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	const seed = 0xa11ce
	// Premise: this walker's first start is far away, so stop arrives during
	// the wait.
	if off := time.Until(newStartPacer(time.Hour, rand.New(rand.NewPCG(seed, ^uint64(seed))), time.Now()).next); off < 10*time.Second {
		t.Fatalf("premise: the seeded first start is only %v away", off)
	}
	a := newArrivalServer(t, 0, 0, 0)
	stop, endPhase := context.WithCancel(context.Background())
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	var tally tier1Tally
	done := make(chan struct{})
	go func() {
		defer close(done)
		runMarkovWalker(ctx, stop, &http.Client{Timeout: time.Second}, a.URL, m, seed, time.Hour, &tally)
	}()
	time.Sleep(100 * time.Millisecond)
	endPhase()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("the walker did not end with the phase")
	}
	if n, sent := tally.walkerLogouts.Load(), len(a.arrivals()); n != 0 || sent != 0 {
		t.Errorf("logouts counted %d, requests sent %d: a logout that never left was counted", n, sent)
	}
}
