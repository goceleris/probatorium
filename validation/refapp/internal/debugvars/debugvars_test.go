package debugvars

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"testing"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/recovery"
)

// startRefapp brings up a std-engine celeris server on 127.0.0.1:0 wired
// exactly like the refapps and returns its base URL, the Vars, and the
// recovery sink's record count.
func startRefapp(t *testing.T) (string, *Vars) {
	t.Helper()
	dv := New()
	cfg := celeris.Config{
		Engine:          celeris.Std,
		Protocol:        celeris.HTTP1,
		ReadTimeout:     5 * time.Second,
		WriteTimeout:    5 * time.Second,
		IdleTimeout:     5 * time.Second,
		ShutdownTimeout: 2 * time.Second,
	}
	srv := dv.NewServer(cfg)
	srv.Use(recovery.New(recovery.Config{Logger: dv.RecoveryLogger(slog.New(slog.NewTextHandler(io.Discard, nil)))}))
	srv.GET("/ok", func(c *celeris.Context) error { return c.String(200, "ok") })
	srv.GET("/boom", func(c *celeris.Context) error { panic("boom") })

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.StartWithListener(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	base := "http://" + ln.Addr().String()
	// Wait for the listener to serve.
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(base + "/ok")
		if err == nil {
			_ = resp.Body.Close()
			return base, dv
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("refapp never became ready")
	return "", nil
}

func getVars(t *testing.T, base string) map[string]any {
	t.Helper()
	resp, err := http.Get(base + Path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var doc map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		t.Fatal(err)
	}
	return doc
}

func num(t *testing.T, doc map[string]any, key string) float64 {
	t.Helper()
	v, ok := doc[key].(float64)
	if !ok {
		t.Fatalf("%s: missing or not a number: %#v", key, doc[key])
	}
	return v
}

func TestDebugVars_DocumentShape(t *testing.T) {
	base, _ := startRefapp(t)
	doc := getVars(t, base)
	// Every key the checker parser reads must be a top-level JSON number
	// (memstats nested, Go-cased).
	for _, k := range []string{"goroutines", "celeris.accepted_conn_total", "celeris.closed_conn_total", "celeris.active_conns", "celeris.panic_count", "celeris.adaptive_switches"} {
		num(t, doc, k)
	}
	if num(t, doc, "goroutines") < 1 {
		t.Fatal("goroutines must be positive")
	}
	ms, ok := doc["memstats"].(map[string]any)
	if !ok {
		t.Fatalf("memstats missing: %#v", doc["memstats"])
	}
	if num(t, ms, "HeapInuse") <= 0 || num(t, ms, "HeapAlloc") <= 0 {
		t.Fatalf("memstats HeapInuse/HeapAlloc must be positive: %v %v", ms["HeapInuse"], ms["HeapAlloc"])
	}
	if doc["celeris.engine"] != "std" {
		t.Fatalf("engine=%v want std", doc["celeris.engine"])
	}
	// The endpoint is served by a pre-routing handler: no route was
	// registered for it, yet it answers 200 (a 404 would mean it fell
	// through to the router).
}

func TestDebugVars_ConnectionCountersAndPanics(t *testing.T) {
	base, dv := startRefapp(t)
	// A short-lived connection bumps accepted and, once closed, closed.
	hc := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	for i := 0; i < 3; i++ {
		resp, err := hc.Get(base + "/ok")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && (dv.Accepted() < 3 || dv.Closed() < 3) {
		time.Sleep(20 * time.Millisecond)
	}
	if dv.Accepted() < 3 || dv.Closed() < 3 {
		t.Fatalf("accepted=%d closed=%d; want >= 3 each after 3 Connection: close requests", dv.Accepted(), dv.Closed())
	}
	doc := getVars(t, base)
	if num(t, doc, "celeris.accepted_conn_total") < 3 || num(t, doc, "celeris.closed_conn_total") < 3 {
		t.Fatalf("document counters: %v / %v", doc["celeris.accepted_conn_total"], doc["celeris.closed_conn_total"])
	}
	if num(t, doc, "celeris.panic_count") != 0 {
		t.Fatalf("panic_count before any panic: %v", doc["celeris.panic_count"])
	}
	// Two recovered panics show up as panic_count=2 (500s to the client).
	for i := 0; i < 2; i++ {
		resp, err := hc.Get(base + "/boom")
		if err != nil {
			t.Fatal(err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != 500 {
			t.Fatalf("panic route status %d", resp.StatusCode)
		}
	}
	if got := num(t, getVars(t, base), "celeris.panic_count"); got != 2 {
		t.Fatalf("panic_count=%v want 2", got)
	}
}

func TestDebugVars_MethodAndPeerGuards(t *testing.T) {
	base, _ := startRefapp(t)
	resp, err := http.Post(base+Path, "text/plain", nil)
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != 405 {
		t.Fatalf("POST status %d want 405", resp.StatusCode)
	}
	// pprof is mounted alongside (forensics fetch /debug/pprof/goroutine).
	resp, err = http.Get(base + "/debug/pprof/goroutine")
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("pprof goroutine status %d want 200", resp.StatusCode)
	}
	for remote, want := range map[string]bool{"127.0.0.1:1234": true, "[::1]:9": true, "10.0.0.7:80": false, "garbage": false, "": false} {
		if got := isLoopback(remote); got != want {
			t.Errorf("isLoopback(%q)=%v want %v", remote, got, want)
		}
	}
}

func TestDebugVars_MemStatsCached(t *testing.T) {
	dv := New()
	a := dv.MemStats()
	b := dv.MemStats()
	if a.NumGC != b.NumGC || a.HeapInuse != b.HeapInuse {
		t.Fatal("second read within MemStatsTTL must return the cached copy")
	}
	if a.HeapInuse == 0 {
		t.Fatal("HeapInuse must be populated")
	}
}

func TestRecoveryLogger_CountsOnlyRecoveredPanics(t *testing.T) {
	dv := New()
	lg := dv.RecoveryLogger(nil)
	lg.Error("panic recovered", "error", "x")
	lg.Warn("broken pipe", "error", "y")
	lg.Error("panic in error handler")
	lg.With("request_id", "r1").Error("panic recovered")
	if dv.PanicCount() != 2 {
		t.Fatalf("panic_count=%d want 2", dv.PanicCount())
	}
}
