package validation

import (
	"context"
	"net"
	"testing"
	"time"
)

// TestArmWalkerCaptureStampsReadyAndHeartbeat: a record made after arming
// carries since_ready_ms from the stamped instant and has a heartbeat to
// consult; a zero stamp leaves since_ready_ms unset rather than measuring
// from 1970.
func TestArmWalkerCaptureStampsReadyAndHeartbeat(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var h2c h2cTally
	var ws wsTally
	ready := time.Now().Add(-90 * time.Second)
	armWalkerCapture(ctx, ready.UnixNano(), &h2c, &ws, nil)
	if h2c.capture.hb.Load() == nil || ws.capture.hb.Load() == nil {
		t.Fatal("heartbeat not armed on both tallies")
	}
	c1, c2 := net.Pipe()
	defer func() { _ = c1.Close(); _ = c2.Close() }()
	h2c.capture.record(c1, 0, 0, 2*time.Second, nil, "declined", "", 1, time.Now().Add(-2*time.Second))
	f := h2c.capture.slow.snapshot()[0]
	if f.SinceReadyMs < 89_000 || f.SinceReadyMs > 95_000 {
		t.Fatalf("since_ready_ms = %d, want ~90000", f.SinceReadyMs)
	}
	var unstamped h2cTally
	var unstampedWS wsTally
	armWalkerCapture(ctx, 0, &unstamped, &unstampedWS, nil)
	unstamped.capture.record(c1, 0, 0, 2*time.Second, nil, "declined", "", 1, time.Now())
	if got := unstamped.capture.slow.snapshot()[0].SinceReadyMs; got != 0 {
		t.Fatalf("unstamped tally must leave since_ready_ms at 0, got %d", got)
	}
}
