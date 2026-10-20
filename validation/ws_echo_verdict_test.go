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
	srv, cli := findEchoSocks(socks, 50278, 30833)
	if srv == nil || cli == nil {
		t.Fatalf("findEchoSocks(50278, 30833): server=%v client=%v, want both", srv != nil, cli != nil)
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

// The server is the socket whose LOCAL port is the listener's and whose PEER
// port is the client's ephemeral port. The listener's own port, another
// connection to the same listener, a socket that merely shares the ephemeral
// number in the wrong column, and one that shares it toward another
// destination (Linux reuses ephemeral numbers across destinations) must not
// be taken for it.
func TestFindEchoSocks_PicksByPortPair(t *testing.T) {
	other := "ESTAB 7 0 127.0.0.1:30833 127.0.0.1:50279\n\t cubic bytes_received:5\n"
	wrongColumn := "ESTAB 9 0 127.0.0.1:50278 127.0.0.1:9999\n\t cubic bytes_received:6\n" // LOCAL port 50278, toward another port
	// Same ephemeral number toward the refapp's side listener, and a server
	// end of another listener whose peer happens to use that number.
	sideClient := "ESTAB 11 0 127.0.0.1:50278 127.0.0.1:41000\n\t cubic bytes_received:7\n"
	sideServer := "ESTAB 13 0 127.0.0.1:41000 127.0.0.1:50278\n\t cubic bytes_received:8\n"
	socks := parseSS(ssText(sideServer, sideClient, other, realServerSock, wrongColumn))
	srv, cli := findEchoSocks(socks, 50278, 30833)
	if srv == nil || srv.RecvQ != 433664 {
		t.Fatalf("server = %+v, want the 433664 socket", srv)
	}
	if cli != nil {
		t.Errorf("client = %+v, want nil: no socket with ports 50278 <-> 30833 as its local/peer", cli)
	}
	socks = parseSS(ssText(sideClient, realClientSock, sideServer, realServerSock))
	if srv, cli = findEchoSocks(socks, 50278, 30833); srv == nil || srv.RecvQ != 433664 || cli == nil || cli.SendQ != 3071716 {
		t.Errorf("with both ends present: server=%+v client=%+v, want the incident's two sockets", srv, cli)
	}
	// The side listener's pair is a different connection.
	if s, c := findEchoSocks(socks, 50278, 41000); s == nil || s.RecvQ != 13 || c == nil || c.RecvQ != 11 {
		t.Errorf("port pair 50278<->41000: server=%+v client=%+v, want the side listener's sockets", s, c)
	}
	if s, c := findEchoSocks(socks, 40000, 30833); s != nil || c != nil {
		t.Errorf("a port nothing uses matched %+v %+v", s, c)
	}
	if s, c := findEchoSocks(socks, 50278, 0); s != nil || c != nil {
		t.Errorf("an unknown server port must match nothing, got %+v %+v", s, c)
	}
	if s, c := findEchoSocks(nil, 50278, 30833); s != nil || c != nil {
		t.Errorf("no sockets matched something")
	}
}

func TestParseSS_IPv6AndEmpty(t *testing.T) {
	v6 := "ESTAB 12 0 [::1]:8080 [::1]:50000\n\t cubic bytes_received:1\n"
	srv, _ := findEchoSocks(parseSS(ssText(v6)), 50000, 8080)
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

// srvSock builds a server-end socket with the given receive queue and
// bytes_received for the classifier tests, and an unchanging write side
// (bytes_acked 4490877, empty Send-Q, the incident's value); srvSockW sets
// the write side too.
func srvSock(recvq int64, bytesRecv int64) *ssSock { return srvSockW(recvq, bytesRecv, 0, 4490877) }

func srvSockW(recvq, bytesRecv, sendq, acked int64) *ssSock {
	return &ssSock{State: "ESTAB", RecvQ: recvq, SendQ: sendq, Info: map[string]string{
		"bytes_received": strconv.FormatInt(bytesRecv, 10),
		"bytes_acked":    strconv.FormatInt(acked, 10),
	}}
}

var verdictBase = time.Date(2026, 10, 13, 15, 40, 0, 0, time.UTC)

// inst builds an instant; each echo is about 64 KiB on the wire, so BytesIn
// follows Echoes the way it does in a real fire.
func inst(at time.Duration, echoes uint64, srv *ssSock) echoInstant {
	return echoInstant{At: verdictBase.Add(at), Echoes: echoes, BytesIn: echoes * 65546, Sent: 1244, Server: srv, Client: &ssSock{State: "ESTAB", SendQ: 3071716}}
}

func withCliRecvQ(i echoInstant, n int64) echoInstant {
	c := *i.Client
	c.RecvQ = n
	i.Client = &c
	return i
}

func withBytes(i echoInstant, n uint64) echoInstant { i.BytesIn += n; return i }

// withDetail marks the socket as one whose -i detail line ss printed (cwnd and
// rto are always on it), and drops counters that are zero, as iproute2 does.
func withDetail(s *ssSock) *ssSock {
	c := *s
	c.Info = map[string]string{"cwnd": "10", "rto": "201"}
	for k, v := range s.Info {
		if v != "0" {
			c.Info[k] = v
		}
	}
	return &c
}

// without returns the socket with one Info key removed.
func without(s *ssSock, key string) *ssSock {
	c := *s
	c.Info = map[string]string{}
	for k, v := range s.Info {
		if k != key {
			c.Info[k] = v
		}
	}
	return &c
}

func TestWSEchoClassify(t *testing.T) {
	win := 3 * time.Second
	const (
		recv0 = 2951836
		acked = 4490877
	)
	cases := []struct {
		name       string
		t0, t1     echoInstant
		wantClass  string
		wantReason string
	}{
		// The fingerprint of the dossier: bytes sit unread, nothing moves.
		{"wedge: nothing moves, Recv-Q above zero", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1180, srvSock(433664, recv0)), "WEDGE_CLASS", "no_app_read_no_app_write_recvq_pending"},
		// The kernel refilled the queue from the client's Send-Q: arrivals
		// equal growth, the application read nothing.
		{"wedge: Recv-Q grew by what arrived, no app read", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1180, srvSock(500000, recv0+66336)), "WEDGE_CLASS", "no_app_read_no_app_write_recvq_pending"},
		{"wedge: the only bytes the client read were already in its queue at t0", withCliRecvQ(inst(0, 1180, srvSock(433664, 1)), 40000), withBytes(inst(win, 1180, srvSock(433664, 1)), 40000), "WEDGE_CLASS", "no_app_read_no_app_write_recvq_pending"},
		// Review probe 2: echoes the server wrote before t0 drain from its own
		// Send-Q into a reading client; the application wrote nothing.
		{"wedge: bytes at the client came out of the server's Send-Q, not the app", inst(0, 1180, srvSockW(433664, recv0, 2000000, acked)), withBytes(inst(win, 1180, srvSockW(433664, recv0, 1800000, acked+200000)), 200000), "WEDGE_CLASS", "no_app_read_no_app_write_recvq_pending"},
		{"wedge: a close frame is not progress", inst(0, 1180, srvSock(433664, recv0)), withBytes(inst(win, 1180, srvSockW(433664, recv0, 0, acked+4)), 4), "WEDGE_CLASS", "no_app_read_no_app_write_recvq_pending"},
		// Slow: the application is moving.
		// Review probe 1: Recv-Q flat because the kernel refills it, while the
		// application read 40,000 bytes.
		{"slow: app read 40000 bytes though Recv-Q is flat", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1180, srvSock(433664, recv0+40000)), "SLOW_CLASS", "server_read"},
		{"slow: Recv-Q drains", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1180, srvSock(65536, recv0)), "SLOW_CLASS", "server_read"},
		{"slow: app wrote echoes though Recv-Q is flat", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1183, srvSockW(433664, recv0, 0, acked+3*65546)), "SLOW_CLASS", "server_wrote"},
		{"slow: app wrote mid-frame, no whole echo at the client yet", inst(0, 1180, srvSock(433664, recv0)), withBytes(inst(win, 1180, srvSockW(433664, recv0, 0, acked+4096)), 4096), "SLOW_CLASS", "server_wrote"},
		{"slow: app wrote into a Send-Q the client has not read", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1180, srvSockW(433664, recv0, 100000, acked)), "SLOW_CLASS", "server_wrote"},
		{"slow: app read and wrote", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1190, srvSockW(0, recv0, 0, acked+10*65546)), "SLOW_CLASS", "server_read_and_wrote"},
		{"slow: writes at the floor count", inst(0, 1180, srvSock(433664, recv0)), withBytes(inst(win, 1180, srvSockW(433664, recv0, 0, acked+wsEchoMinWrite)), wsEchoMinWrite), "SLOW_CLASS", "server_wrote"},
		{"wedge: one byte under the floor does not", inst(0, 1180, srvSock(433664, recv0)), withBytes(inst(win, 1180, srvSockW(433664, recv0, 0, acked+wsEchoMinWrite-1)), wsEchoMinWrite-1), "WEDGE_CLASS", "no_app_read_no_app_write_recvq_pending"},
		// Negative controls: missing or contradictory data must never become
		// a class. Bytes at the client with no ss at all are not evidence of
		// what the application did (the server's own Send-Q may have held them).
		{"indeterminate: bytes arrive with no ss at all", echoInstant{At: verdictBase, Echoes: 5, BytesIn: 100, SSErr: "ss not available"}, echoInstant{At: verdictBase.Add(win), Echoes: 9, BytesIn: 262284, SSErr: "ss not available"}, "INDETERMINATE", "server_socket_missing_t0"},
		{"indeterminate: client bytes beyond every queue and write", withCliRecvQ(inst(0, 1180, srvSock(433664, 1)), 40000), withBytes(inst(win, 1180, srvSock(433664, 1)), 45000), "INDETERMINATE", "client_bytes_unexplained"},
		{"indeterminate: client bytes with no client socket at t0 and nothing written", withoutClient(inst(0, 1180, srvSock(433664, 1))), withBytes(inst(win, 1180, srvSock(433664, 1)), 100), "INDETERMINATE", "client_bytes_unexplained"},
		{"indeterminate: the client's queue explains every byte and the server socket is gone", withCliRecvQ(inst(0, 1180, srvSock(433664, 1)), 40000), withBytes(echoInstant{At: verdictBase.Add(win), Echoes: 1180, BytesIn: 1180 * 65546}, 40000), "INDETERMINATE", "server_socket_missing_t1"},
		{"indeterminate: server socket gone at t1", inst(0, 1180, srvSock(433664, 1)), echoInstant{At: verdictBase.Add(win), Echoes: 1180}, "INDETERMINATE", "server_socket_missing_t1"},
		{"indeterminate: server socket missing at t0", echoInstant{At: verdictBase, Echoes: 1180, BytesIn: 1180 * 65546}, inst(win, 1180, srvSock(433664, 1)), "INDETERMINATE", "server_socket_missing_t0"},
		{"indeterminate: ss failed at t0, no progress", echoInstant{At: verdictBase, Echoes: 1180, BytesIn: 1180 * 65546, SSErr: "exec: ss: not found"}, inst(win, 1180, srvSock(433664, 1)), "INDETERMINATE", "server_socket_missing_t0"},
		{"indeterminate: bytes_received absent at t1", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1180, without(srvSock(433664, recv0), "bytes_received")), "INDETERMINATE", "server_counters_missing"},
		{"indeterminate: bytes_acked absent at t0", inst(0, 1180, without(srvSock(433664, recv0), "bytes_acked")), inst(win, 1180, srvSock(433664, recv0)), "INDETERMINATE", "server_counters_missing"},
		{"indeterminate: counters went backwards (another socket on the port)", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1180, srvSockW(433664, recv0, 0, acked-1000)), "INDETERMINATE", "server_counters_inconsistent"},
		{"indeterminate: bytes_received went backwards", inst(0, 1180, srvSock(433664, recv0)), inst(win, 1180, srvSock(433664, recv0-5000)), "INDETERMINATE", "server_counters_inconsistent"},
		// iproute2 omits a counter that is zero: a server end that has never
		// written prints no bytes_acked. With the detail line present that is
		// a zero, not missing data (the live test records what the runner's ss prints).
		{"wedge: a server that never wrote prints no bytes_acked at either instant", inst(0, 0, withDetail(srvSockW(433664, recv0, 0, 0))), inst(win, 0, withDetail(srvSockW(433664, recv0, 0, 0))), "WEDGE_CLASS", "no_app_read_no_app_write_recvq_pending"},
		{"slow: the first write the server ever made", inst(0, 0, withDetail(srvSockW(433664, recv0, 0, 0))), withBytes(inst(win, 0, withDetail(srvSockW(433664, recv0, 0, 4096))), 4096), "SLOW_CLASS", "server_wrote"},
		{"indeterminate: detail line present but bytes_received absent while the queue holds bytes", inst(0, 1180, without(withDetail(srvSock(433664, recv0)), "bytes_received")), inst(win, 1180, without(withDetail(srvSock(433664, recv0)), "bytes_received")), "INDETERMINATE", "server_counters_missing"},
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

