package validation

import (
	"strconv"
	"strings"
	"testing"
	"time"
)

// Fixtures for the ws_echo timeout discriminator (probatorium#481).
//
// realServerSock is the server end of the 64 KiB echo connection that
// timed out in race run 38410499633 (celeris 7d9ff01, cell-07
// auth_session_ratelimit/adaptive/amd64), copied from the incident's ss.txt
// (`ss -tanpi`): Recv-Q 433,664 unread, rcv_wnd 17,408, lastrcv 60 ms. The
// run's ss had no -m, so the skmem line a `ss -tmi` prints is added in the
// iproute2 spelling; its numbers are not from the run.
const realServerSock = `ESTAB     433664 0                         127.0.0.1:30833              127.0.0.1:50278 users:(("auth_session_ra",pid=840064,fd=13))
	 skmem:(r433664,rb460800,t0,tb2626560,f4096,w0,o0,bl0,d0) cubic wscale:1,10 rto:201 rtt:0.668/1 ato:51 mss:63162 pmtu:65535 rcvmss:65483 advmss:65483 cwnd:10 bytes_sent:2490877 bytes_acked:2490877 bytes_received:2951836 segs_out:106 segs_in:114 data_segs_out:66 data_segs_in:72 send 7564311377bps lastsnd:13 lastrcv:60 lastack:13 pacing_rate 15122962960bps delivery_rate 14861647056bps delivered:67 app_limited busy:96ms rwnd_limited:47ms(49.0%) rcv_rtt:0.018 rcv_space:197632 rcv_ssthresh:326069 minrtt:0.012 snd_wnd:126324 rcv_wnd:17408
`

const realClientSock = `ESTAB     0      3071716                   127.0.0.1:50278              127.0.0.1:30833 users:(("validator",pid=837506,fd=12))
	 skmem:(r0,rb65536,t0,tb4194304,f0,w3071716,o0,bl0,d0) cubic wscale:10,1 rto:201 rtt:0.027/0.013 mss:32768 cwnd:10 bytes_sent:2951836 bytes_acked:2951836 bytes_received:2490877 segs_out:114 segs_in:106 notsent:3071716 lastsnd:60 lastrcv:13 rwnd_limited:96ms(100.0%) rcv_space:65483 snd_wnd:17408 rcv_wnd:65536
`

func ssText(socks ...string) string {
	return "State     Recv-Q Send-Q               Local Address:Port  Peer Address:Port Process\n" + strings.Join(socks, "")
}

func TestParseSS_RealIncidentSockets(t *testing.T) {
	socks := parseSS(ssText(realServerSock, realClientSock))
	if len(socks) != 2 {
		t.Fatalf("parsed %d sockets, want 2", len(socks))
	}
	srv, cli := findEchoSocks(socks, 50278)
	if srv == nil || cli == nil {
		t.Fatalf("findEchoSocks(50278): server=%v client=%v, want both", srv != nil, cli != nil)
	}
	if srv.RecvQ != 433664 || srv.SendQ != 0 {
		t.Errorf("server Recv-Q/Send-Q = %d/%d, want 433664/0", srv.RecvQ, srv.SendQ)
	}
	if v, ok := srv.Int("bytes_received"); !ok || v != 2951836 {
		t.Errorf("server bytes_received = %d (%v), want 2951836", v, ok)
	}
	if v, ok := srv.Int("rcv_wnd"); !ok || v != 17408 {
		t.Errorf("server rcv_wnd = %d (%v), want 17408", v, ok)
	}
	if v, ok := srv.Int("lastrcv"); !ok || v != 60 {
		t.Errorf("server lastrcv = %d (%v), want 60", v, ok)
	}
	if v, ok := srv.Skmem("r"); !ok || v != 433664 {
		t.Errorf("server skmem r = %d (%v), want 433664", v, ok)
	}
	if v, ok := srv.Skmem("rb"); !ok || v != 460800 {
		t.Errorf("server skmem rb = %d (%v), want 460800", v, ok)
	}
	if cli.SendQ != 3071716 {
		t.Errorf("client Send-Q = %d, want 3071716", cli.SendQ)
	}
}

