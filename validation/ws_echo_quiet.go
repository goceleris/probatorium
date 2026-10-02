package validation

import "time"

// Quiet gaps in the WebSocket large-echo slice (probatorium#478 item 1).
//
// I-MEM-1 judges heap_inuse at its 150 s bucket TROUGHS, the lowest sample
// of each bucket (properties/i_mem.go). On the native engines, a fire's echo
// backlog waits in the engine's user-space write buffer, on the refapp's Go
// heap: up to wsEchoMaxInFlight 64 KiB frames the throttled reader has not
// taken yet (celeris middleware/websocket WSRawWriteFn into the engine
// writeBuf). The walker ran one fire straight after another, so nearly every
// sample of an auth_session_ratelimit cell carried somebody's backlog, and a
// bucket's trough was set by whichever GC happened to land on a nearly empty
// one. In soak 36433207097 that scattered the native-engine troughs by
// 0.6-1.6 MB, against 44-84 KB in the 2026-09-11 soak, which had no such
// walker, and it failed I-MEM-1 353 times on arm64 epoll with no leak
// (probatorium#466).
//
// So the walker now holds NO connection during the first wsEchoQuietGap of
// every wsEchoQuietPeriod, on a grid aligned to the Unix epoch. Inside a gap
// the last fire's backlog is garbage, the next GC frees it, and the samples
// the property loop takes there are the refapp's own heap; those are the
// samples a bucket minimum comes from. I-MEM-1 itself is unchanged.
//
// The two numbers:
//
//   - wsEchoQuietPeriod + wsEchoQuietGap <= 75 s, half an I-MEM-1 bucket. The
//     evaluator keeps a bucket only when it holds at least half a bucket of
//     samples, so every bucket it judges, a trailing one of 75 samples
//     included, holds one whole gap, whatever the alignment of its grid.
//   - The gap is a third of the period. Fewer clean samples per bucket make a
//     noisier minimum: measured on the 09-11 soak's 32 native-engine cells, a
//     trough taken from the gap samples alone scatters 1.43x as much as one
//     taken from the whole bucket (median; 2.4x at a 10 s gap).
//
// The cost is ws_echo coverage. A fire may only start when it is sure to be
// closed before the next gap opens (wsEchoFireBound), so about 60 % of the
// fires an uninterrupted walker made still happen.
const (
	wsEchoQuietPeriodDefault = 54 * time.Second
	wsEchoQuietGapDefault    = 18 * time.Second
)

// The schedule the walker follows. Vars, so the unit tests can run several
// periods in a few seconds, and so the package's other tests, which drive
// the walker for a second, can turn the gaps off (TestMain).
var (
	wsEchoQuietPeriod = wsEchoQuietPeriodDefault
	wsEchoQuietGap    = wsEchoQuietGapDefault
)

// wsEchoFireBound is the longest one fire can hold its connection: the dial
// (net.Dialer.Timeout is wsEchoMaxHold), then the stream and the drain (the
// connection deadline, wsEchoMaxHold after the dial), then the close
// handshake (wsEchoCloseWait).
func wsEchoFireBound() time.Duration { return 2*wsEchoMaxHold + wsEchoCloseWait }

// wsEchoQuietWait is how long the walker must wait at now before it may
// fire. It is zero when a fire started now is closed before the next gap
// opens. Otherwise it runs to the end of the gap now is in, or of the next
// gap, which a fire started now could still be holding a connection into. A
// non-positive period or gap turns the gaps off.
func wsEchoQuietWait(now time.Time) time.Duration {
	period, gap := wsEchoQuietPeriod, wsEchoQuietGap
	if period <= 0 || gap <= 0 {
		return 0
	}
	ns := now.UnixNano()
	gapStart := ns - ns%int64(period)
	if ns < gapStart+int64(gap) {
		return time.Duration(gapStart + int64(gap) - ns)
	}
	next := gapStart + int64(period)
	if ns+int64(wsEchoFireBound()) > next {
		return time.Duration(next + int64(gap) - ns)
	}
	return 0
}
