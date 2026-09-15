package validation

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// A driver that cannot be built is the TRANSPORT failing, not the cell:
// with DriverMode=ssh every remaining cell of the matrix would park on a
// host it cannot reach and come back with an all-zero tally, which reads
// like data. The orchestrator records it so the matrix runner can stop
// (probatorium#359).
//
// It has to be recorded rather than returned, because the tier goroutine
// that hits it does not fail the run: it raises a T*-DRIVE incident, which
// the orchestrator classifies as an infra flake and deliberately does not
// halt on, and then parks until the cell's budget expires. Run() returns
// nil. Nothing else in CellResult distinguishes that from a quiet cell.
func TestBuildDriverRecordsTransportLoss(t *testing.T) {
	newOrch := func(t *testing.T, mode string) *Orchestrator {
		t.Helper()
		o, err := New(Config{
			Target:     "localhost",
			Arch:       "amd64",
			Duration:   time.Minute,
			OutDir:     t.TempDir(),
			DriverMode: mode,
		})
		if err != nil {
			t.Fatalf("New: %v", err)
		}
		return o
	}

	// The local driver never fails, and must not set the flag: if it did,
	// every nightly cell would look like a lost transport.
	local := newOrch(t, "local")
	if _, err := local.buildDriver(); err != nil {
		t.Fatalf("local driver: %v", err)
	}
	if got := local.Result().DriverUnavailable; got != "" {
		t.Fatalf("local driver recorded %q", got)
	}

	// ssh with no host: the construction fails.
	o := newOrch(t, "ssh")
	if got := o.Result().DriverUnavailable; got != "" {
		t.Fatalf("fresh orchestrator already reports %q", got)
	}
	if _, err := o.buildDriver(); err == nil {
		t.Fatal("ssh driver with no host built successfully")
	}
	got := o.Result().DriverUnavailable
	if !strings.Contains(got, "ssh driver requires") {
		t.Fatalf("DriverUnavailable = %q, want the driver's own reason", got)
	}
}

// A cell whose refapp never becomes ready writes no tally, and until
// probatorium#359 it wrote no stderr tail either: the clean-path write is
// unreachable from the failure branch. PR-tier run 34920929477 is the
// cost of that — eight cells whose ledger line pointed at a
// refapp_stderr_tail.txt that did not exist, while the cause ("probe:
// dial tcp 127.0.0.1:21211: connect: connection refused") sat in a
// driver error nobody had written down.
func TestRefappThatNeverStartsStillLeavesEvidence(t *testing.T) {
	cfg := Default()
	cfg.Duration = time.Second
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/this/path/does/not/exist/probatorium-test"
	cfg.MarkovPath = "markov/auth_session_ratelimit.yaml"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	violations := make(chan Incident, 1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	o.runTierProperty(ctx, violations)

	b, err := os.ReadFile(filepath.Join(cfg.OutDir, "refapp_stderr_tail.txt"))
	if err != nil {
		t.Fatalf("no evidence written for a cell that never ran: %v", err)
	}
	got := string(b)
	// It must carry something a reader can act on, not just a header.
	var content int
	for _, line := range strings.Split(got, "\n") {
		if l := strings.TrimSpace(line); l != "" && !strings.HasPrefix(l, "#") {
			content++
		}
	}
	if content == 0 {
		t.Fatalf("the file exists and says nothing:\n%s", got)
	}
	// The three facts the summary line cannot supply: what the driver
	// saw, whether the binary was even there, and how the process ended.
	for _, want := range []string{"driver:", "binary:", "IS NOT ON DISK", "process:"} {
		if !strings.Contains(got, want) {
			t.Errorf("start report is missing %q:\n%s", want, got)
		}
	}
}

// The clean path must not grow a diagnostics section it has no business
// carrying: a cell that ran writes its tail and nothing else.
func TestStartReportIsAbsentFromACleanCapture(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "refapp_stderr_tail.txt")
	if err := writeStderrTail(path, []string{"hello from the refapp"}, nil); err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "did not complete") {
		t.Errorf("a clean capture claims the cell failed:\n%s", b)
	}
	if !strings.Contains(string(b), "hello from the refapp") {
		t.Errorf("a clean capture lost the refapp's output:\n%s", b)
	}
}

// A T*-DRIVE incident is an infra flake by design: the orchestrator
// records it and does NOT halt, so the tier parks, Run returns nil, and
// the cell comes back empty with no error attached. The reason has to
// reach the matrix runner some other way, or the ledger falls back to
// "cell produced no requests and no property verdicts" — which is what
// all eight not_run cells of PR-tier run 34920929477 said.
func TestDriveFailureReachesTheCellResult(t *testing.T) {
	cfg := Default()
	cfg.Duration = time.Second
	cfg.OutDir = t.TempDir()
	cfg.CelerisBin = "/this/path/does/not/exist/probatorium-test"
	cfg.MarkovPath = "markov/auth_session_ratelimit.yaml"
	o, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if got := o.Result().DriveFailure; got != "" {
		t.Fatalf("fresh orchestrator already reports %q", got)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err := o.Run(ctx); err != nil {
		// The refapp cannot start, and the run still returns cleanly:
		// that is the behaviour this test exists because of.
		t.Logf("Run returned %v", err)
	}
	got := o.Result().DriveFailure
	if got == "" {
		t.Fatal("the cell recorded no cause; the ledger has nothing to say but 'no requests'")
	}
	if !strings.Contains(got, "start") && !strings.Contains(got, "not ready") {
		t.Errorf("DriveFailure = %q, want the driver's own account", got)
	}
}
