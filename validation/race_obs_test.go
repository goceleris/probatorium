package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// Race tier observability (probatorium#481, celeris#871): what race run
// 39560945814 could not say about its own failures.
//
//  1. A SLOW_CLASS ws_echo fire the guard refuses (the 4th, over N) is counted
//     in ws_echo_timeout and fails. It must also be counted by name, so the
//     failure reads as latency over the tolerance and not as a wedge.
//  2. h2c_hang and ws_handshake_fail had no instant: the slow ring keeps only
//     reads over one second and those hangs closed in 12 ms.
//  3. The refapp's CPU placement was recorded only in a stall-capture run.

// ---------------------------------------------------------------- item 1

// Four SLOW_CLASS timeouts in one cell, a clean fire between each: three are
// tolerated and the fourth is refused for being over N. That is the cell of
// run 39560945814 (ws_echo_timeout_slow=3, ws_echo_timeout=1).
func TestSlowOverNIsCountedByName(t *testing.T) {
	tally, _, _ := slowRig(t, 3)
	late, plain := lateEcho(t), plainEcho(t)
	for i := 1; i <= 4; i++ {
		fireOn(late, tally)
		fireOn(plain, tally)
	}
	s := tally.snapshot()
	if s.TimeoutSlow != 3 || s.Timeout != 1 {
		t.Fatalf("timeout_slow=%d timeout=%d, want 3/1", s.TimeoutSlow, s.Timeout)
	}
	if s.TimeoutSlowOverN != 1 || s.TimeoutSlowNoClean != 0 {
		t.Errorf("slow_over_n=%d slow_no_clean_fire=%d, want 1/0: the 4th SLOW fire failed for being over N", s.TimeoutSlowOverN, s.TimeoutSlowNoClean)
	}
	sum := tier1TallySnapshot{WSEcho: s}.Tier1Summary()
	if sum.WSEcho["ws_echo_slow_over_n"] != 1 || sum.WSEcho["ws_echo_slow_no_clean_fire"] != 0 {
		t.Errorf("not exported to the cell document: %v", sum.WSEcho)
	}
	// The failure is still the same failure: counted once, in ws_echo_timeout.
	if sum.WSEcho["ws_echo_timeout"] != 1 || sum.WSEcho["ws_echo_timeout_slow"] != 3 {
		t.Errorf("ws_echo_timeout / _slow changed: %v", sum.WSEcho)
	}
}

// The other guard reason has its own counter, so the two read apart.
func TestSlowNoCleanFireIsCountedByName(t *testing.T) {
	tally, _, _ := slowRig(t, 3)
	late := lateEcho(t)
	fireOn(late, tally)
	fireOn(late, tally)
	s := tally.snapshot()
	if s.TimeoutSlow != 1 || s.Timeout != 1 {
		t.Fatalf("timeout_slow=%d timeout=%d, want 1/1", s.TimeoutSlow, s.Timeout)
	}
	if s.TimeoutSlowNoClean != 1 || s.TimeoutSlowOverN != 0 {
		t.Errorf("slow_no_clean_fire=%d slow_over_n=%d, want 1/0", s.TimeoutSlowNoClean, s.TimeoutSlowOverN)
	}
}

// Negative controls: a timeout that is not a refused SLOW fire is in neither
// counter, so ws_echo_slow_over_n cannot be read as "every failing timeout".
func TestOnlyRefusedSlowFiresAreCountedByName(t *testing.T) {
	// A WEDGE_CLASS timeout in the race tier.
	flat, _ := flatSS()
	tally, d, _ := slowRig(t, 3)
	d.SS = flat
	fireOn(echoServer(t, swallowAfterTwo()), tally)
	if s := tally.snapshot(); s.Timeout != 1 || s.TimeoutSlowOverN != 0 || s.TimeoutSlowNoClean != 0 {
		t.Errorf("wedge: timeout=%d over_n=%d no_clean=%d, want 1/0/0", s.Timeout, s.TimeoutSlowOverN, s.TimeoutSlowNoClean)
	}
	// A SLOW_CLASS timeout in a tier with no tolerance.
	off, _, _ := slowRig(t, 0)
	fireOn(lateEcho(t), off)
	if s := off.snapshot(); s.Timeout != 1 || s.TimeoutSlowOverN != 0 || s.TimeoutSlowNoClean != 0 {
		t.Errorf("tolerance off: timeout=%d over_n=%d no_clean=%d, want 1/0/0", s.Timeout, s.TimeoutSlowOverN, s.TimeoutSlowNoClean)
	}
	// A tolerated SLOW fire.
	ok, _, _ := slowRig(t, 3)
	fireOn(lateEcho(t), ok)
	if s := ok.snapshot(); s.TimeoutSlow != 1 || s.TimeoutSlowOverN != 0 || s.TimeoutSlowNoClean != 0 {
		t.Errorf("tolerated: timeout_slow=%d over_n=%d no_clean=%d, want 1/0/0", s.TimeoutSlow, s.TimeoutSlowOverN, s.TimeoutSlowNoClean)
	}
}