func withoutClient(i echoInstant) echoInstant { i.Client = nil; return i }

// The promise of the verdict: dropping any one datum it rests on from a
// capture that says WEDGE turns it into INDETERMINATE, never keeps WEDGE and
// never turns into SLOW.
func TestWSEchoClassify_MissingDataNeverWedge(t *testing.T) {
	win := 3 * time.Second
	mk := func() (echoInstant, echoInstant) {
		return inst(0, 1180, srvSock(433664, 2951836)), inst(win, 1180, srvSock(433664, 2951836))
	}
	if t0, t1 := mk(); wsEchoClassify(t0, t1).Class != verdictWedge {
		t.Fatal("the base capture is not a WEDGE: the control below proves nothing")
	}
	drops := map[string]func(t0, t1 *echoInstant){
		"server t0":          func(t0, _ *echoInstant) { t0.Server = nil },
		"server t1":          func(_, t1 *echoInstant) { t1.Server = nil },
		"bytes_received t0":  func(t0, _ *echoInstant) { t0.Server = without(t0.Server, "bytes_received") },
		"bytes_received t1":  func(_, t1 *echoInstant) { t1.Server = without(t1.Server, "bytes_received") },
		"bytes_acked t0":     func(t0, _ *echoInstant) { t0.Server = without(t0.Server, "bytes_acked") },
		"bytes_acked t1":     func(_, t1 *echoInstant) { t1.Server = without(t1.Server, "bytes_acked") },
		"both counters both": func(t0, t1 *echoInstant) { t0.Server = &ssSock{RecvQ: 433664}; t1.Server = &ssSock{RecvQ: 433664} },
	}
	// The same on sockets with the detail line, where a zero counter is
	// legitimately omitted: an ss that prints neither counter (too old) and
	// a bytes_received missing under a pending queue are still missing data.
	detailed := func() (echoInstant, echoInstant) {
		t0, t1 := mk()
		t0.Server, t1.Server = withDetail(t0.Server), withDetail(t1.Server)
		return t0, t1
	}
	if t0, t1 := detailed(); wsEchoClassify(t0, t1).Class != verdictWedge {
		t.Fatal("the detailed base capture is not a WEDGE")
	}
	for name, drop := range map[string]func(t0, t1 *echoInstant){
		"detail: bytes_received t1": func(_, t1 *echoInstant) { t1.Server = without(t1.Server, "bytes_received") },
		"detail: an ss with neither": func(t0, t1 *echoInstant) {
			t0.Server, t1.Server = without(without(t0.Server, "bytes_received"), "bytes_acked"), without(without(t1.Server, "bytes_received"), "bytes_acked")
		},
	} {
		t0, t1 := detailed()
		drop(&t0, &t1)
		if v := wsEchoClassify(t0, t1); v.Class != verdictIndeterminate {
			t.Errorf("%s: %s/%s, want INDETERMINATE\n%s", name, v.Class, v.Reason, v.Line)
		}
	}
	for name, drop := range drops {
		t0, t1 := mk()
		drop(&t0, &t1)
		if v := wsEchoClassify(t0, t1); v.Class != verdictIndeterminate {
			t.Errorf("without %s: %s/%s, want INDETERMINATE\n%s", name, v.Class, v.Reason, v.Line)
		}
	}
}

