package main

import (
	"context"
	"net"
	"os"
	"runtime/debug"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

func envOf(v string, set bool) func(string) string {
	return func(k string) string {
		if k == gcPercentEnv && set {
			return v
		}
		return ""
	}
}

// TestGCPercentFromEnv pins the adapter switch's contract (celeris#756):
// unset, empty and "0" all mean "the library does not touch the runtime"
// (Config.GCPercent == 0), a positive integer is forwarded verbatim, and
// anything else makes the adapter refuse to start. A negative value is
// refused because it would reach debug.SetGCPercent as "GC off".
func TestGCPercentFromEnv(t *testing.T) {
	for _, tc := range []struct {
		name    string
		v       string
		set     bool
		want    int
		wantErr bool
	}{
		{"unset", "", false, 0, false},
		{"empty", "", true, 0, false},
		{"spaces", "  ", true, 0, false},
		{"zero", "0", true, 0, false},
		{"default-equivalent 100", "100", true, 100, false},
		{"400", "400", true, 400, false},
		{"padded", " 400 ", true, 400, false},
		{"negative one (GC off)", "-1", true, 0, true},
		{"negative", "-50", true, 0, true},
		{"word", "off", true, 0, true},
		{"float", "400.5", true, 0, true},
		{"percent sign", "400%", true, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := gcPercentFromEnv(envOf(tc.v, tc.set))
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, wantErr %v", err, tc.wantErr)
			}
			if got != tc.want {
				t.Fatalf("got %d, want %d", got, tc.want)
			}
		})
	}
}

// runStd starts a celeris std-engine server with cfg on a loopback port,
// waits until it is serving (the library applies Config.GCPercent at Start)
// and returns a stop func that shuts it down and waits for Start to return.
func runStd(t *testing.T, cfg celeris.Config) (stop func()) {
	t.Helper()
	cfg.Engine = celeris.Std
	srv := celeris.New(cfg)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.StartWithListenerAndContext(ctx, ln) }()
	deadline := time.Now().Add(10 * time.Second)
	for srv.EngineInfo() == nil {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("server did not start")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var once sync.Once
	stop = func() { once.Do(func() { cancel(); <-done }) }
	t.Cleanup(stop)
	return stop
}

// withSentinelGC sets a non-default GC target (137: not 100, not 400) so a
// stray write of ANY value by the code under test is visible, and restores
// the previous target when the test ends.
func withSentinelGC(t *testing.T) {
	t.Helper()
	prev := debug.SetGCPercent(137)
	t.Cleanup(func() { debug.SetGCPercent(prev) })
}

// TestExistingColumnsAreUnchangedWhenTheEnvIsUnset is the "default-off"
// witness for every production column and the v1.5.11 twin: with the switch
// unset (the state of every column that does not opt in) the adapter builds
// Config.GCPercent == 0 and a STARTED server leaves the process GC target
// exactly where it was, through Start and through Shutdown. The sentinel makes
// any stray SetGCPercent call visible. No t.Parallel: it owns the
// process-global GC target.
func TestExistingColumnsAreUnchangedWhenTheEnvIsUnset(t *testing.T) {
	withSentinelGC(t)
	t.Setenv(gcPercentEnv, "")
	n, err := gcPercentFromEnv(os.Getenv)
	if err != nil || n != 0 {
		t.Fatalf("unset switch: got (%d, %v), want (0, nil)", n, err)
	}
	stop := runStd(t, celeris.Config{GCPercent: n})
	if got := effectiveGCPercent(); got != 137 {
		t.Fatalf("GC target %d while serving with the switch unset, want the untouched sentinel 137", got)
	}
	stop()
	if got := effectiveGCPercent(); got != 137 {
		t.Fatalf("GC target %d after Shutdown with the switch unset, want 137", got)
	}
}

// TestGCPercentEnvReachesTheProcess is the end-to-end path the closing lane's
// identity gate relies on: env -> gcPercentFromEnv -> Config.GCPercent ->
// Start sets the target -> Shutdown gives the previous value back.
func TestGCPercentEnvReachesTheProcess(t *testing.T) {
	withSentinelGC(t)
	t.Setenv(gcPercentEnv, "400")
	n, err := gcPercentFromEnv(os.Getenv)
	if err != nil || n != 400 {
		t.Fatalf("switch=400: got (%d, %v), want (400, nil)", n, err)
	}
	stop := runStd(t, celeris.Config{GCPercent: n})
	if got := effectiveGCPercent(); got != 400 {
		t.Fatalf("GC target %d while serving with the switch at 400, want 400", got)
	}
	stop()
	if got := effectiveGCPercent(); got != 137 {
		t.Fatalf("GC target %d after Shutdown, want the previous value 137 back", got)
	}
}

// TestDebugVarsReportsTheGCTargetInForce: the observer's witness that the
// option reached the process. The value is read, never written.
func TestDebugVarsReportsTheGCTargetInForce(t *testing.T) {
	prev := debug.SetGCPercent(250)
	defer debug.SetGCPercent(prev)
	if got := effectiveGCPercent(); got != 250 {
		t.Fatalf("effectiveGCPercent = %d, want 250", got)
	}
	t.Setenv(gcPercentEnv, "250")
	doc := debugVarsDocument(celeris.New(celeris.Config{Engine: celeris.Std}))
	if g, _ := doc["gc_percent"].(int); g != 250 {
		t.Errorf(`doc["gc_percent"] = %v, want 250`, doc["gc_percent"])
	}
	if e, _ := doc["gc_percent_env"].(string); e != "250" {
		t.Errorf(`doc["gc_percent_env"] = %q, want "250"`, e)
	}
}
