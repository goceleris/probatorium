//go:build linux

package main

// P2c arm "nobp" (throwaway probatorium branch ab/p2c-io512-discriminate-38, not for merge; evidence EV/lanes-20261025/P2C/): celeris main 713b40d with the busy-poll socket
// options switched OFF and nothing else changed. celeris sets SO_BUSY_POLL=50us, SO_PREFER_BUSY_POLL=1 and SO_BUSY_POLL_BUDGET=8 on every accepted socket of both loop engines
// in ONE function, internal/sockopts.applyFD (internal/sockopts/sockopts_linux.go): the io_uring accept path (worker.go), the transplant path, the epoll accept and adopt
// paths all call it, and no other file of the root module sets those options (the drivers set SO_BUSY_POLL on their own client sockets; the io512 handler path uses no driver).
//
// HOW: this module's go.mod carries `replace github.com/goceleris/celeris => ./_celeris`. _celeris/ is the byte-for-byte module-cache copy of
// github.com/goceleris/celeris v1.5.12-0.20261021042539-713b40dfdd0d (= 713b40dfdd0d) with exactly ONE line changed in internal/sockopts/sockopts_linux.go:
//
//	-	if opts.SOBusyPoll > 0 {
//	+	if false && opts.SOBusyPoll > 0 { // P2c nobp: ...
//
// The other options in that function (TCP_NODELAY, TCP_QUICKACK, SO_RCVBUF, SO_SNDBUF) run exactly as on main. nobp_test.go proves the "pinned copy plus one line" claim
// (the module's dirhash with the line reverted equals the h1 in servers/celeris/go.sum) and that the build list equals main's; the probe below proves, on the real engine and
// on msr1 itself, that the accepted sockets carry no busy-poll option. server.go is servers/celeris/server.go plus ONE added line (armBeforeStart(*bind)).

import "log"

// armBeforeStart starts the accepted-socket probe (probe_linux.go). It never blocks and never fails the process.
func armBeforeStart(bind string) {
	log.Printf("[nobp-arm] celeris 713b40d with the busy-poll block of internal/sockopts disabled (replace => ./_celeris); probing the first accepted socket")
	go runAcceptedSocketProbe("nobp", bind)
}