// ---------------------------------------------------------------- item 2

func parseTS(t *testing.T, s string) time.Time {
	t.Helper()
	ts, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		t.Fatalf("ts %q is not RFC3339Nano: %v", s, err)
	}
	if ts.Location() != time.UTC {
		t.Errorf("ts %q is not UTC", s)
	}
	return ts
}

// A hang that closed at once (12 ms in run 39560945814) is under the slow
// ring's one second threshold, so nothing recorded when it happened. The span
// does, with its cause and verbatim error.
func TestFireH2CChurn_FastHangIsStamped(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		_ = c.Close()
	})
	var tally h2cTally
	ready := time.Now().Add(-3 * time.Second)
	tally.capture.readyAt.Store(ready.UnixNano())
	before := time.Now()
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	after := time.Now()
	s := tally.snapshot()
	if s.Hang != 1 || len(s.SlowReads) != 0 {
		t.Fatalf("setup: want one fast hang outside the ring, got hang=%d ring=%v", s.Hang, s.SlowReads)
	}
	sp := s.HangSpan
	if sp == nil {
		t.Fatal("h2c_hang_span is nil: the hang has no instant")
	}
	if sp.Count != 1 || sp.First != sp.Last {
		t.Errorf("one event: count=%d first=%+v last=%+v, want 1 and first == last", sp.Count, sp.First, sp.Last)
	}
	if ts := parseTS(t, sp.First.TS); ts.Before(before.Add(-time.Millisecond)) || ts.After(after.Add(time.Millisecond)) {
		t.Errorf("ts %s is outside the fire [%s, %s]", ts, before, after)
	}
	if sp.First.Cause != "eof" || sp.First.Err != "EOF" {
		t.Errorf("cause=%q err=%q, want eof / EOF", sp.First.Cause, sp.First.Err)
	}
	if sp.First.SinceReadyMs < 2900 || sp.First.SinceReadyMs > 4000 {
		t.Errorf("since_ready_ms=%d, want about 3000 (ready was 3 s before the fire)", sp.First.SinceReadyMs)
	}
	if !strings.HasPrefix(sp.First.LocalAddr, "127.0.0.1:") || sp.First.RemoteAddr != srv.HostPort() {
		t.Errorf("addresses lost: %+v", sp.First)
	}
}

// Two hangs of different causes: the span has the earliest first and the
// latest last, with each one's own cause. (Mutants: last never updated; first
// overwritten every time.)
func TestFireH2CChurn_SpanKeepsFirstAndLastOfSeveral(t *testing.T) {
	var n int
	var mu sync.Mutex
	srv := newFakeH2CServer(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		mu.Lock()
		n++
		k := n
		mu.Unlock()
		if k == 2 {
			if tc, ok := c.(*net.TCPConn); ok {
				_ = tc.SetLinger(0) // close sends an RST
			}
		}
		_ = c.Close()
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	time.Sleep(30 * time.Millisecond)
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.Hang != 2 || s.HangEOF != 1 || s.HangReset != 1 {
		t.Fatalf("setup: hang=%d eof=%d reset=%d, want 2/1/1", s.Hang, s.HangEOF, s.HangReset)
	}
	sp := s.HangSpan
	if sp == nil || sp.Count != 2 {
		t.Fatalf("span %+v, want count 2", sp)
	}
	first, last := parseTS(t, sp.First.TS), parseTS(t, sp.Last.TS)
	if !last.After(first) {
		t.Errorf("last %s is not after first %s", last, first)
	}
	if sp.First.Cause != "eof" || sp.Last.Cause != "reset" {
		t.Errorf("first=%q last=%q, want eof then reset", sp.First.Cause, sp.Last.Cause)
	}
}

// Negative control: an answered fire stamps nothing and the field is absent.
func TestFireH2CChurn_NoHangNoSpan(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		defer func() { _ = c.Close() }()
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 0\r\n\r\n"))
	})
	var tally h2cTally
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	s := tally.snapshot()
	if s.Hang != 0 || s.HangSpan != nil {
		t.Fatalf("hang=%d span=%+v, want none", s.Hang, s.HangSpan)
	}
	if b, _ := json.Marshal(s); strings.Contains(string(b), "h2c_hang_span") {
		t.Errorf("an empty span is serialised: %s", b)
	}
}

