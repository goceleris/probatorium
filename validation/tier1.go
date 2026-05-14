package validation

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
	"net/http/cookiejar"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/probatorium/validation/markov"
	"github.com/goceleris/probatorium/validation/remote"
)

// tier1Config parameterises a single Tier 1 (always-on property
// stress) run. Built from the orchestrator's [Config] but kept as a
// struct so testing can stub each piece independently.
type tier1Config struct {
	// Driver owns the lifecycle of the refapp under test. Required —
	// a nil driver short-circuits the entire tier (useful for unit
	// tests that only exercise plan-mode).
	Driver remote.Driver

	// RefappArgs is the argv passed to Driver.Start. Typically:
	//   -bind 127.0.0.1:<port>
	// plus refapp-specific flags. Caller picks the port; we wait for
	// the refapp's "ready addr=" stdout line before sending traffic.
	RefappArgs []string

	// BaseURL is the http://host:port prefix the Markov walker
	// targets. Caller picks this in lockstep with RefappArgs so both
	// agree on which port the refapp binds.
	BaseURL string

	// Matrix drives the realistic 60% slice of the workload mix.
	Matrix *markov.Matrix

	// Seed gates determinism. Same seed + same celeris commit + same
	// arch must produce the same Markov walk + the same adversarial
	// slice on every replay.
	Seed uint64

	// Concurrency is the number of parallel Markov walkers. Production
	// soak sweeps {1, 10, 100, 1k, 10k, 1}; tests typically use 1-4.
	Concurrency int

	// ReadyTimeout caps how long Tier 1 waits for the refapp's
	// `ready addr=` line on stdout. Zero defaults to 30s.
	ReadyTimeout time.Duration

	// RequestTimeout is the per-HTTP-request timeout. Zero defaults
	// to 5s. Adversarial walkers may exceed this on slowloris by
	// design — the timeout caps individual victim requests, not the
	// adversary's send loop.
	RequestTimeout time.Duration

	// PIDChan, when non-nil, receives the refapp's OS pid exactly
	// once — after Driver.Start succeeds and waitForReady returns.
	// Capacity 1; driveTier1 non-blocking sends. The orchestrator
	// uses this to fan the PID into forensics capture on hard fail
	// without having to introspect Driver internals.
	PIDChan chan<- int

	// TallyCallback, when non-nil, is invoked at a fixed cadence with
	// the current tally snapshot. The orchestrator uses this to watch
	// HIGH-severity sub-counters (adv.WrongAccepted, h2c.Crashed,
	// ws.AcceptedBadFrame) and emit Incidents the FIRST time any of
	// them goes non-zero — without waiting for the run to end. Keeps
	// the auto-bisect + forensics pipeline reactive to in-band traffic
	// findings, not just out-of-band predicate violations.
	//
	// Callback is called from a single goroutine; implementations don't
	// need to be safe for concurrent invocation.
	TallyCallback func(tier1TallySnapshot)

	// TallyCallbackInterval is how often TallyCallback fires. Zero
	// defaults to 2 seconds; only used when TallyCallback is non-nil.
	TallyCallbackInterval time.Duration

	// SnapshotPath, when non-empty, names the path the tier writes the
	// current tally snapshot to on every TallyCallback tick. Letting
	// long-running soaks (24h, 72h, 10d) surface mid-run progress to
	// monitoring tools that ssh in + cat the file. Best-effort: a
	// write failure logs nothing — the in-memory tally is still
	// authoritative.
	SnapshotPath string
}

// tier1Tally accumulates Tier 1 progress across walker goroutines.
// Atomic counters because every walker writes to them concurrently.
// The adversarial, h2c-churn, ws-torture, and sse-kill sub-tallies
// are pointers so the snapshot projection can read them without
// copying atomic.Int64 (govet copylocks).
type tier1Tally struct {
	requestsSent  atomic.Int64
	requests2xx   atomic.Int64
	requests4xx   atomic.Int64
	requests5xx   atomic.Int64
	requestsError atomic.Int64
	adv           *adversarialTally
	h2c           *h2cTally
	ws            *wsTally
	sse           *sseTally
}

