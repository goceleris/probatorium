package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
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
//
// The two arches must not be able to stall each other for longer than a cell.
// A heavy cell wedged in a blocked read (loadgen's workers are not
// ctx-cancellable, the v3.8 failure the hang guard exists for) would keep the
// lock until its own guard kills the runner, hours into a column, and the peer
// would wait behind it and be killed at about the same moment: one arch's bad
// SUT would cost the other arch its column. Two limits prevent that. The
// holder releases the lock itself after maxHold, the longest a cell can run
// when its own contexts work (its saturation pass plus every rated pass, each
// at its context timeout); a wedged cell has driven no load since its context
// fired. And a waiter gives up after maxWait, a little more than maxHold, and
// fails the cell as "client-lock: ..." instead of waiting out a holder that is
// not a runner of this harness. Neither limit touches what the checker sees:
// it reads the passes' own timestamps, so a cell that really did overlap its
// peer is still reported (report.CheckArchOverlap).
//
// The lock is released when a cell's load ends, before the runner's 5 s
// cooldown: the previous heavy cell's sockets are still in TIME_WAIT on the
// client when the peer's heavy cell starts. They are connections to the
// other arch's SUT, so they share neither the 4-tuple space nor a peer address
// with the next cell's, and the planner models the release at the same point
// (interleave/archplan.go); the cost is the kernel's work to close them, not a
// port shortage.

// defaultClientLockPoll is how often a waiting runner retries. Waiting is a
// poll, not a blocking flock, so a SIGTERM or a cancelled run context ends it.
const defaultClientLockPoll = time.Second

// reprobeAfterLockWait is the wait after which executeCell probes the SUT
// again before it drives load.
var reprobeAfterLockWait = 5 * time.Second

// clientLockLimits are the hold and wait limits of the lock (see the file
// comment): the hold limit is the longest a cell can run while its contexts
// work, which is its saturation pass and every rated pass at their context
// timeouts (executeCell, runRatedSweep) plus a minute for the probes around
// them; the wait limit is that plus two minutes, so a waiter outlasts a
// holder's watchdog.
func (c Config) clientLockLimits() (maxHold, maxWait time.Duration) {
	maxHold = 5*c.Warmup + 2*c.Duration + 60*time.Second
	if c.RatedMode {
		maxHold += time.Duration(len(c.RatedFractions)) * (5*c.Warmup + 2*c.RatedDuration + 60*time.Second)
	}
	maxHold += time.Minute
	return maxHold, maxHold + 2*time.Minute
}

// clientLock is the host-wide lock one runner takes around its heavy cells.
// The zero value, and a lock with an empty path, takes nothing.
type clientLock struct {
	path   string
	policy string // report.ExclusionHeavy | ExclusionAll | ExclusionOff
	poll   time.Duration
	// maxWait bounds how long acquire waits for a holder; 0 = until the run
	// context ends (tests). maxHold is how long a holder keeps the lock before
	// the runner releases it itself; 0 = never. See the file comment.
	maxWait, maxHold time.Duration
	log              io.Writer
}

// errClientLockWait is the error acquire returns when it gave up on maxWait.
var errClientLockWait = errors.New("client lock wait limit exceeded")

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

// maxInodeRetries bounds how often acquire re-opens a lock file that was
// replaced under it.
const maxInodeRetries = 5

// open opens the lock file and takes the flock once, without blocking. It
// returns the file, whether the lock is held, and any error other than "held
// by someone else".
//
// Read-only: flock needs no write access, and a lock file left by another
// user (the /tmp sticky bit forbids replacing it) must still be usable.
// O_NOFOLLOW: a symlink planted at the path would send the lock elsewhere.
func (l *clientLock) open() (*os.File, bool, error) {
	f, err := os.OpenFile(l.path, os.O_CREATE|os.O_RDONLY|syscall.O_NOFOLLOW, 0o666)
	if err != nil {
		return nil, false, fmt.Errorf("client lock: open %s: %w", l.path, err)
	}
	held, err := tryFlock(f)
	if err != nil {
		_ = f.Close()
		return nil, false, fmt.Errorf("client lock: flock %s: %w", l.path, err)
	}
	return f, held, nil
}

