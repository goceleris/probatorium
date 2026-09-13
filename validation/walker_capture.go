package validation

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/probatorium/report"
)

// Per-fire capture for the h2c-churn and WS-torture walkers (celeris#588).
//
// Both walkers used to keep a cause class and one global max elapsed for
// their failing reads and nothing else, so the v1.5.11 soak's single
// h2c_hang and single ws_handshake_fail are unattributable forever: no
// timestamp, no error string, no address, no latency of the fire that
// failed, and no view of the fires that were merely slow. The corrected
// design (issue text, "Corrections from the adversarial review") asks for
// three things this file provides to both walkers:
//
//  1. a latency DISTRIBUTION per leg (dial / write / read) in fixed buckets,
//     so a stall shorter than the walker's budget is visible as a burst in
//     the 1-10 s buckets instead of vanishing into `declined`, and so the
//     old 10 s-budget event stays comparable with the current 20 s budget
//     (it is the >=10 s bucket);
//  2. a bounded ring of every fire whose READ leg exceeded slowReadThreshold,
//     carrying the instant, the per-leg elapsed, the outcome, the verbatim
//     error and both socket addresses (the engine<->client join key);
//  3. a validator-side heartbeat, so a record can say whether the validator
//     process itself was frozen while the read was in flight -- the
//     confound the review named as undecidable from the walker's clock.
//
// Nothing here is gated. The gated totals (h2c_hang, ws_handshake_fail)
// keep exactly their meaning; this is the evidence attached to them.

// slowReadThreshold is the read elapsed above which a fire is kept in the
// ring. One second is far under every walker budget (WS 2 s, h2c 20 s) and
// far over a healthy loopback round trip, so the ring holds only fires that
// were slow for a reason.
const slowReadThreshold = time.Second

// slowFireRingSize bounds the ring. Sixteen to sixty-four per the design;
// thirty-two keeps an hour-long systematic stall from costing more than a
// few KB in the cell document while holding more than one minute of a
// once-a-minute burst.
const slowFireRingSize = 32

// latencyBuckets is a fixed-edge histogram for one leg of a fire. The
// edges are those the design names; ge_20s and timeout are the two ways a
// leg can fall off the end.
type latencyBuckets struct {
	lt100ms, lt1s, lt2s, lt5s, lt10s, lt20s, ge20s, timeout atomic.Int64
}

// observe files one leg. timedOut is true when the leg ended with the
// walker's own deadline (os.ErrDeadlineExceeded); such a leg lands in
// `timeout` regardless of its elapsed, so the bucket answers "how many fires
// never got their bytes" and the elapsed buckets answer "how long the ones
// that did took".
func (b *latencyBuckets) observe(d time.Duration, timedOut bool) {
	switch {
	case timedOut:
		b.timeout.Add(1)
	case d < 100*time.Millisecond:
		b.lt100ms.Add(1)
	case d < time.Second:
		b.lt1s.Add(1)
	case d < 2*time.Second:
		b.lt2s.Add(1)
	case d < 5*time.Second:
		b.lt5s.Add(1)
	case d < 10*time.Second:
		b.lt10s.Add(1)
	case d < 20*time.Second:
		b.lt20s.Add(1)
	default:
		b.ge20s.Add(1)
	}
}

func (b *latencyBuckets) snapshot() report.LatencyBuckets {
	return report.LatencyBuckets{
		Lt100ms: b.lt100ms.Load(),
		Lt1s:    b.lt1s.Load(),
		Lt2s:    b.lt2s.Load(),
		Lt5s:    b.lt5s.Load(),
		Lt10s:   b.lt10s.Load(),
		Lt20s:   b.lt20s.Load(),
		Ge20s:   b.ge20s.Load(),
		Timeout: b.timeout.Load(),
	}
}

// walkerLatency is the three legs of one walker.
type walkerLatency struct {
	dial, write, read latencyBuckets
}

