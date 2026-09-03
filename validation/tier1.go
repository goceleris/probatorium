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
	"net"
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

// streamingWalkerMinConcurrency is the concurrency at/above which the WS
// torture and SSE kill walker slices activate. Set LOW (4) so every non-smoke
// Tier 1 run — including the matrix's per-cell default of 10 — drives the
// engine's WebSocket/SSE (and inline-Detach) path. See the walker-budget note
// in driveTier1 for why this used to be 20 and why that hid celeris#309.
const streamingWalkerMinConcurrency = 4

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

	// BaseURL is a FALLBACK http://host:port prefix for the walkers,
	// used only until the refapp announces its real bound address. With a
	// "-bind :0" launch the OS picks the port, so the authoritative
	// target is the addr parsed off the "ready addr=" banner (see
	// driveTier1, which rebuilds the effective base URL from it once the
	// refapp is up). A fixed-port launch makes BaseURL and the announced
	// addr agree, so this stays correct in single-cell mode too.
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

	// AddrChan, when non-nil, receives the refapp's REAL bound address
	// (host:port, no scheme) exactly once — the addr parsed off the
	// "ready addr=" banner after the refapp binds. Capacity 1;
	// driveTier1 non-blocking sends. The orchestrator uses it to target
	// live forensics (pprof) at the actual port when the refapp launched
	// with "-bind :0" and the OS chose the port.
	AddrChan chan<- string

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
	requests5xx   atomic.Int64 // UNEXPECTED 5xx only (see requests5xxExpected)
	requestsError atomic.Int64
	// requests5xxExpected counts 5xx from states the corpus marks
	// `expect: 5xx` (designed-to-fail routes such as observability's
	// /api/error). Kept apart so requests5xx can be gated to zero.
	requests5xxExpected atomic.Int64
	// invariantHits counts unexpected 5xx whose body carries the refapp
	// invariant marker ("x-invariant": e.g. I-DRV-1 read-after-write). A
	// refapp reporting its own invariant violation must surface as an
	// invariant hit, not vanish into a generic 5xx count.
	invariantHits atomic.Int64
	adv           *adversarialTally
	h2c           *h2cTally
	ws            *wsTally
	sse           *sseTally
	liveness      *livenessTally
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

	// Liveness oracle: tracks whether the refapp PROCESS survives the run.
	// Wired before Start so the watchers below can feed it from the first
	// instant. See liveness.go for why the walkers alone can't see a death.
	tally.liveness = &livenessTally{}

	// Start the refapp. Driver.Start is non-blocking; the binary is
	// alive but may not have bound its port yet. Wait for the
	// "ready addr=" line on stdout next.
	proc, err := cfg.Driver.Start(ctx, cfg.RefappArgs)
	if err != nil {
		return tally.snapshot(), fmt.Errorf("tier1: start refapp: %w", err)
	}
	// SIGTERM the refapp on return, regardless of how we exited.
	defer func() { _ = proc.Signal(0xf) /* SIGTERM */ }()

	// runCtx scopes the walkers (plus warmup + the periodic snapshot). It is
	// cancelled by the parent ctx (clean end of run) OR by the liveness
	// watchers the instant the refapp dies — so a crash stops the walkers
	// immediately instead of letting them hammer a dead port until the parent
	// deadline, and lets driveTier1 return promptly with Crashed set.
	runCtx, cancelRun := context.WithCancel(ctx)
	defer cancelRun()

	// superviseStderr is the SINGLE owner of the refapp's stdout+stderr for
	// the whole run: ready detection + crash-signature scan + drain. A second
	// reader would corrupt it, which is exactly why Tier 1 no longer calls
	// waitForReady (that spawned its own competing reader). watchProcessExit
	// is the authoritative death detector (catches silent SIGKILL/OOM too).
	// Both cancelRun on crash so the walkers wind down at once.
	readyCh := make(chan struct{})
	readyErrCh := make(chan error, 1)
	var readyOnce sync.Once
	var readyAddr string // refapp's REAL bound addr; set before close(readyCh)
	go superviseStderr(proc.Stderr(), tally.liveness,
		func(addr string) { readyOnce.Do(func() { readyAddr = addr; close(readyCh) }) },
		func(err error) {
			select {
			case readyErrCh <- err:
			default:
			}
		},
		cancelRun)
	go watchProcessExit(ctx, proc, tally.liveness, cancelRun)

	select {
	case <-readyCh:
		// refapp bound — proceed to fan out walkers.
	case err := <-readyErrCh:
		return tally.snapshot(), fmt.Errorf("tier1: refapp not ready: %w", err)
	case <-time.After(cfg.ReadyTimeout):
		return tally.snapshot(), fmt.Errorf("tier1: refapp not ready: timeout after %s", cfg.ReadyTimeout)
	case <-ctx.Done():
		return tally.snapshot(), ctx.Err()
	}

	// The refapp announced its REAL bound address on the ready banner.
	// With a "-bind :0" launch the OS chose the port, so this — not the
	// pre-launch cfg.BaseURL guess — is the authoritative target for the
	// walkers, the warm-up, and the responsiveness probe. A fixed-port
	// launch makes the two agree, so single-cell mode is unaffected.
	//
	// The banner carries host:port (net.Listener.Addr().String()); strip a
	// stray scheme defensively so a "http://host:port" banner still yields
	// a single, clean base URL rather than "http://http://...".
	if readyAddr != "" {
		readyAddr = strings.TrimPrefix(strings.TrimPrefix(readyAddr, "http://"), "https://")
	}
	baseURL := cfg.BaseURL
	if readyAddr != "" {
		baseURL = "http://" + readyAddr
	}
	// Surface the resolved addr so the orchestrator can target live
	// forensics (pprof) at the actual port. Non-blocking — cap-1 channel,
	// read at most once.
	if cfg.AddrChan != nil && readyAddr != "" {
		select {
		case cfg.AddrChan <- readyAddr:
		default:
		}
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

	// Responsiveness watcher (I-HANG): the crash watchers above catch a refapp
	// that DIES; this catches one that stays alive but stops answering — a
	// deadlock / wedge (celeris#311) the per-request walkers read as mere
	// connection errors. Health-probe timeouts trip it; cancelRun winds the
	// run down so driveTier1 returns promptly with Hung set.
	go watchResponsiveness(runCtx, baseURL, tally.liveness, cancelRun)

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
	hostPort := strings.TrimPrefix(strings.TrimPrefix(baseURL, "http://"), "https://")
	httpc := &http.Client{Timeout: cfg.RequestTimeout}

	// Warm-up phase: hit /healthz repeatedly for 30s before launching
	// the bug-oracle walkers. Lets the kernel's IRQ balance + softirq
	// routing settle, the conn pool reach equilibrium, and the engine's
	// internal pools warm. Without this, the matrix-nightly tier's
	// short-per-cell (2.5min) cold start dominates the error rate on
	// hardware without hardware-RSS NICs — diagnosed from the
	// driver_redis-iouring-msr1 49% transport_err pattern (nightly
	// 26449972230) that disappeared to 0.005% under the 24h soak's
	// steady-state operation (26459413667). On long-budget runs (>10min
	// per cell) the warm-up is a small constant overhead with no impact
	// on bug detection; on short-budget runs (<60s of total ctx budget)
	// it's skipped entirely so smoke / unit tests aren't starved.
	if dl, ok := runCtx.Deadline(); !ok || time.Until(dl) >= time.Minute {
		const warmupBudget = 30 * time.Second
		warmupCtx, cancelWarmup := context.WithTimeout(runCtx, warmupBudget)
		req, werr := http.NewRequestWithContext(warmupCtx, "GET", baseURL+"/healthz", nil)
		if werr == nil {
			for warmupCtx.Err() == nil {
				resp, derr := httpc.Do(req.Clone(warmupCtx))
				if derr == nil {
					_, _ = io.Copy(io.Discard, resp.Body)
					_ = resp.Body.Close()
				}
				// 5ms pace — keeps the kernel/IRQ-routing warming up
				// without saturating the listener (the goal is settling,
				// not stress-testing).
				select {
				case <-warmupCtx.Done():
				case <-time.After(5 * time.Millisecond):
				}
			}
		}
		cancelWarmup()
	}
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
	// concurrencies so single-walker smoke tests don't pay for them:
	//   adv     — at concurrency >= 1 (always at least 1 walker)
	//   h2c     — at concurrency >= 10
	//   ws      — at concurrency >= streamingWalkerMinConcurrency (full WS
	//             handshake per fire)
	//   sse     — at concurrency >= streamingWalkerMinConcurrency (each fire
	//             holds a stream for up to ~1.5s before RST'ing)
	//
	// ws/sse fire from a LOW threshold (4) on purpose: the engine's inline
	// streaming-Detach path (and any future WS/SSE corner) must be exercised
	// on EVERY non-smoke run, including the matrix's per-cell concurrency of
	// 10. They used to gate at 20 — above the per-cell default — so the epoll
	// engine was validated for hours without a single WebSocket/SSE upgrade,
	// which is exactly how the inline detachMu double-unlock (celeris#309)
	// reached a release. The liveness oracle then turns any resulting crash
	// into a hard failure. Smoke runs (concurrency 1-3) still skip them.
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
	if cfg.Concurrency >= streamingWalkerMinConcurrency {
		wsCount = cfg.Concurrency / 20
		if wsCount < 1 {
			wsCount = 1
		}
	}
	sseCount := 0
	if cfg.Concurrency >= streamingWalkerMinConcurrency {
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
			runMarkovWalker(runCtx, httpc, baseURL, cfg.Matrix, seed, tally)
		}(i)
	}
	for i := 0; i < advCount; i++ {
		wg.Add(1)
		go func(walkerID int) {
			defer wg.Done()
			seed := cfg.Seed ^ uint64(0xdead0000+walkerID)*0x9e3779b97f4a7c15
			// Adversarial fires slower than Markov so it stays inside
			// its budget share (~1/5 of the total request volume).
			runAdversarialWalker(runCtx, hostPort, seed, 50*time.Millisecond, advTally)
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
			runH2CChurnWalker(runCtx, hostPort, seed, 100*time.Millisecond, h2cTallyPtr)
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
			runWSTortureWalker(runCtx, hostPort, "/ws", seed, 150*time.Millisecond, wsTallyPtr)
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
			runSSEKillWalker(runCtx, hostPort, "/events", seed, 200*time.Millisecond, sseTallyPtr)
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
				case <-runCtx.Done():
					return
				case <-tick.C:
					emit()
				}
			}
		}()
	}
	wg.Wait()
	snap := tally.snapshot()
	// If the refapp crashed OR hung, the periodic ticker stopped at cancelRun
	// and may not have ticked between the event and here. Fire the callback
	// once more, synchronously, so the I-LIVENESS / I-HANG incident is
	// guaranteed to reach the orchestrator (fire() is deduped, so a double tick
	// is harmless).
	if (snap.Liveness.Crashed || snap.Liveness.Hung) && cfg.TallyCallback != nil {
		cfg.TallyCallback(snap)
	}
	return snap, nil
}

