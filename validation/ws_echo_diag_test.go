package validation

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// shortDiag runs a diagnosis in a fraction of a second.
func shortDiag(t *testing.T, window, minWindow time.Duration) {
	t.Helper()
	savedW, savedM := wsEchoDiagWindow, wsEchoMinWindow
	wsEchoDiagWindow, wsEchoMinWindow = window, minWindow
	t.Cleanup(func() { wsEchoDiagWindow, wsEchoMinWindow = savedW, savedM })
}

// fakeSrv is the server end of the connection as a fake ss prints it: the
// queues and the two counters the verdict rests on. A negative RecvQ leaves
// the server socket out of the output.
type fakeSrv struct{ RecvQ, Recv, Acked, SendQ int64 }

// flatSrv is the incident's server socket with nothing moving.
var flatSrv = fakeSrv{RecvQ: 433664, Recv: 2951836, Acked: 4490877}

// fakeSS answers with the server end of the connection as fakeSrv says (per
// call) and the client end with a deep Send-Q.
func fakeSS(srv func(call int) fakeSrv) (func(context.Context, int, int) (string, error), *int) {
	calls := new(int)
	var mu sync.Mutex
	return func(_ context.Context, port, srvPort int) (string, error) {
		mu.Lock()
		*calls++
		q := srv(*calls)
		mu.Unlock()
		out := "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"
		if q.RecvQ >= 0 {
			out += fmt.Sprintf("ESTAB %d %d 127.0.0.1:%d 127.0.0.1:%d\n\t skmem:(r%d,rb460800,t0,tb2626560,f0,w0,o0,bl0,d0) cubic bytes_acked:%d bytes_received:%d lastrcv:60 rcv_wnd:17408\n", q.RecvQ, q.SendQ, srvPort, port, q.RecvQ, q.Acked, q.Recv)
		}
		out += fmt.Sprintf("ESTAB 0 3071716 127.0.0.1:%d 127.0.0.1:%d\n\t cubic notsent:3071716\n", port, srvPort)
		return out, nil
	}, calls
}

// flatSS is fakeSS with the server socket unchanged at every call.
func flatSS() (func(context.Context, int, int) (string, error), *int) {
	return fakeSS(func(int) fakeSrv { return flatSrv })
}

type diagRecorder struct {
	mu   sync.Mutex
	reps []wsEchoDiagReport
	// timeoutAtDeliver is the tally's timeout count when each report arrived.
	timeoutAtDeliver []int64
}

func (r *diagRecorder) deliver(tally *wsEchoTally) func(context.Context, wsEchoDiagReport) {
	return func(_ context.Context, rep wsEchoDiagReport) {
		r.mu.Lock()
		defer r.mu.Unlock()
		r.reps = append(r.reps, rep)
		r.timeoutAtDeliver = append(r.timeoutAtDeliver, tally.timeout.Load())
	}
}

func fireDiagnosed(t *testing.T, srv *fakeWSServer, d *wsEchoDiag, rec *diagRecorder) (*wsEchoTally, wsEchoSnapshot) {
	t.Helper()
	tally := &wsEchoTally{}
	if d != nil {
		d.Deliver = rec.deliver(tally)
		tally.diag.Store(d)
	}
	rng := rand.New(rand.NewPCG(1, 2))
	fireWSLargeEcho(context.Background(), srv.HostPort(), "/ws", rng, tally)
	return tally, tally.snapshot()
}

func swallowAfterTwo() func(n int, _ bool, _ byte, p []byte) ([]byte, bool) {
	return func(n int, _ bool, _ byte, p []byte) ([]byte, bool) {
		if n > 2 {
			return nil, false
		}
		return p, false
	}
}

