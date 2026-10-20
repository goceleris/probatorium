//go:build linux

package validation

import (
	"context"
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
	if !hasBinary("ss") {
		t.Skip("ss (iproute2) is not installed")
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("no loopback listener")
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

	var sockSrv *ssSock
	var out string
	for i := 0; i < 50; i++ { // the bytes are queued synchronously on loopback; the poll only covers ss start-up
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		out, err = defaultSS(ctx, port)
		cancel()
		if err != nil {
			t.Fatalf("defaultSS: %v\n%s", err, out)
		}
		sockSrv, _ = findEchoSocks(parseSS(out), port)
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
	if _, cliSock := findEchoSocks(parseSS(out), port); cliSock == nil {
		t.Errorf("client end not seen:\n%s", out)
	}
	// The filter names this connection only.
	for _, s := range parseSS(out) {
		if portOf(s.Local) != port && portOf(s.Peer) != port {
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
