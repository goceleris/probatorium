//go:build linux

package main

// P2c accepted-socket probe (identical file in servers/celeris_noc8 and servers/celeris_nobp; throwaway branch ab/p2c-io512-discriminate-38, not for merge).
//
// WHY: the P2c question is whether celeris's busy-poll socket options (SO_BUSY_POLL=50us, SO_PREFER_BUSY_POLL=1, SO_BUSY_POLL_BUDGET=8, applied by
// internal/sockopts to every accepted socket) trigger the io512 stalls. Two facts are NOT known from the P2b artifacts: (1) which of the three options the UNPRIVILEGED SUT
// really gets (SO_PREFER_BUSY_POLL needs CAP_NET_ADMIN in the kernel; celeris ignores the error: `_ = unix.SetsockoptInt(..., 69, 1)`), and (2) that the nobp build really leaves the
// accepted sockets without them. This probe answers both from INSIDE the benched binary, on the real engine (io_uring on msr1), with the real accept path: shortly after startup it
// dials the server's own port over loopback, finds the server-side accepted socket in /proc/self/fd (the socket whose peer is the probe's own local address) and reads the three
// options back with getsockopt. One connection, once, before any load; closed immediately. The result is ONE log line in server.log:
//
//	[<tag>-arm] accepted-socket-probe fd=N peer=127.0.0.1:P so_busy_poll=U prefer_busy_poll=B busy_poll_budget=K (read N ms after the dial returned) uid=U euid=E
//
// noc8 (main's plan minus CPU 8, busy polling as in main) logs what MAIN's sockets get; nobp logs what the patched build's sockets get. The probe does not change any socket option.

import (
	"fmt"
	"log"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"
)

const (
	soPreferBusyPoll = 69 // SO_PREFER_BUSY_POLL, not in x/sys/unix at this version
	soBusyPollBudget = 70 // SO_BUSY_POLL_BUDGET
)

// probeResult is what the probe read from the server-side accepted socket. The *Err fields hold a getsockopt error ("" if none).
type probeResult struct {
	FD             int
	Peer           string
	BusyPoll       int
	PreferBusyPoll int
	BusyPollBudget int
	BusyPollErr    string
	PreferErr      string
	BudgetErr      string
	AcceptToReadMs int64
}

func (r probeResult) String() string {
	e := func(v int, err string) string {
		if err != "" {
			return "ERR(" + err + ")"
		}
		return strconv.Itoa(v)
	}
	return fmt.Sprintf("fd=%d peer=%s so_busy_poll=%s prefer_busy_poll=%s busy_poll_budget=%s (read %d ms after the dial returned) uid=%d euid=%d",
		r.FD, r.Peer, e(r.BusyPoll, r.BusyPollErr), e(r.PreferBusyPoll, r.PreferErr), e(r.BusyPollBudget, r.BudgetErr), r.AcceptToReadMs, os.Getuid(), os.Geteuid())
}

// runAcceptedSocketProbe runs the probe against bind (the -bind value) and logs one line. It never fails the process: a probe that cannot run logs why.
func runAcceptedSocketProbe(tag, bind string) {
	res, err := probeAcceptedSocket(bind, 20*time.Second, 400*time.Millisecond)
	if err != nil {
		log.Printf("[%s-arm] accepted-socket-probe FAILED: %v", tag, err)
		return
	}
	log.Printf("[%s-arm] accepted-socket-probe %s", tag, res)
}

// probeAcceptedSocket dials bind (0.0.0.0 and the empty host map to 127.0.0.1) until the server accepts, waits settle (the engine applies its options right after the accept
// completes), then reads the options of the accepted socket. waitFor bounds the wait for the listener.
func probeAcceptedSocket(bind string, waitFor, settle time.Duration) (probeResult, error) {
	host, portStr, err := net.SplitHostPort(bind)
	if err != nil {
		return probeResult{}, fmt.Errorf("bad bind %q: %v", bind, err)
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	port, err := strconv.Atoi(portStr)
	if err != nil {
		return probeResult{}, fmt.Errorf("bad port in %q: %v", bind, err)
	}
	var c net.Conn
	deadline := time.Now().Add(waitFor)
	for {
		c, err = net.DialTimeout("tcp", net.JoinHostPort(host, portStr), time.Second)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			return probeResult{}, fmt.Errorf("listener never came up on %s: %v", bind, err)
		}
		time.Sleep(100 * time.Millisecond)
	}
	defer c.Close()
	local := c.LocalAddr().(*net.TCPAddr)
	dialed := time.Now()
	time.Sleep(settle)
	var res probeResult
	for tries := 0; tries < 50; tries++ {
		if fd, ok := findAcceptedFD(port, local); ok {
			res = readBusyPollOptions(fd)
			res.Peer = local.String()
			res.AcceptToReadMs = time.Since(dialed).Milliseconds()
			return res, nil
		}
		time.Sleep(100 * time.Millisecond)
	}
	return res, fmt.Errorf("no socket of this process has local port %d and peer %s", port, local)
}

// findAcceptedFD scans /proc/self/fd for the socket whose local port is serverPort and whose peer is peer (the probe's own client socket).
func findAcceptedFD(serverPort int, peer *net.TCPAddr) (int, bool) {
	ents, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return 0, false
	}
	for _, e := range ents {
		fd, err := strconv.Atoi(e.Name())
		if err != nil {
			continue
		}
		if l, err := os.Readlink("/proc/self/fd/" + e.Name()); err != nil || !strings.HasPrefix(l, "socket:") {
			continue
		}
		pn, err := unix.Getpeername(fd)
		if err != nil {
			continue
		}
		pa, ok := pn.(*unix.SockaddrInet4)
		if !ok || pa.Port != peer.Port || net.IP(pa.Addr[:]).String() != peer.IP.String() {
			continue
		}
		sn, err := unix.Getsockname(fd)
		if err != nil {
			continue
		}
		if sa, ok := sn.(*unix.SockaddrInet4); ok && sa.Port == serverPort {
			return fd, true
		}
	}
	return 0, false
}

func readBusyPollOptions(fd int) probeResult {
	r := probeResult{FD: fd}
	get := func(opt int) (int, string) {
		v, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, opt)
		if err != nil {
			return 0, err.Error()
		}
		return v, ""
	}
	r.BusyPoll, r.BusyPollErr = get(unix.SO_BUSY_POLL)
	r.PreferBusyPoll, r.PreferErr = get(soPreferBusyPoll)
	r.BusyPollBudget, r.BudgetErr = get(soBusyPollBudget)
	return r
}