// A server that stops delivering and whose receive queue stays full: the
// walker holds the connection, reads for the window, and the two instants
// say WEDGE_CLASS. The timeout is counted once, and only after the report.
func TestFireWSLargeEcho_DiagWedge(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, 300*time.Millisecond, 100*time.Millisecond)
	ss, calls := flatSS()
	rec := &diagRecorder{}
	proc := procFixture(t)
	d := &wsEchoDiag{SS: ss, Refapp: func(context.Context) string { return threadTable(proc, 100) }}
	_, s := fireDiagnosed(t, echoServer(t, swallowAfterTwo()), d, rec)

	if len(rec.reps) != 1 {
		t.Fatalf("%d reports, want 1", len(rec.reps))
	}
	rep := rec.reps[0]
	if rep.Verdict.Class != verdictWedge || rep.Verdict.Reason != "no_app_read_no_app_write_recvq_pending" {
		t.Fatalf("verdict %s/%s, want WEDGE_CLASS/no_app_read_no_app_write_recvq_pending\n%s", rep.Verdict.Class, rep.Verdict.Reason, rep.Text)
	}
	t.Logf("record as written to ws-echo-diag.txt:\n%s", rep.Text)
	if rec.timeoutAtDeliver[0] != 0 {
		t.Errorf("timeout was already counted (%d) when the report was delivered: the tally tick would end the cell before the second instant", rec.timeoutAtDeliver[0])
	}
	if s.Timeout != 1 || s.Missing != 0 || s.OK != 2 {
		t.Errorf("timeout=%d missing=%d ok=%d, want 1/0/2: the diagnosis must not change what the fire scores", s.Timeout, s.Missing, s.OK)
	}
	if *calls != 2 {
		t.Errorf("ss ran %d times, want 2 (t0 and t1)", *calls)
	}
	for _, want := range []string{"### t0 ", "### t1 ", "--- refapp state at t0", "--- refapp state at t1", "REFAPP_THREADS n=5 leader_cpus_allowed_list=0", "window_end=deadline", "srv_recvq_t0=433664"} {
		if !strings.Contains(rep.Text, want) {
			t.Errorf("record lacks %q:\n%s", want, rep.Text)
		}
	}
	if first, _, _ := strings.Cut(rep.Text, "\n"); !strings.HasPrefix(first, "WS_ECHO_VERDICT=WEDGE_CLASS ") {
		t.Errorf("first line is %q, want the verdict line", first)
	}
}

// Negative control: a server that is only late. Frame 3 sits behind a delay
// longer than the hold, then everything is echoed. The walker, still reading,
// sees the echoes arrive in the window: SLOW_CLASS, whatever ss says.
func TestFireWSLargeEcho_DiagSlow(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, time.Second, 100*time.Millisecond)
	// Recv-Q and bytes_received flat, but the application's write counter
	// moves between the two instants: the echoes it wrote are what arrive.
	ss, _ := fakeSS(func(call int) fakeSrv {
		q := flatSrv
		if call > 1 {
			q.Acked += 3 * 65546
		}
		return q
	})
	rec := &diagRecorder{}
	d := &wsEchoDiag{SS: ss}
	late := echoServer(t, func(n int, _ bool, _ byte, p []byte) ([]byte, bool) {
		if n == 3 {
			time.Sleep(550 * time.Millisecond)
		}
		return p, false
	})
	_, s := fireDiagnosed(t, late, d, rec)
	if len(rec.reps) != 1 {
		t.Fatalf("%d reports, want 1", len(rec.reps))
	}
	if v := rec.reps[0].Verdict; v.Class != verdictSlow || v.Reason != "server_wrote" {
		t.Fatalf("verdict %s/%s, want SLOW_CLASS/server_wrote\n%s", v.Class, v.Reason, rec.reps[0].Text)
	}
	if s.Timeout != 1 {
		t.Errorf("timeout=%d, want 1: the fire still reached its hold with echoes outstanding", s.Timeout)
	}
}

// Negative control: a failed ss and a vanished socket are INDETERMINATE.
func TestFireWSLargeEcho_DiagMissingDataIsIndeterminate(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, 300*time.Millisecond, 100*time.Millisecond)
	for name, ss := range map[string]func(context.Context, int, int) (string, error){
		"ss fails": func(context.Context, int, int) (string, error) { return "", fmt.Errorf("exec: ss: not found") },
		"socket absent": func() func(context.Context, int, int) (string, error) {
			f, _ := fakeSS(func(int) fakeSrv { return fakeSrv{RecvQ: -1} })
			return f
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			rec := &diagRecorder{}
			_, s := fireDiagnosed(t, echoServer(t, swallowAfterTwo()), &wsEchoDiag{SS: ss}, rec)
			if len(rec.reps) != 1 || rec.reps[0].Verdict.Class != verdictIndeterminate {
				t.Fatalf("reports=%d verdict=%v, want one INDETERMINATE", len(rec.reps), rec.reps)
			}
			if s.Timeout != 1 {
				t.Errorf("timeout=%d, want 1", s.Timeout)
			}
		})
	}
}

