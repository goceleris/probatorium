package validation

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/remote"
)

// buildRaceDemo compiles testdata/racedemo for this host, with or without
// -race, and returns the binary path.
func buildRaceDemo(t *testing.T, race bool) string {
	t.Helper()
	if _, err := exec.LookPath("go"); err != nil {
		t.Skip("go toolchain not on PATH")
	}
	src, err := filepath.Abs("testdata/racedemo")
	if err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(t.TempDir(), "racedemo")
	args := []string{"build"}
	if race {
		args = append(args, "-race")
	}
	args = append(args, "-o", bin, ".")
	build := exec.Command("go", args...)
	build.Dir = src
	build.Env = append(os.Environ(), "GOFLAGS=-mod=mod", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH, "CGO_ENABLED=1")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("build racedemo (race=%v): %v\n%s", race, err, out)
	}
	return bin
}

// superviseRaceDemo launches bin through the same remote.Local +
// superviseStderr path Tier 1 uses and returns the liveness snapshot.
func superviseRaceDemo(t *testing.T, bin string) livenessSnapshot {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	proc, err := remote.NewLocal(bin).Start(ctx, nil)
	if err != nil {
		t.Fatalf("start: %v", err)
	}
	l := &livenessTally{}
	scanned := make(chan struct{})
	go func() {
		defer close(scanned)
		superviseStderr(proc.Stderr(), l, func(string) {}, func(error) {}, func() {})
	}()
	_, _ = proc.Wait(ctx)
	select {
	case <-scanned:
	case <-time.After(10 * time.Second):
		t.Fatal("stderr supervisor never reached EOF")
	}
	return l.snapshot()
}

// A -race build of a racy program reports on stderr and keeps running:
// the scan counts the report and records no crash.
func TestRaceReportIsCountedEndToEnd(t *testing.T) {
	s := superviseRaceDemo(t, buildRaceDemo(t, true))
	if s.RaceReports < 1 {
		t.Fatalf("race_reports = %d, want >= 1 (signature=%q trace=%q)", s.RaceReports, s.Signature, s.Trace)
	}
	if s.Crashed {
		t.Fatalf("a race report is not a crash, but the scan recorded one: %q", s.Signature)
	}
}

// Negative control: the same program without -race has nothing to report.
func TestRaceReportAbsentWithoutTheDetector(t *testing.T) {
	s := superviseRaceDemo(t, buildRaceDemo(t, false))
	if s.RaceReports != 0 {
		t.Fatalf("race_reports = %d without -race; the counter is matching something that is not a report", s.RaceReports)
	}
}