// Only one of the three class names may appear on a line, so a grep for
// WEDGE_CLASS finds wedges and nothing else.
func TestWSEchoClassify_LineNamesExactlyOneClass(t *testing.T) {
	win := 3 * time.Second
	v := wsEchoClassify(inst(0, 1180, srvSock(433664, 1)), inst(win, 1183, srvSockW(433664, 1, 0, 4490877+3*65546)))
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
		"window_ms=3000", "echoes_t0=1180", "echoes_t1=1180", "bytes_new=0", "sent=1244",
		"srv_recvq_t0=433664", "srv_recvq_t1=433664",
		"srv_bytes_received_t0=2951836", "srv_bytes_received_t1=2951836",
		"cli_sendq_t0=3071716", "cli_sendq_t1=3071716", "cli_recvq_t0=0",
		"srv_bytes_acked_t0=4490877", "srv_app_reads=0", "srv_app_writes=0", "srv_written_t0=4490877",
	} {
		if !strings.Contains(v.Line, want) {
			t.Errorf("line lacks %q:\n%s", want, v.Line)
		}
	}
	gone := wsEchoClassify(echoInstant{At: verdictBase, Echoes: 1}, echoInstant{At: verdictBase.Add(win), Echoes: 1})
	for _, want := range []string{"srv_recvq_t0=na", "srv_recvq_t1=na", "srv_bytes_received_t0=na", "cli_sendq_t0=na", "cli_recvq_t0=na", "srv_written_t0=na", "srv_app_reads=na", "srv_app_writes=na", "srv_bytes_acked_t0=na"} {
		if !strings.Contains(gone.Line, want) {
			t.Errorf("a missing socket must print %q, line:\n%s", want, gone.Line)
		}
	}
}