// The ws handshake failure, reset kind: a peer that resets before the status
// line.
func TestFireWSTorture_ResetHandshakeIsStamped(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		if tc, ok := c.(*net.TCPConn); ok {
			_ = tc.SetLinger(0)
		}
		_ = c.Close()
	})
	var tally wsTally
	tally.capture.readyAt.Store(time.Now().Add(-2 * time.Second).UnixNano())
	before := time.Now()
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModePingFlood, &tally)
	after := time.Now()
	s := tally.snapshot()
	if s.HandshakeFail != 1 || s.HandshakeFailReset != 1 {
		t.Fatalf("setup: handshake_fail=%d reset=%d", s.HandshakeFail, s.HandshakeFailReset)
	}
	sp := s.HandshakeFailSpan
	if sp == nil || sp.Count != 1 || sp.First != sp.Last {
		t.Fatalf("span %+v, want one event", sp)
	}
	if ts := parseTS(t, sp.First.TS); ts.Before(before.Add(-time.Millisecond)) || ts.After(after.Add(time.Millisecond)) {
		t.Errorf("ts %s outside [%s, %s]", ts, before, after)
	}
	if sp.First.Cause != "reset" || !strings.Contains(sp.First.Err, "reset") {
		t.Errorf("cause=%q err=%q, want reset and the verbatim connection reset error", sp.First.Cause, sp.First.Err)
	}
	if sp.First.SinceReadyMs < 1900 {
		t.Errorf("since_ready_ms=%d, want about 2000", sp.First.SinceReadyMs)
	}
}

// A non-101 answer is a handshake failure too; the status line is its cause.
// A clean 101 stamps nothing.
func TestFireWSTorture_StatusRejectIsStampedAndUpgradeIsNot(t *testing.T) {
	rej := newFakeWSServer(t, true, nil)
	var rt wsTally
	fireWSTorture(context.Background(), rej.HostPort(), "/ws", ModeUnmaskedClient, &rt)
	if sp := rt.snapshot().HandshakeFailSpan; sp == nil || sp.First.Cause != "status" {
		t.Errorf("rejected handshake: span %+v, want cause status", sp)
	}
	ok := newFakeWSServer(t, false, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		_, _ = c.Write([]byte{0x88, 0x02, 0x03, 0xEA})
	})
	var ut wsTally
	fireWSTorture(context.Background(), ok.HostPort(), "/ws", ModeUnmaskedClient, &ut)
	if s := ut.snapshot(); s.Upgraded != 1 || s.HandshakeFailSpan != nil {
		t.Errorf("clean upgrade: upgraded=%d span=%+v, want 1 and none", s.Upgraded, s.HandshakeFailSpan)
	}
}

// The span is the true minimum and maximum, not arrival order: walkers run
// concurrently, so a later-arriving mark can carry an earlier instant. Run
// with -race.
func TestEventSpanKeepsTrueMinAndMaxUnderConcurrency(t *testing.T) {
	var sp eventSpan
	base := time.Date(2026, 10, 21, 9, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			at := base.Add(time.Duration((i*37)%64) * time.Second)
			sp.mark(at, report.EventStamp{TS: at.Format(time.RFC3339Nano), Cause: fmt.Sprintf("c%d", (i*37)%64)})
		}()
	}
	wg.Wait()
	got := sp.snapshot()
	if got == nil || got.Count != 64 {
		t.Fatalf("span %+v, want count 64", got)
	}
	if got.First.TS != base.Format(time.RFC3339Nano) || got.First.Cause != "c0" {
		t.Errorf("first %+v, want the earliest (c0 at %s)", got.First, base)
	}
	if last := base.Add(63 * time.Second); got.Last.TS != last.Format(time.RFC3339Nano) || got.Last.Cause != "c63" {
		t.Errorf("last %+v, want the latest (c63 at %s)", got.Last, last)
	}
	if (&eventSpan{}).snapshot() != nil {
		t.Error("an empty span must snapshot to nil")
	}
}