func (w *walkerLatency) snapshot() report.WalkerLatency {
	return report.WalkerLatency{
		Dial:  w.dial.snapshot(),
		Write: w.write.snapshot(),
		Read:  w.read.snapshot(),
	}
}

// isDeadline reports whether err is the walker's own deadline expiring: a
// conn deadline (os.ErrDeadlineExceeded), a dialer timeout (a net.Error
// whose Timeout() is true) or a context deadline.
func isDeadline(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, context.DeadlineExceeded) {
		return true
	}
	var ne net.Error
	return errors.As(err, &ne) && ne.Timeout()
}

// slowFireRing keeps the last slowFireRingSize slow fires in arrival order.
// Overwrite-oldest, not first-N: a stall that recurs is more diagnostic in
// its most recent shape, and total counts how many the ring could not hold.
type slowFireRing struct {
	mu    sync.Mutex
	buf   [slowFireRingSize]report.SlowFire
	n     int // entries written so far, unbounded
	total atomic.Int64
}

// add files one slow fire.
func (r *slowFireRing) add(f report.SlowFire) {
	r.total.Add(1)
	r.mu.Lock()
	r.buf[r.n%slowFireRingSize] = f
	r.n++
	r.mu.Unlock()
}

// snapshot returns the retained fires oldest first. nil when empty, so the
// cell document omits the key on a clean cell.
func (r *slowFireRing) snapshot() []report.SlowFire {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.n == 0 {
		return nil
	}
	kept := min(r.n, slowFireRingSize)
	out := make([]report.SlowFire, 0, kept)
	start := 0
	if r.n > slowFireRingSize {
		start = r.n % slowFireRingSize
	}
	for i := 0; i < kept; i++ {
		out = append(out, r.buf[(start+i)%slowFireRingSize])
	}
	return out
}

// fireCapture is the per-walker capture state both tallies embed: the
// histogram, the ring, the refapp's ready instant (for since_ready_ms and
// the mod-60 phase histogram the design computes offline) and the
// validator heartbeat. readyAt and hb are set once by driveTier1 through
// armWalkerCapture; a zero readyAt (unit tests firing the walker directly)
// leaves since_ready_ms at zero and a nil hb leaves validator_skew_ms at
// zero.
type fireCapture struct {
	latency walkerLatency
	slow    slowFireRing
	readyAt atomic.Int64 // unix nanos; 0 = unknown
	hb      atomic.Pointer[heartbeat]
}

// record files the three legs of one fire into the histogram and, when the
// read leg was slow, into the ring. It is the single choke point so a
// future refactor of either walker cannot drop the ring without also
// dropping the histogram, which the completeness test would catch.
//
// outcome is the walker's classification of the fire ("upgraded",
// "hang-timeout", "handshake-fail-eof", ...); err is the error that ended
// the read leg (nil for a completed read); status is the non-101 status line
// for WS, empty otherwise; nRead is the bytes the read leg returned.
func (c *fireCapture) record(conn net.Conn, dial, write, read time.Duration,
	readErr error, outcome, status string, nRead int, readStart time.Time,
) {
	c.latency.dial.observe(dial, false)
	c.latency.write.observe(write, false)
	c.latency.read.observe(read, isDeadline(readErr))
	if read < slowReadThreshold {
		return
	}
	now := time.Now()
	f := report.SlowFire{
		TS:      now.UTC().Format(time.RFC3339Nano),
		DialMs:  dial.Milliseconds(),
		WriteMs: write.Milliseconds(),
		ReadMs:  read.Milliseconds(),
		Outcome: outcome,
		Status:  status,
		NRead:   nRead,
	}
	if readErr != nil {
		f.Err = readErr.Error()
	}
	if conn != nil {
		if a := conn.LocalAddr(); a != nil {
			f.LocalAddr = a.String()
		}
		if a := conn.RemoteAddr(); a != nil {
			f.RemoteAddr = a.String()
		}
	}
	if r := c.readyAt.Load(); r != 0 {
		f.SinceReadyMs = now.Sub(time.Unix(0, r)).Milliseconds()
	}
	if hb := c.hb.Load(); hb != nil {
		// The window that matters is the one the read was in flight for:
		// a validator freeze inside it is what would make a server look
		// stalled from here.
		f.ValidatorSkewMs = hb.maxGapSince(readStart).Milliseconds()
	}
	c.slow.add(f)
}

