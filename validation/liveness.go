package validation

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/probatorium/validation/remote"
)

// livenessTally is the engine-agnostic crash oracle for a Tier 1 run.
//
// The per-request walkers cannot see a server death: to a Markov / WS / SSE
// walker, a process that has crashed is just "connection refused" or an
// EOF mid-stream — byte-for-byte indistinguishable from a deliberately
// RST'd torture connection, which the walkers (correctly) classify as a
// PASS. So a refapp that dies with `fatal error: sync: unlock of unlocked
// mutex` partway through a soak leaves NO walker-visible signal; the run
// just shows a spike in connection errors and finishes "green".
//
// livenessTally closes that hole. Two independent detectors feed it (see
// superviseStderr + watchProcessExit): the process exiting on its own while
// the orchestrator still wants it up, OR a Go-runtime crash signature
// appearing on the merged stdout+stderr stream. Either flips Crashed, which
// the orchestrator surfaces as the I-LIVENESS invariant — a hard validation
// failure with full forensics, regardless of which walker (if any) provoked
// the death. This is the general defence: it catches THIS bug and any future
// crash from any cause (panic escape, SIGSEGV, OOM kill, fatal throw).
type livenessTally struct {
	crashed  atomic.Bool
	exitCode atomic.Int64 // process exit status when it self-exited
	signal   atomic.Int64 // terminating signal number, 0 if none
	exited   atomic.Bool  // true once a real (non-ctx) process exit was observed

	// hung is the SECOND failure mode the per-request walkers are blind to: a
	// process that stays ALIVE but stops servicing requests (a deadlock / wedge
	// — e.g. the iouring auto+upg drainRecvBuffer hang, celeris#311). The crash
	// watchers above never fire (the process never exits); only an independent
	// health probe that times out while the process is up can see it.
	hung      atomic.Bool
	hangFails atomic.Int64 // consecutive health-probe timeouts that tripped it

	mu        sync.Mutex
	signature string // first crash-signature line scraped from stderr
	trace     string // bounded stderr tail captured around the crash
}

// recordHang marks the refapp as wedged: alive but unresponsive (health probe
// timed out fails consecutive times). Distinct from a crash — the process is
// still running, so it needs its own oracle (I-HANG).
func (l *livenessTally) recordHang(fails int) {
	l.hangFails.Store(int64(fails))
	l.hung.Store(true)
}

// recordExit marks an unexpected process exit (the watcher only calls this
// when ctx was still live, i.e. we had not asked the process to stop).
func (l *livenessTally) recordExit(code, sig int) {
	l.exitCode.Store(int64(code))
	l.signal.Store(int64(sig))
	l.exited.Store(true)
	l.crashed.Store(true)
}

// recordSignature stashes the first crash-signature line seen on stderr and
// flips Crashed. Idempotent — only the first signature is kept (the one that
// names the failure; later lines are stack frames).
func (l *livenessTally) recordSignature(line string) {
	l.mu.Lock()
	if l.signature == "" {
		l.signature = line
	}
	l.mu.Unlock()
	l.crashed.Store(true)
}

// attachTrace records the bounded crash trace captured after the signature
// line. Best-effort enrichment for the incident dossier.
func (l *livenessTally) attachTrace(trace string) {
	l.mu.Lock()
	if l.trace == "" {
		l.trace = trace
	}
	l.mu.Unlock()
}

func (l *livenessTally) snapshot() livenessSnapshot {
	l.mu.Lock()
	sig, tr := l.signature, l.trace
	l.mu.Unlock()
	return livenessSnapshot{
		Crashed:   l.crashed.Load(),
		Exited:    l.exited.Load(),
		ExitCode:  int(l.exitCode.Load()),
		Signal:    int(l.signal.Load()),
		Signature: sig,
		Trace:     tr,
		Hung:      l.hung.Load(),
		HangFails: int(l.hangFails.Load()),
	}
}

// livenessSnapshot is the value-typed projection of livenessTally.
type livenessSnapshot struct {
	// Crashed is the headline oracle: the refapp process did not survive
	// the run. Any true value is a hard validation failure.
	Crashed bool `json:"crashed"`
	// Exited is true when the death was observed as a process exit (vs.
	// only a stderr signature on a process that then hung).
	Exited bool `json:"exited,omitempty"`
	// ExitCode / Signal describe HOW it died when Exited. A Go fatal error
	// exits 2; an OOM/segfault shows Signal=9/11.
	ExitCode int `json:"exit_code,omitempty"`
	Signal   int `json:"signal,omitempty"`
	// Signature is the crash line scraped from stderr (e.g.
	// "fatal error: sync: unlock of unlocked mutex"), empty if the process
	// died without printing a recognised marker (e.g. SIGKILL/OOM).
	Signature string `json:"signature,omitempty"`
	// Trace is a bounded tail of the stderr output following the signature —
	// or, when the process died WITHOUT a recognised signature (e.g. a clean
	// os.Exit(1)), the tail of its final merged stdout+stderr, so the cause is
	// still recorded.
	Trace string `json:"trace,omitempty"`
	// Hung is the deadlock oracle: the process stayed alive but stopped
	// answering a periodic health probe. A true value is a hard failure.
	Hung bool `json:"hung,omitempty"`
	// HangFails is the consecutive probe-timeout count that tripped Hung.
	HangFails int `json:"hang_fails,omitempty"`
}