// The server is the socket whose PEER port is the client's ephemeral port.
// The listener's own port, another connection to the same listener, and a
// socket that merely shares the ephemeral number in the wrong column must
// not be taken for it.
func TestFindEchoSocks_PicksByPeerPort(t *testing.T) {
	other := "ESTAB 7 0 127.0.0.1:30833 127.0.0.1:50279\n\t cubic bytes_received:5\n"
	wrongColumn := "ESTAB 9 0 127.0.0.1:50278 127.0.0.1:9999\n\t cubic bytes_received:6\n" // LOCAL port 50278: a client-side socket of something else
	socks := parseSS(ssText(other, realServerSock, wrongColumn))
	srv, cli := findEchoSocks(socks, 50278)
	if srv == nil || srv.RecvQ != 433664 {
		t.Fatalf("server = %+v, want the 433664 socket", srv)
	}
	if cli == nil || cli.RecvQ != 9 {
		t.Errorf("client = %+v, want the socket whose local port is 50278", cli)
	}
	if s, _ := findEchoSocks(socks, 40000); s != nil {
		t.Errorf("a port nothing uses matched %+v", s)
	}
	if s, c := findEchoSocks(nil, 50278); s != nil || c != nil {
		t.Errorf("no sockets matched something")
	}
}

func TestParseSS_IPv6AndEmpty(t *testing.T) {
	v6 := "ESTAB 12 0 [::1]:8080 [::1]:50000\n\t cubic bytes_received:1\n"
	srv, _ := findEchoSocks(parseSS(ssText(v6)), 50000)
	if srv == nil || srv.RecvQ != 12 {
		t.Fatalf("IPv6 server socket not found: %+v", srv)
	}
	if got := parseSS(""); len(got) != 0 {
		t.Errorf("empty input parsed to %d sockets", len(got))
	}
	if got := parseSS("State Recv-Q Send-Q Local Address:Port Peer Address:Port Process\n"); len(got) != 0 {
		t.Errorf("header only parsed to %d sockets", len(got))
	}
}

// sock builds a server-end socket with the given queues for the classifier
// tests; srvInstant wraps it in an instant `at` seconds after a base time.
func srvSock(recvq int64, bytesRecv int64) *ssSock {
	return &ssSock{State: "ESTAB", RecvQ: recvq, Info: map[string]string{"bytes_received": strconv.FormatInt(bytesRecv, 10)}}
}

var verdictBase = time.Date(2026, 10, 13, 15, 40, 0, 0, time.UTC)

func inst(at time.Duration, echoes uint64, srv *ssSock) echoInstant {
	return echoInstant{At: verdictBase.Add(at), Echoes: echoes, Sent: 1244, Server: srv, Client: &ssSock{State: "ESTAB", SendQ: 3071716}}
}

func withBytes(i echoInstant, n uint64) echoInstant { i.BytesIn += n; return i }

