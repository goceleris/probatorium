package validation

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// shrinkResult is one shrink-attempt outcome. Recorded in the
// shrink_log.json so postmortem can see the bisection trail.
type shrinkResult struct {
	Attempt    int           `json:"attempt"`
	Duration   time.Duration `json:"duration"`
	DurationNS int64         `json:"duration_ns"`
	Reproduced bool          `json:"reproduced"`
	ExitCode   int           `json:"exit_code"`
}

// shrinkCfg parameterises shrinkFailingSeed. Built from the outer
// orchestrator's Config so the shrinker can fork validator-replay
// itself rather than re-entering the orchestrator's full lifecycle.
type shrinkCfg struct {
	// ReplayBin is cmd/validator-replay's absolute path. Required;
	// the shrinker forks it per attempt.
	ReplayBin string
	// RefappBin is the same celeris-bin the orchestrator launches
	// per seed. Each shrink attempt brings up a fresh refapp so
	// state from the failing replay doesn't bleed in.
	RefappBin string
	// RefappListenAddr is the -bind value for the fresh refapp. Same
	// value the orchestrator passed in — typically "127.0.0.1:0" in
	// matrix mode, where the OS picks the port and the refapp announces
	// the real one on its "ready addr=" banner (attemptShrink reads it
	// back rather than assuming the port from this field).
	RefappListenAddr string
	// CelerisCommit is recorded in the per-attempt log.
	CelerisCommit string
	// OriginalDuration is the duration the failing replay used.
	// shrinkFailingSeed bisects DOWNWARD from here.
	OriginalDuration time.Duration
	// MaxAttempts caps the total shrink-step count. Zero defaults
	// to 8 (covers seed durations from 10m down to ~2s in halves).
	MaxAttempts int
	// MinDuration floors the bisection. Below this we declare
	// "shrunken enough" even if the bug still reproduces.
	// Zero defaults to 500ms.
	MinDuration time.Duration
}

// shrinkFailingSeed bisects the failing seed's run duration. The
// shrinker forks validator-replay at duration=N/2 of original; if
// it still fails the bug reproduces faster, recurse. If it passes,
// the bug needed the longer window — we've found the lower bound.
//
// Output: shrink_log.json records every attempt; shrink_minimal.json
// records the smallest duration that still reproduced + the seed
// value, so a triage cmd-line invocation
//
//	cmd/validator-replay -seed=<value> -duration=<minimal>
//
// always reproduces.
//
// This is a deliberately thin first slice — bisect only the
// duration, not the traffic-prefix or fault-event schedule. Those
// require validator-replay to accept finer-grained overrides; see
// the per-attempt design in #57.
//
// shrinkFailingSeed is best-effort: forking failures, ctx-cancel,
// or never reproducing at the original duration don't propagate as
// errors — the function records what it tried and returns. The
// orchestrator's incident dossier already has the original failing
// state, so a busted shrink doesn't lose evidence.
func shrinkFailingSeed(ctx context.Context, dir string, seed uint64, cfg shrinkCfg) error {
	if cfg.ReplayBin == "" || cfg.RefappBin == "" {
		// Best-effort: log the absence + return.
		return writePlainText(filepath.Join(dir, "shrink_log.txt"),
			"shrinker skipped: ReplayBin or RefappBin unset\n")
	}
	if cfg.MaxAttempts == 0 {
		cfg.MaxAttempts = 8
	}
	if cfg.MinDuration == 0 {
		cfg.MinDuration = 500 * time.Millisecond
	}
	if cfg.OriginalDuration < cfg.MinDuration {
		// Already shorter than the floor.
		return writePlainText(filepath.Join(dir, "shrink_log.txt"),
			fmt.Sprintf("shrinker skipped: original=%s < min=%s\n",
				cfg.OriginalDuration, cfg.MinDuration))
	}

	var attempts []shrinkResult
	var minimal *shrinkResult

	low := cfg.MinDuration
	high := cfg.OriginalDuration
	for i := 0; i < cfg.MaxAttempts; i++ {
		if ctx.Err() != nil {
			break
		}
		// Try the midpoint between low and high.
		mid := low + (high-low)/2
		if mid < cfg.MinDuration {
			mid = cfg.MinDuration
		}
		reproduced, exit, err := attemptShrink(ctx, seed, mid, cfg)
		if err != nil {
			// Driver / fork error — record and stop. We can't
			// tell repro vs not-repro from an infra flake.
			attempts = append(attempts, shrinkResult{
				Attempt: i, Duration: mid, DurationNS: mid.Nanoseconds(),
				ExitCode: -1,
			})
			break
		}
		attempts = append(attempts, shrinkResult{
			Attempt: i, Duration: mid, DurationNS: mid.Nanoseconds(),
			Reproduced: reproduced, ExitCode: exit,
		})
		if reproduced {
			// Bug reproduced at mid — try smaller next round.
			ms := attempts[len(attempts)-1]
			minimal = &ms
			high = mid
		} else {
			// Bug didn't reproduce — we need at least `mid` time.
			low = mid
		}
		if high-low < cfg.MinDuration {
			break
		}
	}

	logPath := filepath.Join(dir, "shrink_log.json")
	if err := writeJSONIndent(logPath, map[string]any{
		"seed":              fmt.Sprintf("0x%x", seed),
		"original_duration": cfg.OriginalDuration.String(),
		"min_duration":      cfg.MinDuration.String(),
		"max_attempts":      cfg.MaxAttempts,
		"attempts":          attempts,
	}); err != nil {
		return err
	}
	if minimal != nil {
		return writeJSONIndent(filepath.Join(dir, "shrink_minimal.json"),
			map[string]any{
				"seed":     fmt.Sprintf("0x%x", seed),
				"duration": minimal.Duration.String(),
				"reproduce_cmd": fmt.Sprintf(
					"validator-replay -seed=0x%x -duration=%s -celeris-pid=<pid> -celeris-port=<port>",
					seed, minimal.Duration),
			})
	}
	return writePlainText(filepath.Join(dir, "shrink_minimal.txt"),
		"could not shrink: bug did not reproduce at any duration <= original\n")
}

