package main

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

func TestParseArgs_Defaults(t *testing.T) {
	cfg, err := ParseArgs(nil, &bytes.Buffer{})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MetricsURL == "" {
		t.Fatal("default metrics url empty")
	}
	if cfg.Interval <= 0 {
		t.Fatalf("interval=%s", cfg.Interval)
	}
	if cfg.ValidationSocketPath == "" {
		t.Fatal("default validation socket path empty")
	}
}

func TestSelectPredicates_TierFilter(t *testing.T) {
	specs := selectPredicates("core")
	if len(specs) == 0 {
		t.Fatal("expected core specs")
	}
	for _, s := range specs {
		if s.Tier != "core" {
			t.Errorf("non-core %s tier=%s", s.ID, s.Tier)
		}
	}
}

func TestSelectPredicates_MultiTier(t *testing.T) {
	specs := selectPredicates("core,middleware")
	saw := map[string]bool{}
	for _, s := range specs {
		saw[s.Tier] = true
	}
	if !saw["core"] || !saw["middleware"] {
		t.Fatalf("expected both tiers, saw %v", saw)
	}
}

func TestSelectPredicates_Empty(t *testing.T) {
	if len(selectPredicates("")) == 0 {
		t.Fatal("empty filter should return all")
	}
}

// fakeValidationSocket binds a unix socket and serves the passed
// Counters JSON on GET /snapshot. Returns the socket path; the
// listener is closed via t.Cleanup.
//
// Use /tmp directly (not t.TempDir) — macOS unix-domain socket paths
// are capped at 104 chars; the testing tempdir path easily exceeds
// that with TestPollValidationSocket_… names baked in.
func fakeValidationSocket(t *testing.T, body []byte) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "probtest-vsock-*.sock")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	sock := f.Name()
	_ = f.Close()
	_ = os.Remove(sock) // unix.Listen needs the path to not exist
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv := &http.Server{
		Handler:           mux,
		ReadHeaderTimeout: time.Second,
	}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		_ = os.Remove(sock)
	})
	return sock
}

func TestPollValidationSocket_PopulatesSnapshot(t *testing.T) {
	body, _ := json.Marshal(map[string]int64{
		"panic_count":                0,
		"ratelimit_token_violations": 3,
		"session_owner_mismatches":   1,
		"jwt_late_admits":            0,
		"iouring_sqe_corruptions":    2,
	})
	sock := fakeValidationSocket(t, body)

	hc := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}
	var snap properties.Snapshot
	pollValidationSocket(context.Background(), hc, &snap)

	if snap.RatelimitTokenViolations != 3 {
		t.Errorf("ratelimit: got %d, want 3", snap.RatelimitTokenViolations)
	}
	if snap.SessionOwnerMismatches != 1 {
		t.Errorf("session: got %d, want 1", snap.SessionOwnerMismatches)
	}
	if snap.IouringSQECorruptions != 2 {
		t.Errorf("iouring: got %d, want 2", snap.IouringSQECorruptions)
	}
	if snap.JWTLateAdmits != 0 {
		t.Errorf("jwt: got %d, want 0", snap.JWTLateAdmits)
	}
}

func TestPollValidationSocket_PanicCountWinsOverDebugVars(t *testing.T) {
	// Validation socket reports panic_count=5; legacy /debug/vars
	// already populated PanicCount=2 (a stale-by-one race). The socket
	// value must win because it's the canonical source.
	body, _ := json.Marshal(map[string]int64{"panic_count": 5})
	sock := fakeValidationSocket(t, body)

	hc := &http.Client{
		Timeout: 500 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", sock)
			},
		},
	}
	snap := properties.Snapshot{PanicCount: 2}
	pollValidationSocket(context.Background(), hc, &snap)
	if snap.PanicCount != 5 {
		t.Errorf("PanicCount: got %d, want 5 (socket value wins)", snap.PanicCount)
	}
}

func TestPollValidationSocket_MissingSocketIsNonFatal(t *testing.T) {
	// Production build under test → socket doesn't exist. The poll
	// must leave Snapshot slots untouched (zero); no panic, no error.
	hc := &http.Client{
		Timeout: 200 * time.Millisecond,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				return net.Dial("unix", "/tmp/this-socket-does-not-exist-probatorium-test")
			},
		},
	}
	snap := properties.Snapshot{
		RatelimitTokenViolations: 99, // sentinel; must remain
	}
	pollValidationSocket(context.Background(), hc, &snap)
	if snap.RatelimitTokenViolations != 99 {
		t.Errorf("missing socket clobbered snapshot: got %d, want 99",
			snap.RatelimitTokenViolations)
	}
}
