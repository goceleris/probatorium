package validation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goceleris/probatorium/validation/corpus"
	"github.com/goceleris/probatorium/validation/remote"
)

// tier3Config parameterises a single Tier 3 (corpus replay) run.
// Mirror of tier1Config but per-seed: every seed gets its own fresh
// refapp, its own validator-replay subprocess, and its own slot in
// the per-seed result log.
type tier3Config struct {
	// Driver owns the refapp lifecycle. One Start / Stop cycle per
	// seed so the bug-scope is bounded to a single deterministic
	// (seed, commit, arch) tuple.
	Driver remote.Driver

	// RefappArgs is the argv passed to Driver.Start. Same convention
	// as Tier 1.
	RefappArgs []string

	// ReplayBin is the path to the cmd/validator-replay binary. The
	// per-seed loop forks this with -seed=<hex> -celeris-pid=<pid>.
	// On a clean exit (rc=0) the seed passed; non-zero is an
	// incident-shaped failure.
	ReplayBin string

	// CelerisListenPort is the port the refapp binds + that
	// validator-replay passes via -celeris-port. Caller pins this in
	// lockstep with RefappArgs.
	CelerisListenPort int

	// CelerisCommit is the celeris git SHA recorded in any incident
	// raised by a failing seed. Empty allowed (incident dossier just
	// omits the field).
	CelerisCommit string

	// PerSeedDuration is the time budget given to one validator-replay
	// invocation. Zero defaults to 15s — tight enough that a 200/h
	// cadence works (18s / seed average); long enough that the fault
	// schedule has time to inject + observe + recover.
	PerSeedDuration time.Duration

	// ReadyTimeout caps how long the per-seed refapp has to bind.
	// Zero defaults to 10s.
	ReadyTimeout time.Duration

	// RefappStopGrace is how long a seed's teardown waits for its refapp
	// to exit after SIGTERM before it sends SIGKILL (stopAndReapRefapp).
	// Zero defaults to 5s.
	RefappStopGrace time.Duration

	// Seeds is the corpus to walk. Caller filters / orders; Tier 3
	// walks the slice in order and loops back to index 0 on
	// exhaustion (so 6h runs against a 100-seed corpus replay 12
	// rounds, not 100 cells).
	Seeds []corpus.Seed

	// SnapshotPath, when non-empty, names the path the tier writes the
	// current tally snapshot to after every seed completion (so the
	// disk view stays at most one seed-cycle behind in-memory state).
	// Letting long-running soaks surface mid-run progress without
	// having to wait for the orchestrator's final flush. Best-effort:
	// a write failure is silently ignored — the in-memory tally is
	// authoritative.
	SnapshotPath string

	// SeedLogDir, when non-empty, is the directory under which the
	// tier writes one JSON record per non-zero-exit seed
	// (`<exit-class>-seed-<value>-<ts>.json`).
	//
	// Closes a gap the 3-day soak exposed: the non-blocking send to
	// the `results` channel below silently drops failures when the
	// consumer goroutine has exited (ctx-cancel race) or the buffer
	// is saturated. The counter still increments, so the run reports
	// "N errored seeds" but no on-disk dossier exists for postmortem.
	// SeedLogDir writes the record SYNCHRONOUSLY — independent of
	// channel state — so the durable record always lands.
	SeedLogDir string
}

// tier3Tally accumulates per-seed result counts.
type tier3Tally struct {
	seedsAttempted atomic.Int64
	seedsPassed    atomic.Int64
	seedsFailed    atomic.Int64
	seedsErrored   atomic.Int64

	// What the refapp teardowns of the attempted seeds cost: how many
	// refapps had to be SIGKILLed, and the total and the longest time from
	// SIGTERM to reap (probatorium#415).
	seedsTeardownKilled atomic.Int64
	teardownTotalNS     atomic.Int64
	teardownMaxNS       atomic.Int64
}

