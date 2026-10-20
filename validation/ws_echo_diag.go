package validation

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strings"
	"sync/atomic"
	"syscall"
	"time"
)

// Two-instant capture of a timed-out ws_echo fire (probatorium#481, celeris#871).
//
// A stall-capture run only. When a fire reaches wsEchoMaxHold with echoes
// outstanding, the walker does not close the connection and count the
// timeout at once. It stops the sender, so nothing new enters the server's
// receive queue, and keeps READING for wsEchoDiagWindow: a client that stops
// reading would fill the server's send queue and park the handler in a write,
// and the receive queue would then sit flat for a reason that has nothing to
// do with the server being slow. At the start and at the end of that window
// it takes the host's view of the connection (ss -tmi of both ends, the
// walker's echo counters, the refapp's thread table and engine counters),
// classifies the pair (ws_echo_verdict.go) and hands the record to the
// orchestrator, which writes it into a record-only dossier.
//
// The timeout is counted only after the second instant: the tally tick turns
// a ws_echo_timeout into a hard I-WS-ECHO incident, which cancels the cell
// and kills the refapp, and the second snapshot would be lost with it.
//
// The extended fire holds its connection about wsEchoDiagWindow longer than
// wsEchoFireBound allows. That is bounded (one fire, in a cell that is about
// to end on the hard incident) and is far under I-CONN-1's deadline
// (TestStreamHoldsStayUnderTheConnCloseBound).

// wsEchoDiagWindow is the gap between the two instants.
var wsEchoDiagWindow = 3 * time.Second

// The two host calls of one instant are capped separately, and the window is
// measured AFTER the first instant's calls and BEFORE the second's, so a slow
// call can neither eat the window nor stretch it. The refapp leg is the one
// that can be slow: it is served by a goroutine of the very process whose
// scheduling is in question.
var (
	wsEchoDiagSSBudget     = 2 * time.Second
	wsEchoDiagRefappBudget = 1500 * time.Millisecond
)

// wsEchoDiagPerCell bounds the diagnosed fires per cell: each holds the
// walker for the window, and a second one in a cell that is already ending
// adds nothing.
const wsEchoDiagPerCell = 3

// wsEchoDiag is what a stall-capture run gives the ws_echo walker. A nil
// *wsEchoDiag (every other run) leaves fireWSLargeEcho exactly as it was.
type wsEchoDiag struct {
	// SS returns `ss -tmi` text for the connection whose client port is
	// given (both ends). nil runs the host's ss.
	SS func(ctx context.Context, clientPort int) (string, error)
	// Refapp returns the refapp's state at this instant as text (thread
	// masks and states, GOMAXPROCS, engine counters); nil adds nothing.
	Refapp func(ctx context.Context) string
	// Deliver receives the finished record; nil drops it.
	Deliver func(ctx context.Context, rep wsEchoDiagReport)

	taken atomic.Int32
}

// wsEchoDiagReport is one diagnosed fire.
type wsEchoDiagReport struct {
	Verdict wsEchoVerdict
	// Text is the whole record: the verdict line, then both instants.
	Text string
	At   time.Time
}

// claim reserves one of the cell's diagnoses; false when spent.
func (d *wsEchoDiag) claim() bool {
	if d == nil {
		return false
	}
	if d.taken.Add(1) > wsEchoDiagPerCell {
		d.taken.Add(-1)
		return false
	}
	return true
}

// defaultSS runs the host's ss for the two ends of one loopback connection.
func defaultSS(ctx context.Context, clientPort int) (string, error) {
	if !hasBinary("ss") {
		return "", errors.New("ss not available on this host")
	}
	expr := fmt.Sprintf("( sport = :%d or dport = :%d )", clientPort, clientPort)
	out, err := exec.CommandContext(ctx, "ss", "-tmin", expr).CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("ss -tmin: %w", err)
	}
	return string(out), nil
}

// instant takes one instant's view: the counters first, then ss, then the
// refapp. The text is what goes into the dossier for it.
func (d *wsEchoDiag) instant(ctx context.Context, label string, clientPort int, received, sent, bytesIn *atomic.Uint64) (echoInstant, string) {
	in := echoInstant{At: time.Now(), Echoes: received.Load(), Sent: sent.Load(), BytesIn: bytesIn.Load()}
	var b strings.Builder
	fmt.Fprintf(&b, "### %s %s echoes=%d bytes_in=%d sent=%d client_port=%d\n", label, in.At.UTC().Format(time.RFC3339Nano), in.Echoes, in.BytesIn, in.Sent, clientPort)
	ss := d.SS
	if ss == nil {
		ss = defaultSS
	}
	sctx, cancel := context.WithTimeout(ctx, wsEchoDiagSSBudget)
	text, err := ss(sctx, clientPort)
	cancel()
	if err != nil {
		in.SSErr = err.Error()
		fmt.Fprintf(&b, "ss error: %v\n", err)
	}
	b.WriteString(text)
	if !strings.HasSuffix(text, "\n") {
		b.WriteString("\n")
	}
	in.Server, in.Client = findEchoSocks(parseSS(text), clientPort)
	if in.Server == nil && in.SSErr == "" {
		fmt.Fprintf(&b, "note: no server-end socket with peer port %d in the ss output\n", clientPort)
	}
	if d.Refapp != nil {
		state, ok := callWithin(ctx, wsEchoDiagRefappBudget, d.Refapp)
		if !ok {
			state = fmt.Sprintf("refapp state not read within %s (the refapp's own listener did not answer; this is a finding)", wsEchoDiagRefappBudget)
		}
		fmt.Fprintf(&b, "--- refapp state at %s\n%s\n", label, strings.TrimRight(state, "\n"))
	}
	return in, b.String()
}

