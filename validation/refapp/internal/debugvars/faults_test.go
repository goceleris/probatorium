package debugvars

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

func TestParseFaults(t *testing.T) {
	holds, err := ParseFaults(" /:40s@60s , /ws:8s@30s ")
	if err != nil {
		t.Fatal(err)
	}
	if len(holds) != 2 || holds[0].Path != "/ws" || holds[0].Hold != 8*time.Second || holds[0].At != 30*time.Second ||
		holds[1].Path != "/" || holds[1].Hold != 40*time.Second || holds[1].At != 60*time.Second {
		t.Fatalf("parsed %+v, want /ws 8s@30s then / 40s@60s (sorted by offset)", holds)
	}
	if h, err := ParseFaults(""); h != nil || err != nil {
		t.Fatalf("empty spec: %v %v, want nil nil", h, err)
	}
	for _, bad := range []string{"ws:1s@1s", "/ws:1s", "/ws@1s", "/ws:0s@1s", "/ws:6m@1s", "/ws:1s@-1s", "/ws:x@1s", "/ws:1s@1s,/ws:2s@3s"} {
		if _, err := ParseFaults(bad); err == nil {
			t.Errorf("%q: parsed, want an error", bad)
		}
	}
}

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  strings.Builder
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

func timedGet(t *testing.T, url string) (time.Duration, int) {
	t.Helper()
	start := time.Now()
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	return time.Since(start), resp.StatusCode
}

// TestFaultHoldStallsOnlyItsPath is the fault's contract with the #588
// control: while a hold is in force its path blocks, every other path and
// the dossier's endpoints keep answering, the goroutine dump taken through
// the refapp's own /debug/pprof inside the hold carries both halves of the
// signature (the holder asleep in run, a request blocked in wait), and the
// hold logs its own start and release as ground truth.
func TestFaultHoldStallsOnlyItsPath(t *testing.T) {
	holds, err := ParseFaults("/slow:900ms@150ms")
	if err != nil {
		t.Fatal(err)
	}
	dv := New()
	srv := dv.NewServer(celeris.Config{Engine: celeris.Std, Protocol: celeris.HTTP1,
		ReadTimeout: 5 * time.Second, WriteTimeout: 5 * time.Second, ShutdownTimeout: 2 * time.Second})
	InstallFaults(srv, holds)
	srv.GET("/slow", func(c *celeris.Context) error { return c.String(200, "slow") })
	srv.GET("/fast", func(c *celeris.Context) error { return c.String(200, "fast") })
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
	for i := 0; ; i++ {
		if resp, err := http.Get(base + "/fast"); err == nil {
			_ = resp.Body.Close()
			break
		}
		if i > 200 {
			t.Fatal("server never came up")
		}
		time.Sleep(10 * time.Millisecond)
	}
	var log syncBuf
	StartFaults(holds, time.Now(), &log)
	time.Sleep(300 * time.Millisecond) // inside the hold (150 ms .. 1050 ms)

	slowDone := make(chan time.Duration, 1)
	go func() { d, _ := timedGet(t, base+"/slow"); slowDone <- d }()
	time.Sleep(100 * time.Millisecond)

	if d, code := timedGet(t, base+"/fast"); code != 200 || d > 300*time.Millisecond {
		t.Errorf("/fast during the hold: %d in %s, want 200 promptly", code, d)
	}
	if d, code := timedGet(t, base+"/debug/vars"); code != 200 || d > 300*time.Millisecond {
		t.Errorf("/debug/vars during the hold: %d in %s, want 200 promptly", code, d)
	}
	resp, err := http.Get(base + "/debug/pprof/goroutine?debug=2")
	if err != nil {
		t.Fatalf("goroutine dump during the hold: %v", err)
	}
	dump, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	for _, frame := range []string{"debugvars.(*FaultHold).run", "debugvars.(*FaultHold).wait"} {
		if !strings.Contains(string(dump), frame) {
			t.Errorf("goroutine dump taken inside the hold lacks %s (%d bytes)", frame, len(dump))
		}
	}
	if d := <-slowDone; d < 450*time.Millisecond {
		t.Errorf("/slow returned in %s, want it held until the release (~650 ms after it was sent)", d)
	}
	time.Sleep(100 * time.Millisecond)
	out := log.String()
	if !strings.Contains(out, "[fault] hold path=/slow hold=900ms start=") || !strings.Contains(out, "[fault] release path=/slow end=") {
		t.Errorf("fault log lacks the hold / release ground truth:\n%s", out)
	}
	if !strings.Contains(out, "waiters=1") {
		t.Errorf("fault log should count the one blocked request:\n%s", out)
	}
	if d, _ := timedGet(t, base+"/slow"); d > 300*time.Millisecond {
		t.Errorf("/slow after the release took %s", d)
	}
}