// waitForReady tails the refapp's combined stderr+stdout until it
// emits the canonical `ready addr=<addr>` line (see
// validation/refapp/auth_session_ratelimit/main.go:10). On success it
// returns the announced addr — the refapp's REAL bound address, which
// with a "-bind :0" launch is the OS-chosen port the caller cannot know
// otherwise. Returns a non-nil error (and "") on timeout or process
// death before ready.
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
//
// refappOutputCapMax bounds the buffered refapp output retained on
// readiness failure. Big enough to capture a typical Go panic + stack
// trace (~32 KiB on a fresh runtime) plus the celeris startup banner;
// small enough that a runaway log-loop refapp doesn't OOM the validator
// or fill incident dossiers indefinitely.
const refappOutputCapMax = 64 * 1024

func waitForReady(ctx context.Context, proc remote.Process, timeout time.Duration) (string, error) {
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

	// Accumulate every non-ready line the refapp wrote, capped at
	// refappOutputCapMax. When the refapp exits before printing
	// "ready addr=" (cell-09 driver_postgres-iouring seed 0x1, where
	// the refapp died in 14 ms with zero diagnostic surface), the
	// returned error embeds these lines so the seed-failure dossier
	// records WHY the refapp died, not just THAT it did. drainRemaining
	// Output above (which reads pipes AFTER the process exits) never
	// fires here because the refapp's pipes are already closed by
	// the time the caller looks at them.
	var captured strings.Builder
	captureLine := func(line string) {
		if captured.Len() >= refappOutputCapMax {
			return
		}
		captured.WriteString(line)
		captured.WriteByte('\n')
	}
	appendCaptured := func(err error) error {
		if captured.Len() == 0 {
			return err
		}
		return fmt.Errorf("%w\n--- refapp stderr ---\n%s", err, captured.String())
	}

	for {
		select {
		case <-readyCtx.Done():
			return "", appendCaptured(fmt.Errorf("ready timeout: %w", readyCtx.Err()))
		case line, ok := <-lineCh:
			if !ok {
				return "", appendCaptured(fmt.Errorf("refapp exited before ready: %w", io.EOF))
			}
			if strings.HasPrefix(line, "ready addr=") {
				return strings.TrimSpace(strings.TrimPrefix(line, "ready addr=")), nil
			}
			captureLine(line)
		case err := <-errCh:
			return "", appendCaptured(fmt.Errorf("refapp exited before ready: %w", err))
		}
	}
}

