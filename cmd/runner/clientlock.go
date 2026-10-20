package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"github.com/goceleris/probatorium/report"
	"github.com/goceleris/probatorium/scenarios"
)

// The client lock (probatorium#442).
//
// BENCH_TARGET=both runs one runner process per arch, and both run on the same
// loadgen host (msa2-client) and drive it at the same time. In the 0829 run an
// x86 ws-large-echo pass that overlapped the arm64 run of the same row read a
// backlog clock of 1 s or more in 6 of 10 passes, against 0 of 42 for the rest.
// A cell of a heavy scenario (scenarios.ClientLoadOf) therefore holds a
// host-wide flock for the whole cell: saturation pass, rated sweep and every
// warmup. Two runner processes on the host conflict on it; so do two opens in
// one process, which is how the test exercises it. The kernel drops the lock
// when the holder dies, so a runner killed by its hang guard frees the peer.

// defaultClientLockPoll is how often a waiting runner retries. Waiting is a
// poll, not a blocking flock, so a SIGTERM or a cancelled run context ends it.
const defaultClientLockPoll = time.Second

// clientLock is the host-wide lock one runner takes around its heavy cells.
// The zero value, and a lock with an empty path, takes nothing.
type clientLock struct {
	path   string
	policy string // report.ExclusionHeavy | ExclusionAll | ExclusionOff
	poll   time.Duration
	log    io.Writer
}

// newClientLock validates the flags. An empty path means no lock (a single-arch
// run); a policy is required only with a path and defaults to heavy.
func newClientLock(path, policy string, log io.Writer) (*clientLock, error) {
	if path == "" {
		if policy != "" && policy != report.ExclusionOff {
			return nil, fmt.Errorf("-client-lock-policy %q needs -client-lock <path>", policy)
		}
		return &clientLock{log: log}, nil
	}
	switch policy {
	case "":
		policy = report.ExclusionHeavy
	case report.ExclusionHeavy, report.ExclusionAll, report.ExclusionOff:
	default:
		return nil, fmt.Errorf("-client-lock-policy %q: want heavy, all or off", policy)
	}
	return &clientLock{path: path, policy: policy, poll: defaultClientLockPoll, log: log}, nil
}

// needs reports whether a cell of scenario takes the lock under the policy.
func (l *clientLock) needs(scenario string) bool {
	if l == nil || l.path == "" {
		return false
	}
	switch l.policy {
	case report.ExclusionAll:
		return true
	case report.ExclusionHeavy:
		return scenarios.ClientLoadOf(scenario) == scenarios.LoadHeavy
	}
	return false
}

// acquire takes the lock for a cell of scenario when the policy asks for it
// and returns the function that releases it and the time spent waiting. A cell
// that needs no lock returns at once. It returns ctx's error when the run is
// cancelled while waiting.
func (l *clientLock) acquire(ctx context.Context, scenario string) (release func(), waited time.Duration, err error) {
	if !l.needs(scenario) {
		return func() {}, 0, nil
	}
	// Read-only: flock needs no write access, and a lock file left by another
	// user (the /tmp sticky bit forbids replacing it) must still be usable.
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDONLY, 0o666)
	if err != nil {
		return nil, 0, fmt.Errorf("client lock: open %s: %w", l.path, err)
	}
	start := time.Now()
	announced := false
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			_ = f.Close()
			return nil, 0, fmt.Errorf("client lock: flock %s: %w", l.path, err)
		}
		if !announced && l.log != nil {
			_, _ = fmt.Fprintf(l.log, "  client lock: %s waits for the other arch (a heavy cell holds %s)\n", scenario, l.path)
			announced = true
		}
		select {
		case <-ctx.Done():
			_ = f.Close()
			return nil, time.Since(start), ctx.Err()
		case <-time.After(l.poll):
		}
	}
	waited = time.Since(start)
	if announced && l.log != nil {
		_, _ = fmt.Fprintf(l.log, "  client lock: %s acquired after %s\n", scenario, waited.Round(time.Millisecond))
	}
	return func() {
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
	}, waited, nil
}