// The spans reach the cell document, and are absent from a cell with none.
func TestTier1SummaryCarriesTheEventSpans(t *testing.T) {
	st := func(ts, cause string) report.EventStamp { return report.EventStamp{TS: ts, Cause: cause} }
	var snap tier1TallySnapshot
	snap.H2CChurn.HangSpan = &report.EventSpan{Count: 2, First: st("2026-10-21T09:36:10Z", "eof"), Last: st("2026-10-21T09:40:00Z", "reset")}
	snap.WSTorture.HandshakeFailSpan = &report.EventSpan{Count: 1, First: st("2026-10-21T09:37:00Z", "reset"), Last: st("2026-10-21T09:37:00Z", "reset")}
	sum := snap.Tier1Summary()
	if sum.H2CHangSpan == nil || sum.H2CHangSpan.Last.Cause != "reset" || sum.H2CHangSpan.Count != 2 {
		t.Errorf("h2c_hang_span not projected: %+v", sum.H2CHangSpan)
	}
	if sum.WSHandshakeFailSpan == nil || sum.WSHandshakeFailSpan.First.Cause != "reset" {
		t.Errorf("ws_handshake_fail_span not projected: %+v", sum.WSHandshakeFailSpan)
	}
	var quiet tier1TallySnapshot
	q := quiet.Tier1Summary()
	if q.H2CHangSpan != nil || q.WSHandshakeFailSpan != nil || q.RefappSched != nil {
		t.Errorf("a cell with no event must omit its spans: %+v", q)
	}
	b, _ := json.Marshal(q)
	for _, k := range []string{"h2c_hang_span", "ws_handshake_fail_span", "refapp_sched"} {
		if strings.Contains(string(b), k) {
			t.Errorf("%s serialised for a cell without it", k)
		}
	}
}

// ---------------------------------------------------------------- item 3

// The refapp's own CPU view, from /proc, in a run with no arm and no stall
// capture: the routine race run that raised the question.
func TestCollectRefappSchedRecordsWithoutArmsOrStallCapture(t *testing.T) {
	before := time.Now()
	rec := collectRefappSched(context.Background(), envMap(nil), "local", procFixture(t), 100, t.TempDir(), "")
	if rec == nil {
		t.Fatal("no record in a routine run: the placement is only read when an arm or the stall capture is on")
	}
	if rec.Unavailable != "" {
		t.Fatalf("unavailable: %q", rec.Unavailable)
	}
	if rec.Threads != 5 || rec.LeaderCPUsAllowedList != "0" {
		t.Errorf("threads=%d leader=%q, want 5 and 0", rec.Threads, rec.LeaderCPUsAllowedList)
	}
	want := map[string]int{"0": 1, "0-15": 2, "1": 1, "2-3,8": 1}
	if len(rec.ThreadMasks) != len(want) {
		t.Fatalf("thread_masks %v, want %v", rec.ThreadMasks, want)
	}
	for k, n := range want {
		if rec.ThreadMasks[k] != n {
			t.Errorf("thread_masks[%q]=%d, want %d (the leader's mask alone is not the process's)", k, rec.ThreadMasks[k], n)
		}
	}
	if rec.GOMAXPROCS != 0 || !strings.Contains(rec.GOMAXPROCSNote, "side listener") {
		t.Errorf("gomaxprocs=%d note=%q: with no side listener it is unknown and the note says why", rec.GOMAXPROCS, rec.GOMAXPROCSNote)
	}
	if ts := parseTS(t, rec.At); ts.Before(before.Add(-time.Second)) {
		t.Errorf("at %s is stale", ts)
	}
}