func TestWSEchoClassify(t *testing.T) {
	win := 3 * time.Second
	cases := []struct {
		name       string
		t0, t1     echoInstant
		wantClass  string
		wantReason string
	}{
		// The fingerprint of the dossier: bytes sit unread, nothing moves.
		{"wedge: Recv-Q flat above zero, no echo", inst(0, 1180, srvSock(433664, 2951836)), inst(win, 1180, srvSock(433664, 2951836)), "WEDGE_CLASS", "recvq_flat_no_progress"},
		{"wedge: Recv-Q grows, no echo, handler not reading", inst(0, 1180, srvSock(433664, 2951836)), inst(win, 1180, srvSock(500000, 3018172)), "WEDGE_CLASS", "recvq_grew_no_progress"},
		// Slow: the server is moving.
		{"slow: Recv-Q drains", inst(0, 1180, srvSock(433664, 2951836)), inst(win, 1180, srvSock(65536, 2951836)), "SLOW_CLASS", "recvq_drained"},
		{"slow: echoes progress though Recv-Q is flat", inst(0, 1180, srvSock(433664, 2951836)), inst(win, 1183, srvSock(433664, 2951836)), "SLOW_CLASS", "echo_progress"},
		{"slow: bytes arrive mid-frame, no whole echo yet", inst(0, 1180, srvSock(433664, 2951836)), withBytes(inst(win, 1180, srvSock(433664, 2951836)), 4096), "SLOW_CLASS", "echo_progress"},
		{"slow: echoes progress and Recv-Q drains", inst(0, 1180, srvSock(433664, 2951836)), inst(win, 1190, srvSock(0, 2951836)), "SLOW_CLASS", "echo_progress"},
		{"slow: echoes progress with no ss at all", echoInstant{At: verdictBase, Echoes: 5, SSErr: "ss not available"}, echoInstant{At: verdictBase.Add(win), Echoes: 9, SSErr: "ss not available"}, "SLOW_CLASS", "echo_progress"},
		// Negative controls: missing or empty data must never become a class.
		{"indeterminate: server socket gone at t1", inst(0, 1180, srvSock(433664, 1)), echoInstant{At: verdictBase.Add(win), Echoes: 1180}, "INDETERMINATE", "server_socket_missing_t1"},
		{"indeterminate: server socket missing at t0", echoInstant{At: verdictBase, Echoes: 1180}, inst(win, 1180, srvSock(433664, 1)), "INDETERMINATE", "server_socket_missing_t0"},
		{"indeterminate: ss failed at t0, no progress", echoInstant{At: verdictBase, Echoes: 1180, SSErr: "exec: ss: not found"}, inst(win, 1180, srvSock(433664, 1)), "INDETERMINATE", "server_socket_missing_t0"},
		{"indeterminate: Recv-Q zero at both instants", inst(0, 1180, srvSock(0, 10)), inst(win, 1180, srvSock(0, 10)), "INDETERMINATE", "recvq_zero_no_progress"},
		{"indeterminate: Recv-Q appears only at t1", inst(0, 1180, srvSock(0, 10)), inst(win, 1180, srvSock(65536, 65546)), "INDETERMINATE", "recvq_zero_no_progress"},
		{"indeterminate: window shorter than the minimum", inst(0, 1180, srvSock(433664, 1)), inst(500*time.Millisecond, 1180, srvSock(433664, 1)), "INDETERMINATE", "window_too_short"},
		{"indeterminate: clock went backwards", inst(win, 1180, srvSock(433664, 1)), inst(0, 1180, srvSock(433664, 1)), "INDETERMINATE", "window_too_short"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			v := wsEchoClassify(tc.t0, tc.t1)
			if v.Class != tc.wantClass || v.Reason != tc.wantReason {
				t.Fatalf("class=%s reason=%s, want %s %s\nline: %s", v.Class, v.Reason, tc.wantClass, tc.wantReason, v.Line)
			}
			// The line is the machine-readable record: it starts with the
			// verdict key and carries the class, whatever it is.
			if !strings.HasPrefix(v.Line, "WS_ECHO_VERDICT="+tc.wantClass+" ") {
				t.Errorf("line %q does not start with WS_ECHO_VERDICT=%s", v.Line, tc.wantClass)
			}
			if strings.ContainsAny(v.Line, "\r\n") {
				t.Errorf("verdict line spans lines: %q", v.Line)
			}
		})
	}
}

// Only one of the three class names may appear on a line, so a grep for
// WEDGE_CLASS finds wedges and nothing else.
func TestWSEchoClassify_LineNamesExactlyOneClass(t *testing.T) {
	win := 3 * time.Second
	v := wsEchoClassify(inst(0, 1180, srvSock(433664, 1)), inst(win, 1183, srvSock(433664, 1)))
	n := 0
	for _, c := range []string{"SLOW_CLASS", "WEDGE_CLASS", "INDETERMINATE"} {
		if strings.Contains(v.Line, c) {
			n++
		}
	}
	if n != 1 {
		t.Errorf("line %q names %d classes, want 1", v.Line, n)
	}
}

// The numbers behind the class are on the line, so the reader can re-derive
// it, and an absent number is "na", never a zero that reads as a measurement.
func TestWSEchoClassify_LineCarriesTheRawNumbers(t *testing.T) {
	win := 3 * time.Second
	v := wsEchoClassify(inst(0, 1180, srvSock(433664, 2951836)), inst(win, 1180, srvSock(433664, 2951836)))
	for _, want := range []string{
		"window_ms=3000", "echoes_t0=1180", "echoes_t1=1180", "bytes_in_t0=0", "sent=1244",
		"srv_recvq_t0=433664", "srv_recvq_t1=433664",
		"srv_bytes_received_t0=2951836", "srv_bytes_received_t1=2951836",
		"cli_sendq_t0=3071716", "cli_sendq_t1=3071716",
	} {
		if !strings.Contains(v.Line, want) {
			t.Errorf("line lacks %q:\n%s", want, v.Line)
		}
	}
	gone := wsEchoClassify(echoInstant{At: verdictBase, Echoes: 1}, echoInstant{At: verdictBase.Add(win), Echoes: 1})
	for _, want := range []string{"srv_recvq_t0=na", "srv_recvq_t1=na", "srv_bytes_received_t0=na", "cli_sendq_t0=na"} {
		if !strings.Contains(gone.Line, want) {
			t.Errorf("a missing socket must print %q, line:\n%s", want, gone.Line)
		}
	}
}