// tier3TallySnapshot is the value-typed projection of tier3Tally.
type tier3TallySnapshot struct {
	SeedsAttempted int64 `json:"seeds_attempted"`
	SeedsPassed    int64 `json:"seeds_passed"`
	SeedsFailed    int64 `json:"seeds_failed"`
	SeedsErrored   int64 `json:"seeds_errored"`

	// SeedsTeardownKilled counts the attempted seeds whose refapp did not
	// exit within the stop grace of its SIGTERM and died of SIGKILL.
	// TeardownTotalNS and TeardownMaxNS are the sum and the longest of
	// their SIGTERM-to-reap times. All three reach the artifacts through
	// each cell's tier3_tally.json; they are recorded, not gated.
	SeedsTeardownKilled int64 `json:"seeds_teardown_killed"`
	TeardownTotalNS     int64 `json:"teardown_total_ns"`
	TeardownMaxNS       int64 `json:"teardown_max_ns"`
}

// String formats the tally for the run summary log line.
func (s tier3TallySnapshot) String() string {
	return fmt.Sprintf("attempted=%d passed=%d failed=%d errored=%d teardown_killed=%d teardown_total=%s teardown_max=%s",
		s.SeedsAttempted, s.SeedsPassed, s.SeedsFailed, s.SeedsErrored,
		s.SeedsTeardownKilled, time.Duration(s.TeardownTotalNS), time.Duration(s.TeardownMaxNS))
}

func (t *tier3Tally) snapshot() tier3TallySnapshot {
	return tier3TallySnapshot{
		SeedsAttempted:      t.seedsAttempted.Load(),
		SeedsPassed:         t.seedsPassed.Load(),
		SeedsFailed:         t.seedsFailed.Load(),
		SeedsErrored:        t.seedsErrored.Load(),
		SeedsTeardownKilled: t.seedsTeardownKilled.Load(),
		TeardownTotalNS:     t.teardownTotalNS.Load(),
		TeardownMaxNS:       t.teardownMaxNS.Load(),
	}
}

// addTeardown records one attempted seed's refapp teardown.
func (t *tier3Tally) addTeardown(d time.Duration, killed bool) {
	if killed {
		t.seedsTeardownKilled.Add(1)
	}
	t.teardownTotalNS.Add(int64(d))
	for {
		cur := t.teardownMaxNS.Load()
		if int64(d) <= cur || t.teardownMaxNS.CompareAndSwap(cur, int64(d)) {
			return
		}
	}
}

// tier3Result is one per-seed outcome. Fed to the orchestrator's
// incident channel for failing seeds.
type tier3Result struct {
	Seed     uint64
	Tag      string
	ExitCode int
	Stderr   string
	Stdout   string
	Duration time.Duration
	// RefappPID is the OS pid of the refapp that hosted this seed. By
	// the time the result is emitted that refapp has been stopped and
	// reaped (stopAndReapRefapp), so nothing can be sampled from it any
	// more and the PID may already belong to another process: it is a
	// cross-reference for the dossier, not a live target.
	RefappPID int
	// TeardownDuration is the refapp teardown's SIGTERM-to-reap time, and
	// TeardownKilled whether the refapp died of SIGKILL rather than
	// exiting on the SIGTERM. Zero when no refapp was started.
	TeardownDuration time.Duration
	TeardownKilled   bool
}

