package validation

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// TestLatencyBucketsEdges pins every edge of the histogram the design names:
// a reading exactly on an edge belongs to the bucket ABOVE it (the buckets
// are half-open, "< edge"), and a deadline lands in timeout whatever its
// elapsed was. Off-by-one here would move the v1.5.11 10 s-budget event out
// of the >=10 s bucket the design uses to compare it with a 20 s walker.
func TestLatencyBucketsEdges(t *testing.T) {
	cases := []struct {
		d        time.Duration
		timedOut bool
		want     string
	}{
		{0, false, "lt_100ms"},
		{99 * time.Millisecond, false, "lt_100ms"},
		{100 * time.Millisecond, false, "lt_1s"},
		{999 * time.Millisecond, false, "lt_1s"},
		{time.Second, false, "lt_2s"},
		{1999 * time.Millisecond, false, "lt_2s"},
		{2 * time.Second, false, "lt_5s"},
		{4999 * time.Millisecond, false, "lt_5s"},
		{5 * time.Second, false, "lt_10s"},
		{9999 * time.Millisecond, false, "lt_10s"},
		{10 * time.Second, false, "lt_20s"},
		{19999 * time.Millisecond, false, "lt_20s"},
		{20 * time.Second, false, "ge_20s"},
		{time.Hour, false, "ge_20s"},
		{50 * time.Millisecond, true, "timeout"},
		{20 * time.Second, true, "timeout"},
	}
	for _, tc := range cases {
		t.Run(fmt.Sprintf("%v/timeout=%v", tc.d, tc.timedOut), func(t *testing.T) {
			var b latencyBuckets
			b.observe(tc.d, tc.timedOut)
			got := b.snapshot()
			if got.Total() != 1 {
				t.Fatalf("observed 1, histogram holds %d", got.Total())
			}
			have := map[string]int64{
				"lt_100ms": got.Lt100ms, "lt_1s": got.Lt1s, "lt_2s": got.Lt2s, "lt_5s": got.Lt5s,
				"lt_10s": got.Lt10s, "lt_20s": got.Lt20s, "ge_20s": got.Ge20s, "timeout": got.Timeout,
			}
			for k, v := range have {
				if (k == tc.want) != (v == 1) {
					t.Errorf("bucket %s = %d, want %s = 1 and every other 0: %+v", k, v, tc.want, got)
				}
			}
		})
	}
}

// TestSlowFireRingIsBoundedAndOrdered: the ring keeps exactly the last
// slowFireRingSize fires in arrival order and counts every one it saw.
// A once-a-minute stall over an hour must not become a memory problem in
// the cell that is already failing, and the reader must be able to tell
// how many records the ring dropped.
func TestSlowFireRingIsBoundedAndOrdered(t *testing.T) {
	var r slowFireRing
	if got := r.snapshot(); got != nil {
		t.Fatalf("empty ring must snapshot as nil (omitted from the document), got %v", got)
	}
	const n = slowFireRingSize*3 + 5
	for i := 0; i < n; i++ {
		r.add(report.SlowFire{ReadMs: int64(i)})
	}
	got := r.snapshot()
	if len(got) != slowFireRingSize {
		t.Fatalf("ring holds %d, want %d", len(got), slowFireRingSize)
	}
	if r.total.Load() != n {
		t.Fatalf("total = %d, want %d: retention is bounded but COUNTING must not be", r.total.Load(), n)
	}
	for i, f := range got {
		want := int64(n - slowFireRingSize + i)
		if f.ReadMs != want {
			t.Fatalf("entry %d = %d, want %d (oldest first, the last %d fires)", i, f.ReadMs, want, slowFireRingSize)
		}
	}
	// Under the bound the ring is a plain list.
	var small slowFireRing
	for i := 0; i < 3; i++ {
		small.add(report.SlowFire{ReadMs: int64(i)})
	}
	if got := small.snapshot(); len(got) != 3 || got[0].ReadMs != 0 || got[2].ReadMs != 2 {
		t.Fatalf("under the bound: got %v", got)
	}
}

// TestFireCaptureThreshold is the negative control at the unit level: a
// read under slowReadThreshold is filed in the histogram and NOT in the
// ring; one over it is in both, with the error string and both addresses.
func TestFireCaptureThreshold(t *testing.T) {
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close(); _ = c2.Close() }()
	var c fireCapture
	now := time.Now()
	c.record(c1, time.Millisecond, time.Millisecond, slowReadThreshold-time.Millisecond, nil, "declined", "", 17, now)
	if s := c.slow.snapshot(); s != nil {
		t.Fatalf("a %v read must not land in the ring, got %+v", slowReadThreshold-time.Millisecond, s)
	}
	if l := c.latency.snapshot(); l.Read.Lt1s != 1 || l.Dial.Lt100ms != 1 || l.Write.Lt100ms != 1 {
		t.Fatalf("histogram did not file the fast fire: %+v", l)
	}
	readErr := fmt.Errorf("read tcp: %w", os.ErrDeadlineExceeded)
	c.record(c1, 2*time.Millisecond, time.Millisecond, slowReadThreshold, readErr, "hang-timeout", "", 0, now)
	s := c.slow.snapshot()
	if len(s) != 1 {
		t.Fatalf("a %v read must land in the ring, got %d entries", slowReadThreshold, len(s))
	}
	f := s[0]
	if f.Outcome != "hang-timeout" || !strings.Contains(f.Err, "i/o timeout") {
		t.Fatalf("ring entry lost the outcome or the error: %+v", f)
	}
	if f.LocalAddr != c1.LocalAddr().String() || f.RemoteAddr != c1.RemoteAddr().String() {
		t.Fatalf("ring entry lost the addresses: %+v", f)
	}
	if f.ReadMs != slowReadThreshold.Milliseconds() || f.DialMs != 2 || f.WriteMs != 1 {
		t.Fatalf("ring entry lost the per-leg elapsed: %+v", f)
	}
	if _, err := time.Parse(time.RFC3339Nano, f.TS); err != nil {
		t.Fatalf("ts %q is not RFC3339Nano: %v", f.TS, err)
	}
	if l := c.latency.snapshot(); l.Read.Timeout != 1 {
		t.Fatalf("a deadline read must land in the timeout bucket: %+v", l.Read)
	}
	if c.slow.total.Load() != 1 {
		t.Fatalf("slow total = %d, want 1", c.slow.total.Load())
	}
}

