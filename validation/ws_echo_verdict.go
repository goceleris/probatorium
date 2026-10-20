package validation

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// The ws_echo timeout discriminator (probatorium#481, celeris#871).
//
// The race tier's adaptive/amd64 cell times out a 64 KiB echo fire: at the
// failing fire's end the server socket holds hundreds of KiB unread in the
// kernel and the handler waits for the rest of a frame. Two readings fit one
// snapshot of that: the refapp is SLOW (a race-instrumented process that has
// not got to the socket yet), or it is WEDGED (the bytes will never be read).
// One ss snapshot cannot tell them apart; two a few seconds apart can, but
// not from queue LEVELS: the client's kernel Send-Q (3 MB in the incident)
// keeps refilling the server's Recv-Q every time the server reads and its
// window reopens, and the server's own Send-Q keeps draining into a reading
// client. A flat Recv-Q therefore proves nothing about the handler, and bytes
// reaching the client prove nothing about it either. What the application did
// is in two exact counters that ss prints for the server socket:
//
//	app reads   = delta(bytes_received) - delta(Recv-Q)   (Recv-Q is rcv_nxt-copied_seq)
//	app writes  = delta(bytes_acked + Send-Q)             (every byte the app handed over)
//
// and the verdict is drawn from those:
//
//	SLOW_CLASS   the server application read bytes, or wrote more than a
//	             ping or a close frame could account for (wsEchoMinWrite):
//	             it is working, only late.
//	WEDGE_CLASS  neither counter moved, Recv-Q is above zero at both
//	             instants, and every byte that reached the client is
//	             accounted for by what the client and server kernels already
//	             queued at t0: the application did nothing with bytes that
//	             were there for it.
//	INDETERMINATE  anything else, above all missing data. A socket that
//	             could not be found, an ss that failed, a counter that is
//	             absent (bytes_received, bytes_acked), counters that went
//	             backwards, bytes at the client that no queue explains, or an
//	             empty queue is not evidence of a wedge and is never booked
//	             as one.
//
// The classifier and the parser are plain Go with no build tag, so they run
// on any host; only the capture that feeds them (ws_echo_diag.go) is a host
// action.

// Verdict classes. The strings are the machine-readable values of the
// WS_ECHO_VERDICT key.
const (
	verdictSlow          = "SLOW_CLASS"
	verdictWedge         = "WEDGE_CLASS"
	verdictIndeterminate = "INDETERMINATE"
)

// wsEchoMinWindow is the shortest gap between the two instants a verdict is
// drawn from. A shorter one (a cancelled run, a clock that stepped) proves
// nothing about progress. A var so the walker's unit tests can run a whole
// diagnosis in a fraction of a second.
var wsEchoMinWindow = 2 * time.Second

// wsEchoMinWrite is the least the server application must have written in the
// window for the write counter to count as progress. A WebSocket control frame
// from the server (ping, pong, close) is at most 2+125 bytes, so anything
// below this can be a keepalive or a close frame from a wedged server, not an
// echo.
const wsEchoMinWrite = 256

// ssSock is one socket of `ss -tmi` output.
type ssSock struct {
	State        string
	RecvQ, SendQ int64
	Local, Peer  string
	// Info holds every key:value token of the detail lines (bytes_received,
	// rcv_wnd, lastrcv, rwnd_limited, ...), values verbatim.
	Info map[string]string
	// skmem holds the fields of skmem:(r..,rb..,t..,tb..,f..,w..,o..,bl..,d..).
	skmem map[string]int64
	// Raw is the socket's lines as printed.
	Raw string
}

// Int returns the numeric value of an Info key.
func (s *ssSock) Int(key string) (int64, bool) {
	if s == nil {
		return 0, false
	}
	v, ok := s.Info[key]
	if !ok {
		return 0, false
	}
	n, err := strconv.ParseInt(v, 10, 64)
	return n, err == nil
}