// driveTier3 walks the seed corpus until ctx is done. For each seed:
//  1. Start a fresh refapp via Driver.
//  2. Wait for `ready addr=` on its stderr+stdout.
//  3. Resolve the refapp's PID via Process.PID().
//  4. Fork validator-replay with -seed -celeris-pid -celeris-port
//     -duration. Wait up to PerSeedDuration.
//  5. Capture exit code + stderr.
//  6. SIGTERM the refapp and reap it: it gets RefappStopGrace to
//     exit cleanly, then SIGKILL (stopAndReapRefapp).
//  7. Emit one tier3Result on the results channel.
//
// Returns when ctx is cancelled. The returned tally snapshot is the
// final state across every seed visited.
func driveTier3(ctx context.Context, cfg tier3Config, results chan<- tier3Result) (tier3TallySnapshot, error) {
	tally := &tier3Tally{}
	if cfg.Driver == nil {
		return tally.snapshot(), errors.New("tier3: nil Driver")
	}
	if cfg.ReplayBin == "" {
		return tally.snapshot(), errors.New("tier3: ReplayBin empty")
	}
	if len(cfg.Seeds) == 0 {
		return tally.snapshot(), errors.New("tier3: empty seeds")
	}
	if cfg.PerSeedDuration <= 0 {
		cfg.PerSeedDuration = 15 * time.Second
	}
	if cfg.ReadyTimeout <= 0 {
		cfg.ReadyTimeout = 10 * time.Second
	}
	if cfg.RefappStopGrace <= 0 {
		cfg.RefappStopGrace = defaultRefappStopGrace
	}
	if cfg.CelerisListenPort == 0 {
		cfg.CelerisListenPort = 8080
	}

	idx := 0
	for ctx.Err() == nil {
		seed := cfg.Seeds[idx%len(cfg.Seeds)]
		idx++

		res := replayOneSeed(ctx, cfg, seed)
		// If the parent ctx cancelled DURING the replay (run deadline
		// hit, SIGTERM, etc.), the replay's exec.CommandContext
		// SIGKILLs the validator-replay subprocess → ExitCode=-1 +
		// stderr="signal: killed". That's not an infra flake on our
		// side; it's normal end-of-run shutdown catching a seed mid-
		// flight. Don't count this as an attempt — exit the loop.
		//
		// Without this guard, every soak ended with `seedsErrored=1`
		// PER ARCH (whichever seed happened to be in flight when the
		// duration deadline tripped). The 12h soak's two "errored"
		// seeds (amd64 0x1a h1-bare-lf-in-header @ 3.7s, arm64 0x17
		// h1-oversize-uri @ 9.4s — both well under the 13s budget)
		// were exactly this artifact.
		if ctx.Err() != nil && res.ExitCode < 0 {
			return tally.snapshot(), nil
		}
		tally.seedsAttempted.Add(1)
		tally.addTeardown(res.TeardownDuration, res.TeardownKilled)
		exitClass := "passed"
		switch {
		case res.ExitCode == 0:
			tally.seedsPassed.Add(1)
		case res.ExitCode < 0:
			// Negative exit code = driver / fork error (not a seed
			// failure). Count separately so postmortem can tell
			// infra flake from genuine seed-found bug.
			tally.seedsErrored.Add(1)
			exitClass = "errored"
		default:
			tally.seedsFailed.Add(1)
			exitClass = "failed"
		}

		// Durable per-failure log — written synchronously so the
		// on-disk record survives the orchestrator's channel-state
		// races at ctx-cancel. Closed the 3-day-soak gap where
		// seedsErrored counter ticked but no dossier existed on disk.
		if exitClass != "passed" && cfg.SeedLogDir != "" {
			writeSeedFailureLog(cfg.SeedLogDir, exitClass, res)
		}

		// Non-blocking send — if the orchestrator's incident pipeline
		// is full or absent (nil channel) we drop the result. The
		// tally still counts it, AND the seed log above durably
		// records non-zero exits regardless of channel state.
		if results != nil {
			select {
			case results <- res:
			default:
			}
		}

		// Backoff on infra-flake to avoid pathological spin. Real fork
		// or SSH failures can fire at ~kHz; without this the loop burns
		// CPU and floods seedsErrored at thousands per second. 100ms is
		// fast enough to recover quickly when the issue clears, slow
		// enough that 12s of fork contention doesn't bury the tally.
		if res.ExitCode < 0 {
			select {
			case <-ctx.Done():
			case <-time.After(100 * time.Millisecond):
			}
		}

		// Best-effort snapshot to disk for mid-run progress
		// monitoring. Writes after every seed (or seed-error) so the
		// view stays at most one seed-cycle behind. Failure is
		// silent; in-memory tally remains authoritative.
		if cfg.SnapshotPath != "" {
			if data, err := json.MarshalIndent(tally.snapshot(), "", "  "); err == nil {
				_ = os.WriteFile(cfg.SnapshotPath, data, 0o644)
			}
		}
	}
	return tally.snapshot(), nil
}

// defaultRefappStopGrace is tier3Config.RefappStopGrace's default.
const defaultRefappStopGrace = 5 * time.Second

