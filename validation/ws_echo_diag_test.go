package validation

import (
	"context"
	"fmt"
	"math/rand/v2"
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

// fakeSS answers with the server end of the connection at the given
// Recv-Q (per call) and the client end with a deep Send-Q. A negative queue
// leaves the server socket out of the output.
func fakeSS(recvq func(call int) int64) (func(context.Context, int) (string, error), *int) {
	calls := new(int)
	var mu sync.Mutex
	return func(_ context.Context, port int) (string, error) {
		mu.Lock()
		*calls++
		q := recvq(*calls)
		mu.Unlock()
		out := "State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"
		if q >= 0 {
			out += fmt.Sprintf("ESTAB %d 0 127.0.0.1:30833 127.0.0.1:%d\n\t skmem:(r%d,rb460800,t0,tb2626560,f0,w0,o0,bl0,d0) cubic bytes_received:%d lastrcv:60 rcv_wnd:17408\n", q, port, q, 2951836+q)
		}
		out += fmt.Sprintf("ESTAB 0 3071716 127.0.0.1:%d 127.0.0.1:30833\n\t cubic notsent:3071716\n", port)
		return out, nil
	}, calls
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
	ss, calls := fakeSS(func(int) int64 { return 433664 })
	rec := &diagRecorder{}
	d := &wsEchoDiag{SS: ss, Refapp: func(context.Context) string { return "REFAPP_CPUS leader=0" }}
	_, s := fireDiagnosed(t, echoServer(t, swallowAfterTwo()), d, rec)

	if len(rec.reps) != 1 {
		t.Fatalf("%d reports, want 1", len(rec.reps))
	}
	rep := rec.reps[0]
	if rep.Verdict.Class != verdictWedge || rep.Verdict.Reason != "recvq_flat_no_progress" {
		t.Fatalf("verdict %s/%s, want WEDGE_CLASS/recvq_flat_no_progress\n%s", rep.Verdict.Class, rep.Verdict.Reason, rep.Text)
	}
	if rec.timeoutAtDeliver[0] != 0 {
		t.Errorf("timeout was already counted (%d) when the report was delivered: the tally tick would end the cell before the second instant", rec.timeoutAtDeliver[0])
	}
	if s.Timeout != 1 || s.Missing != 0 || s.OK != 2 {
		t.Errorf("timeout=%d missing=%d ok=%d, want 1/0/2: the diagnosis must not change what the fire scores", s.Timeout, s.Missing, s.OK)
	}
	if *calls != 2 {
		t.Errorf("ss ran %d times, want 2 (t0 and t1)", *calls)
	}
	for _, want := range []string{"### t0 ", "### t1 ", "--- refapp state at t0", "--- refapp state at t1", "REFAPP_CPUS leader=0", "window_end=deadline", "srv_recvq_t0=433664"} {
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
	ss, _ := fakeSS(func(int) int64 { return 433664 }) // flat: ss alone would say WEDGE
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
	if v := rec.reps[0].Verdict; v.Class != verdictSlow || v.Reason != "echo_progress" {
		t.Fatalf("verdict %s/%s, want SLOW_CLASS/echo_progress\n%s", v.Class, v.Reason, rec.reps[0].Text)
	}
	if s.Timeout != 1 {
		t.Errorf("timeout=%d, want 1: the fire still reached its hold with echoes outstanding", s.Timeout)
	}
}

// Negative control: a failed ss and a vanished socket are INDETERMINATE.
func TestFireWSLargeEcho_DiagMissingDataIsIndeterminate(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, 300*time.Millisecond, 100*time.Millisecond)
	for name, ss := range map[string]func(context.Context, int) (string, error){
		"ss fails": func(context.Context, int) (string, error) { return "", fmt.Errorf("exec: ss: not found") },
		"socket absent": func() func(context.Context, int) (string, error) {
			f, _ := fakeSS(func(int) int64 { return -1 })
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
	d := &wsEchoDiag{SS: func(context.Context, int) (string, error) {
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