// attemptShrink runs validator-replay at the given duration against
// a fresh refapp. Returns (reproduced, exit_code, infra_error).
// reproduced = true means the replay exited non-zero (genuine seed
// fail). reproduced = false + nil err means clean pass.
func attemptShrink(ctx context.Context, seed uint64, dur time.Duration, cfg shrinkCfg) (bool, int, error) {
	// Bring up a fresh refapp. Local driver because the shrinker
	// runs on the same host as the orchestrator's handleIncident.
	// (Cross-host shrink lives on top of the SSH driver in a
	// follow-up.)
	refappCmd := exec.CommandContext(ctx, cfg.RefappBin, "-bind", cfg.RefappListenAddr)
	stdout, err := refappCmd.StdoutPipe()
	if err != nil {
		return false, 0, fmt.Errorf("refapp stdout pipe: %w", err)
	}
	if err := refappCmd.Start(); err != nil {
		return false, 0, fmt.Errorf("start refapp: %w", err)
	}
	defer func() {
		if refappCmd.Process != nil {
			_ = refappCmd.Process.Kill()
		}
		_ = refappCmd.Wait()
	}()

	// Read the refapp's "ready addr=" banner to learn its REAL bound
	// address. With "-bind :0" the OS picks the port, so we can't assume
	// it from RefappListenAddr — that's exactly how matrix mode dodges the
	// ephemeral-port allocation race. Bounded so a refapp that never binds
	// doesn't hang the (best-effort) shrink.
	readyAddr := waitReadyLine(stdout, 10*time.Second)
	if readyAddr == "" {
		return false, 0, fmt.Errorf("refapp did not announce ready addr")
	}

	args := []string{
		"-seed", fmt.Sprintf("0x%x", seed),
		"-celeris-pid", strconv.Itoa(refappCmd.Process.Pid),
		"-celeris-port", portFromAddr(readyAddr),
		"-duration", dur.String(),
	}
	if cfg.CelerisCommit != "" {
		args = append(args, "-commit", cfg.CelerisCommit)
	}
	replayCtx, cancel := context.WithTimeout(ctx, dur+5*time.Second)
	defer cancel()
	replay := exec.CommandContext(replayCtx, cfg.ReplayBin, args...)
	if err := replay.Run(); err == nil {
		return false, 0, nil
	} else if exitErr, ok := err.(*exec.ExitError); ok {
		return true, exitErr.ExitCode(), nil
	} else {
		return false, 0, fmt.Errorf("replay fork: %w", err)
	}
}

// waitReadyLine scans r for the refapp's "ready addr=<addr>" banner and
// returns the announced addr (host:port), or "" if it doesn't arrive
// within timeout (or the stream ends first). After matching it keeps
// draining r so the refapp never blocks writing stdout for the rest of
// the attempt; the goroutine exits when the pipe closes (refapp killed).
func waitReadyLine(r io.Reader, timeout time.Duration) string {
	ch := make(chan string, 1)
	go func() {
		sc := bufio.NewScanner(r)
		sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
		sent := false
		for sc.Scan() {
			if !sent && strings.HasPrefix(sc.Text(), "ready addr=") {
				ch <- strings.TrimSpace(strings.TrimPrefix(sc.Text(), "ready addr="))
				sent = true
			}
		}
		if !sent {
			ch <- ""
		}
	}()
	select {
	case addr := <-ch:
		return addr
	case <-time.After(timeout):
		return ""
	}
}

// portFromAddr extracts "8080" from "127.0.0.1:8080". On malformed
// input returns the input verbatim — caller's problem.
func portFromAddr(addr string) string {
	for i := len(addr) - 1; i >= 0; i-- {
		if addr[i] == ':' {
			return addr[i+1:]
		}
	}
	return addr
}

// writeJSONIndent writes v to path with 2-space indent. Used by the
// shrinker to keep the log human-readable.
func writeJSONIndent(path string, v any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	buf, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, buf, 0o644)
}
