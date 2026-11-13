package main

// net/http/pprof on the bench SUT's loopback side listener (v1.6.0 attribution campaign,
// harness change H7', PPROF-1 reduced to what is not already built).
//
// debugvars.go already runs a loopback-only side listener with its own mux (PROBATORIUM_DEBUG_ADDR,
// set by ansible/tasks/run_bench_cell.yml); this file adds the pprof handlers to THAT mux and to
// nothing else, behind a second gate, PROBATORIUM_PPROF:
//
//   - unset, empty or "0": off. Nothing is mounted; the listener answers /debug/pprof/ with 404.
//   - "dump", "profile" or "both": on. The handlers (index, heap, goroutine, profile, ...) are
//     mounted on the side mux. The value is also what ansible/files/pprofcap.py reads to decide
//     what to collect: "dump" = the goroutine?debug=2 dump after a second with zero completions
//     (celeris#854), "profile" = a 30 s CPU profile and a goroutine profile in the middle of the
//     window (celeris#857), "both" = both. Any other value is a fatal start error, never a silent
//     "off": a column that was meant to be profiled and was not would cost its hours.
//
// What keeps the profile off the benched engine (pinned by pprof_sidecar_test.go):
//
//   - the handlers are registered one by one on the side mux, never through the package's init,
//     which registers on http.DefaultServeMux. Importing net/http/pprof therefore still puts them
//     on DefaultServeMux, and nothing in this binary serves DefaultServeMux (the bench engine is
//     celeris's own router; the Std engine builds its own http.Server). TestPprofIsNotOnTheBenchedEngine
//     fails if a future celeris serves it;
//   - the side listener must be bound to a loopback address, or the mount is refused (fatal): a
//     profile endpoint on a routable address would be a remote-readable dump of the process.
//     PROBATORIUM_DEBUG_ADDR is set to 127.0.0.1:<port> by the playbook; this check turns "loopback
//     only" from a convention into a property;
//   - the CPU profile is in-process (SIGPROF); no kernel setting is touched (perf_event_paranoid
//     stays as it is). Neither a dispatch-wide sut_env nor a registry column env can reach an
//     attribution column's gate by accident: BENCH_SUT_ENV must be empty for the attribution
//     columns (refuseSUTEnvOnArmColumns), so the gate only exists on columns whose own SUTEnv carries
//     it. The D9 run (c1arm-*) carries none.
//
// This file is mirrored byte for byte in servers/celeris_v1511 and in every ladder module
// (servers/celeris_*), like busypoll_knob.go; it uses only the standard library, so it compiles
// against any celeris.

import (
	"fmt"
	"net"
	"net/http"
	"net/http/pprof"
	"os"
	"strings"
)

// pprofEnv is the gate. See the file comment for the values.
const pprofEnv = "PROBATORIUM_PPROF"

// pprofMode returns the validated mode of the env value: "" (off), "dump", "profile" or "both".
func pprofMode(raw string, set bool) (string, error) {
	if !set {
		return "", nil
	}
	switch raw {
	case "", "0":
		return "", nil
	case "dump", "profile", "both":
		return raw, nil
	}
	return "", fmt.Errorf("%s=%q: want one of unset, 0, dump, profile, both", pprofEnv, raw)
}

// isLoopbackAddr reports whether addr (host:port, as net.Listener.Addr().String() prints it) is a
// loopback IP literal. A hostname is refused: the check must not depend on a resolver.
func isLoopbackAddr(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && ip.IsLoopback()
}

// mountPprof registers the pprof handlers on mux, which must be the mux of the side listener bound
// at addr. It registers the same routes net/http/pprof's init puts on http.DefaultServeMux, but
// explicitly and on this mux only.
func mountPprof(mux *http.ServeMux, addr string) error {
	if !isLoopbackAddr(addr) {
		return fmt.Errorf("refusing to mount /debug/pprof on %s: the side listener is not bound to a loopback IP", addr)
	}
	mux.HandleFunc("GET /debug/pprof/", pprof.Index)
	mux.HandleFunc("GET /debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("GET /debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("GET /debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("GET /debug/pprof/trace", pprof.Trace)
	return nil
}

// mountPprofFromEnv reads PROBATORIUM_PPROF and, when it is on, mounts the handlers on the side
// mux. It returns the mode ("" = off). A bad value or a non-loopback listener is an error; the
// caller (startDebugVars) treats it as fatal.
func mountPprofFromEnv(mux *http.ServeMux, addr string) (string, error) {
	raw, set := os.LookupEnv(pprofEnv)
	mode, err := pprofMode(raw, set)
	if err != nil || mode == "" {
		return "", err
	}
	if err := mountPprof(mux, addr); err != nil {
		return "", err
	}
	return mode, nil
}
