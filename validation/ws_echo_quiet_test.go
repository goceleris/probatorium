package validation

import (
	"bufio"
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// The ws_echo walker's quiet gaps (ws_echo_quiet.go, probatorium#478 item 1).
// TestMain turns the gaps off for the rest of the package; every test here
// sets the schedule it exercises and puts the package's back afterwards.
func useQuietSchedule(t *testing.T, period, gap time.Duration) {
	t.Helper()
	savedPeriod, savedGap := wsEchoQuietPeriod, wsEchoQuietGap
	wsEchoQuietPeriod, wsEchoQuietGap = period, gap
	t.Cleanup(func() { wsEchoQuietPeriod, wsEchoQuietGap = savedPeriod, savedGap })
}

// gapAt returns the quiet gap [start, start+gap) of the period that holds t,
// on a grid aligned to the Unix epoch. Written out here rather than taken
// from wsEchoQuietWait, so the tests check that function against arithmetic
// of their own.
func gapAt(t time.Time, period, gap time.Duration) (start, end time.Time) {
	ns := t.UnixNano()
	k := ns / int64(period)
	if ns < 0 && ns%int64(period) != 0 {
		k--
	}
	start = time.Unix(0, k*int64(period))
	return start, start.Add(gap)
}

// connSpan is one connection's life as the echo server saw it: from the end
// of its upgrade to the moment its handler returned.
type connSpan struct{ open, close time.Time }

type connRecorder struct {
	mu    sync.Mutex
	spans []connSpan
}

func (r *connRecorder) add(open, close time.Time) {
	r.mu.Lock()
	r.spans = append(r.spans, connSpan{open, close})
	r.mu.Unlock()
}

func (r *connRecorder) snapshot() []connSpan {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]connSpan(nil), r.spans...)
}

// recordingEchoServer is echoServer's faithful echo, recording every
// connection's span.
func recordingEchoServer(t *testing.T, rec *connRecorder) *fakeWSServer {
	t.Helper()
	return newFakeWSServer(t, false, func(c net.Conn) {
		open := time.Now()
		defer func() { rec.add(open, time.Now()) }()
		br := bufio.NewReaderSize(c, wsEchoFrameBytes)
		for {
			fin, op, payload, err := wsReadFrame(br, wsEchoFrameBytes)
			if err != nil {
				return
			}
			if op == 0x8 {
				_, _ = c.Write(wsAppendFrame(nil, true, 0x8, payload, nil))
				return
			}
			if _, err := c.Write(wsAppendFrame(nil, fin, op, payload, nil)); err != nil {
				return
			}
		}
	})
}