// A routine run writes no new file: the record goes to the cell document
// only (refapp_sched.txt stays the arm / stall-capture readback).
func TestCollectRefappSchedWritesNoFileInARoutineRun(t *testing.T) {
	dir := t.TempDir()
	_ = collectRefappSched(context.Background(), envMap(nil), "local", procFixture(t), 100, dir, "")
	if _, err := os.Stat(filepath.Join(dir, refappReadbackFile)); err == nil {
		t.Errorf("a routine run wrote %s", refappReadbackFile)
	}
}

// With the side listener up (stall capture) GOMAXPROCS is the refapp's own
// answer, and the readback file is still written once.
func TestCollectRefappSchedTakesGOMAXPROCSFromTheSideListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	var hits int
	var mu sync.Mutex
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/sched", func(w http.ResponseWriter, _ *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write([]byte(`{"gomaxprocs":8,"numcpu":16,"engine":{}}` + "\n"))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })

	dir := t.TempDir()
	env := envMap(map[string]string{stallCaptureEnv: "1"})
	rec := collectRefappSched(context.Background(), env, "local", procFixture(t), 100, dir, ln.Addr().String())
	if rec == nil || rec.GOMAXPROCS != 8 || rec.GOMAXPROCSNote != "" {
		t.Fatalf("record %+v, want gomaxprocs 8 and no note", rec)
	}
	if _, err := os.Stat(filepath.Join(dir, refappReadbackFile)); err != nil {
		t.Errorf("the stall-capture readback file is gone: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if hits != 1 {
		t.Errorf("/debug/sched fetched %d times, want once: the record reuses the readback's answer", hits)
	}
}

// Nothing local can be said about a refapp the ssh driver started elsewhere,
// and nothing about a cell with no pid or no /proc.
func TestCollectRefappSchedSaysWhyItCannotRead(t *testing.T) {
	for name, tc := range map[string]struct {
		mode, root string
		pid        int
		want       string
	}{
		"ssh driver": {"ssh", procFixture(t), 100, "ssh"},
		"no pid":     {"local", procFixture(t), 0, "pid"},
		"no /proc":   {"local", t.TempDir(), 100, "unreadable"},
	} {
		rec := collectRefappSched(context.Background(), envMap(nil), tc.mode, tc.root, tc.pid, t.TempDir(), "")
		if rec == nil || !strings.Contains(rec.Unavailable, tc.want) {
			t.Errorf("%s: record %+v, want unavailable naming %q", name, rec, tc.want)
			continue
		}
		if rec.ThreadMasks != nil || rec.GOMAXPROCS != 0 {
			t.Errorf("%s: an unavailable record carries data: %+v", name, rec)
		}
	}
}

func TestGOMAXPROCSFromSchedText(t *testing.T) {
	for text, want := range map[string]int{
		"ARM x\nREFAPP_SCHED {\"gomaxprocs\":16,\"numcpu\":16}\nREFAPP_THREADS n=1\n": 16,
		"REFAPP_SCHED {\"numcpu\":16,\"gomaxprocs\":2,\"engine\":{\"workers\":4}}\n":  2,
		"REFAPP_SCHED unavailable: no side listener\n":                                0,
		"REFAPP_SCHED {not json}\n":                                                   0,
		"no sched line at all\n":                                                      0,
		"REFAPP_SCHED {\"gomaxprocs\":0}\n":                                           0,
	} {
		if got := gomaxprocsFromSchedText(text); got != want {
			t.Errorf("%q: got %d, want %d", text, got, want)
		}
	}
}

func TestTier1SummaryCarriesTheRefappSched(t *testing.T) {
	var snap tier1TallySnapshot
	snap.RefappSched = &report.RefappSched{Threads: 5, LeaderCPUsAllowedList: "0", ThreadMasks: map[string]int{"0-15": 4, "0": 1}}
	sum := snap.Tier1Summary()
	if sum.RefappSched == nil || sum.RefappSched.Threads != 5 || sum.RefappSched.ThreadMasks["0-15"] != 4 {
		t.Fatalf("refapp_sched not projected: %+v", sum.RefappSched)
	}
	// The record is a copy: the document does not alias the live snapshot.
	snap.RefappSched.ThreadMasks["0-15"] = 99
	if sum.RefappSched.ThreadMasks["0-15"] != 4 {
		t.Error("the projected record aliases the snapshot's map")
	}
}