// driveTier1 is the production Tier 1 entry point. Starts the refapp,
// waits for ready, fans Concurrency Markov walkers, returns when ctx
// is done. Refapp lifecycle is bounded by ctx — driveTier1 sends
// SIGTERM on return; if the refapp doesn't exit within 5s the caller
// is expected to escalate to SIGKILL via Driver.Kill.
//
// Returns a snapshot tally (value-typed, atomic-free) plus the first
// error encountered, if any. nil err means the run terminated cleanly
// (ctx cancelled). The internal tally is heap-allocated so the
// returned snapshot reflects the final state without copying atomic
// counters (govet copylocks).
func driveTier1(ctx context.Context, cfg tier1Config) (tier1TallySnapshot, error) {
	tally := &tier1Tally{}
	if cfg.Driver == nil {
		return tally.snapshot(), errors.New("tier1: nil Driver")
	}
	if cfg.Matrix == nil {
		return tally.snapshot(), errors.New("tier1: nil Matrix")
	}
	if cfg.Concurrency < 1 {
		cfg.Concurrency = 1
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 30 * time.Second
	}
	if cfg.RequestTimeout <= 0 {
		cfg.RequestTimeout = 5 * time.Second
	}

	// Start the refapp. Driver.Start is non-blocking; the binary is
	// alive but may not have bound its port yet. Wait for the
	// "ready addr=" line on stdout next.
	proc, err := cfg.Driver.Start(ctx, cfg.RefappArgs)
	if err != nil {
		return tally.snapshot(), fmt.Errorf("tier1: start refapp: %w", err)
	}
	// SIGTERM the refapp on return, regardless of how we exited.
	// Driver.Wait inside Stderr() drains until the process actually
	// dies; we don't block on it here so the caller can escalate.
	defer func() { _ = proc.Signal(0xf) /* SIGTERM */ }()

	if err := waitForReady(ctx, proc, cfg.ReadyTimeout); err != nil {
		return tally.snapshot(), fmt.Errorf("tier1: refapp not ready: %w", err)
	}

	// Refapp is bound; surface its PID for the orchestrator's
	// forensics path. Non-blocking — if the channel is full (the
	// orchestrator only ever reads once) we drop silently.
	if cfg.PIDChan != nil {
		select {
		case cfg.PIDChan <- proc.PID():
		default:
		}
	}

	// Tier 1 fan-out. Five slices today (matches the full workload
	// mix called out in issue #55):
	//
	//   ~60% Markov-shaped traffic  — runMarkovWalker, BaseURL.
	//   ~20% adversarial            — runAdversarialWalker, hostPort.
	//   ~10% h2c upgrade churn      — runH2CChurnWalker, hostPort.
	//   ~5%  WS frame torture       — runWSTortureWalker, hostPort + "/ws".
	//   ~5%  SSE kill-mid-stream    — runSSEKillWalker, hostPort + "/events".
	//
	// hostPort is BaseURL with the "http://" stripped — adversarial,
	// h2c churn, WS torture, and SSE kill all speak raw TCP so they
	// can send bytes net/http would otherwise rewrite (or, for h2c,
	// complete handshakes we explicitly want to RST mid-flight).
	hostPort := strings.TrimPrefix(strings.TrimPrefix(cfg.BaseURL, "http://"), "https://")
	httpc := &http.Client{Timeout: cfg.RequestTimeout}
	var wg sync.WaitGroup
	advTally := &adversarialTally{}
	tally.adv = advTally
	h2cTallyPtr := &h2cTally{}
	tally.h2c = h2cTallyPtr
	wsTallyPtr := &wsTally{}
	tally.ws = wsTallyPtr
	sseTallyPtr := &sseTally{}
	tally.sse = sseTallyPtr
	// Walker budget. Higher-overhead slices activate only at higher
	// concurrencies so small smoke tests don't pay for them:
	//   adv     — at concurrency >= 1 (always at least 1 walker)
	//   h2c     — at concurrency >= 10
	//   ws      — at concurrency >= 20 (full WS handshake per fire)
	//   sse     — at concurrency >= 20 (each fire holds a stream for
	//             up to ~1.5s before RST'ing)
	advCount := cfg.Concurrency / 5
	if advCount < 1 && cfg.Concurrency >= 1 {
		advCount = 1
	}
	h2cCount := 0
	if cfg.Concurrency >= 10 {
		h2cCount = cfg.Concurrency / 10
		if h2cCount < 1 {
			h2cCount = 1
		}
	}
	wsCount := 0
	if cfg.Concurrency >= 20 {
		wsCount = cfg.Concurrency / 20
		if wsCount < 1 {
			wsCount = 1
		}
	}
	sseCount := 0
	if cfg.Concurrency >= 20 {
		sseCount = cfg.Concurrency / 20
		if sseCount < 1 {
			sseCount = 1
		}
	}
	markovCount := cfg.Concurrency - advCount - h2cCount - wsCount - sseCount
	if markovCount < 1 {
		markovCount = 1
	}
	for i := 0; i < markovCount; i++ {
		wg.Add(1)
		go func(walkerID int) {
			defer wg.Done()
			seed := cfg.Seed ^ uint64(walkerID)*0x9e3779b97f4a7c15
			runMarkovWalker(ctx, httpc, cfg.BaseURL, cfg.Matrix, seed, tally)
		}(i)
	}
	for i := 0; i < advCount; i++ {
		wg.Add(1)
		go func(walkerID int) {
			defer wg.Done()
			seed := cfg.Seed ^ uint64(0xdead0000+walkerID)*0x9e3779b97f4a7c15
			// Adversarial fires slower than Markov so it stays inside
			// its budget share (~1/5 of the total request volume).
			runAdversarialWalker(ctx, hostPort, seed, 50*time.Millisecond, advTally)
		}(i)
	}
	for i := 0; i < h2cCount; i++ {
		wg.Add(1)
		go func(walkerID int) {
			defer wg.Done()
			seed := cfg.Seed ^ uint64(0xc0de0000+walkerID)*0x9e3779b97f4a7c15
			// h2c churn ticks slower than adversarial — the
			// PauseAccept race we're hunting fires at H2-dial
			// frequency, not header-parse frequency. 100ms keeps the
			// rate below the engine's own listener turnover so we're
			// racing the engine's state machine, not just creating
			// backlog.
			runH2CChurnWalker(ctx, hostPort, seed, 100*time.Millisecond, h2cTallyPtr)
		}(i)
	}
	for i := 0; i < wsCount; i++ {
		wg.Add(1)
		go func(walkerID int) {
			defer wg.Done()
			seed := cfg.Seed ^ uint64(0xfade0000+walkerID)*0x9e3779b97f4a7c15
			// WS torture ticks slowest — each cell does a full HTTP
			// upgrade + read 101 + send torture frame + classify. At
			// 150ms tick and 2s per-fire timeout the worst-case rate
			// is ~6 fires/sec per walker.
			runWSTortureWalker(ctx, hostPort, "/ws", seed, 150*time.Millisecond, wsTallyPtr)
		}(i)
	}
	for i := 0; i < sseCount; i++ {
		wg.Add(1)
		go func(walkerID int) {
			defer wg.Done()
			seed := cfg.Seed ^ uint64(0x55e50000+walkerID)*0x9e3779b97f4a7c15
			// SSE kill ticks at 200ms — each fire holds a stream for
			// 50ms–1.5s then RSTs, so realistic walker rate is one
			// stream-kill per few hundred ms. The point is to keep
			// fresh disconnect events flowing to the I-CONN-2 oracle,
			// not to maximise throughput.
			runSSEKillWalker(ctx, hostPort, "/events", seed, 200*time.Millisecond, sseTallyPtr)
		}(i)
	}
	// Optional periodic tally-callback + snapshot-to-disk for reactive
	// incident emission AND mid-run progress visibility. Stops when ctx
	// is done; doesn't participate in wg because the observation work
	// is best-effort, not workload.
	if cfg.TallyCallback != nil || cfg.SnapshotPath != "" {
		interval := cfg.TallyCallbackInterval
		if interval <= 0 {
			interval = 2 * time.Second
		}
		go func() {
			tick := time.NewTicker(interval)
			defer tick.Stop()
			emit := func() {
				snap := tally.snapshot()
				if cfg.TallyCallback != nil {
					cfg.TallyCallback(snap)
				}
				if cfg.SnapshotPath != "" {
					if data, err := json.MarshalIndent(snap, "", "  "); err == nil {
						_ = os.WriteFile(cfg.SnapshotPath, data, 0o644)
					}
				}
			}
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					emit()
				}
			}
		}()
	}
	wg.Wait()
	return tally.snapshot(), nil
}