// Skmem returns one field of the skmem tuple ("r", "rb", "t", "tb", "f",
// "w", "o", "bl", "d").
func (s *ssSock) Skmem(key string) (int64, bool) {
	if s == nil {
		return 0, false
	}
	v, ok := s.skmem[key]
	return v, ok
}

// portOf is the port of an ss address ("127.0.0.1:50278", "[::1]:80",
// "*:80"), -1 when it has none.
func portOf(addr string) int {
	i := strings.LastIndexByte(addr, ':')
	if i < 0 {
		return -1
	}
	n, err := strconv.Atoi(addr[i+1:])
	if err != nil {
		return -1
	}
	return n
}

var (
	ssSkmemRe   = regexp.MustCompile(`skmem:\(([^)]*)\)`)
	ssSkmemFld  = regexp.MustCompile(`^([a-z]+)(\d+)$`)
	ssInfoTokRe = regexp.MustCompile(`^([A-Za-z_][A-Za-z0-9_]*):(\S+)$`)
)

// parseSS parses `ss -tmi` (also -tanpi) output: a header, then per socket
// one line (state, Recv-Q, Send-Q, local, peer, optional process) followed by
// indented detail lines. Lines it cannot read are skipped; it never fails.
func parseSS(text string) []ssSock {
	var out []ssSock
	var cur *ssSock
	flush := func() {
		if cur != nil {
			out = append(out, *cur)
			cur = nil
		}
	}
	for _, line := range strings.Split(text, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		if line[0] != ' ' && line[0] != '\t' {
			flush()
			f := strings.Fields(line)
			if len(f) < 5 || f[0] == "State" || strings.HasPrefix(f[0], "###") {
				continue
			}
			rq, err1 := strconv.ParseInt(f[1], 10, 64)
			sq, err2 := strconv.ParseInt(f[2], 10, 64)
			if err1 != nil || err2 != nil {
				continue
			}
			cur = &ssSock{State: f[0], RecvQ: rq, SendQ: sq, Local: f[3], Peer: f[4],
				Info: map[string]string{}, skmem: map[string]int64{}, Raw: line + "\n"}
			continue
		}
		if cur == nil {
			continue
		}
		cur.Raw += line + "\n"
		if m := ssSkmemRe.FindStringSubmatch(line); m != nil {
			for _, fld := range strings.Split(m[1], ",") {
				if g := ssSkmemFld.FindStringSubmatch(fld); g != nil {
					if n, err := strconv.ParseInt(g[2], 10, 64); err == nil {
						cur.skmem[g[1]] = n
					}
				}
			}
		}
		for _, tok := range strings.Fields(line) {
			if g := ssInfoTokRe.FindStringSubmatch(tok); g != nil && g[1] != "skmem" {
				cur.Info[g[1]] = g[2]
			}
		}
	}
	flush()
	return out
}

// findEchoSocks picks the two ends of the echo connection out of socks, given
// the client's ephemeral port and the refapp's listening port (the remote port
// of the walker's connection): the client end is the socket whose LOCAL port
// is clientPort and whose peer port is serverPort, the server end the one whose
// local port is serverPort and whose PEER port is clientPort. Either is nil
// when absent. Matching the port PAIR matters: Linux reuses an ephemeral port
// number for connections to different destinations (the validator's polls of
// the refapp's side listener, for one), and the number alone would pick the
// wrong socket. The client and the refapp share a host (the validator drives a
// loopback refapp), so both ends are in one ss.
func findEchoSocks(socks []ssSock, clientPort, serverPort int) (srv, cli *ssSock) {
	if clientPort <= 0 || serverPort <= 0 {
		return nil, nil
	}
	for i := range socks {
		s := &socks[i]
		lp, pp := portOf(s.Local), portOf(s.Peer)
		switch {
		case lp == clientPort && pp == serverPort && cli == nil:
			cli = s
		case lp == serverPort && pp == clientPort && srv == nil:
			srv = s
		}
	}
	return srv, cli
}