// refappTeardown is what stopping and reaping one seed's refapp cost.
type refappTeardown struct {
	// Duration runs from just before the SIGTERM to the reap, or to the
	// error.
	Duration time.Duration
	// Killed: the refapp died of SIGKILL rather than exiting on the
	// SIGTERM, whether our timer sent it or the per-seed context's kill
	// got there first (see stopAndReapRefapp). It is read from the exit
	// status, so it says how the refapp died, not which signals were sent.
	Killed bool
}

// stopAndReapRefapp ends one seed's refapp and reaps it. SIGTERM alone is not
// enough: an exited child stays in the process table as a zombie until its
// parent Waits for it, so a loop that only signals leaves one zombie per seed
// for as long as the validator runs. `go test ./validation/...` built up
// ~1,100 of them in about 60 s, enough to exhaust a 2,666-process per-user
// limit (probatorium#415), and the cluster's validator runs this same loop
// with the local driver.
//
// It Waits once, with a bound of twice the grace, and escalates to SIGKILL
// from a timer at the grace. One Wait call is all a Process has to support: a
// refapp that ignores SIGTERM is killed at the grace and reaped inside the
// same Wait. The error is that Wait's: the refapp outlived SIGKILL for the
// whole bound, or the driver could not observe its exit.
//
// The grace is an upper bound, not a promise. The local driver started the
// refapp under the per-seed context, which SIGKILLs it the moment that
// context ends, so the refapp really gets min(grace, time left on the seed's
// context): none at all when the run itself is ending. Killed covers both
// routes, because it reads how the refapp died.
//
// A refapp whose PID the driver never learned (the SSH driver's pidfile read
// failed) cannot be signalled at all; that is reported at once rather than
// after a Wait that nothing would ever end.
func stopAndReapRefapp(proc remote.Process, grace time.Duration) (refappTeardown, error) {
	if grace <= 0 {
		grace = defaultRefappStopGrace
	}
	start := time.Now()
	if proc.PID() <= 0 {
		return refappTeardown{}, errors.New("tier3: stop refapp: its pid is unknown, so it cannot be signalled")
	}
	_ = proc.Signal(int(syscall.SIGTERM))
	kill := time.AfterFunc(grace, func() { _ = proc.Signal(int(syscall.SIGKILL)) })
	ctx, cancel := context.WithTimeout(context.Background(), 2*grace)
	defer cancel()
	wr, err := proc.Wait(ctx)
	kill.Stop()
	td := refappTeardown{
		Duration: time.Since(start),
		Killed:   wr.Signaled && wr.Signal == int(syscall.SIGKILL),
	}
	if err != nil {
		return td, fmt.Errorf("tier3: stop refapp pid %d: %w", proc.PID(), err)
	}
	return td, nil
}

