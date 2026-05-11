package validation

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"net/http"
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
}

// tier1Tally accumulates Tier 1 progress across walker goroutines.
// Atomic counters because every walker writes to them concurrently.
type tier1Tally struct {
	requestsSent  atomic.Int64
	requests2xx   atomic.Int64
	requests4xx   atomic.Int64
	requests5xx   atomic.Int64
	requestsError atomic.Int64
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

	// Tier 1 production-step #1: fan walkers.
	//
	// The 60% Markov / 20% adversarial / 10% h2c-upgrade / 5% WS /
	// 5% SSE workload mix lives in driveTier1's full implementation.
	// This first cut wires only the 60% Markov slice — the adversarial
	// walker is enough independent code to live in its own follow-up
	// (issue #55, follow-up scope). Concurrency sweeps stay
	// representative because the workload-mix split is per-tick not
	// per-walker.
	httpc := &http.Client{Timeout: cfg.RequestTimeout}
	var wg sync.WaitGroup
	for i := 0; i < cfg.Concurrency; i++ {
		wg.Add(1)
		go func(walkerID int) {
			defer wg.Done()
			seed := cfg.Seed ^ uint64(walkerID)*0x9e3779b97f4a7c15
			runMarkovWalker(ctx, httpc, cfg.BaseURL, cfg.Matrix, seed, tally)
		}(i)
	}
	wg.Wait()
	return tally.snapshot(), nil
}

// waitForReady tails the refapp's combined stderr+stdout until it
// emits the canonical `ready addr=<addr>` line (see
// validation/refapp/auth_session_ratelimit/main.go:10). Returns
// non-nil error on timeout or process death before ready.
func waitForReady(ctx context.Context, proc remote.Process, timeout time.Duration) error {
	readyCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	scanner := bufio.NewScanner(proc.Stderr())
	scanner.Buffer(make([]byte, 64*1024), 1024*1024)

	lineCh := make(chan string, 1)
	errCh := make(chan error, 1)
	go func() {
		for scanner.Scan() {
			lineCh <- scanner.Text()
		}
		if err := scanner.Err(); err != nil {
			errCh <- err
		} else {
			errCh <- io.EOF
		}
	}()

	for {
		select {
		case <-readyCtx.Done():
			return fmt.Errorf("ready timeout: %w", readyCtx.Err())
		case line := <-lineCh:
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
func runMarkovWalker(ctx context.Context, hc *http.Client, base string,
	m *markov.Matrix, seed uint64, tally *tier1Tally,
) {
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
			doMarkovRequest(ctx, hc, base+path, tally)
		}
		if _, ok := chain.Next(); !ok {
			// Terminal state; reset and continue. Soak workloads are
			// unbounded.
			chain.Reset()
		}
	}
}

// doMarkovRequest issues one GET against base+path and folds the
// result into tally. Errors are folded into requestsError, not
// returned — Tier 1's contract is "keep firing until ctx is done."
func doMarkovRequest(ctx context.Context, hc *http.Client, url string, tally *tier1Tally) {
	tally.requestsSent.Add(1)
	req, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		tally.requestsError.Add(1)
		return
	}
	resp, err := hc.Do(req)
	if err != nil {
		tally.requestsError.Add(1)
		return
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
	return tier1TallySnapshot{
		RequestsSent:  t.requestsSent.Load(),
		Requests2xx:   t.requests2xx.Load(),
		Requests4xx:   t.requests4xx.Load(),
		Requests5xx:   t.requests5xx.Load(),
		RequestsError: t.requestsError.Load(),
	}
}

// tier1TallySnapshot is the value-typed projection of tier1Tally
// (atomic counters loaded at one instant).
type tier1TallySnapshot struct {
	RequestsSent  int64 `json:"requests_sent"`
	Requests2xx   int64 `json:"requests_2xx"`
	Requests4xx   int64 `json:"requests_4xx"`
	Requests5xx   int64 `json:"requests_5xx"`
	RequestsError int64 `json:"requests_error"`
}

// String formats a tally for the run summary log line.
func (s tier1TallySnapshot) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "sent=%d 2xx=%d 4xx=%d 5xx=%d err=%d",
		s.RequestsSent, s.Requests2xx, s.Requests4xx, s.Requests5xx, s.RequestsError)
	return b.String()
}