// ---------------------------------------------------------- orchestrator

// fakeRefappBin is a stand-in refapp: it announces ready on the address the
// walkers will find nothing at, and stays up. The point is its pid, which the
// orchestrator hands to the readback goroutine.
func fakeRefappBin(t *testing.T) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "fakerefapp")
	if err := os.WriteFile(p, []byte("#!/bin/sh\necho 'ready addr=127.0.0.1:1'\nexec sleep 60\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	return p
}

// The placement record is in the cell document when the orchestrator runs a
// whole Tier 1 against a refapp: the goroutine that reads it is started, joined
// and its result attached. (Mutants: the attach removed; the goroutine not
// started.) Off Linux there is no /proc, and the record says so instead of
// being absent.
func TestRunTierPropertyAttachesTheRefappSchedToTheCellDocument(t *testing.T) {
	cfg := Default()
	cfg.Duration = 2 * time.Second
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = fakeRefappBin(t)
	cfg.MarkovPath = "markov/auth_session_ratelimit.yaml"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	violations := make(chan Incident, 64)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	o.runTierProperty(ctx, violations)
	if !o.tier1Ran {
		t.Fatalf("premise: Tier 1 did not complete (%d incident(s) queued)", len(violations))
	}
	rec := o.tier1Snapshot.RefappSched
	if rec == nil {
		t.Fatal("the cell's tally carries no refapp_sched: the record was read but not attached")
	}
	if runtime.GOOS == "linux" {
		if rec.Unavailable != "" || rec.Threads < 1 || len(rec.ThreadMasks) == 0 {
			t.Errorf("on Linux the refapp's threads must be read: %+v", rec)
		}
	} else if rec.Unavailable == "" {
		t.Errorf("without /proc the record must say it is unavailable: %+v", rec)
	}
	if sum := o.tier1Snapshot.Tier1Summary(); sum.RefappSched == nil {
		t.Error("the record does not reach the document's Tier1Summary")
	}
}

// Under the ssh driver the local /proc and the local readback say nothing
// about the refapp, so a record that is Unavailable carries nothing read from
// them, GOMAXPROCS included, even when an arm or the stall capture is on (the
// readback used to run there against the wrong host's /proc).
func TestCollectRefappSchedUnderSSHIsConsistent(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/sched", func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"gomaxprocs":8}` + "\n"))
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	dir := t.TempDir()
	rec := collectRefappSched(context.Background(), envMap(map[string]string{stallCaptureEnv: "1"}), "ssh", procFixture(t), 100, dir, ln.Addr().String())
	if rec == nil || rec.Unavailable == "" {
		t.Fatalf("record %+v, want unavailable under ssh", rec)
	}
	if rec.GOMAXPROCS != 0 || rec.Threads != 0 || rec.ThreadMasks != nil || !strings.Contains(rec.GOMAXPROCSNote, "ssh") {
		t.Errorf("an unavailable record carries local data: %+v", rec)
	}
	if _, err := os.Stat(filepath.Join(dir, refappReadbackFile)); err == nil {
		t.Errorf("the local readback file was written for a refapp on another host")
	}
}

// A readback that cannot be written still prints the stderr line the validator
// log has always carried, as well as the note in the record.
func TestCollectRefappSchedKeepsTheReadbackStderrLine(t *testing.T) {
	old := os.Stderr
	r, w, err := os.Pipe()
	if err != nil {
		t.Skip("no pipe")
	}
	os.Stderr = w
	defer func() { os.Stderr = old }()
	// A regular file where the directory should be: nothing can be created under it.
	missingDir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(missingDir, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	rec := collectRefappSched(context.Background(), envMap(map[string]string{stallCaptureEnv: "1"}), "local", procFixture(t), 100, missingDir, "")
	_ = w.Close()
	b, _ := io.ReadAll(r)
	if !strings.Contains(string(b), "validation: refapp readback: ") {
		t.Errorf("stderr %q lacks the readback line", b)
	}
	if rec == nil || !strings.Contains(rec.GOMAXPROCSNote, "readback could not be written") {
		t.Errorf("record %+v lacks the note", rec)
	}
}
