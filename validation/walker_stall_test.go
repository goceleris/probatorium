package validation

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"
)

// stallRecorder is an in-stall trigger that records when it fired.
type stallRecorder struct {
	mu    sync.Mutex
	kinds []string
	at    []time.Time
}

func (r *stallRecorder) fn(kind string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.kinds = append(r.kinds, kind)
	r.at = append(r.at, time.Now())
}

func (r *stallRecorder) get() ([]string, []time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.kinds...), append([]time.Time(nil), r.at...)
}

// TestWSStallTriggerFiresInsideASilentHandshake (celeris#588): a WS
// handshake whose read has waited wsStallThreshold with no byte back fires
// the in-stall trigger WHILE the read still waits -- before the fire's own
// 2 s deadline ends it -- which is the only moment a dossier can still see
// a stall shorter than the walker's budget. The first live fault-injected
// run showed why: the post-failure dossier caught an 8 s /ws hold 12 ms
// after its release.
func TestWSStallTriggerFiresInsideASilentHandshake(t *testing.T) {
	srv := newFakeH2CServer(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		time.Sleep(wsMaxHold + time.Second)
		_ = c.Close()
	})
	var tally wsTally
	var h2c h2cTally
	var rec stallRecorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	armWalkerCapture(ctx, 0, &h2c, &tally, rec.fn)
	start := time.Now()
	fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModePingFlood, &tally)
	end := time.Now()
	kinds, at := rec.get()
	if len(kinds) != 1 || kinds[0] != "ws" {
		t.Fatalf("in-stall trigger: kinds=%v, want exactly one \"ws\"", kinds)
	}
	if d := at[0].Sub(start); d < wsStallThreshold || !at[0].Before(end) {
		t.Fatalf("trigger fired %s after the fire started, fire ended %s after: want >= %s and before the fire returned",
			d, end.Sub(start), wsStallThreshold)
	}
}

// The negative control: a prompt handshake, and one answered just under the
// threshold, never fire the trigger -- a busy host must not turn every
// merely-slow fire into a dossier.
func TestWSStallTriggerQuietOnAPromptHandshake(t *testing.T) {
	for _, delay := range []time.Duration{0, wsStallThreshold - 400*time.Millisecond} {
		srv := newFakeWSServer(t, false, func(c net.Conn) {
			_, _ = c.Read(make([]byte, 4096))
			time.Sleep(delay)
			_, _ = c.Write([]byte{0x88, 0x02, 0x03, 0xEA})
		})
		var tally wsTally
		var h2c h2cTally
		var rec stallRecorder
		ctx, cancel := context.WithCancel(context.Background())
		armWalkerCapture(ctx, 0, &h2c, &tally, rec.fn)
		fireWSTorture(context.Background(), srv.HostPort(), "/ws", ModeUnmaskedClient, &tally)
		time.Sleep(wsStallThreshold) // a stray timer would fire by now
		cancel()
		if kinds, _ := rec.get(); len(kinds) != 0 {
			t.Errorf("answered after %s: trigger fired %v, want nothing", delay, kinds)
		}
	}
}

// The h2c side: the preamble read waited h2cStallThreshold with no byte ->
// the trigger fires before the read ends (here the server closes at 3.5 s).
func TestH2CStallTriggerFiresBeforeTheReadEnds(t *testing.T) {
	const hold = h2cStallThreshold + 500*time.Millisecond
	srv := newFakeH2CServer(t, func(c net.Conn) {
		_, _ = c.Read(make([]byte, 4096))
		time.Sleep(hold)
		_ = c.Close()
	})
	var tally h2cTally
	var ws wsTally
	var rec stallRecorder
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	armWalkerCapture(ctx, 0, &tally, &ws, rec.fn)
	start := time.Now()
	fireH2CChurn(context.Background(), srv.HostPort(), ChurnRSTAfter101, &tally)
	end := time.Now()
	kinds, at := rec.get()
	if len(kinds) != 1 || kinds[0] != "h2c" {
		t.Fatalf("in-stall trigger: kinds=%v, want exactly one \"h2c\"", kinds)
	}
	if d := at[0].Sub(start); d < h2cStallThreshold || !at[0].Before(end) {
		t.Fatalf("trigger fired %s after the fire started, fire ended %s after", d, end.Sub(start))
	}
}

// Unarmed (unit tests, or a caller that passes nil): watchRead is free and
// never fires.
func TestWatchReadUnarmedIsANoOp(t *testing.T) {
	var c fireCapture
	disarm := c.watchRead(time.Nanosecond)
	time.Sleep(5 * time.Millisecond)
	disarm()
	if c.onStall.Load() != nil {
		t.Fatal("unarmed capture grew a trigger")
	}
}