// TestIsDeadline: the three shapes a walker's own deadline arrives in.
func TestIsDeadline(t *testing.T) {
	if !isDeadline(os.ErrDeadlineExceeded) || !isDeadline(fmt.Errorf("x: %w", os.ErrDeadlineExceeded)) {
		t.Fatal("conn deadline not recognised")
	}
	if !isDeadline(context.DeadlineExceeded) {
		t.Fatal("context deadline not recognised")
	}
	// A dialer timeout is a net.Error with Timeout() true that wraps
	// neither sentinel.
	d := net.Dialer{Timeout: time.Nanosecond}
	_, err := d.Dial("tcp", "10.255.255.1:9")
	if err == nil {
		t.Skip("dial to a blackhole address succeeded; cannot make a dial timeout here")
	}
	var ne net.Error
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Skipf("dial failed without a timeout (%v); cannot test the dialer shape here", err)
	}
	if !isDeadline(err) {
		t.Fatalf("dialer timeout %v not recognised", err)
	}
	if isDeadline(nil) || isDeadline(errors.New("EOF")) {
		t.Fatal("non-deadline recognised as one")
	}
}

// TestHeartbeatMaxGapSince: only gaps at or above heartbeatSkewMin are
// kept, and a record consults them by window.
func TestHeartbeatMaxGapSince(t *testing.T) {
	var hb heartbeat
	t0 := time.Now()
	hb.observe(t0, 100*time.Millisecond) // normal tick, dropped
	hb.observe(t0.Add(time.Second), 700*time.Millisecond)
	hb.observe(t0.Add(3*time.Second), 2*time.Second)
	if g := hb.maxGapSince(t0.Add(-time.Second)); g != 2*time.Second {
		t.Fatalf("max gap over everything = %v, want 2s", g)
	}
	if g := hb.maxGapSince(t0.Add(2 * time.Second)); g != 2*time.Second {
		t.Fatalf("max gap since 2s = %v, want 2s", g)
	}
	if g := hb.maxGapSince(t0.Add(4 * time.Second)); g != 0 {
		t.Fatalf("max gap since 4s = %v, want 0", g)
	}
	var nilHB *heartbeat
	if nilHB.maxGapSince(t0) != 0 {
		t.Fatal("nil heartbeat must read as no skew")
	}
	// Bounded.
	for i := 0; i < heartbeatRingSize*2; i++ {
		hb.observe(t0.Add(time.Duration(10+i)*time.Second), time.Second)
	}
	if hb.n != heartbeatRingSize*2+2 {
		t.Fatalf("observed %d, want %d", hb.n, heartbeatRingSize*2+2)
	}
}

// TestLivenessTailIsBoundedAndReadableWhileAlive: the refapp's post-ready
// output is readable from the tally while the process is still up -- the
// engine's Warn/Error lines used to reach the artifact only on death.
func TestLivenessTailIsBoundedAndReadableWhileAlive(t *testing.T) {
	var b strings.Builder
	b.WriteString("ready addr=127.0.0.1:8080\n")
	const n = refappTailMaxLines*2 + 3
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "line %d\n", i)
	}
	l := &livenessTally{}
	superviseStderr(strings.NewReader(b.String()), l, func(string) {}, func(error) {}, func() {})
	tail := l.tailSnapshot()
	if len(tail) != refappTailMaxLines {
		t.Fatalf("tail holds %d lines, want %d", len(tail), refappTailMaxLines)
	}
	if tail[0] != fmt.Sprintf("line %d", n-refappTailMaxLines) || tail[len(tail)-1] != fmt.Sprintf("line %d", n-1) {
		t.Fatalf("tail is not the last %d lines oldest first: first=%q last=%q", refappTailMaxLines, tail[0], tail[len(tail)-1])
	}
	// Pre-ready lines never enter the tail.
	l2 := &livenessTally{}
	superviseStderr(strings.NewReader("booting\nready addr=127.0.0.1:1\n"), l2, func(string) {}, func(error) {}, func() {})
	if got := l2.tailSnapshot(); got != nil {
		t.Fatalf("a refapp that wrote nothing after ready must have a nil tail, got %v", got)
	}
}