// The regression test for probatorium#478 item 1: the real walker, against
// a faithful echo, holds no connection inside a quiet gap, and fires again
// between gaps. On origin/main the walker fires back to back, so connections
// are open inside every gap.
//
// The schedule is scaled down: fires of 120 ms (shortEchoFire) under a
// 400 ms hold, so a fire's bound is 2 x 400 ms + wsEchoCloseWait = 1.3 s; a
// 3 s period with a 500 ms gap then leaves 3 - 0.5 - 1.3 = 1.2 s per period
// in which a fire may start. That is more than half the period minus the gap
// (1 s), so a walker whose gaps sit half a period off the epoch grid starts
// fires inside the test's gaps.
// The walker starts 50 ms into a gap, as a cell can, so its first look at
// the schedule is from inside one. The run covers that gap, the period after
// it and the next gap.
func TestRunWSLargeEchoWalker_HoldsNoConnectionInAQuietGap(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	const period, gap = 3 * time.Second, 500 * time.Millisecond
	useQuietSchedule(t, period, gap)
	if bound := wsEchoFireBound(); bound != 1300*time.Millisecond {
		t.Fatalf("fire bound %v, want 1.3s (2 x hold + close wait); the schedule below assumes it", bound)
	}
	rec := &connRecorder{}
	srv := recordingEchoServer(t, rec)

	g0, _ := gapAt(time.Now(), period, gap)
	firstGap := g0.Add(period)
	time.Sleep(time.Until(firstGap.Add(50 * time.Millisecond)))
	start := time.Now()
	secondGap := firstGap.Add(period)
	end := secondGap.Add(gap + 100*time.Millisecond)
	ctx, cancel := context.WithDeadline(context.Background(), end)
	defer cancel()
	var tally wsEchoTally
	runWSLargeEchoWalker(ctx, srv.HostPort(), "/ws", 0x478, 20*time.Millisecond, &tally)

	// The walker has returned; wait (bounded) for the server to see the last
	// connection end. Every upgraded fire is one handler.
	upgraded := int(tally.snapshot().Upgraded)
	waitUntil := time.Now().Add(5 * time.Second)
	for len(rec.snapshot()) < upgraded && time.Now().Before(waitUntil) {
		time.Sleep(10 * time.Millisecond)
	}
	spans := rec.snapshot()
	if len(spans) < upgraded {
		t.Fatalf("server saw %d connections end, the walker upgraded %d", len(spans), upgraded)
	}
	t.Logf("run %v..%v: %d fires, %d upgraded, gaps at %v and %v (+%v)",
		start.Format("15:04:05.000"), end.Format("15:04:05.000"), tally.snapshot().Fires, upgraded,
		firstGap.Format("15:04:05.000"), secondGap.Format("15:04:05.000"), gap)

	between := 0
	for i, s := range spans {
		// Every gap the span could touch: the one its open falls in, and the
		// ones that open before it closes.
		for g, _ := gapAt(s.open, period, gap); g.Before(s.close); g = g.Add(period) {
			gEnd := g.Add(gap)
			if s.open.Before(gEnd) && s.close.After(g) {
				t.Errorf("connection %d open %v..%v, inside the quiet gap %v..%v",
					i, s.open.Format("15:04:05.000"), s.close.Format("15:04:05.000"),
					g.Format("15:04:05.000"), gEnd.Format("15:04:05.000"))
			}
		}
		if !s.open.Before(firstGap.Add(gap)) && s.open.Before(secondGap) {
			between++
		}
	}
	if between == 0 {
		t.Errorf("no connection opened between the two gaps (%v..%v): the walker must fire again after a gap",
			firstGap.Add(gap).Format("15:04:05.000"), secondGap.Format("15:04:05.000"))
	}
	t.Logf("%d connections, %d of them between the two gaps", len(spans), between)
}