// Reason renders a one-line human-readable cause for the incident message.
func (s livenessSnapshot) Reason() string {
	switch {
	case s.Signature != "":
		if s.Exited {
			return fmt.Sprintf("%s (exit=%d signal=%d)", s.Signature, s.ExitCode, s.Signal)
		}
		return s.Signature
	case s.Exited && s.Signal != 0:
		return fmt.Sprintf("process killed by signal %d", s.Signal)
	case s.Exited:
		return fmt.Sprintf("process exited unexpectedly (code=%d)", s.ExitCode)
	case s.Hung:
		return fmt.Sprintf("process alive but unresponsive — %d consecutive health-probe timeouts (deadlock/wedge)", s.HangFails)
	default:
		return "process died mid-run"
	}
}

// Hang-probe tuning. The probe must tolerate genuine load spikes / GC pauses
// (a slow but live server) yet still catch a real wedge well before the run
// ends. hangThreshold consecutive timeouts of hangProbeTimeout each, spaced
// hangProbeInterval apart, means the server has to be unresponsive for ~tens of
// seconds — far beyond any load blip, far below a multi-minute deadlock. Vars
// (not consts) so tests can tighten them; production never changes them.
var (
	hangProbeInterval = 2 * time.Second
	hangProbeTimeout  = 5 * time.Second
	hangThreshold     = 4
)

// watchResponsiveness periodically probes the refapp's /healthz endpoint. A
// crash makes the probe fail FAST (connection refused) — handled by the crash
// watchers, and explicitly NOT counted here. A wedge makes the probe TIME OUT
// (the kernel accepts the connection but no worker ever answers — exactly the
// celeris#311 state of ~580 held connections serviced by nobody). hangThreshold
// consecutive timeouts while the process is alive flips Hung and cancels the
// run via onHang.
func watchResponsiveness(ctx context.Context, baseURL string, l *livenessTally, onHang func()) {
	url := strings.TrimRight(baseURL, "/") + "/healthz"
	client := &http.Client{Timeout: hangProbeTimeout}
	ticker := time.NewTicker(hangProbeInterval)
	defer ticker.Stop()
	fails := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
			if err != nil {
				return
			}
			resp, derr := client.Do(req)
			if derr == nil {
				_ = resp.Body.Close()
				fails = 0
				continue
			}
			// Only a TIMEOUT counts as a hang signal: the server accepted us
			// but never answered. A fast failure (connection refused = crash,
			// or ctx cancellation = clean shutdown) is someone else's job.
			if ctx.Err() != nil {
				return
			}
			if isProbeTimeout(derr) {
				fails++
			} else {
				fails = 0
			}
			if fails >= hangThreshold {
				l.recordHang(fails)
				onHang()
				return
			}
		}
	}
}

// isProbeTimeout reports whether a health-probe error is a timeout (server
// accepted but did not answer = wedge) rather than a fast refusal (= crash).
func isProbeTimeout(err error) bool {
	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return false
}

const (
	crashTraceMaxLines = 60
	crashTraceMaxBytes = 16 * 1024
	// refappTailMaxLines is how many trailing lines of the refapp's merged
	// stdout+stderr to retain when it dies WITHOUT a recognised crash
	// signature (e.g. a clean os.Exit(1) from its own log.Fatalf). The
	// signature scan only keeps a trace once a runtime crash marker matches, so
	// a clean exit previously recorded no output at all — this tail names the
	// cause (e.g. the refapp's "<app>: start: <err>" line).
	refappTailMaxLines = 80
)

// crashLineMarkers are line-start tokens the Go runtime ONLY emits on an
// UNRECOVERABLE failure — never normal application logging, and never the
// recovery middleware's recovered-panic log (which the engine handles and
// counts via I-PANIC, not a process death). Anchored at line start because
// the runtime prints these at column 0. "panic:" is deliberately ABSENT:
// a recovered panic may be logged with that prefix, and an UNrecovered one
// exits the process anyway — the exit watcher catches it without risking a
// false positive on a handled panic.
var crashLineMarkers = []string{
	"fatal error:", // runtime.fatalthrow: sync misuse, concurrent map write, stack overflow
	"[signal SIG",  // runtime signal preamble: SIGSEGV / SIGBUS / SIGABRT / SIGILL / SIGFPE
	"runtime: ",    // runtime distress: "runtime: out of memory", "runtime: goroutine stack exceeds"
}

