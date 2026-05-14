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
}

// tier3TallySnapshot is the value-typed projection of tier3Tally.
type tier3TallySnapshot struct {
	SeedsAttempted int64 `json:"seeds_attempted"`
	SeedsPassed    int64 `json:"seeds_passed"`
	SeedsFailed    int64 `json:"seeds_failed"`
	SeedsErrored   int64 `json:"seeds_errored"`
}

// String formats the tally for the run summary log line.
func (s tier3TallySnapshot) String() string {
	return fmt.Sprintf("attempted=%d passed=%d failed=%d errored=%d",
		s.SeedsAttempted, s.SeedsPassed, s.SeedsFailed, s.SeedsErrored)
}

func (t *tier3Tally) snapshot() tier3TallySnapshot {
	return tier3TallySnapshot{
		SeedsAttempted: t.seedsAttempted.Load(),
		SeedsPassed:    t.seedsPassed.Load(),
		SeedsFailed:    t.seedsFailed.Load(),
		SeedsErrored:   t.seedsErrored.Load(),
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
	// RefappPID is the OS pid of the refapp that hosted this seed.
	// Recorded so the orchestrator's forensics path can sample /proc
	// + pprof from the same process the failing replay observed —
	// even if the refapp has since been SIGTERMed, the dossier
	// records the PID for postmortem cross-reference.
	RefappPID int
}

// driveTier3 walks the seed corpus until ctx is done. For each seed:
//  1. Start a fresh refapp via Driver.
//  2. Wait for `ready addr=` on its stderr+stdout.
//  3. Resolve the refapp's PID via Process.PID().
//  4. Fork validator-replay with -seed -celeris-pid -celeris-port
//     -duration. Wait up to PerSeedDuration.
//  5. Capture exit code + stderr.
//  6. SIGTERM the refapp; wait for it to exit cleanly (5s grace
//     handled by Driver implementations).
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
	if cfg.CelerisListenPort == 0 {
		cfg.CelerisListenPort = 8080
	}

	idx := 0
	for ctx.Err() == nil {
		seed := cfg.Seeds[idx%len(cfg.Seeds)]
		idx++

		res := replayOneSeed(ctx, cfg, seed)
		tally.seedsAttempted.Add(1)
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

// replayOneSeed runs the per-seed lifecycle. Errors during refapp
// boot are surfaced as ExitCode = -1 with the cause in Stderr.
func replayOneSeed(ctx context.Context, cfg tier3Config, seed corpus.Seed) tier3Result {
	started := time.Now()
	res := tier3Result{Seed: seed.Value, Tag: seed.Tag}

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
	defer func() { _ = proc.Signal(0xf) /* SIGTERM */ }()

	if err := waitForReady(seedCtx, proc, cfg.ReadyTimeout); err != nil {
		res.ExitCode = -1
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
		"-celeris-port", strconv.Itoa(cfg.CelerisListenPort),
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
