package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/remote"
)

// TestDriveTier1_LivenessSignatureSurvivesExitRace is the deterministic form
// of the race behind probatorium#278's CI failure. driveTier1 runs two death
// watchers: superviseStderr (scans the merged stdout+stderr for a crash line)
// and watchProcessExit (proc.Wait). Wait returns the instant the process is
// reaped; a snapshot taken before the scanner has drained the pipe loses the
// crash line: Crashed=true, ExitCode=2, Signature="".
//
// To make "bytes still in the pipe after Wait returned" deterministic, the
// fake refapp hands its stderr to a background child that prints the fatal
// banner 200 ms AFTER the parent has exited 2. Without joining the scanner
// before snapshotting, the signature is lost every time.
func TestDriveTier1_LivenessSignatureSurvivesExitRace(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	script := `echo "ready addr=` + srv.URL + `"
sleep 0.3
( sleep 0.2
  echo "fatal error: sync: unlock of unlocked mutex" >&2
  echo "" >&2
  echo "goroutine 42 [running]:" >&2
  echo "runtime.throw(...)" >&2 ) &
exit 2`
	cfg := tier1Config{
		Driver:         d,
		RefappArgs:     []string{"-c", script},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    10,
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if !s.Liveness.Crashed || !s.Liveness.Exited || s.Liveness.ExitCode != 2 {
		t.Fatalf("crash not observed as exit 2: %+v", s.Liveness)
	}
	if !strings.Contains(s.Liveness.Signature, "unlock of unlocked mutex") {
		t.Fatalf("crash signature lost to the exit race -- Signature=%q Trace=%q", s.Liveness.Signature, s.Liveness.Trace)
	}
	if !strings.Contains(s.Liveness.Trace, "goroutine 42") {
		t.Errorf("trace after the signature not captured -- Trace=%q", s.Liveness.Trace)
	}
}
