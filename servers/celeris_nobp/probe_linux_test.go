//go:build linux

package main

import (
	"context"
	"net"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// freeAddr returns a loopback address with a free port.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// startEpollServer runs a real celeris epoll engine on a free loopback port (the Docker test VM has no io_uring; the accept path and sockopts.ApplyFD are the same code)
// and returns its address.
func startEpollServer(t *testing.T) string {
	t.Helper()
	addr := freeAddr(t)
	srv := celeris.New(celeris.Config{Addr: addr, Engine: celeris.Epoll, Protocol: celeris.HTTP1})
	srv.GET("/", func(c *celeris.Context) error { return c.String(200, "ok") })
	errc := make(chan error, 1)
	go func() { errc <- srv.Start() }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
		select {
		case <-errc:
		case <-time.After(5 * time.Second):
		}
	})
	return addr
}

// nobp patches internal/sockopts so that NO busy-poll option is issued. The accepted socket of the real engine must read SO_BUSY_POLL=0 and SO_PREFER_BUSY_POLL=0 whatever the
// uid (the budget option is not issued either, so it reads the kernel default; it is logged, not asserted). The control (the same probe against unpatched celeris main reads
// busy_poll=50 prefer=1 budget=8 as root) is the noc8 module's test of the same name; the preparation ran both (EV/lanes-20261025/P2C/p2c-logs/).
func TestNobpAcceptedSocketsCarryNoBusyPollOption(t *testing.T) {
	addr := startEpollServer(t)
	res, err := probeAcceptedSocket(addr, 10*time.Second, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("uid=%d: %s", os.Geteuid(), res)
	if res.BusyPollErr != "" || res.PreferErr != "" { // the budget option has no getsockopt in the kernel: its read error is expected
		t.Fatalf("getsockopt failed: %+v", res)
	}
	if res.BusyPoll != 0 || res.PreferBusyPoll != 0 {
		t.Errorf("nobp accepted socket reads so_busy_poll=%d prefer_busy_poll=%d, want 0 and 0", res.BusyPoll, res.PreferBusyPoll)
	}
}

func TestProbeFailsClosedWhenNothingListens(t *testing.T) {
	if _, err := probeAcceptedSocket(freeAddr(t), 300*time.Millisecond, 10*time.Millisecond); err == nil {
		t.Fatal("expected an error when no listener comes up")
	}
}