// callWithin runs fn with a context that ends after d, and gives up waiting
// for it then: ok is false when fn had not returned. fn may keep running in the
// background until it notices its context; the caller must not need it.
func callWithin(ctx context.Context, d time.Duration, fn func(context.Context) string) (string, bool) {
	cctx, cancel := context.WithTimeout(ctx, d)
	defer cancel()
	done := make(chan string, 1)
	go func() { done <- fn(cctx) }()
	select {
	case s := <-done:
		return s, true
	case <-cctx.Done():
		select {
		case s := <-done:
			return s, true
		default:
			return "", false
		}
	}
}

// run is the diagnosis of one timed-out fire: conn is still open and the
// sender already stopped and joined. received counts the echo frames the
// reader has taken, sent is the highest sequence written, bytesIn the bytes
// pulled off the socket (see countingConn).
func (d *wsEchoDiag) run(ctx context.Context, conn net.Conn, br *bufio.Reader, received, sent, bytesIn *atomic.Uint64) wsEchoDiagReport {
	clientPort := 0
	if a, ok := conn.LocalAddr().(*net.TCPAddr); ok {
		clientPort = a.Port
	}
	t0, text0 := d.instant(ctx, "t0", clientPort, received, sent, bytesIn)
	// The window starts when the client starts reading again, after the first
	// instant's host calls, so a slow call cannot eat it.
	t0.At = time.Now()
	end := wsEchoDiagDrain(ctx, conn, br, t0.At.Add(wsEchoDiagWindow))
	t1, text1 := d.instant(ctx, "t1", clientPort, received, sent, bytesIn)
	v := wsEchoClassify(t0, t1)
	v.Line += " window_end=" + end
	var b strings.Builder
	b.WriteString(v.Line + "\n")
	fmt.Fprintf(&b, "# window %s read, configured %s; the sender was stopped and the walker kept reading between the instants\n",
		t1.At.Sub(t0.At).Round(time.Millisecond), wsEchoDiagWindow)
	b.WriteString(text0)
	b.WriteString(text1)
	return wsEchoDiagReport{Verdict: v, Text: b.String(), At: t1.At}
}

// countingConn counts the bytes pulled off the socket. The fire reads
// through it so the diagnosis has a progress counter that does not depend on
// frame boundaries: the timeout cuts a read wherever it falls, often inside a
// 64 KiB frame, and a frame parser resumed from there would be out of step.
type countingConn struct {
	net.Conn
	n *atomic.Uint64
}

func (c countingConn) Read(p []byte) (int, error) {
	n, err := c.Conn.Read(p)
	c.n.Add(uint64(n))
	return n, err
}

// wsEchoDiagDrain keeps taking whatever the server sends until the deadline,
// as raw bytes (the read that timed out may have been cut inside a frame), and
// says how the window ended: "deadline", "server_closed", "ctx_cancelled" or
// "read_error".
func wsEchoDiagDrain(ctx context.Context, conn net.Conn, br *bufio.Reader, until time.Time) string {
	stopWatch := context.AfterFunc(ctx, func() { _ = conn.SetReadDeadline(time.Now()) })
	defer stopWatch()
	buf := make([]byte, 32<<10)
	for {
		_ = conn.SetReadDeadline(until)
		// The cancel watcher may have set an expired deadline just before the
		// line above replaced it; checked after, a cancel that landed first is
		// still seen, and one that lands later sets the deadline again.
		if ctx.Err() != nil {
			return "ctx_cancelled"
		}
		_, err := br.Read(buf)
		switch {
		case err == nil:
		case ctx.Err() != nil:
			return "ctx_cancelled"
		case errors.Is(err, os.ErrDeadlineExceeded):
			return "deadline"
		case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF), errors.Is(err, syscall.ECONNRESET), errors.Is(err, syscall.EPIPE):
			return "server_closed"
		default:
			return "read_error"
		}
	}
}