// waitForReady tails the refapp's combined stderr+stdout until it
// emits the canonical `ready addr=<addr>` line (see
// validation/refapp/auth_session_ratelimit/main.go:10). Returns
// non-nil error on timeout or process death before ready.
//
// The scanner goroutine selects on readyCtx for EVERY channel send,
// not just the loop boundary. This closes the leak the 3-day soak
// found: pre-fix, when waitForReady returned early on a successful
// ready line, the scanner goroutine kept running, eventually blocked
// on lineCh because nobody read it, and retained its 64 KiB buffer
// + the bufio.Scanner state forever. Tier 3 calls waitForReady once
// per seed → ~20K orphan goroutines + ~1.4 GB retained over a 72h
// soak. (Soak data: validator RSS climbed 60 MB/h linear-fit; this
// goroutine leak accounts for the bulk of it.)
//
// Post-fix:
//   - readyCtx cancels on function return (deferred above) AND on
//     timeout
//   - every channel send selects on readyCtx, so the goroutine
//     observes cancel even when blocked on a full lineCh
//   - lineCh is closed when the goroutine exits so a concurrent
//     reader doesn't deadlock either
func waitForReady(ctx context.Context, proc remote.Process, timeout time.Duration) error {
	readyCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	if timeout > 0 {
		var timeoutCancel context.CancelFunc
		readyCtx, timeoutCancel = context.WithTimeout(readyCtx, timeout)
		defer timeoutCancel()
	}

	scanner := bufio.NewScanner(proc.Stderr())
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		// Close lineCh on exit so any concurrent reader (none today,
		// but defensive against future refactors) doesn't deadlock.
		defer close(lineCh)
		for scanner.Scan() {
			select {
			case lineCh <- scanner.Text():
			case <-readyCtx.Done():
				return
			}
		}
		select {
		case errCh <- func() error {
			if err := scanner.Err(); err != nil {
				return err
			}
			return io.EOF
		}():
		case <-readyCtx.Done():
		}
	}()

	for {
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("ready timeout: %w", readyCtx.Err())
		case line, ok := <-lineCh:
			if !ok {
				return fmt.Errorf("refapp exited before ready: %w", io.EOF)
			}
			if strings.HasPrefix(line, "ready addr=") {
				return nil
			}
		case err := <-errCh:
			return fmt.Errorf("refapp exited before ready: %w", err)
		}
	}
}