// replayOneSeed runs the per-seed lifecycle. Errors during refapp
// boot are surfaced as ExitCode = -1 with the cause in Stderr.
func replayOneSeed(ctx context.Context, cfg tier3Config, seed corpus.Seed) (res tier3Result) {
	started := time.Now()
	res = tier3Result{Seed: seed.Value, Tag: seed.Tag}

	// Per-seed context bounds the whole boot+replay+teardown cycle so
	// a stuck refapp doesn't stall the whole soak.
	seedCtx, cancel := context.WithTimeout(ctx, cfg.PerSeedDuration+cfg.ReadyTimeout+5*time.Second)
	defer cancel()

	proc, err := cfg.Driver.Start(seedCtx, cfg.RefappArgs)
	if err != nil {
		res.ExitCode = -1
		res.Stderr = fmt.Sprintf("tier3: start refapp: %v", err)
		res.Duration = time.Since(started)
		return res
	}
	defer func() {
		td, err := stopAndReapRefapp(proc, cfg.RefappStopGrace)
		res.TeardownDuration = td.Duration
		res.TeardownKilled = td.Killed
		if err == nil {
			return
		}
		// A refapp that could not be stopped is a driver failure, so a seed
		// that otherwise passed is reported as errored (T3-DRIVE) rather than
		// passed. A seed that already failed or errored keeps its exit code:
		// a teardown problem must never hide what the replay found.
		if res.ExitCode == 0 {
			res.ExitCode = -1
		}
		if res.Stderr != "" {
			res.Stderr += "\n"
		}
		res.Stderr += err.Error()
	}()

	readyAddr, err := waitForReady(seedCtx, proc, cfg.ReadyTimeout)
	if err != nil {
		res.ExitCode = -1
		// waitForReady already embeds any captured refapp stderr lines
		// in the returned error string (see tier1.go), so the dossier
		// records WHY the refapp died, not just THAT it did.
		res.Stderr = fmt.Sprintf("tier3: refapp not ready: %v", err)
		res.Duration = time.Since(started)
		return res
	}

	// Refapp is bound; record its PID so the orchestrator's
	// forensics layer can target it on a non-zero replay exit.
	res.RefappPID = proc.PID()

	// Fork validator-replay against the now-bound refapp. We use
	// exec.CommandContext directly here — the validator-replay binary
	// is a probatorium-owned tool we always run locally, never via
	// remote.Driver. (Even when the orchestrator drives celeris via
	// SSH, validator-replay runs on the same host as the refapp so
	// its fault-injection commands hit the right /proc namespace.)
	//
	// Two timeouts in play, both critical:
	//
	//   - `replayInternalDuration` is what we pass to the replay
	//     binary's `-duration` flag. The replay's own context is
	//     bounded by this; fault.Run returns cleanly when it expires.
	//   - the exec.CommandContext deadline is replayInternalDuration
	//     + 2s grace. SIGKILL is only delivered if the replay refuses
	//     to exit within the grace window — which would itself be a
	//     bug worth surfacing (T3-DRIVE), not the per-seed timing
	//     race we used to hit.
	//
	// WaitDelay (Go 1.20+) tightens the grace: stdout/stderr pipes are
	// forcibly closed after the delay even if the process is stuck.
	pidStr := strconv.Itoa(res.RefappPID)
	// The refapp announced its real bound port on the ready banner; with a
	// "-bind :0" launch that's the OS-chosen port the replay's fault
	// injection must target. Fall back to the configured port only if the
	// banner carried no parseable port (shouldn't happen) — which also
	// fixes the prior default-8080 mismatch in matrix mode, where the
	// refapp bound an ephemeral port but the replay was told 8080.
	celerisPort := portFromAddr(readyAddr)
	if celerisPort == "" || celerisPort == readyAddr {
		celerisPort = strconv.Itoa(cfg.CelerisListenPort)
	}
	const replayGrace = 2 * time.Second
	replayInternalDuration := cfg.PerSeedDuration
	if replayInternalDuration > replayGrace {
		replayInternalDuration -= replayGrace
	}
	replayCtx, replayCancel := context.WithTimeout(seedCtx,
		replayInternalDuration+replayGrace)
	defer replayCancel()
	args := []string{
		"-seed", fmt.Sprintf("0x%x", seed.Value),
		"-celeris-pid", pidStr,
		"-celeris-port", celerisPort,
		"-duration", replayInternalDuration.String(),
	}
	if cfg.CelerisCommit != "" {
		args = append(args, "-commit", cfg.CelerisCommit)
	}
	cmd := exec.CommandContext(replayCtx, cfg.ReplayBin, args...)
	cmd.WaitDelay = replayGrace
	out, err := cmd.CombinedOutput()
	res.Duration = time.Since(started)
	res.Stdout = string(out)
	if err == nil {
		res.ExitCode = 0
		return res
	}
	if exitErr, ok := err.(*exec.ExitError); ok {
		res.ExitCode = exitErr.ExitCode()
		res.Stderr = err.Error()
		return res
	}
	// Couldn't fork the binary, etc. — surface as infra error.
	res.ExitCode = -1
	res.Stderr = fmt.Sprintf("tier3: exec %s: %v", cfg.ReplayBin, err)
	return res
}

// _ keeps the io import live for future per-seed stdout tee work;
// drop when that goroutine pipe stitching lands.
var _ io.Reader = (*staticReader)(nil)