// A run that ends inside the window still counts the timeout.
func TestFireWSLargeEcho_DiagCancelledWindowStillCountsTheTimeout(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, 5*time.Second, 100*time.Millisecond)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	tally := &wsEchoTally{}
	d := &wsEchoDiag{SS: func(context.Context, int, int) (string, error) {
		cancel() // the cell's budget ends during the first instant
		return "", fmt.Errorf("cancelled")
	}}
	rec := &diagRecorder{}
	d.Deliver = rec.deliver(tally)
	tally.diag.Store(d)
	start := time.Now()
	fireWSLargeEcho(ctx, echoServer(t, swallowAfterTwo()).HostPort(), "/ws", rand.New(rand.NewPCG(1, 2)), tally)
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("fire took %v: a cancelled run must not sit out the whole window", el)
	}
	if got := tally.timeout.Load(); got != 1 {
		t.Errorf("timeout=%d, want 1", got)
	}
	if len(rec.reps) != 1 || !strings.Contains(rec.reps[0].Verdict.Line, "window_end=ctx_cancelled") {
		t.Errorf("want one report ending ctx_cancelled, got %+v", rec.reps)
	}
}

// With no diagnosis installed the fire is as it was: the timeout is counted
// before the function returns and nothing else changes (the other timeout
// test, TestFireWSLargeEcho_StalledEchoesAreTimeout, holds the rest).
func TestFireWSLargeEcho_NoDiagCountsAtOnce(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	_, s := fireDiagnosed(t, echoServer(t, swallowAfterTwo()), nil, nil)
	if s.Timeout != 1 {
		t.Fatalf("timeout=%d, want 1", s.Timeout)
	}
}

// The per-cell budget caps the diagnoses.
func TestWSEchoDiag_ClaimIsBounded(t *testing.T) {
	d := &wsEchoDiag{}
	for i := 0; i < wsEchoDiagPerCell; i++ {
		if !d.claim() {
			t.Fatalf("claim %d refused", i)
		}
	}
	if d.claim() {
		t.Errorf("claim %d granted, budget is %d", wsEchoDiagPerCell+1, wsEchoDiagPerCell)
	}
	if (*wsEchoDiag)(nil).claim() {
		t.Errorf("a nil diagnosis granted a claim")
	}
}

// A slow refapp leg (it is served by a goroutine of the very process whose
// scheduling is in question) must neither eat the reading window nor stall the
// fire: the call is capped, its absence is written down, and the window
// measured is the time the walker really read.
func TestFireWSLargeEcho_DiagSlowRefappLegDoesNotEatTheWindow(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, 600*time.Millisecond, 400*time.Millisecond)
	savedB := wsEchoDiagRefappBudget
	wsEchoDiagRefappBudget = 400 * time.Millisecond
	t.Cleanup(func() { wsEchoDiagRefappBudget = savedB })
	ss, _ := flatSS()
	rec := &diagRecorder{}
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	d := &wsEchoDiag{SS: ss, Refapp: func(context.Context) string { <-release; return "late" }}
	start := time.Now()
	_, s := fireDiagnosed(t, echoServer(t, swallowAfterTwo()), d, rec)
	if len(rec.reps) != 1 {
		t.Fatalf("%d reports, want 1", len(rec.reps))
	}
	rep := rec.reps[0]
	if !strings.Contains(rep.Text, "refapp state not read within") {
		t.Errorf("a refapp leg that did not answer must be written down:\n%s", rep.Text)
	}
	if v := rep.Verdict; v.Class != verdictWedge {
		t.Errorf("verdict %s/%s, want WEDGE_CLASS: the window was a full reading window, so the flat queue counts\n%s", v.Class, v.Reason, rep.Text)
	}
	// The window on the line is the time spent reading: the configured 600 ms,
	// not that plus the refapp leg's 400 ms budget (which is what a window
	// measured from the start of the first capture would show, about 1000 ms).
	// Without the anchor after the first capture this fails.
	m := regexp.MustCompile(`window_ms=(\d+)`).FindStringSubmatch(rep.Verdict.Line)
	if m == nil {
		t.Fatalf("no window_ms on the line:\n%s", rep.Verdict.Line)
	}
	if ms, _ := strconv.Atoi(m[1]); ms < 580 || ms > 800 {
		t.Errorf("window_ms=%d, want about the configured 600 (580..800): the first capture's refapp leg ate into or stretched it\n%s", ms, rep.Verdict.Line)
	}
	if el := time.Since(start); el > 3*time.Second {
		t.Errorf("fire took %v: the slow refapp leg stalled it", el)
	}
	if s.Timeout != 1 {
		t.Errorf("timeout=%d, want 1", s.Timeout)
	}
}