// The line prints the exact application counters the class rests on, so the
// reader re-derives a SLOW that the queue levels hide (review probe 1).
func TestWSEchoClassify_LinePrintsAppReadsAndWrites(t *testing.T) {
	win := 3 * time.Second
	v := wsEchoClassify(inst(0, 1180, srvSock(433664, 2951836)), inst(win, 1180, srvSockW(433664, 2991836, 0, 4490877+70000)))
	for _, want := range []string{"srv_app_reads=40000", "srv_app_writes=70000"} {
		if !strings.Contains(v.Line, want) {
			t.Errorf("line lacks %q:\n%s", want, v.Line)
		}
	}
	if v.Class != verdictSlow || v.Reason != "server_read_and_wrote" {
		t.Errorf("class=%s reason=%s, want SLOW_CLASS/server_read_and_wrote", v.Class, v.Reason)
	}
}

func TestSSSockCounter(t *testing.T) {
	cases := []struct {
		name   string
		s      *ssSock
		key    string
		want   int64
		wantOK bool
	}{
		{"present", withDetail(srvSock(10, 500)), "bytes_received", 500, true},
		{"present without the detail marker", srvSock(10, 500), "bytes_acked", 4490877, true},
		{"acked omitted as zero", withDetail(srvSockW(10, 500, 0, 0)), "bytes_acked", 0, true},
		{"acked absent, detail line but no counters at all (an old ss): missing", &ssSock{RecvQ: 10, Info: map[string]string{"cwnd": "10"}}, "bytes_acked", 0, false},
		{"acked absent, detail line, bytes_sent printed: zero", &ssSock{RecvQ: 10, Info: map[string]string{"cwnd": "10", "bytes_sent": "7"}}, "bytes_acked", 0, true},
		{"acked absent and no detail line: missing", without(srvSock(10, 500), "bytes_acked"), "bytes_acked", 0, false},
		{"received omitted as zero with an empty queue", withDetail(&ssSock{RecvQ: 0, Info: map[string]string{}}), "bytes_received", 0, true},
		{"received absent with bytes queued: missing", without(withDetail(srvSock(10, 500)), "bytes_received"), "bytes_received", 0, false},
		{"not a number: missing", &ssSock{Info: map[string]string{"cwnd": "10", "bytes_acked": "x"}}, "bytes_acked", 0, false},
		{"nil socket", nil, "bytes_acked", 0, false},
	}
	for _, tc := range cases {
		if got, ok := tc.s.Counter(tc.key); got != tc.want || ok != tc.wantOK {
			t.Errorf("%s: Counter(%s) = %d, %v; want %d, %v", tc.name, tc.key, got, ok, tc.want, tc.wantOK)
		}
	}
}