func looksLikeCrash(line string) bool {
	t := strings.TrimSpace(line)
	for _, m := range crashLineMarkers {
		if strings.HasPrefix(t, m) {
			return true
		}
	}
	return false
}

// superviseStderr is the SINGLE owner of the refapp's merged stdout+stderr
// for the lifetime of a Tier 1 run. Owning it in one place is deliberate: a
// second concurrent reader on the same pipe would split bytes mid-line and
// corrupt both readers' view (the reason Tier 1 no longer also calls
// waitForReady — that spawned its own short-lived reader).
//
// It does three jobs from one scan loop:
//  1. ready detection — signals onReady when it sees the "ready addr=" banner;
//     if EOF arrives first, the refapp died before binding → onReadyFail with
//     the captured pre-ready output (so the dossier records WHY).
//  2. crash-signature scan — after ready, the first line matching a runtime
//     crash marker is recorded on the liveness tally and triggers onCrash.
//  3. unconditional drain — every post-ready line is consumed even when no
//     reader cares, so the refapp can never block writing a multi-megabyte
//     goroutine dump on its way down. A stalled write there would stop the
//     process from exiting and HIDE the crash from the exit watcher.
func superviseStderr(r io.Reader, l *livenessTally, onReady func(), onReadyFail func(error), onCrash func()) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	ready := false
	var preReady strings.Builder
	capturing := false
	var trace strings.Builder
	traceLines := 0
	// Rolling ring of the last refappTailMaxLines post-ready lines, so a death
	// with NO recognised crash signature still records its final output.
	ring := make([]string, refappTailMaxLines)
	ringLen, ringPos := 0, 0
	for sc.Scan() {
		line := sc.Text()
		if !ready {
			if strings.HasPrefix(line, "ready addr=") {
				ready = true
				onReady()
				continue
			}
			if preReady.Len() < refappOutputCapMax {
				preReady.WriteString(line)
				preReady.WriteByte('\n')
			}
			continue
		}
		ring[ringPos] = line
		ringPos = (ringPos + 1) % refappTailMaxLines
		if ringLen < refappTailMaxLines {
			ringLen++
		}
		if capturing {
			if traceLines < crashTraceMaxLines && trace.Len() < crashTraceMaxBytes {
				trace.WriteString(line)
				trace.WriteByte('\n')
				traceLines++
			}
			continue
		}
		if looksLikeCrash(line) {
			l.recordSignature(strings.TrimSpace(line))
			trace.WriteString(line)
			trace.WriteByte('\n')
			traceLines++
			capturing = true
			onCrash()
		}
	}
	// Scan loop ended: pipe closed (process exited) or scanner error.
	if !ready {
		err := io.EOF
		if serr := sc.Err(); serr != nil {
			err = serr
		}
		if preReady.Len() > 0 {
			err = fmt.Errorf("%w\n--- refapp stderr ---\n%s", err, preReady.String())
		}
		onReadyFail(err)
	}
	switch {
	case capturing:
		l.attachTrace(trace.String())
	case ready && ringLen > 0:
		// Died after ready with no recognised crash signature (e.g. a clean
		// os.Exit(1) from the refapp's own log.Fatalf). Attach the tail of the
		// merged stream so the incident records WHY instead of just "exit=1".
		start := 0
		if ringLen == refappTailMaxLines {
			start = ringPos
		}
		var tail strings.Builder
		tail.WriteString("--- refapp stdout+stderr tail (no crash signature) ---\n")
		for i := 0; i < ringLen; i++ {
			tail.WriteString(ring[(start+i)%refappTailMaxLines])
			tail.WriteByte('\n')
		}
		l.attachTrace(tail.String())
	}
}

// watchProcessExit is the authoritative liveness detector: it blocks on the
// process and flags a crash if the process exits on its own while ctx is
// still live (i.e. the orchestrator had NOT asked it to stop). This catches
// every death mode — fatal throw (exit 2), uncaught panic (exit 2), SIGKILL
// from an OOM, SIGSEGV — independent of whether anything was printed to
// stderr, so it works even for a silent kill. proc.Wait returns ctx.Err()
// when ctx is cancelled, so a clean orchestrator shutdown is never mistaken
// for a crash.
func watchProcessExit(ctx context.Context, proc remote.Process, l *livenessTally, onCrash func()) {
	res, err := proc.Wait(ctx)
	if err != nil || ctx.Err() != nil {
		return // ctx-cancelled clean shutdown, or a generic wait error
	}
	sig := 0
	if res.Signaled {
		sig = res.Signal
	}
	l.recordExit(res.ExitCode, sig)
	onCrash()
}