// staticReader is a placeholder for future per-seed log capture
// where the orchestrator wants to tee validator-replay's stdout to
// the run directory as it streams (instead of CombinedOutput's
// after-the-fact buffer). Lands separately in #57 (forensics).
type staticReader struct{ b []byte }

func (r *staticReader) Read(p []byte) (int, error) {
	if len(r.b) == 0 {
		return 0, io.EOF
	}
	n := copy(p, r.b)
	r.b = r.b[n:]
	return n, nil
}

// Ensure the JSON encoder doesn't bake in time.Time defaults for
// tier3Result Duration — keep it a plain int64-as-nanoseconds for
// straightforward downstream parsing.
var _ json.Marshaler = (*durationOnlyResult)(nil)

// durationOnlyResult is the JSON-emitted shape of tier3Result. We
// use it for the per-seed log records so the file is grep-friendly.
type durationOnlyResult struct {
	Seed       string `json:"seed"`
	Tag        string `json:"tag,omitempty"`
	ExitCode   int    `json:"exit_code"`
	DurationNS int64  `json:"duration_ns"`
	Stderr     string `json:"stderr,omitempty"`
}

func (d *durationOnlyResult) MarshalJSON() ([]byte, error) {
	type alias durationOnlyResult
	return json.Marshal((*alias)(d))
}

func toJSON(r tier3Result) durationOnlyResult {
	return durationOnlyResult{
		Seed:       fmt.Sprintf("0x%x", r.Seed),
		Tag:        r.Tag,
		ExitCode:   r.ExitCode,
		DurationNS: r.Duration.Nanoseconds(),
		Stderr:     r.Stderr,
	}
}

// writeSeedFailureLog persists one tier3Result to a per-seed JSON
// file under dir. File name is `<class>-seed-<hex>-<ns>.json` —
// class is "errored" or "failed", <hex> is the seed value, <ns> is
// the unix nanos timestamp (so duplicates from corpus loops don't
// overwrite each other).
//
// Best-effort: any failure (MkdirAll, WriteFile) is silently
// ignored. In-memory tally + the results channel remain the
// authoritative violation count; this file is the durable
// postmortem record.
func writeSeedFailureLog(dir, class string, r tier3Result) {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return
	}
	rec := struct {
		Class      string `json:"class"`
		Seed       string `json:"seed"`
		Tag        string `json:"tag,omitempty"`
		ExitCode   int    `json:"exit_code"`
		DurationNS int64  `json:"duration_ns"`
		RefappPID  int    `json:"refapp_pid,omitempty"`
		TeardownNS int64  `json:"teardown_ns,omitempty"`
		Killed     bool   `json:"teardown_killed,omitempty"`
		Stderr     string `json:"stderr,omitempty"`
		Stdout     string `json:"stdout,omitempty"`
		LoggedAt   string `json:"logged_at"`
	}{
		Class:      class,
		Seed:       fmt.Sprintf("0x%x", r.Seed),
		Tag:        r.Tag,
		ExitCode:   r.ExitCode,
		DurationNS: r.Duration.Nanoseconds(),
		RefappPID:  r.RefappPID,
		TeardownNS: r.TeardownDuration.Nanoseconds(),
		Killed:     r.TeardownKilled,
		Stderr:     truncateAt(r.Stderr, 4096),
		Stdout:     truncateAt(r.Stdout, 4096),
		LoggedAt:   time.Now().UTC().Format(time.RFC3339Nano),
	}
	data, err := json.MarshalIndent(rec, "", "  ")
	if err != nil {
		return
	}
	name := fmt.Sprintf("%s-seed-0x%x-%d.json", class, r.Seed, time.Now().UnixNano())
	_ = os.WriteFile(filepath.Join(dir, name), data, 0o644)
}

// truncateAt caps s to maxLen bytes. Used to bound seed-failure log
// records — Stdout/Stderr from a wedged refapp can otherwise grow
// unbounded. A truncation marker keeps the file diff-friendly.
func truncateAt(s string, maxLen int) string {
	if len(s) <= maxLen {
		return s
	}
	return s[:maxLen] + "...(truncated)"
}
