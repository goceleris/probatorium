package debugvars

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// startHeld builds a refapp-shaped server on engine e (NewServer, so Mount
// and its side listener run exactly as in a refapp) with a hold on /slow,
// starts it, and returns its base URL. The hold starts holdAt after the
// server answers.
func startHeld(t *testing.T, e celeris.EngineType, workers int, spec string) (dv *Vars, base string, log *syncBuf) {
	t.Helper()
	holds, err := ParseFaults(spec)
	if err != nil {
		t.Fatal(err)
	}
	dv = New()
	srv := dv.NewServer(celeris.Config{Engine: e, Protocol: celeris.HTTP1, Workers: workers,
		ReadTimeout: 10 * time.Second, WriteTimeout: 10 * time.Second, ShutdownTimeout: 2 * time.Second})
	InstallFaults(srv, holds)
	srv.GET("/slow", func(c *celeris.Context) error { return c.String(200, "slow") })
	srv.GET("/fast", func(c *celeris.Context) error { return c.String(200, "fast") })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = srv.StartWithListener(ln) }()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	})
	base = "http://" + ln.Addr().String()
	for i := 0; ; i++ {
		if resp, err := http.Get(base + "/fast"); err == nil {
			_ = resp.Body.Close()
			break
		}
		if i > 300 {
			t.Fatal("server never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	log = &syncBuf{}
	StartFaults(holds, time.Now(), log)
	return dv, base, log
}

// getDump fetches a text goroutine dump with a client timeout.
func getDump(url string, timeout time.Duration) (string, time.Duration, error) {
	start := time.Now()
	resp, err := (&http.Client{Timeout: timeout}).Get(url + "/debug/pprof/goroutine?debug=2")
	if err != nil {
		return "", time.Since(start), err
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	return string(b), time.Since(start), err
}

// TestDebugListenerFromEnv is the side listener's contract (celeris#588):
// with DebugAddrEnv set, Mount binds it, announces the bound address on the
// banner line the validator parses, and serves a goroutine dump that names
// both halves of a hold in force -- the holder asleep in run and a request
// blocked in wait -- from its own listener. Unset, nothing is bound and
// nothing is announced.
func TestDebugListenerFromEnv(t *testing.T) {
	var banner syncBuf
	prev := debugBannerOut
	debugBannerOut = &banner
	t.Cleanup(func() { debugBannerOut = prev })

	t.Setenv(DebugAddrEnv, "127.0.0.1:0")
	dv, base, _ := startHeld(t, celeris.Std, 0, "/slow:1500ms@100ms")
	side := dv.DebugAddr()
	if side == "" {
		t.Fatalf("%s set, but Mount bound no side listener", DebugAddrEnv)
	}
	if want := DebugBannerPrefix + side + "\n"; banner.String() != want {
		t.Fatalf("banner %q, want %q", banner.String(), want)
	}
	if side == strings.TrimPrefix(base, "http://") {
		t.Fatalf("side listener %s is the engine's own address", side)
	}
	time.Sleep(250 * time.Millisecond) // inside the hold (100 ms .. 1600 ms)
	go func() { _, _ = (&http.Client{Timeout: 5 * time.Second}).Get(base + "/slow") }()
	time.Sleep(150 * time.Millisecond)
	dump, d, err := getDump("http://"+side, 2*time.Second)
	if err != nil {
		t.Fatalf("side listener dump inside the hold: %v", err)
	}
	for _, frame := range []string{"debugvars.(*FaultHold).run", "debugvars.(*FaultHold).wait"} {
		if !strings.Contains(dump, frame) {
			t.Errorf("side-listener dump taken inside the hold (%s) lacks %s (%d bytes)", d, frame, len(dump))
		}
	}

	t.Run("unset", func(t *testing.T) {
		var b syncBuf
		debugBannerOut = &b
		t.Setenv(DebugAddrEnv, "")
		dv := New()
		_ = dv.NewServer(celeris.Config{Engine: celeris.Std, Protocol: celeris.HTTP1})
		if a := dv.DebugAddr(); a != "" || b.String() != "" {
			t.Fatalf("unset: side listener %q, banner %q; want neither", a, b.String())
		}
	})
}
