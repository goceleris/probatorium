package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func httpGet(t *testing.T, url string) (int, string) {
	t.Helper()
	c := &http.Client{Timeout: 20 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

func TestPprofMode(t *testing.T) {
	for _, tc := range []struct {
		raw  string
		set  bool
		want string
		err  bool
	}{
		{"", false, "", false},
		{"", true, "", false},
		{"0", true, "", false},
		{"dump", true, "dump", false},
		{"profile", true, "profile", false},
		{"both", true, "both", false},
		{"1", true, "", true},
		{"true", true, "", true},
		{"Dump", true, "", true},
		{"dump,profile", true, "", true},
		{" dump", true, "", true},
	} {
		got, err := pprofMode(tc.raw, tc.set)
		if (err != nil) != tc.err || got != tc.want {
			t.Errorf("pprofMode(%q, %v) = %q, %v; want %q, err=%v", tc.raw, tc.set, got, err, tc.want, tc.err)
		}
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:18089":      true,
		"127.0.0.1:0":          true,
		"127.1.2.3:80":         true,
		"[::1]:18089":          true,
		"0.0.0.0:18089":        false,
		"[::]:18089":           false,
		":18089":               false,
		"10.0.0.7:18089":       false,
		"192.168.1.5:80":       false,
		"localhost:18089":      false, // a hostname depends on a resolver: refused
		"127.0.0.1":            false, // no port: not what net.Listener.Addr prints
		"":                     false,
		"[::ffff:10.0.0.1]:80": false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
	}
}

func TestMountPprofRefusesANonLoopbackListener(t *testing.T) {
	for _, addr := range []string{"0.0.0.0:18089", "[::]:18089", "10.0.0.7:18089", "localhost:80"} {
		mux := http.NewServeMux()
		if err := mountPprof(mux, addr); err == nil || !strings.Contains(err.Error(), "loopback") {
			t.Errorf("mountPprof on %s: err = %v, want a loopback refusal", addr, err)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/", nil))
		if rec.Code != 404 {
			t.Errorf("after the refusal on %s the mux serves /debug/pprof/ with %d, want 404 (nothing mounted)", addr, rec.Code)
		}
	}
	t.Setenv(pprofEnv, "both")
	if mode, err := mountPprofFromEnv(http.NewServeMux(), "0.0.0.0:18089"); err == nil || mode != "" {
		t.Errorf("mountPprofFromEnv on a non-loopback address = %q, %v; want a refusal", mode, err)
	}
}

func TestMountPprofFromEnvBadValueIsAnError(t *testing.T) {
	for _, v := range []string{"1", "true", "yes", "Dump", "dump,profile"} {
		t.Setenv(pprofEnv, v)
		mux := http.NewServeMux()
		if mode, err := mountPprofFromEnv(mux, "127.0.0.1:1"); err == nil || mode != "" {
			t.Errorf("%s=%q: mode %q, err %v; want an error", pprofEnv, v, mode, err)
		}
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest("GET", "/debug/pprof/", nil))
		if rec.Code != 404 {
			t.Errorf("%s=%q mounted pprof (%d) although the value is bad", pprofEnv, v, rec.Code)
		}
	}
}

// With the gate unset (every column that does not ask for it) the side listener answers
// /debug/pprof with 404 and /debug/vars as before.
func TestPprofOffByDefaultOnTheSideListener(t *testing.T) {
	srv, _ := startStd(t)
	t.Setenv(debugAddrEnv, "127.0.0.1:0")
	for _, v := range []string{"", "0"} {
		if v == "" {
			t.Setenv(pprofEnv, "")
		} else {
			t.Setenv(pprofEnv, v)
		}
		addr, stop := startDebugVars(srv)
		if addr == "" {
			t.Fatal("side listener did not start")
		}
		for _, path := range []string{"/debug/pprof/", "/debug/pprof/goroutine?debug=1", "/debug/pprof/heap?debug=1", "/debug/pprof/profile?seconds=1", "/debug/pprof/cmdline"} {
			if code, _ := httpGet(t, "http://"+addr+path); code != 404 {
				t.Errorf("%s=%q: GET %s on the side listener = %d, want 404", pprofEnv, v, path, code)
			}
		}
		if code, _ := httpGet(t, "http://"+addr+"/debug/vars"); code != 200 {
			t.Errorf("/debug/vars on the side listener = %d, want 200", code)
		}
		stop()
	}
}

// With the gate on, heap, goroutine and the CPU profile are served on the side listener, and
// /debug/vars is unchanged.
func TestPprofOnServesTheProfilesOnTheSideListener(t *testing.T) {
	srv, _ := startStd(t)
	t.Setenv(debugAddrEnv, "127.0.0.1:0")
	t.Setenv(pprofEnv, "both")
	addr, stop := startDebugVars(srv)
	defer stop()
	if addr == "" {
		t.Fatal("side listener did not start")
	}
	if !isLoopbackAddr(addr) {
		t.Fatalf("test listener %s is not loopback", addr)
	}
	for _, tc := range []struct{ path, want string }{
		{"/debug/pprof/", "goroutine"},
		{"/debug/pprof/goroutine?debug=1", "goroutine profile:"},
		{"/debug/pprof/goroutine?debug=2", "goroutine "},
		{"/debug/pprof/heap?debug=1", "heap profile:"},
	} {
		code, body := httpGet(t, "http://"+addr+tc.path)
		if code != 200 || !strings.Contains(body, tc.want) {
			t.Errorf("GET %s = %d, want 200 containing %q (body %.80q)", tc.path, code, tc.want, body)
		}
	}
	// The CPU profile is binary (gzip): 1 s is enough to prove the handler works in-process.
	code, body := httpGet(t, "http://"+addr+"/debug/pprof/profile?seconds=1")
	if code != 200 || len(body) < 10 || body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("GET /debug/pprof/profile?seconds=1 = %d, %d bytes, not gzip", code, len(body))
	}
	// The binary goroutine profile (what pprofcap.py stores) is gzip too.
	code, body = httpGet(t, "http://"+addr+"/debug/pprof/goroutine")
	if code != 200 || len(body) < 10 || body[0] != 0x1f || body[1] != 0x8b {
		t.Errorf("GET /debug/pprof/goroutine = %d, %d bytes, not gzip", code, len(body))
	}
	code, doc := getJSON(t, "http://"+addr+"/debug/vars")
	if code != 200 || doc["pid"] == nil || doc["celeris.engine_metrics"] == nil {
		t.Errorf("/debug/vars changed with pprof on: %d %v", code, doc)
	}
}

// THE negative test: the benched engine (the public listener) never serves pprof, with the gate
// on and the side listener running. net/http/pprof's init registers its routes on
// http.DefaultServeMux, so this also fails if the engine (or anything it links) ever starts
// serving that mux.
func TestPprofIsNotOnTheBenchedEngine(t *testing.T) {
	srv, benchAddr := startStd(t)
	t.Setenv(debugAddrEnv, "127.0.0.1:0")
	t.Setenv(pprofEnv, "both")
	sideAddr, stop := startDebugVars(srv)
	defer stop()
	if sideAddr == "" {
		t.Fatal("side listener did not start")
	}
	// the side listener serves it (so a 404 on the bench port is not "pprof is broken")
	if code, _ := httpGet(t, "http://"+sideAddr+"/debug/pprof/goroutine?debug=1"); code != 200 {
		t.Fatalf("side listener GET goroutine = %d, want 200", code)
	}
	for _, path := range []string{
		"/debug/pprof/", "/debug/pprof", "/debug/pprof/goroutine?debug=1", "/debug/pprof/goroutine?debug=2",
		"/debug/pprof/heap?debug=1", "/debug/pprof/profile?seconds=1", "/debug/pprof/cmdline", "/debug/pprof/symbol", "/debug/pprof/trace?seconds=1",
	} {
		if code, body := httpGet(t, "http://"+benchAddr+path); code != 404 {
			t.Errorf("GET %s on the BENCH port = %d, want 404 (body %.80q)", path, code, body)
		}
	}
}