// runMarkovWalker walks the Markov chain until ctx is cancelled,
// sending one HTTP request per state visit. The endpoint URL for each
// state comes from the matrix's data-driven Requests map.
//
// Each walker gets its own [http.CookieJar] + [http.Client] so the
// N concurrent walkers exercise N parallel session lifecycles
// against the refapp's session middleware. If the matrix declares a
// top-level `login: METHOD path` directive, the walker POSTs that
// endpoint with deterministic-per-seed credentials BEFORE the chain
// starts, and again whenever a chain request returns 401.
//
// Refapps without auth (kitchen_sink, observability, static_swagger_proxy,
// driver_*) omit the login directive — walker skips authentication
// entirely. Pre-fix: walkerLogin hardcoded to /login → for refapps
// without that route, every initial request returned 404 from the
// "login" call (silently swallowed) and there was no auth-flow
// realism; refapps WITH login at a non-/login path (auth_session_ratelimit
// serves /login but some others serve /api/login) had silent
// path-mismatch bugs.
func runMarkovWalker(ctx context.Context, parent *http.Client, base string,
	m *markov.Matrix, seed uint64, tally *tier1Tally,
) {
	// Per-walker cookie jar. nil error per cookiejar.New's contract
	// when options is nil.
	jar, _ := cookiejar.New(nil)
	// Per-walker [http.Transport] with a single-conn keep-alive pool.
	//
	// Pre-fix: every walker built `&http.Client{Timeout, Jar}` with no
	// Transport set → all walkers shared [http.DefaultTransport], whose
	// `MaxIdleConnsPerHost` defaults to 2. With N (default 19–30)
	// concurrent markov walkers racing for those 2 idle slots, ~N-2
	// connections close per "round of N finishing simultaneously",
	// each closure consuming an ephemeral port on the validator host.
	// msa2-client runs `tcp_tw_reuse=2` (loopback-only on Linux ≥4.12),
	// so non-loopback (client→bench_target:8080) 4-tuples cannot reuse
	// TIME_WAIT slots; the kernel cycles through the full 28K ephemeral
	// range, then `connect()` starts returning EADDRNOTAVAIL — the
	// matrix nightly's std-engine "connection storm" with no obvious
	// celeris-side cause (cells 02/05/17/23 across both archs, 35–86%
	// transport-error rates uncorrelated with the refapp's actual
	// failure surface).
	//
	// Isolation probe verified the diagnosis: shared-DefaultTransport
	// shape exhausts ephemeral ports at ~t=80s on BOTH std and iouring
	// (5.3–9.9% err, 28k TIME_WAIT). Per-walker `MaxIdleConnsPerHost=1`
	// gives each walker its own keep-alive conn that never overflows
	// the idle pool: 0.00% err, 14x throughput, 30 TIME_WAIT at end.
	//
	// Why `MaxConnsPerHost=1`: walkers are strictly sequential
	// (chain.Next → doMarkovRequest → repeat), so a single conn per
	// walker is sufficient. Capping it eliminates the dial-then-close
	// race when walkerLogin and the first chain request happen to
	// overlap with idle-pool eviction.
	tr := &http.Transport{
		MaxIdleConns:        1,
		MaxIdleConnsPerHost: 1,
		MaxConnsPerHost:     1,
		IdleConnTimeout:     90 * time.Second,
		DisableKeepAlives:   false,
		DialContext: (&net.Dialer{
			Timeout:   5 * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext,
	}
	hc := &http.Client{
		Timeout:   parent.Timeout,
		Jar:       jar,
		Transport: tr,
	}
	defer tr.CloseIdleConnections()
	// Deterministic credentials so re-running with the same seed
	// produces identical wire traffic.
	username := fmt.Sprintf("walker-%016x", seed)
	password := fmt.Sprintf("pw-%016x", ^seed)
	hasLogin := m.Login.Method != "" && m.Login.Path != ""
	if hasLogin {
		_ = walkerLogin(ctx, hc, base, m.Login, username, password)
	}

	chain := markov.New(m, seed)
	rng := rand.New(rand.NewPCG(seed, ^seed))
	_ = rng // reserved for adversarial-slice follow-up
	for {
		if ctx.Err() != nil {
			return
		}
		state := chain.Current()
		// Look up the HTTP request the walker should fire for this
		// state from the matrix's data-driven Requests map. The
		// schema requires each non-terminal state to declare
		// `request: METHOD path`; states without an entry are silent.
		// See validation/markov/<refapp>.yaml.
		if req, ok := m.Requests[state]; ok {
			status := doMarkovRequest(ctx, hc, req.Method, base+req.Path, req.Expect5xx, tally)
			if status == 401 && hasLogin {
				// Session likely expired — re-login and keep walking.
				// The next request will pick up the fresh cookie.
				_ = walkerLogin(ctx, hc, base, m.Login, username, password)
			}
		}
		if _, ok := chain.Next(); !ok {
			// Terminal state; reset and continue. Soak workloads are
			// unbounded.
			chain.Reset()
		}
	}
}

// walkerLogin fires the matrix's configured login request with the
// given username + password (JSON body). The refapp's policy for our
// auth_session_ratelimit refapp is "any non-empty (username, password)
// authenticates" — see registerRoutes — so this always succeeds when
// the server is healthy. Failure is silently swallowed; the walker's
// 401-retry path handles the case where a later request reveals auth
// is missing.
//
// The login request shape (method + path) comes from the matrix's
// top-level `login:` directive, so this function works for any refapp
// regardless of where login lives in its URL space.
//
// Response cookie is stored in hc.Jar; subsequent hc.Do() calls
// carry it automatically.
func walkerLogin(ctx context.Context, hc *http.Client, base string, login markov.Request, username, password string) error {
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	req, err := http.NewRequestWithContext(ctx, login.Method, base+login.Path,
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

// doMarkovRequest issues one HTTP request (method + url) and folds
// the result into tally. Errors are folded into requestsError, not
// returned — Tier 1's contract is "keep firing until ctx is done."
// Returns the response status code (0 on transport error) so the
// caller can react to session-expiry 401s with a re-login.
//
// POST/PUT/PATCH requests fire with an empty body for now. Body
// generation is the Tier 2 RESTler fuzzer's job; Tier 1's purpose
// here is volume + endpoint coverage, not payload variation.
func doMarkovRequest(ctx context.Context, hc *http.Client, method, url string, expect5xx bool, tally *tier1Tally) int {
	tally.requestsSent.Add(1)
	if method == "" {
		method = "GET"
	}
	req, err := http.NewRequestWithContext(ctx, method, url, nil)
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
	switch {
	case resp.StatusCode >= 500 && expect5xx:
		tally.requests5xxExpected.Add(1)
	case resp.StatusCode >= 500:
		tally.requests5xx.Add(1)
		// Peek the body for the refapp invariant marker. 5xx is rare on
		// a healthy run, so the extra read costs nothing in steady state.
		head := make([]byte, 512)
		n, _ := io.ReadFull(resp.Body, head)
		if bytes.Contains(head[:n], []byte(`"x-invariant"`)) {
			tally.invariantHits.Add(1)
		}
	case resp.StatusCode >= 400:
		tally.requests4xx.Add(1)
	default:
		tally.requests2xx.Add(1)
	}
	// Drain body — keep-alive requires it.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
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

		Requests5xxExpected: t.requests5xxExpected.Load(),
		InvariantHits:       t.invariantHits.Load(),
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
	if t.liveness != nil {
		s.Liveness = t.liveness.snapshot()
	}
	return s
}

// tier1TallySnapshot is the value-typed projection of tier1Tally
// (atomic counters loaded at one instant).
type tier1TallySnapshot struct {
	RequestsSent        int64               `json:"requests_sent"`
	Requests2xx         int64               `json:"requests_2xx"`
	Requests4xx         int64               `json:"requests_4xx"`
	Requests5xx         int64               `json:"requests_5xx"`
	RequestsError       int64               `json:"requests_error"`
	Requests5xxExpected int64               `json:"requests_5xx_expected"`
	InvariantHits       int64               `json:"invariant_hits"`
	Adversarial         adversarialSnapshot `json:"adversarial,omitempty"`
	H2CChurn            h2cSnapshot         `json:"h2c_churn,omitempty"`
	WSTorture           wsSnapshot          `json:"ws_torture,omitempty"`
	SSEKill             sseSnapshot         `json:"sse_kill,omitempty"`
	Liveness            livenessSnapshot    `json:"liveness,omitempty"`
}

// String formats a tally for the run summary log line.
func (s tier1TallySnapshot) String() string {
	var b bytes.Buffer
	fmt.Fprintf(&b, "sent=%d 2xx=%d 4xx=%d 5xx=%d err=%d",
		s.RequestsSent, s.Requests2xx, s.Requests4xx, s.Requests5xx, s.RequestsError)
	if s.Liveness.Crashed {
		fmt.Fprintf(&b, " CRASHED[%s]", s.Liveness.Reason())
	}
	return b.String()
}