// echoInstant is the host's view of the echo connection at one instant: the
// walker's echo counters, and the two sockets as ss printed them.
type echoInstant struct {
	At time.Time
	// Echoes is the number of echo frames the walker had read back, Sent the
	// highest sequence it had written, BytesIn the bytes it had pulled off the
	// socket in all. BytesIn is the progress counter: it survives a read cut in
	// the middle of a frame, which Echoes does not.
	Echoes, Sent, BytesIn uint64
	// SSErr says why no ss output exists ("" when it ran).
	SSErr string
	// Server and Client are nil when the socket was not in the output.
	Server, Client *ssSock
}

// wsEchoVerdict is the discriminator's answer.
type wsEchoVerdict struct {
	Class  string // verdictSlow, verdictWedge or verdictIndeterminate
	Reason string
	// Line is the machine-readable record: one line, starting
	// "WS_ECHO_VERDICT=<class> ", every number the class rests on as key=value.
	Line string
}

// wsEchoClassify draws the verdict from the two instants. t0 is the earlier;
// At of t0 is when the client resumed reading and At of t1 when it stopped, so
// the window is the time the walker spent reading.
//
// Both server sockets must be found and carry bytes_received and bytes_acked:
// the verdict rests on the application's own read and write counters (see the
// header), never on queue levels. Any of those missing is INDETERMINATE, as is
// a counter that went backwards (a different socket on the same port). Reads
// above zero or writes of at least wsEchoMinWrite are SLOW. With neither, a
// server Recv-Q above zero at both instants is a handler that did not read
// bytes that were there for it, and only then is it WEDGE, and only when the
// bytes the walker took off the socket are explained by queues that existed
// at t0 (a contradiction between the walker's counter and ss is not a wedge).
func wsEchoClassify(t0, t1 echoInstant) wsEchoVerdict {
	window := t1.At.Sub(t0.At)
	class, reason := verdictIndeterminate, ""
	reads, haveReads := srvReads(t0, t1)
	writes, haveWrites := srvWrites(t0, t1)
	switch {
	case window < wsEchoMinWindow:
		reason = "window_too_short"
	case t0.Server == nil:
		reason = "server_socket_missing_t0"
	case t1.Server == nil:
		reason = "server_socket_missing_t1"
	case !haveReads || !haveWrites:
		reason = "server_counters_missing"
	case reads < 0 || writes < 0:
		reason = "server_counters_inconsistent"
	case reads > 0 && writes >= wsEchoMinWrite:
		class, reason = verdictSlow, "server_read_and_wrote"
	case reads > 0:
		class, reason = verdictSlow, "server_read"
	case writes >= wsEchoMinWrite:
		class, reason = verdictSlow, "server_wrote"
	case t0.Server.RecvQ == 0 || t1.Server.RecvQ == 0:
		reason = "recvq_zero_no_progress"
	case clientBytesUnexplained(t0, t1, writes):
		reason = "client_bytes_unexplained"
	default:
		class, reason = verdictWedge, "no_app_read_no_app_write_recvq_pending"
	}
	return wsEchoVerdict{Class: class, Reason: reason, Line: verdictLine(class, reason, window, t0, t1)}
}

// srvReads is how many bytes the server application took off its socket
// between the instants: the growth of bytes_received (rcv_nxt) less the growth
// of the receive queue (rcv_nxt - copied_seq) is the growth of copied_seq.
// ok is false when either end lacks the socket or the counter.
func srvReads(t0, t1 echoInstant) (int64, bool) {
	r0, ok0 := t0.Server.Int("bytes_received")
	r1, ok1 := t1.Server.Int("bytes_received")
	if t0.Server == nil || t1.Server == nil || !ok0 || !ok1 {
		return 0, false
	}
	return (r1 - r0) - (t1.Server.RecvQ - t0.Server.RecvQ), true
}

