//go:build linux

package main

import (
	"context"
	"net"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// freeAddr returns a loopback address with a free port.
// hasNetAdmin reports whether this process holds CAP_NET_ADMIN (bit 12 of CapEff).
func hasNetAdmin() bool {
	b, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return false
	}
	for _, l := range strings.Split(string(b), "\n") {
		if v, ok := strings.CutPrefix(l, "CapEff:"); ok {
			n, err := strconv.ParseUint(strings.TrimSpace(v), 16, 64)
			return err == nil && n&(1<<12) != 0
		}
	}
	return false
}

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

// noc8 keeps celeris main's busy-poll options. SO_BUSY_POLL=50 is set whoever runs it (an unprivileged process can raise it on this kernel);
// SO_PREFER_BUSY_POLL needs CAP_NET_ADMIN, so an unprivileged SUT (the bench default, celeris_needs_root=false) gets prefer_busy_poll=0 and celeris ignores the EPERM. The
// budget option (70) has no getsockopt, so only the first two are asserted; the probe line logs all three as read.
func TestProbeSeesTheOptionsCelerisMainSetsOnAcceptedSockets(t *testing.T) {
	addr := startEpollServer(t)
	res, err := probeAcceptedSocket(addr, 10*time.Second, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("probe: %v", err)
	}
	t.Logf("uid=%d: %s", os.Geteuid(), res)
	if res.BusyPollErr != "" || res.PreferErr != "" { // the budget option has no getsockopt in the kernel: its read error is expected
		t.Fatalf("getsockopt failed: %+v", res)
	}
	if res.BusyPoll != 50 {
		t.Errorf("main's accepted sockets must read so_busy_poll=50 (the kernel lets an unprivileged process raise it), got %s", res)
	}
	if hasNetAdmin() && res.PreferBusyPoll != 1 {
		t.Errorf("with CAP_NET_ADMIN main's sockets must read prefer_busy_poll=1, got %s", res)
	}
	if !hasNetAdmin() && res.PreferBusyPoll != 0 {
		t.Errorf("without CAP_NET_ADMIN the kernel refuses SO_PREFER_BUSY_POLL (EPERM, which celeris ignores): want prefer_busy_poll=0, got %s", res)
	}
}

func TestProbeFailsClosedWhenNothingListens(t *testing.T) {
	if _, err := probeAcceptedSocket(freeAddr(t), 300*time.Millisecond, 10*time.Millisecond); err == nil {
		t.Fatal("expected an error when no listener comes up")
	}
}