// runMarkovWalker walks the Markov chain until ctx is cancelled,
// sending one HTTP request per state visit. The endpoint URL is
// derived from the state name via [markovStateToPath].
//
// Each walker gets its own [http.CookieJar] + [http.Client] so the
// N concurrent walkers exercise N parallel session lifecycles
// against the refapp's session middleware. At walker start, it POSTs
// `/login` with deterministic-per-seed credentials; subsequent
// requests carry the resulting cookie, hitting the 2xx handler
// paths instead of the 401 reject path.
//
// On a 401 mid-run (session expired by idle timeout) the walker
// re-logs in transparently. Soak runs at 72h with 30-min session
// idle timeout would otherwise drift into all-401 territory after
// the first idle window.
func runMarkovWalker(ctx context.Context, parent *http.Client, base string,
	m *markov.Matrix, seed uint64, tally *tier1Tally,
) {
	// Per-walker cookie jar. nil error per cookiejar.New's contract
	// when options is nil.
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{
		Timeout: parent.Timeout,
		Jar:     jar,
	}
	// Deterministic credentials so re-running with the same seed
	// produces identical wire traffic.
	username := fmt.Sprintf("walker-%016x", seed)
	password := fmt.Sprintf("pw-%016x", ^seed)
	_ = walkerLogin(ctx, hc, base, username, password)

	chain := markov.New(m, seed)
	rng := rand.New(rand.NewPCG(seed, ^seed))
	_ = rng // reserved for adversarial-slice follow-up
	for {
		if ctx.Err() != nil {
			return
		}
		state := chain.Current()
		path := markovStateToPath(state)
		if path != "" {
			status := doMarkovRequest(ctx, hc, base+path, tally)
			if status == 401 {
				// Session likely expired — re-login and keep walking.
				// The next request will pick up the fresh cookie.
				_ = walkerLogin(ctx, hc, base, username, password)
			}
		}
		if _, ok := chain.Next(); !ok {
			// Terminal state; reset and continue. Soak workloads are
			// unbounded.
			chain.Reset()
		}
	}
}

