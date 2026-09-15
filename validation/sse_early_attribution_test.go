package validation

import (
	"fmt"
	"io"
	"net"
	"strings"
	"syscall"
	"testing"

	"os"
)

// TestSSEEarlyCloseIsAttributed guards the split that made soak 34616620237's
// single failure diagnosable.
//
// That run failed the absolute-zero gate on one counter:
//
//	auth_session_ratelimit  epoll  arm64  sse_server_closed_early  1
//
// Six independent code reviews could not determine what closed the stream,
// because the walker classified purely on TIMING -- any read error more than
// 50ms before its own deadline became "the server closed it" -- and then
// discarded the error. A clean FIN and a transport reset are different
// events with different owners, and the counter asserted the first for both.
func TestSSEEarlyCloseIsAttributed(t *testing.T) {
	cases := []struct {
		name  string
		err   error
		count func(sseSnapshot) int64
		field string
	}{
		{
			name:  "server sent FIN — the only one the old name actually claimed",
			err:   io.EOF,
			count: func(s sseSnapshot) int64 { return s.ServerClosedEarly },
			field: "sse_server_closed_early",
		},
		{
			name:  "connection reset — engine or transport, not distinguishable from here",
			err:   &net.OpError{Op: "read", Err: syscall.ECONNRESET},
			count: func(s sseSnapshot) int64 { return s.PeerResetEarly },
			field: "sse_peer_reset_early",
		},
		{
			name:  "an errno the walker did not anticipate",
			err:   &net.OpError{Op: "read", Err: syscall.ENOBUFS},
			count: func(s sseSnapshot) int64 { return s.ReadErrEarly },
			field: "sse_read_err_early",
		},
		{
			name:  "wrapped EOF still reads as a server close",
			err:   fmt.Errorf("reading stream: %w", io.EOF),
			count: func(s sseSnapshot) int64 { return s.ServerClosedEarly },
			field: "sse_server_closed_early",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var tally sseTally
			tally.recordEarly(tc.err)
			got := tally.snapshot()
			if n := tc.count(got); n != 1 {
				t.Fatalf("%s = %d, want 1 for %v", tc.field, n, tc.err)
			}
			// Exactly one bucket, or the split has not separated anything.
			if total := got.ServerClosedEarly + got.PeerResetEarly + got.ReadErrEarly; total != 1 {
				t.Fatalf("error landed in %d buckets, want exactly 1", total)
			}
		})
	}
}

// TestSSEEarlyCloseKeepsTheError is the whole point. Counting an error and
// throwing it away is what made one occurrence unattributable across six
// reviews; the next one must carry its own evidence.
func TestSSEEarlyCloseKeepsTheError(t *testing.T) {
	var tally sseTally
	tally.recordEarly(&net.OpError{Op: "read", Err: syscall.ECONNRESET})
	errs := tally.snapshot().EarlyErrs
	if len(errs) != 1 {
		t.Fatalf("kept %d error strings, want 1", len(errs))
	}
	if !strings.Contains(errs[0], "reset") {
		t.Fatalf("retained error %q does not name the cause", errs[0])
	}
}

// TestSSEEarlyErrsAreBounded: a systematic failure must not turn the cell
// that is already failing into a memory problem.
func TestSSEEarlyErrsAreBounded(t *testing.T) {
	var tally tallyAlias
	for range sseMaxEarlyErrs * 10 {
		tally.recordEarly(io.EOF)
	}
	s := tally.snapshot()
	if len(s.EarlyErrs) > sseMaxEarlyErrs {
		t.Fatalf("kept %d error strings, want at most %d", len(s.EarlyErrs), sseMaxEarlyErrs)
	}
	if s.ServerClosedEarly != int64(sseMaxEarlyErrs*10) {
		t.Fatalf("counter = %d, want %d: retention is bounded but COUNTING must not be",
			s.ServerClosedEarly, sseMaxEarlyErrs*10)
	}
}

type tallyAlias = sseTally

// TestSSEAllThreeEarlyCountersAreGated is the guard against the tempting
// mistake this split invites: splitting a failing counter into named parts
// and gating only the part that happens to be zero. All three fail the
// absolute-zero gate. The split is for attribution, not tolerance.
func TestSSEAllThreeEarlyCountersAreGated(t *testing.T) {
	// Mirrors report.invariantCounters; kept here so a change to the gate
	// that drops one of these fails a test in the package that produces them.
	for _, field := range []string{"sse_server_closed_early", "sse_peer_reset_early", "sse_read_err_early"} {
		if !gateChecksSSECounter(t, field) {
			t.Errorf("%s is not gated; splitting a failing counter and gating only some of "+
				"the pieces would silently weaken the absolute-zero gate", field)
		}
	}
}

func gateChecksSSECounter(t *testing.T, field string) bool {
	t.Helper()
	src, err := readGateSource()
	if err != nil {
		t.Fatalf("read gate source: %v", err)
	}
	return strings.Contains(src, `"`+field+`"`)
}

func readGateSource() (string, error) {
	b, err := os.ReadFile("../report/gate.go")
	return string(b), err
}