// srvWrites is how many bytes the server application handed to its socket
// between the instants: bytes_acked plus the send queue is the total written,
// so a queue draining into a reading client does not move it.
func srvWrites(t0, t1 echoInstant) (int64, bool) {
	a0, ok0 := t0.Server.Int("bytes_acked")
	a1, ok1 := t1.Server.Int("bytes_acked")
	if t0.Server == nil || t1.Server == nil || !ok0 || !ok1 {
		return 0, false
	}
	return (a1 + t1.Server.SendQ) - (a0 + t0.Server.SendQ), true
}

// clientBytesUnexplained reports whether the walker took more bytes off the
// socket in the window than the kernels could have supplied: what the
// client's receive queue and the server's send queue held at t0, plus what the
// server application wrote. Such a contradiction between the walker's counter
// and ss means one of them is not about the connection it should be, and no
// class may rest on it. A client socket missing at t0 supplies nothing to
// subtract, so the surplus then counts in full.
func clientBytesUnexplained(t0, t1 echoInstant, writes int64) bool {
	arrived := int64(t1.BytesIn) - int64(t0.BytesIn)
	supply := t0.Server.SendQ + writes
	if t0.Client != nil {
		supply += t0.Client.RecvQ
	}
	return arrived > supply
}

// bytesNew is the bytes the walker took off the socket between the instants
// beyond what its own kernel queue already held at t0. With no client socket
// in the t0 ss output nothing is subtracted. It is on the verdict line for
// audit and decides nothing: bytes can also come from the server's Send-Q.
func bytesNew(t0, t1 echoInstant) int64 {
	n := int64(t1.BytesIn) - int64(t0.BytesIn)
	if t0.Client != nil {
		n -= t0.Client.RecvQ
	}
	return n
}

// verdictLine formats the record. An absent number is "na", never zero.
func verdictLine(class, reason string, window time.Duration, t0, t1 echoInstant) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WS_ECHO_VERDICT=%s reason=%s window_ms=%d echoes_t0=%d echoes_t1=%d bytes_in_t0=%d bytes_in_t1=%d bytes_new=%d sent=%d",
		class, reason, window.Milliseconds(), t0.Echoes, t1.Echoes, t0.BytesIn, t1.BytesIn, bytesNew(t0, t1), max(t0.Sent, t1.Sent))
	kv := func(name string, v int64, ok bool) {
		if ok {
			fmt.Fprintf(&b, " %s=%d", name, v)
		} else {
			fmt.Fprintf(&b, " %s=na", name)
		}
	}
	reads, haveReads := srvReads(t0, t1)
	kv("srv_app_reads", reads, haveReads)
	writes, haveWrites := srvWrites(t0, t1)
	kv("srv_app_writes", writes, haveWrites)
	for i, in := range []echoInstant{t0, t1} {
		t := fmt.Sprintf("t%d", i)
		kv("srv_recvq_"+t, recvQ(in.Server), in.Server != nil)
		kv("srv_sendq_"+t, sendQ(in.Server), in.Server != nil)
		n, ok := in.Server.Int("bytes_received")
		kv("srv_bytes_received_"+t, n, ok)
		n, ok = in.Server.Int("bytes_acked")
		kv("srv_bytes_acked_"+t, n, ok)
		n, ok = in.Server.Skmem("r")
		kv("srv_skmem_r_"+t, n, ok)
		n, ok = in.Server.Int("rcv_wnd")
		kv("srv_rcv_wnd_"+t, n, ok)
		kv("cli_sendq_"+t, sendQ(in.Client), in.Client != nil)
		kv("cli_recvq_"+t, recvQ(in.Client), in.Client != nil)
		// Bytes the refapp has handed to the server socket in all (acked plus
		// still queued): moves only when the application writes.
		acked, ok := in.Server.Int("bytes_acked")
		kv("srv_written_"+t, acked+sendQ(in.Server), ok && in.Server != nil)
	}
	return b.String()
}

func recvQ(s *ssSock) int64 {
	if s == nil {
		return 0
	}
	return s.RecvQ
}

func sendQ(s *ssSock) int64 {
	if s == nil {
		return 0
	}
	return s.SendQ
}