// wsEchoQuietWait against the production schedule, at 1 ms steps over two
// whole periods: a fire it allows is closed before the next gap opens, and
// a wait it asks for ends exactly when a fire becomes allowed again.
func TestWSEchoQuietWait_AllowsOnlyFiresThatEndBeforeTheNextGap(t *testing.T) {
	useQuietSchedule(t, wsEchoQuietPeriodDefault, wsEchoQuietGapDefault)
	period, gap, bound := wsEchoQuietPeriodDefault, wsEchoQuietGapDefault, wsEchoFireBound()
	if bound != 2*wsEchoMaxHold+wsEchoCloseWait {
		t.Fatalf("fire bound %v, want 2 x %v + %v", bound, wsEchoMaxHold, wsEchoCloseWait)
	}
	// A period boundary well inside the Unix era: 2026-09-28 is a multiple
	// of nothing in particular, so the grid is found by arithmetic.
	base, _ := gapAt(time.Unix(1790611276, 0), period, gap)
	if base.UnixNano()%int64(period) != 0 {
		t.Fatalf("gap start %v is not on the epoch grid", base)
	}
	allowed, waited := 0, 0
	for off := time.Duration(0); off < 2*period; off += time.Millisecond {
		now := base.Add(off)
		w := wsEchoQuietWait(now)
		gStart, gEnd := gapAt(now, period, gap)
		next := gStart.Add(period)
		switch {
		case w < 0:
			t.Fatalf("at +%v: negative wait %v", off, w)
		case w == 0:
			allowed++
			if now.Before(gEnd) {
				t.Fatalf("at +%v: allowed a fire inside the gap %v..%v", off, gStart, gEnd)
			}
			if now.Add(bound).After(next) {
				t.Fatalf("at +%v: allowed a fire that may still be open at the next gap (%v + %v > %v)", off, now, bound, next)
			}
		default:
			waited++
			resume := now.Add(w)
			if wsEchoQuietWait(resume) != 0 {
				t.Fatalf("at +%v: waited %v to %v, where a fire is still not allowed", off, w, resume)
			}
			if _, e := gapAt(resume.Add(-time.Nanosecond), period, gap); !resume.Equal(e) {
				t.Fatalf("at +%v: the wait ends at %v, not at the end of a gap", off, resume)
			}
			if !now.Before(gEnd) && !now.Add(bound).After(next) {
				t.Fatalf("at +%v: waited %v although a fire would end before the next gap", off, w)
			}
		}
	}
	// One period: 18 s of gap plus the 4.5 s before the next gap in which a
	// fire could still be running, the rest allowed, both ends included (a
	// fire that ends exactly as the gap opens is allowed).
	if want := int((period-gap-bound)/time.Millisecond) + 1; allowed != 2*want {
		t.Errorf("allowed fire starts in %d ms of two periods, want %d", allowed, 2*want)
	}
	t.Logf("two periods: %d ms allowed, %d ms waiting", allowed, waited)
	// Off means off.
	for _, s := range [][2]time.Duration{{0, gap}, {period, 0}, {-period, gap}} {
		useQuietSchedule(t, s[0], s[1])
		if w := wsEchoQuietWait(base); w != 0 {
			t.Errorf("period %v gap %v: wait %v, want 0 (gaps off)", s[0], s[1], w)
		}
	}
}

// The coupling with I-MEM-1. Its evaluator keeps a trough bucket only when
// the bucket holds at least half a bucket of 1 Hz samples, and its bucket is
// one persistence period wide (properties.IMEM1.Persist samples at 1 Hz;
// i_mem.go: slopePersistSamples = slopeBucket / 1 s). So every stretch of
// half a bucket, wherever the evaluator's grid puts it, must hold one whole
// quiet gap, or a bucket can take its trough from a backlog.
func TestWSEchoQuietSchedule_holdsAWholeGapInEveryIMEM1Bucket(t *testing.T) {
	period, gap := wsEchoQuietPeriodDefault, wsEchoQuietGapDefault
	bucket := time.Duration(properties.IMEM1.Persist) * time.Second
	if bucket != 150*time.Second {
		t.Fatalf("I-MEM-1 bucket %v: this schedule was sized for 150 s; re-size it", bucket)
	}
	half := bucket / 2
	if period+gap > half {
		t.Errorf("period %v + gap %v > %v: a half bucket can miss a whole gap", period, gap, half)
	}
	// The same claim, checked rather than derived: a half bucket starting at
	// any second of the period holds a whole gap.
	for off := time.Duration(0); off < period; off += time.Second {
		s := time.Unix(0, 0).Add(off)
		g, _ := gapAt(s, period, gap)
		if g.Before(s) {
			g = g.Add(period)
		}
		if g.Add(gap).After(s.Add(half)) {
			t.Errorf("a half bucket from +%v (to +%v) holds no whole gap (next %v..%v)", off, off+half, g.Sub(time.Unix(0, 0)), g.Add(gap).Sub(time.Unix(0, 0)))
		}
	}
	// Each gap must hold clean samples: the property loop polls at 1 Hz and
	// the refapp caches its memstats for 1 s (debugvars.MemStatsTTL), so a
	// 2 s settle after the last fire's close leaves gap - 2 s of them. At
	// least 10 per gap.
	if gap-2*time.Second < 10*time.Second {
		t.Errorf("gap %v leaves fewer than 10 clean 1 Hz samples after a 2 s settle", gap)
	}
	// And the walker must still fire in every period.
	if period-gap <= wsEchoFireBound() {
		t.Errorf("period %v - gap %v leaves no room for a fire of up to %v", period, gap, wsEchoFireBound())
	}
}