// recordNoRead files a fire that by design has no read leg (the h2c
// rst-before-read mode): dial and write only.
func (c *fireCapture) recordNoRead(dial, write time.Duration) {
	c.latency.dial.observe(dial, false)
	c.latency.write.observe(write, false)
}

// recordDialFail files a fire whose dial leg failed: the dial bucket gets
// its elapsed (or timeout), the ring nothing -- there is no read to be slow
// and no conn to join on. The walker's own dial_fail counter is the count.
func (c *fireCapture) recordDialFail(d time.Duration, err error) {
	c.latency.dial.observe(d, isDeadline(err))
}

// recordWriteFail is the same for the write leg.
func (c *fireCapture) recordWriteFail(dial, write time.Duration, err error) {
	c.latency.dial.observe(dial, false)
	c.latency.write.observe(write, isDeadline(err))
}

// heartbeat is a validator-side stall detector: a ticker at
// heartbeatInterval whose observed gaps above heartbeatSkewMin are kept with
// their instants. A record consults it for the window its read was in
// flight. It replaces the design's runtime GC-pause field, which the review
// showed reads ~0 in exactly the case it was meant to flag (scheduler or
// host-wide starvation is not a GC pause).
type heartbeat struct {
	mu   sync.Mutex
	gaps [heartbeatRingSize]heartbeatGap
	n    int
}

type heartbeatGap struct {
	at  time.Time // the tick that arrived late
	gap time.Duration
}

const (
	heartbeatInterval = 100 * time.Millisecond
	heartbeatSkewMin  = 500 * time.Millisecond
	heartbeatRingSize = 64
)

// startHeartbeat runs the ticker until ctx is done.
func startHeartbeat(ctx context.Context) *heartbeat {
	hb := &heartbeat{}
	go hb.run(ctx)
	return hb
}

func (h *heartbeat) run(ctx context.Context) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	last := time.Now()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			h.observe(now, now.Sub(last))
			last = now
		}
	}
}

// observe files one tick gap. Exposed for the unit test.
func (h *heartbeat) observe(at time.Time, gap time.Duration) {
	if gap < heartbeatSkewMin {
		return
	}
	h.mu.Lock()
	h.gaps[h.n%heartbeatRingSize] = heartbeatGap{at: at, gap: gap}
	h.n++
	h.mu.Unlock()
}

// maxGapSince returns the largest tick gap whose late tick arrived at or
// after since, or 0.
func (h *heartbeat) maxGapSince(since time.Time) time.Duration {
	if h == nil {
		return 0
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	var out time.Duration
	kept := min(h.n, heartbeatRingSize)
	for i := 0; i < kept; i++ {
		g := h.gaps[i]
		if !g.at.Before(since) && g.gap > out {
			out = g.gap
		}
	}
	return out
}

// armWalkerCapture stamps the refapp's ready instant (unix nanos, 0 =
// unknown) on the walker tallies and starts the validator heartbeat for
// them. One call from driveTier1 once the tallies exist.
func armWalkerCapture(ctx context.Context, readyAtNanos int64, h2c *h2cTally, ws *wsTally) {
	hb := startHeartbeat(ctx)
	for _, c := range []*fireCapture{&h2c.capture, &ws.capture} {
		if readyAtNanos != 0 {
			c.readyAt.Store(readyAtNanos)
		}
		c.hb.Store(hb)
	}
}
