//go:build linux

package validation

import (
	"context"
	"io"
	"net"
	"os"
	"strings"
	"testing"
	"time"
)

// The capture's two host legs against the real kernel: the ss the walker
// runs, parsed by the parser the verdict reads, on a live loopback connection
// whose server end has bytes it never reads; and the thread table of this
// very process read from the real /proc. The rest of the tests feed the
// parser text; this one is where a changed ss spelling, a filter the ss of a
// runner rejects, or a /proc layout surprise would show.
func TestDefaultSS_LiveLoopbackShowsTheUnreadBytes(t *testing.T) {
	// In CI the test must run: a skip there would leave "Linux evidence" that
	// is only an absence.
	skipOrFail := func(why string) {
		if os.Getenv("CI") != "" {
			t.Fatalf("CI is set and the live ss leg cannot run: %s", why)
		}
		t.Skip(why)
	}
	if !hasBinary("ss") {
		skipOrFail("ss (iproute2) is not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		skipOrFail("no loopback listener")
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, err := ln.Accept()
		if err == nil {
			accepted <- c
		}
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	var srv net.Conn
	select {
	case srv = <-accepted:
		defer func() { _ = srv.Close() }()
	case <-time.After(5 * time.Second):
		t.Fatal("accept timed out")
	}
	const unread = 5000
	if _, err := cli.Write(make([]byte, unread)); err != nil { // the server never reads them
		t.Fatal(err)
	}
	port := cli.LocalAddr().(*net.TCPAddr).Port
	srvPort := ln.Addr().(*net.TCPAddr).Port

	var sockSrv *ssSock
	var out string
	for i := 0; i < 50; i++ { // the bytes are queued synchronously on loopback; the poll only covers ss start-up
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err = defaultSS(ctx, port, srvPort)
		cancel()
		if err != nil {
			t.Fatalf("defaultSS: %v\n%s", err, out)
		}
		sockSrv, _ = findEchoSocks(parseSS(out), port, srvPort)
		if sockSrv != nil && sockSrv.RecvQ >= unread {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if sockSrv == nil || sockSrv.RecvQ < unread {
		t.Fatalf("server end not seen with Recv-Q >= %d in ss output:\n%s", unread, out)
	}
	if _, ok := sockSrv.Skmem("r"); !ok {
		t.Errorf("ss -m printed no skmem tuple for the server end:\n%s", sockSrv.Raw)
	}
	if _, ok := sockSrv.Int("rcv_space"); !ok {
		t.Errorf("ss -i printed no rcv_space for the server end:\n%s", sockSrv.Raw)
	}
	if _, cliSock := findEchoSocks(parseSS(out), port, srvPort); cliSock == nil {
		t.Errorf("client end not seen:\n%s", out)
	}
	// The filter names this connection only.
	for _, s := range parseSS(out) {
		lp, pp := portOf(s.Local), portOf(s.Peer)
		if (lp != port || pp != srvPort) && (lp != srvPort || pp != port) {
			t.Errorf("ss returned a socket of another connection: %s", s.Raw)
		}
	}
}

func TestThreadTable_RealProc(t *testing.T) {
	got := threadTable("/proc", os.Getpid())
	if !strings.Contains(got, "REFAPP_THREADS n=") || !strings.Contains(got, " leader\n") {
		t.Fatalf("no thread table or no leader row for this process:\n%s", got)
	}
	want := procStatusField("/proc/self/status", "Cpus_allowed_list")
	if !strings.Contains(got, "leader_cpus_allowed_list="+want+" ") {
		t.Errorf("leader mask is not this process's Cpus_allowed_list %q:\n%s", want, got)
	}
}

// The verdict rests on two counters of the server socket, bytes_received less
// Recv-Q (what the application read) and bytes_acked plus Send-Q (what it
// wrote). This measures both on the runner's kernel with the runner's ss, the
// same walker filter included: a server that has never written, then one that
// wrote 100 bytes, then one that read exactly 2000 of 5000 unread bytes and
// wrote 3000.
func TestDefaultSS_LiveLoopbackAppCounters(t *testing.T) {
	if !hasBinary("ss") {
		if os.Getenv("CI") != "" {
			t.Fatal("CI is set and the live ss leg cannot run: ss (iproute2) is not installed")
		}
		t.Skip("ss (iproute2) is not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skipf("no loopback listener: %v", err)
	}
	defer func() { _ = ln.Close() }()
	accepted := make(chan net.Conn, 1)
	go func() {
		if c, err := ln.Accept(); err == nil {
			accepted <- c
		}
	}()
	cli, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cli.Close() }()
	var srv net.Conn
	select {
	case srv = <-accepted:
		defer func() { _ = srv.Close() }()
	case <-time.After(5 * time.Second):
		t.Fatal("accept timed out")
	}
	_ = cli.SetDeadline(time.Now().Add(20 * time.Second))
	_ = srv.SetDeadline(time.Now().Add(20 * time.Second))
	port := cli.LocalAddr().(*net.TCPAddr).Port
	srvPort := ln.Addr().(*net.TCPAddr).Port

	// snap takes one ss of the connection, polling until the server end
	// satisfies ok (the bytes are queued synchronously on loopback; the poll
	// covers ss start-up and ack timing).
	snap := func(label string, ok func(*ssSock) bool) echoInstant {
		t.Helper()
		var last string
		for i := 0; i < 100; i++ {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			out, err := defaultSS(ctx, port, srvPort)
			cancel()
			if err != nil {
				t.Fatalf("%s: defaultSS: %v\n%s", label, err, out)
			}
			last = out
			s, c := findEchoSocks(parseSS(out), port, srvPort)
			if s != nil && ok(s) {
				return echoInstant{At: time.Now(), Server: s, Client: c}
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("%s: the server end never reached the expected state in ss output:\n%s", label, last)
		return echoInstant{}
	}
	written := func(s *ssSock) int64 { a, _ := s.Counter("bytes_acked"); return a + s.SendQ }

	// 0: the server has written nothing. Record what this ss prints for the
	// counter: iproute2 omits a zero.
	i0 := snap("before any write", func(*ssSock) bool { return true })
	_, hasAcked := i0.Server.Info["bytes_acked"]
	_, hasRecv := i0.Server.Info["bytes_received"]
	t.Logf("server end before any write: bytes_acked printed=%v bytes_received printed=%v detail=%v\n%s", hasAcked, hasRecv, i0.Server.hasDetail(), i0.Server.Raw)
	if !i0.Server.hasDetail() {
		t.Fatalf("ss printed no detail line (-i) for the server end:\n%s", i0.Server.Raw)
	}
	if a, ok := i0.Server.Counter("bytes_acked"); !ok || a != 0 {
		t.Errorf("a server that never wrote: Counter(bytes_acked) = %d, %v, want 0, true", a, ok)
	}

	// 1: 5000 bytes toward the server, which never reads them; it writes 100.
	if _, err := cli.Write(make([]byte, 5000)); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Write(make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(cli, make([]byte, 100)); err != nil {
		t.Fatal(err)
	}
	a := snap("snapshot A", func(s *ssSock) bool { return s.RecvQ == 5000 && written(s) == 100 })

	// 2: the server reads exactly 2000 of the 5000 and writes 3000, which the
	// client reads.
	if _, err := io.ReadFull(srv, make([]byte, 2000)); err != nil {
		t.Fatal(err)
	}
	if _, err := srv.Write(make([]byte, 3000)); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(cli, make([]byte, 3000)); err != nil {
		t.Fatal(err)
	}
	b := snap("snapshot B", func(s *ssSock) bool { return s.RecvQ == 3000 && written(s) == 3100 })

	reads, okR := srvReads(a, b)
	writes, okW := srvWrites(a, b)
	t.Logf("A:\n%sB:\n%s", a.Server.Raw, b.Server.Raw)
	if !okR || reads != 2000 {
		t.Errorf("srvReads(A, B) = %d, %v; want 2000, true", reads, okR)
	}
	if !okW || writes != 3000 {
		t.Errorf("srvWrites(A, B) = %d, %v; want 3000, true", writes, okW)
	}
	// And the classifier, on these two real snapshots with a window long
	// enough: the server read and wrote, so the class is SLOW.
	a.At, b.At = verdictBase, verdictBase.Add(3*time.Second)
	a.BytesIn, b.BytesIn = 100, 3100
	if v := wsEchoClassify(a, b); v.Class != verdictSlow || v.Reason != "server_read_and_wrote" {
		t.Errorf("verdict %s/%s, want SLOW_CLASS/server_read_and_wrote\n%s", v.Class, v.Reason, v.Line)
	}
	// Two identical snapshots of the same state: nothing moved, bytes pending.
	c := b
	c.At, c.BytesIn = verdictBase.Add(6*time.Second), 3100
	if v := wsEchoClassify(b, c); v.Class != verdictWedge {
		t.Errorf("verdict %s/%s for an unchanged server, want WEDGE_CLASS\n%s", v.Class, v.Reason, v.Line)
	}
}
