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
// One ss snapshot cannot tell them apart; two a few seconds apart, with the
// walker still draining the echoes in between, can:
//
//	SLOW_CLASS   the server read bytes (Recv-Q fell) or echoes arrived
//	             between the two instants: it is delivering, only late.
//	WEDGE_CLASS  bytes sit unread (Recv-Q above zero at both instants and not
//	             falling) and no echo arrived: it delivers nothing.
//	INDETERMINATE  anything else, above all missing data. A socket that
//	             could not be found, an ss that failed, or an empty queue is
//	             not evidence of a wedge and is never booked as one.
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
// nothing about progress.
const wsEchoMinWindow = 2 * time.Second

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
// the client's ephemeral port: the client end is the socket whose LOCAL port
// is it, the server end the one whose PEER port is it (and whose local port
// is not). Either is nil when absent. The client and the refapp share a host
// (the validator drives a loopback refapp), so both ends are in one ss.
func findEchoSocks(socks []ssSock, clientPort int) (srv, cli *ssSock) {
	if clientPort <= 0 {
		return nil, nil
	}
	for i := range socks {
		s := &socks[i]
		lp, pp := portOf(s.Local), portOf(s.Peer)
		switch {
		case lp == clientPort && pp != clientPort && cli == nil:
			cli = s
		case pp == clientPort && lp != clientPort && srv == nil:
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
	// highest sequence it had written.
	Echoes, Sent uint64
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

// wsEchoClassify draws the verdict from the two instants. t0 is the earlier.
//
// Order matters. Echo progress is the walker's own counter and needs no ss,
// so it decides first: an echo that arrived in the window is a server that
// delivers. Next the data must exist (both server sockets found) and the
// window must be long enough. Then Recv-Q: a fall is a server that reads; a
// queue above zero at both instants that did not fall, with no echo, is one
// that does not. Everything else is INDETERMINATE.
func wsEchoClassify(t0, t1 echoInstant) wsEchoVerdict {
	window := t1.At.Sub(t0.At)
	class, reason := verdictIndeterminate, ""
	switch {
	case window < wsEchoMinWindow:
		reason = "window_too_short"
	case t1.Echoes > t0.Echoes:
		class, reason = verdictSlow, "echo_progress"
	case t0.Server == nil:
		reason = "server_socket_missing_t0"
	case t1.Server == nil:
		reason = "server_socket_missing_t1"
	case t1.Server.RecvQ < t0.Server.RecvQ:
		class, reason = verdictSlow, "recvq_drained"
	case t0.Server.RecvQ > 0 && t1.Server.RecvQ == t0.Server.RecvQ:
		class, reason = verdictWedge, "recvq_flat_no_progress"
	case t0.Server.RecvQ > 0:
		class, reason = verdictWedge, "recvq_grew_no_progress"
	default:
		reason = "recvq_zero_no_progress"
	}
	return wsEchoVerdict{Class: class, Reason: reason, Line: verdictLine(class, reason, window, t0, t1)}
}

// verdictLine formats the record. An absent number is "na", never zero.
func verdictLine(class, reason string, window time.Duration, t0, t1 echoInstant) string {
	var b strings.Builder
	fmt.Fprintf(&b, "WS_ECHO_VERDICT=%s reason=%s window_ms=%d echoes_t0=%d echoes_t1=%d sent=%d",
		class, reason, window.Milliseconds(), t0.Echoes, t1.Echoes, max(t0.Sent, t1.Sent))
	kv := func(name string, v int64, ok bool) {
		if ok {
			fmt.Fprintf(&b, " %s=%d", name, v)
		} else {
			fmt.Fprintf(&b, " %s=na", name)
		}
	}
	for i, in := range []echoInstant{t0, t1} {
		t := fmt.Sprintf("t%d", i)
		kv("srv_recvq_"+t, recvQ(in.Server), in.Server != nil)
		kv("srv_sendq_"+t, sendQ(in.Server), in.Server != nil)
		n, ok := in.Server.Int("bytes_received")
		kv("srv_bytes_received_"+t, n, ok)
		n, ok = in.Server.Skmem("r")
		kv("srv_skmem_r_"+t, n, ok)
		n, ok = in.Server.Int("rcv_wnd")
		kv("srv_rcv_wnd_"+t, n, ok)
		kv("cli_sendq_"+t, sendQ(in.Client), in.Client != nil)
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
