package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/corpus"
	"github.com/goceleris/probatorium/validation/remote"
)

// Helper binaries are built ONCE per `go test` invocation via
// TestMain so:
//   1. Concurrent subtests don't trigger the fork-exec storm we'd
//      hit if each test re-invoked `go build` ("resource
//      temporarily unavailable" under load).
//   2. The cached paths stay valid for every test in the package
//      (t.Cleanup wouldn't work because it'd nuke the binary
//      after the first test, leaving later ones stuck on missing
//      files).
var (
	cachedPassingReplayPath string
	cachedFailingReplayPath string
)

func TestMain(m *testing.M) {
	if pp, err := compileHelperBin("passing-replay-*.go", `package main
import "fmt"
func main() { fmt.Println("ok"); }
`); err == nil {
		cachedPassingReplayPath = pp
	}
	if pp, err := compileHelperBin("failing-replay-*.go", `package main
import (
    "fmt"
    "os"
)
func main() {
    fmt.Fprintln(os.Stderr, "I-PANIC violated: 1 panic(s) observed")
    os.Exit(1)
}
`); err == nil {
		cachedFailingReplayPath = pp
	}
	code := m.Run()
	if cachedPassingReplayPath != "" {
		_ = os.Remove(cachedPassingReplayPath)
		_ = os.Remove(cachedPassingReplayPath + ".go")
	}
	if cachedFailingReplayPath != "" {
		_ = os.Remove(cachedFailingReplayPath)
		_ = os.Remove(cachedFailingReplayPath + ".go")
	}
	os.Exit(code)
}

func buildPassingReplay(t *testing.T) string {
	t.Helper()
	if cachedPassingReplayPath == "" {
		t.Skip("helper binary not compiled (likely no `go` on PATH)")
	}
	return cachedPassingReplayPath
}

func buildFailingReplay(t *testing.T) string {
	t.Helper()
	if cachedFailingReplayPath == "" {
		t.Skip("helper binary not compiled (likely no `go` on PATH)")
	}
	return cachedFailingReplayPath
}

func compileHelperBin(namePat, src string) (string, error) {
	srcF, err := os.CreateTemp("/tmp", namePat)
	if err != nil {
		return "", err
	}
	if _, err := srcF.WriteString(src); err != nil {
		_ = srcF.Close()
		return "", err
	}
	_ = srcF.Close()
	bin := strings.TrimSuffix(srcF.Name(), ".go")
	cmd := exec.Command("go", "build", "-o", bin, srcF.Name())
	if out, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("go build %s: %w (%s)", srcF.Name(), err, out)
	}
	return bin, nil
}

// fakeRefappCmd returns a /bin/sh script as RefappArgs that prints
// `ready addr=<url>` then serves an HTTP server until killed. The
// HTTP server isn't strictly necessary for Tier 3 (validator-replay
// uses celeris-pid not -url) but keeps the refapp alive for the
// duration of the fork.
func fakeRefappArgs(srv *httptest.Server) []string {
	return []string{
		"-c",
		`echo "ready addr=` + srv.URL + `"; sleep 30`,
	}
}

