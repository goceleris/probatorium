package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// The I-HANG dossier of a wedged refapp is the one the stall capture's side
// listener exists for: the engine-routed /debug/pprof is parked by the wedge,
// the side listener is not. Through the whole run path -- Tier 1's hang
// detector, the final tally tick, Run's hard-fail capture -- the dossier must
// be taken from the side listener while the refapp is still alive. Until the
// re-review of probatorium#412 round 3 it never was: the final tick returned
// once the wedge was in the channel, driveTier1 returned and SIGTERMed the
// refapp, runTierProperty forgot the side-listener accessor, and only then
// did Run's loop capture -- pprof_source=engine against the wedged engine, no
// goroutine dump (0 of 17 live I-HANG dossiers since b7d91bf had one).

// wedgedEngine stands in for a wedged refapp's engine: /healthz is accepted
// and never answered (what the hang detector reads as a wedge), the
// engine-routed /debug/pprof is parked (503), /debug/vars still answers so
// the property loop has samples, and every other route is 404.
func wedgedEngine(t *testing.T) *httptest.Server {
	t.Helper()
	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/healthz":
			select {
			case <-r.Context().Done():
			case <-release:
			}
		case r.URL.Path == "/debug/vars":
			_, _ = w.Write([]byte(`{"goroutines": 40, "celeris.accepted_conn_total": 1, "celeris.closed_conn_total": 0,
				"celeris.active_conns": 1, "celeris.panic_count": 0, "memstats": {"HeapInuse": 4194304, "HeapAlloc": 3000000}}`))
		case strings.HasPrefix(r.URL.Path, "/debug/pprof/"):
			w.WriteHeader(http.StatusServiceUnavailable)
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(release) }) // runs first: unpark /healthz so Close returns
	return srv
}

// sideListener stands in for the refapp's debug side listener. It lives as
// long as the refapp does: once the refapp has been sent SIGTERM (its trap
// touches term) every request is refused. The text goroutine dump takes a
// while on a real refapp; here it waits up to a second for a SIGTERM before
// answering, so a dump fetched while the refapp is being torn down fails the
// way it does on the cluster instead of winning the race by luck.
func sideListener(t *testing.T, term string, refused *atomic.Int64) *httptest.Server {
	t.Helper()
	terminated := func() bool { _, err := os.Stat(term); return err == nil }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/debug/pprof/goroutine" && r.URL.Query().Get("debug") == "2" {
			for deadline := time.Now().Add(time.Second); time.Now().Before(deadline) && !terminated(); {
				time.Sleep(10 * time.Millisecond)
			}
		}
		if terminated() {
			refused.Add(1)
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		if r.URL.Query().Get("debug") == "2" {
			_, _ = w.Write([]byte("goroutine 1 [sync.Mutex.Lock, 3 minutes]:\nmain.wedged()\n"))
			return
		}
		_, _ = w.Write([]byte("side-profile:" + strings.TrimPrefix(r.URL.Path, "/debug/pprof/")))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestRun_WedgeDossierIsTakenFromTheSideListenerBeforeTheRefappIsStopped(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	t.Setenv(stallCaptureEnv, "1") // the side listener exists in a stall-capture run
	t.Setenv(refappDebugAddrEnv, "")
	defer tightenHangProbe()()

	dir := t.TempDir()
	term := filepath.Join(dir, "sigterm")
	var refused atomic.Int64
	engine := wedgedEngine(t)
	side := sideListener(t, term, &refused)
	engineAddr := engine.Listener.Addr().String()
	sideAddr := side.Listener.Addr().String()

	// The refapp announces its side listener, then its engine, and lives
	// until signalled; a SIGTERM marks it terminated (the local driver
	// signals the whole group, so the sleep ends at once and the trap runs).
	bin := filepath.Join(dir, "refapp.sh")
	script := "#!/bin/sh\n" +
		"trap 'touch \"" + term + "\"; exit 0' TERM\n" +
		"echo \"" + refappDebugBannerPrefix + sideAddr + "\"\n" +
		"echo \"ready addr=" + engineAddr + "\"\n" +
		"while :; do sleep 1; done\n"
	if err := os.WriteFile(bin, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := Default()
	cfg.OutDir = filepath.Join(dir, "out")
	cfg.Duration = 30 * time.Second
	cfg.CelerisBin = bin
	cfg.CelerisListenAddr = engineAddr
	cfg.MarkovPath = "markov/auth_session_ratelimit.yaml"
	cfg.PropertyTier = "core"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	started := time.Now()
	err = o.Run(ctx)
	if err == nil || !strings.Contains(err.Error(), "I-HANG") {
		t.Fatalf("a wedged refapp must end the run as I-HANG, got %v", err)
	}
	if took := time.Since(started); took > 20*time.Second {
		t.Fatalf("the wedge must end the cell early, took %s", took)
	}

	dirs := incidentDirs(t, cfg.OutDir, "I-HANG")
	if len(dirs) != 1 {
		t.Fatalf("want exactly one I-HANG dossier, got %v", dirs)
	}
	status, err := os.ReadFile(filepath.Join(dirs[0], "forensics_status.txt"))
	if err != nil {
		t.Fatalf("forensics_status.txt: %v", err)
	}
	if !strings.Contains(string(status), `pprof="`+sideAddr+`" pprof_source=side-listener`) {
		t.Errorf("the I-HANG dossier did not take its pprof from the live side listener %s (the accessor was already reset?):\n%s", sideAddr, status)
	}
	stacks, err := os.ReadFile(filepath.Join(dirs[0], "goroutine-stacks.txt"))
	if err != nil {
		missing, _ := os.ReadFile(filepath.Join(dirs[0], "goroutine-stacks.txt.missing"))
		t.Fatalf("the I-HANG dossier has no goroutine dump of the wedged refapp (%v; %s): %s", err, strings.TrimSpace(string(missing)), status)
	}
	if !strings.Contains(string(stacks), "main.wedged()") {
		t.Errorf("goroutine-stacks.txt is not the side listener's dump: %q", stacks)
	}
	if n := refused.Load(); n != 0 {
		t.Errorf("the side listener refused %d request(s) of the capture: the refapp had been sent SIGTERM before its I-HANG dossier was taken", n)
	}
}
