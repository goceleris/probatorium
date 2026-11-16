package main

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// startStd starts a celeris std-engine server with the bench routes on a
// loopback port and returns it with its address. std runs on every OS, so
// these tests run on the dev Mac and in CI alike.
func startStd(t *testing.T) (*celeris.Server, string) {
	t.Helper()
	srv := celeris.New(celeris.Config{Engine: celeris.Std})
	registerRoutes(srv)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListenerAndContext(ctx, ln) }()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(10 * time.Second)
	for srv.EngineInfo() == nil {
		if time.Now().After(deadline) {
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	return srv, ln.Addr().String()
}

func getJSON(t *testing.T, url string) (int, map[string]any) {
	t.Helper()
	c := &http.Client{Timeout: 5 * time.Second}
	resp, err := c.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	var doc map[string]any
	_ = json.Unmarshal(body, &doc)
	return resp.StatusCode, doc
}

// TestDebugVarsSideListenerServesTheEngineBlock is the celeris#585
// prerequisite: the URL the bench observer scrapes must answer with the
// runtime fields it already reads AND the engine counters a SEND_ZC A/B
// needs, from the process the observer samples.
func TestDebugVarsSideListenerServesTheEngineBlock(t *testing.T) {
	srv, _ := startStd(t)
	t.Setenv(debugAddrEnv, "127.0.0.1:0")
	addr, stop := startDebugVars(srv)
	defer stop()
	if addr == "" {
		t.Fatal("side listener did not start with PROBATORIUM_DEBUG_ADDR set")
	}
	code, doc := getJSON(t, "http://"+addr+"/debug/vars")
	if code != 200 {
		t.Fatalf("GET /debug/vars on the side listener: status %d", code)
	}
	if pid, _ := doc["pid"].(float64); int(pid) != os.Getpid() {
		t.Errorf("pid=%v, want this process %d (the observer refuses a document from another pid)", doc["pid"], os.Getpid())
	}
	if g, _ := doc["goroutines"].(float64); g <= 0 {
		t.Errorf("goroutines=%v, want > 0", doc["goroutines"])
	}
	ms, _ := doc["memstats"].(map[string]any)
	if h, _ := ms["HeapInuse"].(float64); h <= 0 {
		t.Errorf("memstats.HeapInuse=%v, want > 0", ms["HeapInuse"])
	}
	if e, _ := doc["celeris.engine"].(string); e == "" {
		t.Errorf("celeris.engine missing: %v", doc)
	}
	em, ok := doc["celeris.engine_metrics"].(map[string]any)
	if !ok {
		t.Fatalf("celeris.engine_metrics missing or not an object: %v", doc["celeris.engine_metrics"])
	}
	// The field names cmd/observer reads (observer engineKeys). A celeris
	// that renames one would silently turn the A/B's exposure witness into
	// "absent"; this pins them at the pinned version.
	for _, k := range []string{"ZCSendsSubmitted", "ZCNotifs", "InlineBytes", "RingBytes", "BytesWritten"} {
		if _, ok := em[k].(float64); !ok {
			t.Errorf("celeris.engine_metrics.%s missing or not a number (have %d keys)", k, len(em))
		}
	}
}

// TestDebugVarsIsNotARouteOnTheBenchedEngine: the document must not be
// served by the engine under test -- a route there changes the router the
// bench measures and puts the observer's connection on the benched engine.
func TestDebugVarsIsNotARouteOnTheBenchedEngine(t *testing.T) {
	srv, benchAddr := startStd(t)
	t.Setenv(debugAddrEnv, "127.0.0.1:0")
	_, stop := startDebugVars(srv)
	defer stop()
	code, _ := getJSON(t, "http://"+benchAddr+"/debug/vars")
	if code != 404 {
		t.Fatalf("GET /debug/vars on the BENCH port: status %d, want 404", code)
	}
}

// TestDebugVarsOffWithoutTheEnv: no env, no listener -- a bench that does
// not ask for it runs exactly the binary it ran before.
func TestDebugVarsOffWithoutTheEnv(t *testing.T) {
	srv, _ := startStd(t)
	t.Setenv(debugAddrEnv, "")
	addr, stop := startDebugVars(srv)
	defer stop()
	if addr != "" {
		t.Fatalf("side listener started on %s with %s unset", addr, debugAddrEnv)
	}
}

// TestDebugVarsBindFailureIsNotFatal: a taken sidecar port disables the
// document with a log line; it must never take the bench down.
func TestDebugVarsBindFailureIsNotFatal(t *testing.T) {
	srv, _ := startStd(t)
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = taken.Close() }()
	t.Setenv(debugAddrEnv, taken.Addr().String())
	addr, stop := startDebugVars(srv)
	defer stop()
	if addr != "" {
		t.Fatalf("side listener claims %s although the port is taken", addr)
	}
}