func TestDriveTier3_NilDriverRejected(t *testing.T) {
	_, err := driveTier3(context.Background(), tier3Config{
		ReplayBin: "/bin/true",
		Seeds:     []corpus.Seed{{Value: 1}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for nil Driver")
	}
}

func TestDriveTier3_EmptyReplayBinRejected(t *testing.T) {
	_, err := driveTier3(context.Background(), tier3Config{
		Driver: remote.NewLocal("/usr/bin/true"),
		Seeds:  []corpus.Seed{{Value: 1}},
	}, nil)
	if err == nil {
		t.Fatal("expected error for empty ReplayBin")
	}
}

func TestDriveTier3_EmptySeedsRejected(t *testing.T) {
	_, err := driveTier3(context.Background(), tier3Config{
		Driver:    remote.NewLocal("/usr/bin/true"),
		ReplayBin: "/usr/bin/true",
	}, nil)
	if err == nil {
		t.Fatal("expected error for empty Seeds")
	}
}

func TestDriveTier3_PassingSeeds(t *testing.T) {
	// Refapp fake: shell script that prints ready then sleeps.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      fakeRefappArgs(srv),
		ReplayBin:       buildPassingReplay(t),
		PerSeedDuration: 2 * time.Second,
		ReadyTimeout:    2 * time.Second,
		Seeds: []corpus.Seed{
			{Value: 0x1, Tag: "smoke-a"},
			{Value: 0x2, Tag: "smoke-b"},
		},
	}

	results := make(chan tier3Result, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	tally, err := driveTier3(ctx, cfg, results)
	if err != nil {
		t.Fatalf("driveTier3: %v", err)
	}
	if tally.SeedsAttempted < 2 {
		t.Errorf("SeedsAttempted: got %d, want >= 2", tally.SeedsAttempted)
	}
	if tally.SeedsFailed > 0 {
		t.Errorf("SeedsFailed: got %d, want 0", tally.SeedsFailed)
	}
	if tally.SeedsPassed < 2 {
		t.Errorf("SeedsPassed: got %d, want >= 2 (loop reuses seeds)", tally.SeedsPassed)
	}
}

func TestDriveTier3_FailingSeedCounted(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      fakeRefappArgs(srv),
		ReplayBin:       buildFailingReplay(t),
		PerSeedDuration: 2 * time.Second,
		ReadyTimeout:    2 * time.Second,
		Seeds: []corpus.Seed{
			{Value: 0xdead, Tag: "canary-fail"},
		},
	}

	results := make(chan tier3Result, 8)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	tally, err := driveTier3(ctx, cfg, results)
	if err != nil {
		t.Fatalf("driveTier3: %v", err)
	}
	if tally.SeedsFailed == 0 {
		t.Errorf("SeedsFailed: got %d, want >= 1", tally.SeedsFailed)
	}
	// Verify at least one result on the channel carries the
	// non-zero exit + the panic marker in stdout.
	select {
	case res := <-results:
		if res.ExitCode == 0 {
			t.Errorf("ExitCode: got 0, want non-zero")
		}
		// stdout from the failing helper is captured into Stdout
		// (CombinedOutput); it should mention "I-PANIC".
		if !strings.Contains(res.Stdout, "I-PANIC") {
			t.Errorf("expected I-PANIC in output, got %q", res.Stdout)
		}
	case <-time.After(time.Second):
		t.Fatal("no result emitted within 1s of run completion")
	}
}

func TestDriveTier3_RefappNeverReadyErrors(t *testing.T) {
	// /bin/sh script that never prints `ready addr=`. waitForReady
	// will time out; replayOneSeed reports ExitCode=-1.
	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      []string{"-c", "sleep 30"},
		ReplayBin:       "/usr/bin/true",
		PerSeedDuration: time.Second,
		ReadyTimeout:    200 * time.Millisecond,
		Seeds:           []corpus.Seed{{Value: 0x1}},
	}
	results := make(chan tier3Result, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tally, _ := driveTier3(ctx, cfg, results)
	if tally.SeedsErrored == 0 {
		t.Errorf("SeedsErrored: got %d, want >= 1", tally.SeedsErrored)
	}
	if tally.SeedsPassed > 0 || tally.SeedsFailed > 0 {
		t.Errorf("infra error should not count as passed/failed: %+v", tally)
	}
}

func TestToJSON_HexSeed(t *testing.T) {
	res := tier3Result{
		Seed:     0xdeadbeef,
		Tag:      "smoke",
		ExitCode: 1,
		Duration: 500 * time.Millisecond,
		Stderr:   "I-PANIC violated",
	}
	d := toJSON(res)
	if d.Seed != "0xdeadbeef" {
		t.Errorf("Seed: got %q, want 0xdeadbeef", d.Seed)
	}
	if d.DurationNS != 500_000_000 {
		t.Errorf("DurationNS: got %d, want 500_000_000", d.DurationNS)
	}
	// Roundtrip through JSON to verify the MarshalJSON path doesn't
	// drop fields.
	buf, err := json.Marshal(&d)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if !strings.Contains(string(buf), `"0xdeadbeef"`) {
		t.Errorf("marshaled body missing seed: %s", buf)
	}
	if !strings.Contains(string(buf), `"smoke"`) {
		t.Errorf("marshaled body missing tag: %s", buf)
	}
}

func TestReplayOneSeed_ShortCircuitsDriverStartError(t *testing.T) {
	cfg := tier3Config{
		Driver:          remote.NewLocal("/nope/missing-binary"),
		RefappArgs:      []string{},
		ReplayBin:       "/usr/bin/true",
		PerSeedDuration: time.Second,
		ReadyTimeout:    200 * time.Millisecond,
	}
	res := replayOneSeed(context.Background(), cfg, corpus.Seed{Value: 0x1})
	if res.ExitCode != -1 {
		t.Errorf("ExitCode: got %d, want -1", res.ExitCode)
	}
	if !strings.Contains(res.Stderr, "start refapp") {
		t.Errorf("Stderr should mention refapp start, got %q", res.Stderr)
	}
}

// Sanity check: confirm the test helpers actually produce
// runnable binaries on the current host arch.
func TestBuildHelperBin_Sanity(t *testing.T) {
	bin := buildPassingReplay(t)
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("helper bin missing: %v", err)
	}
	// Re-run to confirm path resolution
	cmd := exec.Command(bin)
	if err := cmd.Run(); err != nil {
		t.Errorf("helper bin failed: %v", err)
	}
	// Best-effort name check — filepath shouldn't be empty.
	if filepath.Base(bin) == "" {
		t.Errorf("bin path missing basename: %q", bin)
	}
}