// The reading window is anchored after the first capture: the walker reads
// for the configured window from the moment the first capture's host calls are
// done, not from when they started. A slow first capture (here the refapp leg
// takes its whole 600 ms budget) would otherwise leave a reading window of
// nothing; the window_ms on the line cannot show it (it is t1-t0 either way),
// so the test makes the server's echoes arrive between the two possible window
// ends: the late echoes land after the end of an unanchored window (about the
// timeout plus 600 ms) and before the end of the anchored one (about the
// timeout plus 600 ms refapp leg plus 600 ms), and the line must show them in
// bytes_new.
func TestFireWSLargeEcho_DiagWindowIsAnchoredAfterTheFirstCapture(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, 600*time.Millisecond, 400*time.Millisecond)
	savedB := wsEchoDiagRefappBudget
	wsEchoDiagRefappBudget = 600 * time.Millisecond
	t.Cleanup(func() { wsEchoDiagRefappBudget = savedB })
	// Recv-Q and bytes_received flat, the write counter moving: the echoes the
	// server sends are explained, so the class is not what this test is about.
	ss, _ := fakeSS(func(call int) fakeSrv {
		q := flatSrv
		if call > 1 {
			q.Acked += 3 * 65546
		}
		return q
	})
	release := make(chan struct{})
	t.Cleanup(func() { close(release) })
	rec := &diagRecorder{}
	d := &wsEchoDiag{SS: ss, Refapp: func(context.Context) string { <-release; return "late" }}
	late := echoServer(t, func(n int, _ bool, _ byte, p []byte) ([]byte, bool) {
		if n == 3 {
			time.Sleep(1350 * time.Millisecond)
		}
		return p, false
	})
	fireDiagnosed(t, late, d, rec)
	if len(rec.reps) != 1 {
		t.Fatalf("%d reports, want 1", len(rec.reps))
	}
	line := rec.reps[0].Verdict.Line
	m := regexp.MustCompile(`bytes_new=(-?\d+)`).FindStringSubmatch(line)
	if m == nil {
		t.Fatalf("no bytes_new on the line:\n%s", line)
	}
	if n, _ := strconv.Atoi(m[1]); n <= 0 {
		t.Errorf("bytes_new=%d: the echoes that arrive after the first capture's slow refapp leg were not read, so the window was not anchored after it\n%s", n, rec.reps[0].Text)
	}
}

// The connection is closed before the record is handed over: Deliver can wait
// a long time for the orchestrator's slot, and an open detached stream keeps
// ageing in the refapp's last-byte table (I-CONN-1).
func TestFireWSLargeEcho_DiagClosesTheConnectionBeforeDeliver(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, 300*time.Millisecond, 100*time.Millisecond)
	serverSawClose := make(chan struct{})
	var once sync.Once
	srv := newFakeWSServer(t, false, func(c net.Conn) {
		br := bufio.NewReaderSize(c, wsEchoFrameBytes)
		n := 0
		for {
			fin, op, payload, err := wsReadFrame(br, wsEchoFrameBytes)
			if err != nil {
				once.Do(func() { close(serverSawClose) })
				return
			}
			if op == 0x8 {
				return
			}
			if n++; n <= 2 {
				_, _ = c.Write(wsAppendFrame(nil, fin, op, payload, nil))
			}
		}
	})
	ss, _ := flatSS()
	closedAtDeliver := false
	tally := &wsEchoTally{}
	tally.diag.Store(&wsEchoDiag{SS: ss, Deliver: func(context.Context, wsEchoDiagReport) {
		select {
		case <-serverSawClose:
			closedAtDeliver = true
		case <-time.After(2 * time.Second):
		}
	}})
	fireWSLargeEcho(context.Background(), srv.HostPort(), "/ws", rand.New(rand.NewPCG(1, 2)), tally)
	if !closedAtDeliver {
		t.Error("the server had not seen the connection close when Deliver ran")
	}
	if tally.timeout.Load() != 1 {
		t.Errorf("timeout=%d, want 1", tally.timeout.Load())
	}
}