// walkerLogin POSTs /login with the given username + password. The
// refapp's policy is "any non-empty (username, password)
// authenticates" — see registerRoutes — so this always succeeds when
// the server is healthy. Failure is silently swallowed; the walker's
// 401-retry path handles the case where the eventual GET reveals
// auth is missing.
//
// Response cookie is stored in hc.Jar; subsequent hc.Do() calls
// carry it automatically.
func walkerLogin(ctx context.Context, hc *http.Client, base, username, password string) error {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req, err := http.NewRequestWithContext(ctx, "POST", base+"/login",
		strings.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	_, _ = io.Copy(io.Discard, resp.Body)
	if resp.StatusCode != 200 {
		return fmt.Errorf("login %s: status %d", username, resp.StatusCode)
	}
	return nil
}

// doMarkovRequest issues one GET against base+path and folds the
// result into tally. Errors are folded into requestsError, not
// returned — Tier 1's contract is "keep firing until ctx is done."
// Returns the response status code (0 on transport error) so the
// caller can react to session-expiry 401s with a re-login.
func doMarkovRequest(ctx context.Context, hc *http.Client, url string, tally *tier1Tally) int {
	tally.requestsSent.Add(1)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		tally.requestsError.Add(1)
		return 0
	}
	resp, err := hc.Do(req)
	if err != nil {
		tally.requestsError.Add(1)
		return 0
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain body — keep-alive requires it.
	_, _ = io.Copy(io.Discard, resp.Body)
	switch {
	case resp.StatusCode >= 500:
		tally.requests5xx.Add(1)
	case resp.StatusCode >= 400:
		tally.requests4xx.Add(1)
	default:
		tally.requests2xx.Add(1)
	}
	return resp.StatusCode
}

// markovStateToPath maps a Markov state name to the corresponding
// refapp endpoint path. Empty string = "this state issues no HTTP
// request" (terminal states like `logout` that conceptually fire one
// path are mapped explicitly here so the walker treats them as
// regular request states; truly silent states return "").
//
// Keep in lockstep with validation/markov/auth_session_ratelimit.yaml.
// Adding a state in the yaml without an entry here is fine — the
// walker just doesn't fire a request for it, which preserves the
// chain shape. Removing an entry that the yaml still references is
// the loud failure case.
func markovStateToPath(state string) string {
	switch state {
	case "home":
		return "/"
	case "login":
		return "/api/login"
	case "list_users":
		return "/api/users"
	case "user_detail":
		// Pick a deterministic id within the seeded range. We don't
		// vary it per request — the orchestrator's Tier 3 replay
		// generates the user fixtures, and a single canonical id
		// keeps Tier 1 traffic legible in cell-completion reports.
		return "/api/users/u1"
	case "create_user":
		// POST handler — Tier 1 doesn't currently mutate (preserves
		// the Markov walker's idempotency). Treat as silent for now;
		// the RESTler-style fuzzer (Tier 2) is the proper place for
		// stateful create/update flows.
		return ""
	case "update_user":
		return ""
	case "logout":
		return "/api/logout"
	}
	return ""
}

// snapshot returns the current tally as a value. Used by tests and
// by the orchestrator's per-tick reporter.
func (t *tier1Tally) snapshot() tier1TallySnapshot {
	s := tier1TallySnapshot{
		RequestsSent:  t.requestsSent.Load(),
		Requests2xx:   t.requests2xx.Load(),
		Requests4xx:   t.requests4xx.Load(),
		Requests5xx:   t.requests5xx.Load(),
		RequestsError: t.requestsError.Load(),
	}
	if t.adv != nil {
		s.Adversarial = t.adv.snapshot()
	}
	if t.h2c != nil {
		s.H2CChurn = t.h2c.snapshot()
	}
	if t.ws != nil {
		s.WSTorture = t.ws.snapshot()
	}
	if t.sse != nil {
		s.SSEKill = t.sse.snapshot()
	}
	return s
}

// tier1TallySnapshot is the value-typed projection of tier1Tally
// (atomic counters loaded at one instant).
type tier1TallySnapshot struct {
	RequestsSent  int64               `json:"requests_sent"`
	Requests2xx   int64               `json:"requests_2xx"`
	Requests4xx   int64               `json:"requests_4xx"`
	Requests5xx   int64               `json:"requests_5xx"`
	RequestsError int64               `json:"requests_error"`
	Adversarial   adversarialSnapshot `json:"adversarial,omitempty"`
	H2CChurn      h2cSnapshot         `json:"h2c_churn,omitempty"`
	WSTorture     wsSnapshot          `json:"ws_torture,omitempty"`
	SSEKill       sseSnapshot         `json:"sse_kill,omitempty"`
}

// String formats a tally for the run summary log line.
func (s tier1TallySnapshot) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "sent=%d 2xx=%d 4xx=%d 5xx=%d err=%d",
		s.RequestsSent, s.Requests2xx, s.Requests4xx, s.Requests5xx, s.RequestsError)
	return b.String()
}
