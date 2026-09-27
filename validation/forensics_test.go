package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestCaptureForensicsLive_NoPIDNoListenStillWritesStatus(t *testing.T) {
	// Worst-case path: no PID, no pprof endpoint. Forensics still
	// has to produce the status manifest so the incident dossier
	// is grep-able from postmortem.
	dir := t.TempDir()
	err := captureForensicsLive(context.Background(), dir, 0, "")
	if err != nil {
		t.Fatalf("captureForensicsLive: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "forensics_status.txt"))
	if err != nil {
		t.Fatalf("status file missing: %v", err)
	}
	if !strings.Contains(string(body), "pid=0") {
		t.Errorf("status should record pid=0, got %q", body)
	}
}

func TestCaptureForensicsLive_OurOwnPIDOnLinux(t *testing.T) {
	if runtime.GOOS != "linux" {
		t.Skip("/proc only exists on linux")
	}
	dir := t.TempDir()
	if err := captureForensicsLive(context.Background(), dir, os.Getpid(), ""); err != nil {
		t.Fatalf("captureForensicsLive: %v", err)
	}
	// On Linux we should at least get cmdline + status + fd.
	for _, want := range []string{"proc-cmdline.txt", "proc-status.txt", "proc-fd.txt"} {
		if _, err := os.Stat(filepath.Join(dir, want)); err != nil {
			t.Errorf("expected %s, missing: %v", want, err)
		}
	}
	// Status should always be written.
	if _, err := os.Stat(filepath.Join(dir, "forensics_status.txt")); err != nil {
		t.Errorf("forensics_status.txt missing")
	}
}

func TestCaptureForensicsLive_NonLinuxStillWritesStatus(t *testing.T) {
	if runtime.GOOS == "linux" {
		t.Skip("this branch covers macOS / non-linux dev hosts")
	}
	dir := t.TempDir()
	// Real PID, but /proc doesn't exist — every snapshotFile call
	// fails and writes a .missing marker. forensics_status.txt is
	// still written.
	if err := captureForensicsLive(context.Background(), dir, os.Getpid(), ""); err != nil {
		t.Fatalf("captureForensicsLive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "forensics_status.txt")); err != nil {
		t.Fatalf("status file missing: %v", err)
	}
	// At least one .missing marker should be present.
	matches, _ := filepath.Glob(filepath.Join(dir, "*.missing"))
	if len(matches) == 0 {
		t.Errorf("expected .missing markers on non-linux, got none")
	}
}

func TestCaptureForensicsLive_PprofCurl(t *testing.T) {
	// Fake pprof server returns a small body for every /debug/pprof/*
	// request, and the full-text goroutine dump for ?debug=2 only.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		if r.URL.Path == "/debug/pprof/goroutine" && r.URL.Query().Get("debug") == "2" {
			_, _ = w.Write([]byte("goroutine 1 [running]:"))
			return
		}
		_, _ = w.Write([]byte("pprof-bytes"))
	}))
	defer srv.Close()
	// Strip http:// so captureForensicsLive's "http://"+listenAddr prefix
	// works (matches the orchestrator's CelerisListenAddr shape).
	listenAddr := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	if err := captureForensicsLive(context.Background(), dir, 0, listenAddr); err != nil {
		t.Fatalf("captureForensicsLive: %v", err)
	}
	for _, want := range []string{
		"heap.pprof",
		"goroutine.pprof",
		"block.pprof",
		"mutex.pprof",
		"threadcreate.pprof",
	} {
		body, err := os.ReadFile(filepath.Join(dir, want))
		if err != nil {
			t.Errorf("expected %s, missing: %v", want, err)
			continue
		}
		if string(body) != "pprof-bytes" {
			t.Errorf("%s body: got %q, want pprof-bytes", want, body)
		}
	}
	// celeris#588: the text stacks, fetched with debug=2.
	if body, err := os.ReadFile(filepath.Join(dir, "goroutine-stacks.txt")); err != nil || string(body) != "goroutine 1 [running]:" {
		t.Errorf("goroutine-stacks.txt: %q %v, want the debug=2 text dump", body, err)
	}
}

func TestCaptureForensicsLive_PprofCurlMissingMarker(t *testing.T) {
	// Pprof endpoint returns 500 — captureForensicsLive should write
	// a .missing marker, not abort.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(500)
	}))
	defer srv.Close()
	listenAddr := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	if err := captureForensicsLive(context.Background(), dir, 0, listenAddr); err != nil {
		t.Fatalf("captureForensicsLive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "heap.pprof.missing")); err != nil {
		t.Errorf("expected heap.pprof.missing on HTTP 500, got %v", err)
	}
}

func TestCaptureForensicsLive_HonoursContextCancel(t *testing.T) {
	// Slow pprof server — never responds — combined with a tight
	// context should produce .missing markers fast, not hang.
	srv := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(30 * time.Second):
		}
	}))
	defer srv.Close()
	listenAddr := strings.TrimPrefix(srv.URL, "http://")

	dir := t.TempDir()
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	started := time.Now()
	_ = captureForensicsLive(ctx, dir, 0, listenAddr)
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("forensics didn't respect ctx, took %s", elapsed)
	}
}

func TestHasBinary(t *testing.T) {
	// /bin/sh exists on every reasonable host. /not/a/real/binary doesn't.
	if !hasBinary("sh") {
		t.Error("hasBinary(sh) should be true")
	}
	if hasBinary("definitely-not-a-binary-anywhere-on-PATH") {
		t.Error("hasBinary returned true for non-existent")
	}
}

// TestCaptureForensicsLive_SocketSnapshot (celeris#588): every dossier
// carries the socket view -- listener queues and every TCP socket with its
// owner -- captured with ss. A fake ss on PATH stands in for iproute2.
func TestCaptureForensicsLive_SocketSnapshot(t *testing.T) {
	bin := t.TempDir()
	script := "#!/bin/sh\necho \"fake-ss $*\"\n"
	if err := os.WriteFile(filepath.Join(bin, "ss"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin+string(os.PathListSeparator)+os.Getenv("PATH"))
	dir := t.TempDir()
	if err := captureForensicsLive(context.Background(), dir, 0, ""); err != nil {
		t.Fatalf("captureForensicsLive: %v", err)
	}
	body, err := os.ReadFile(filepath.Join(dir, "ss.txt"))
	if err != nil {
		t.Fatalf("ss.txt: %v", err)
	}
	for _, want := range []string{"### ss -ltn", "fake-ss -ltn", "### ss -tanpi", "fake-ss -tanpi"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("ss.txt lacks %q:\n%s", want, body)
		}
	}
	status, _ := os.ReadFile(filepath.Join(dir, "forensics_status.txt"))
	if !strings.Contains(string(status), "ss=true") {
		t.Errorf("forensics_status.txt does not record ss: %s", status)
	}
}

// Without ss the dossier says so instead of silently lacking the view.
func TestCaptureForensicsLive_NoSocketToolLeavesAMarker(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	dir := t.TempDir()
	if err := captureForensicsLive(context.Background(), dir, 0, ""); err != nil {
		t.Fatalf("captureForensicsLive: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "ss.txt.missing")); err != nil {
		t.Fatalf("no ss.txt.missing marker: %v", err)
	}
}