// tryFlock takes the exclusive flock without blocking: held=false with a nil
// error means another open file description holds it.
func tryFlock(f *os.File) (held bool, err error) {
	err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
	switch {
	case err == nil:
		return true, nil
	case errors.Is(err, syscall.EWOULDBLOCK), errors.Is(err, syscall.EAGAIN), errors.Is(err, syscall.EINTR):
		return false, nil
	}
	return false, err
}

// sameFile reports whether the path still names the inode f is open on. A tmp
// cleaner that unlinks the lock file between two opens leaves a runner holding
// a lock on an inode nobody else will open.
func (l *clientLock) sameFile(f *os.File) bool {
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	pi, err := os.Stat(l.path)
	return err == nil && os.SameFile(fi, pi)
}

// acquire takes the lock for a cell of scenario when the policy asks for it
// and returns the function that releases it and the time spent waiting. A cell
// that needs no lock, and a lock taken at the first attempt, return a wait of
// exactly 0. It returns ctx's error when the run is cancelled while waiting and
// errClientLockWait (wrapped) when maxWait ran out. The returned release is
// safe to call more than once and from the maxHold watchdog.
func (l *clientLock) acquire(ctx context.Context, scenario string) (release func(), waited time.Duration, err error) {
	if !l.needs(scenario) {
		return func() {}, 0, nil
	}
	start := time.Now()
	contended := false
	announced := false
	var f *os.File
	for retries := 0; ; retries++ {
		var held bool
		f, held, err = l.open()
		if err != nil {
			return nil, 0, err
		}
		for !held {
			contended = true
			if !announced && l.log != nil {
				_, _ = fmt.Fprintf(l.log, "  client lock: %s waits for the other arch (a heavy cell holds %s)\n", scenario, l.path)
				announced = true
			}
			if l.maxWait > 0 && time.Since(start) >= l.maxWait {
				_ = f.Close()
				return nil, time.Since(start), fmt.Errorf("client lock: %s waited %s for %s and the other runner still holds it (limit %s): %w",
					scenario, time.Since(start).Round(time.Second), l.path, l.maxWait, errClientLockWait)
			}
			// Waiting is a poll on one open file, not a blocking flock, so a
			// SIGTERM or a cancelled run context ends it.
			select {
			case <-ctx.Done():
				_ = f.Close()
				return nil, time.Since(start), ctx.Err()
			case <-time.After(l.poll):
			}
			if held, err = tryFlock(f); err != nil {
				_ = f.Close()
				return nil, 0, fmt.Errorf("client lock: flock %s: %w", l.path, err)
			}
		}
		if l.sameFile(f) {
			break
		}
		// The file was replaced or unlinked while we waited: this lock guards
		// nothing. Drop it and lock the file the path names now.
		_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
		_ = f.Close()
		if retries >= maxInodeRetries {
			return nil, 0, fmt.Errorf("client lock: %s was replaced %d times while it was being locked", l.path, retries+1)
		}
	}
	if contended {
		waited = time.Since(start)
		if announced && l.log != nil {
			_, _ = fmt.Fprintf(l.log, "  client lock: %s acquired after %s\n", scenario, waited.Round(time.Millisecond))
		}
	}
	// Keep the file's age fresh for a host that age-cleans /tmp: the lock file
	// is never written, so its times would otherwise date from the first run.
	now := time.Now()
	_ = os.Chtimes(l.path, now, now)

	var once sync.Once
	unlock := func() {
		once.Do(func() {
			_ = syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
			_ = f.Close()
		})
	}
	release = unlock
	if l.maxHold > 0 {
		hold := l.maxHold
		t := time.AfterFunc(hold, func() {
			if l.log != nil {
				_, _ = fmt.Fprintf(l.log, "  client lock: %s held for %s, longer than any cell can run: releasing it (the cell is wedged; probatorium#442)\n",
					scenario, hold.Round(time.Second))
			}
			unlock()
		})
		release = func() { t.Stop(); unlock() }
	}
	return release, waited, nil
}
