package debugvars

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
	"time"
)

// DebugAddrEnv names the address of the refapp's debug SIDE LISTENER
// (celeris#588). When set, Mount also serves /debug/pprof/ from net/http on
// that address -- its own listener and goroutines, outside the benched
// engine -- and announces the bound address on stdout, before the ready
// banner, as DebugBannerPrefix + host:port. The validator sets it to
// 127.0.0.1:0 for every refapp it launches locally and points the incident
// dossier's pprof leg at the announced address.
//
// Why a second listener: on celeris's event-loop engines (epoll, io_uring,
// adaptive) a handler runs on its loop, so a stall that parks handlers --
// the #493 shape the #588 capture control injects -- parks the loops, and
// the engine-routed /debug/pprof then waits in a parked loop's accept queue
// until the stall is over. The control's first run at probatorium 5f6b6e6
// showed exactly that: the dossiers taken inside the hold on io_uring got
// their goroutine dump seconds late (after the release, naming nothing) or
// not at all, while std, one goroutine per connection, named holder and
// waiters. A dump that cannot be taken during the stall cannot root-cause it.
const DebugAddrEnv = "PROBATORIUM_REFAPP_DEBUG_ADDR"

// DebugBannerPrefix starts the stdout line that announces the side
// listener's bound address. The validator's stderr supervisor parses it
// (validation.refappDebugBannerPrefix; a test keeps the two equal).
const DebugBannerPrefix = "debug addr="

// debugBannerOut is where the banner goes; stdout, which the validator reads
// merged with stderr. A variable so a test can capture it.
var debugBannerOut io.Writer = os.Stdout

// startDebugListenerFromEnv starts the side listener when DebugAddrEnv is
// set. A bind failure is reported on stderr and is not fatal: the dossier
// then falls back to the engine-routed /debug/pprof, as before.
func (v *Vars) startDebugListenerFromEnv() {
	addr := strings.TrimSpace(os.Getenv(DebugAddrEnv))
	if addr == "" {
		return
	}
	a, err := StartDebugListener(addr, debugBannerOut)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debugvars: %s=%s: %v (the dossier's pprof leg falls back to the engine)\n", DebugAddrEnv, addr, err)
		return
	}
	s := a.String()
	v.debugAddr.Store(&s)
}

// DebugAddr is the side listener's bound address, "" when none is running.
func (v *Vars) DebugAddr() string {
	if p := v.debugAddr.Load(); p != nil {
		return *p
	}
	return ""
}

// StartDebugListener serves /debug/pprof/ on addr from net/http, to loopback
// peers only, and writes DebugBannerPrefix + the bound address to w. It
// returns once the listener is bound; serving runs on its own goroutine for
// the life of the process.
func StartDebugListener(addr string, w io.Writer) (net.Addr, error) {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return nil, err
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{
		Handler: http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			if !isLoopback(r.RemoteAddr) {
				http.Error(rw, "forbidden", http.StatusForbidden)
				return
			}
			mux.ServeHTTP(rw, r)
		}),
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	if w != nil {
		_, _ = fmt.Fprintf(w, "%s%s\n", DebugBannerPrefix, ln.Addr())
	}
	return ln.Addr(), nil
}
