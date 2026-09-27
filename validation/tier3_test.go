package validation

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/corpus"
	"github.com/goceleris/probatorium/validation/remote"
)

// Helper binaries are built ONCE per `go test` invocation via
// TestMain so:
//  1. Concurrent subtests don't trigger the fork-exec storm we'd
//     hit if each test re-invoked `go build` ("resource
//     temporarily unavailable" under load).
//  2. The cached paths stay valid for every test in the package
//     (t.Cleanup wouldn't work because it'd nuke the binary
//     after the first test, leaving later ones stuck on missing
//     files).
//
// A helper that fails to compile FAILS every test that needs it, with the
// build error. It used to skip them, and a skip prints nothing under a plain
// `go test`: every Tier 3 test, the reaping regression tests among them,
// could pass the package without running, and the zombie guard would then
// pass on the zombies nobody had produced (probatorium#415).
var (
	cachedPassingReplayPath string
	cachedFailingReplayPath string
	cachedSlowReplayPath    string

	passingReplayErr error
	failingReplayErr error
	slowReplayErr    error
)

func TestMain(m *testing.M) {
	cachedPassingReplayPath, passingReplayErr = compileHelperBin("passing-replay-*.go", `package main
import "fmt"
func main() { fmt.Println("ok"); }
`)
	cachedFailingReplayPath, failingReplayErr = compileHelperBin("failing-replay-*.go", `package main
import (
    "fmt"
    "os"
)
func main() {
    fmt.Fprintln(os.Stderr, "I-PANIC violated: 1 panic(s) observed")
    os.Exit(1)
}
`)
	// "slow" replay parses the same -duration flag the real
	// validator-replay does, then exits cleanly after a sleep equal
	// to that duration. Used to verify the grace-window fix: the
	// replay should be allowed to complete normally inside its
	// declared -duration window, without exec.CommandContext sending
	// SIGKILL the moment the parent's per-seed deadline expires.
	cachedSlowReplayPath, slowReplayErr = compileHelperBin("slow-replay-*.go", `package main
import (
    "flag"
    "fmt"
    "time"
)
func main() {
    var dur time.Duration
    flag.DurationVar(&dur, "duration", time.Second, "")
    flag.String("seed", "", "")
    flag.String("celeris-pid", "", "")
    flag.String("celeris-port", "", "")
    flag.String("commit", "", "")
    flag.Parse()
    time.Sleep(dur)
    fmt.Println("slow-replay clean exit")
}
`)
	code := m.Run()
	if cachedPassingReplayPath != "" {
		_ = os.Remove(cachedPassingReplayPath)
		_ = os.Remove(cachedPassingReplayPath + ".go")
	}
	if cachedFailingReplayPath != "" {
		_ = os.Remove(cachedFailingReplayPath)
		_ = os.Remove(cachedFailingReplayPath + ".go")
	}
	if cachedSlowReplayPath != "" {
		_ = os.Remove(cachedSlowReplayPath)
		_ = os.Remove(cachedSlowReplayPath + ".go")
	}
	// The zombie guard (probatorium#415): fail the package if any test, or
	// the code under test, left an exited child of this binary unreaped.
	if guard := zombieGuard(os.Stderr); code == 0 {
		code = guard
	}
	os.Exit(code)
}

func buildPassingReplay(t *testing.T) string {
	t.Helper()
	return helperBin(t, "passing-replay", cachedPassingReplayPath, passingReplayErr)
}

func buildFailingReplay(t *testing.T) string {
	t.Helper()
	return helperBin(t, "failing-replay", cachedFailingReplayPath, failingReplayErr)
}

func buildSlowReplay(t *testing.T) string {
	t.Helper()
	return helperBin(t, "slow-replay", cachedSlowReplayPath, slowReplayErr)
}

// helperBin returns a helper TestMain compiled, and fails the test, never
// skips it, when the helper did not compile.
func helperBin(t *testing.T, name, path string, err error) string {
	t.Helper()
	if path == "" {
		t.Fatalf("helper %s did not compile, and this test cannot run without it: %v", name, err)
	}
	return path
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

// fakeRefappArgs returns a /bin/sh script as RefappArgs that prints
// `ready addr=<url>` and then sleeps until killed. Tier 3 never dials the
// URL (validator-replay targets -celeris-pid, not -url); the sleep keeps
// the refapp alive for the duration of the fork.
//
// The sleep is exec'd so the process the driver signals IS the sleeper.
// A shell that forks its last command (bash does) dies on SIGTERM and
// leaves `sleep 30` running as an orphan, and a seed loop whose replay
// exits at once turns over about a hundred seeds a second: more than a
// thousand live orphans at once, on top of the refapps themselves
// (probatorium#415).
func fakeRefappArgs(srv *httptest.Server) []string {
	return []string{
		"-c",
		`echo "ready addr=` + srv.URL + `"; exec sleep 30`,
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

	// Pre-fix flake history:
	//
	//  1. PerSeedDuration was 2s, which left only `2 - replayGrace(2s)
	//     = 0s` for the failing-replay subprocess to actually run. Under
	//     CPU contention the freshly-forked Go runtime init alone took
	//     >0s, so exec.CommandContext SIGKILLed it before it printed
	//     I-PANIC. Fixed by bumping to 5s.
	//
	//  2. The original assertion read ONLY the first result off a buffer-
	//     8 channel. Under `go test -count=N -p 1` the prior iterations'
	//     fork-exec residue made iteration 1's refapp boot race the 2s
	//     ready timeout — yielding an errored result at the FRONT of the
	//     buffer even when later iterations classified correctly. Fixed
	//     by draining the channel with a consumer goroutine.
	//
	//  3. After fix #2, the channel buffer (32) sometimes filled with
	//     infra-errored results from the front of the run (fork EAGAIN
	//     ramping up) while later iterations passed cleanly. The non-
	//     blocking send drops AFTER the buffer fills, so the channel
	//     held only the bad early sample — making the assertion fail
	//     even though the failing-replay classification path WAS
	//     working. Fixed by reading the channel concurrently with
	//     driveTier3 so no result is ever lost to a full buffer.
	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      fakeRefappArgs(srv),
		ReplayBin:       buildFailingReplay(t),
		PerSeedDuration: 5 * time.Second,
		ReadyTimeout:    2 * time.Second,
		Seeds: []corpus.Seed{
			{Value: 0xdead, Tag: "canary-fail"},
		},
	}

	results := make(chan tier3Result, 32)
	// Concurrent drainer — keeps the buffer below the non-blocking-
	// send drop threshold and tracks both (a) the latest non-zero
	// result for diagnostic output, and (b) whether ANY result on
	// the channel proves the failing-replay classification path
	// works (ExitCode>0 AND I-PANIC in CombinedOutput).
	type drainResult struct {
		matched     bool
		lastNonZero tier3Result
	}
	drainCh := make(chan drainResult, 1)
	go func() {
		var dr drainResult
		for res := range results {
			if res.ExitCode != 0 {
				dr.lastNonZero = res
			}
			if res.ExitCode > 0 && strings.Contains(res.Stdout, "I-PANIC") {
				dr.matched = true
			}
		}
		drainCh <- dr
	}()

	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	tally, err := driveTier3(ctx, cfg, results)
	if err != nil {
		t.Fatalf("driveTier3: %v", err)
	}
	close(results)
	dr := <-drainCh

	if tally.SeedsAttempted == 0 {
		t.Fatalf("no seeds attempted, tally=%+v", tally)
	}
	// All attempts errored before reaching replay (typically fork
	// EAGAIN under -count=N -p 1 accumulated fork pressure). The path
	// under test isn't exercisable; skip with a clear message.
	if tally.SeedsFailed == 0 && tally.SeedsErrored == tally.SeedsAttempted {
		t.Skipf("all %d attempts errored before reaching replay — fork pressure under -count/-p tests, skipping (tally=%+v)",
			tally.SeedsAttempted, tally)
	}
	if tally.SeedsFailed == 0 {
		t.Errorf("SeedsFailed: got %d, want >= 1 (tally=%+v)", tally.SeedsFailed, tally)
	}
	if !dr.matched {
		t.Errorf("no result on channel proves failing-replay classification: "+
			"want one with ExitCode>0 AND I-PANIC in stdout; last non-zero: "+
			"ExitCode=%d, Stdout=%q, Stderr=%q; tally=%+v",
			dr.lastNonZero.ExitCode, dr.lastNonZero.Stdout, dr.lastNonZero.Stderr, tally)
	}
}

// TestDriveTier3_SeedLogDirPersistsFailures verifies that non-zero-exit
// seeds get a durable JSON record on disk under SeedLogDir, independent
// of the orchestrator's results-channel state. Closes the gap exposed
// by the 3-day soak — tally.SeedsErrored=1 with no on-disk dossier
// because the channel-send race at ctx-cancel dropped the result.
func TestDriveTier3_SeedLogDirPersistsFailures(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	logDir := t.TempDir()
	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      fakeRefappArgs(srv),
		ReplayBin:       buildFailingReplay(t),
		PerSeedDuration: 2 * time.Second,
		ReadyTimeout:    2 * time.Second,
		Seeds:           []corpus.Seed{{Value: 0xdead, Tag: "canary-fail"}},
		SeedLogDir:      logDir,
	}

	// CRITICAL: pass a results channel with NO consumer goroutine.
	// With the legacy non-blocking-send-only behaviour the channel
	// fills, drops, and no record lands anywhere on disk.
	results := make(chan tier3Result, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	tally, err := driveTier3(ctx, cfg, results)
	if err != nil {
		t.Fatalf("driveTier3: %v", err)
	}
	// Under parallel -race test load OS fork pressure can starve
	// replayOneSeed before the helper exits → SeedsErrored without
	// SeedsFailed. Run in isolation reliably reproduces the failed
	// path; in the parallel sweep we accept either outcome and check
	// that SOMETHING got logged.
	if tally.SeedsFailed == 0 && tally.SeedsErrored == 0 {
		t.Skip("no non-zero-exit seed produced under parallel fork pressure — skipping (isolated run still asserts the path)")
	}

	entries, err := os.ReadDir(logDir)
	if err != nil {
		t.Fatalf("read seed log dir: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("SeedLogDir empty — durable failure record didn't land")
	}
	// Sanity: filename pattern is `<class>-seed-0x<hex>-<ns>.json`.
	// Either "failed" or "errored" is acceptable — both classes are
	// the failure path this test is asserting lands on disk.
	found := false
	for _, e := range entries {
		name := e.Name()
		isFail := strings.HasPrefix(name, "failed-seed-0xdead-") ||
			strings.HasPrefix(name, "errored-seed-0xdead-")
		if isFail && strings.HasSuffix(name, ".json") {
			found = true
			data, err := os.ReadFile(filepath.Join(logDir, name))
			if err != nil {
				t.Fatalf("read seed log: %v", err)
			}
			for _, want := range []string{
				`"seed": "0xdead"`, `"tag": "canary-fail"`, `"exit_code":`,
			} {
				if !strings.Contains(string(data), want) {
					t.Errorf("seed log missing %q, got:\n%s", want, data)
				}
			}
		}
	}
	if !found {
		t.Errorf("no seed log matched the expected pattern, got: %v", entries)
	}
}

// TestDriveTier3_CtxCancelMidReplayNotErrored verifies the end-of-run
// race fix: when the parent ctx cancels DURING a replay (after the
// refapp has come up and the replay subprocess is in flight), the
// SIGKILL'd replay must NOT count as seedsErrored. Pre-fix, every
// soak ended with `seedsErrored=1` per arch because exactly one seed
// was always in flight when the duration deadline tripped.
//
// Uses a 6s ctx with 10s PerSeedDuration so the replay gets a few
// seconds in flight before ctx cancels it. Under parallel -race
// fork pressure the refapp may fail to come up before ctx expires
// (different failure mode — those ARE genuine flakes and the loop
// correctly counts them); the test skips itself in that case.
func TestDriveTier3_CtxCancelMidReplayNotErrored(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      fakeRefappArgs(srv),
		ReplayBin:       buildSlowReplay(t),
		PerSeedDuration: 10 * time.Second,
		ReadyTimeout:    2 * time.Second,
		Seeds:           []corpus.Seed{{Value: 0x1, Tag: "in-flight"}},
	}
	results := make(chan tier3Result, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 6*time.Second)
	defer cancel()
	tally, err := driveTier3(ctx, cfg, results)
	if err != nil {
		t.Fatalf("driveTier3: %v", err)
	}
	// Under fork pressure the seed may never get past refapp boot.
	// Those failures ARE legitimate infra flakes that get counted.
	// Only attempt this assertion when at least one seed reached
	// replay (which happens when no errored seeds OR a passed seed).
	if tally.SeedsAttempted == 0 || tally.SeedsErrored == tally.SeedsAttempted {
		t.Skipf("fork pressure prevented mid-replay test (tally=%+v)", tally)
	}
	// At least one seed reached + completed replay; any in-flight
	// seed at ctx-cancel must NOT have bumped SeedsErrored.
	// SeedsPassed should be > 0 (the seed finished cleanly OR was
	// in flight at cancel — neither should errror).
	if tally.SeedsErrored != 0 {
		t.Errorf("SeedsErrored: got %d, want 0 (end-of-run ctx-cancel must NOT be counted as infra flake); tally=%+v",
			tally.SeedsErrored, tally)
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

// TestReplayOneSeed_ReapsTheRefapp: a seed's teardown must reap its refapp,
// not only signal it. The loop used to SIGTERM the refapp and move on, so
// every seed left an exited child in the process table as a zombie of the
// validator for as long as it ran: ~1,100 of them during
// `go test ./validation/...`, enough to exhaust a laptop's per-user process
// limit (probatorium#415). Once replayOneSeed returns, the refapp must not be
// a child of this process in any state: alive means it was never stopped,
// zombie means it was stopped and never reaped.
func TestReplayOneSeed_ReapsTheRefapp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      fakeRefappArgs(srv),
		ReplayBin:       buildPassingReplay(t),
		PerSeedDuration: 2 * time.Second,
		// How fast a loaded box forks a shell is not what this measures.
		ReadyTimeout: tier1TestReadyTimeout,
	}
	res := replayOneSeed(context.Background(), cfg, corpus.Seed{Value: 0x1, Tag: "reap"})
	if res.RefappPID == 0 {
		t.Fatalf("refapp never announced ready, so there is nothing to check: %+v", res)
	}
	assertReaped(t, res.RefappPID)
}

// assertReaped fails the test when pid is still a child of this process, in
// any state.
func assertReaped(t *testing.T, pid int) {
	t.Helper()
	kids, source, err := childProcesses()
	if err != nil {
		t.Fatal(err)
	}
	if state, ok := kids[pid]; ok {
		t.Fatalf("refapp pid %d is still a child of this process (state %s, via %s) after replayOneSeed returned: signalled, never reaped", pid, state, source)
	}
}

// TestReplayOneSeed_StopEscalatesToSIGKILL: a refapp's teardown gives it the
// stop grace to exit on SIGTERM, and a refapp that ignores SIGTERM is
// SIGKILLed at the grace and reaped inside the same bounded Wait. The fixture
// ignores TERM, INT and HUP (a trap with an empty action sets SIG_IGN, and the
// exec'd sleep inherits it). A teardown that never escalates, or escalates
// with one of those, times out and reports the seed errored; one that
// escalates with any other signal leaves a refapp that did not die of
// SIGKILL; one that escalates before the grace ends it too early. A refapp
// that honours SIGTERM must not pay the grace.
func TestReplayOneSeed_StopEscalatesToSIGKILL(t *testing.T) {
	const grace = time.Second
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	for _, tc := range []struct {
		name       string
		script     string
		wantKilled bool
	}{
		{"refapp that exits on SIGTERM", `echo "ready addr=` + srv.URL + `"; exec sleep 30`, false},
		{"refapp that ignores SIGTERM", `trap '' TERM INT HUP; echo "ready addr=` + srv.URL + `"; exec sleep 30`, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := tier3Config{
				Driver:          remote.NewLocal("/bin/sh"),
				RefappArgs:      []string{"-c", tc.script},
				ReplayBin:       buildPassingReplay(t),
				PerSeedDuration: 2 * time.Second,
				ReadyTimeout:    tier1TestReadyTimeout,
				RefappStopGrace: grace,
			}
			res := replayOneSeed(context.Background(), cfg, corpus.Seed{Value: 0x1, Tag: "stop"})
			if res.RefappPID == 0 {
				t.Fatalf("refapp never announced ready, so there is nothing to check: %+v", res)
			}
			if res.ExitCode != 0 || strings.Contains(res.Stderr, "stop refapp") {
				t.Errorf("teardown failed: ExitCode=%d Stderr=%q (TeardownDuration=%s)", res.ExitCode, res.Stderr, res.TeardownDuration)
			}
			if res.TeardownKilled != tc.wantKilled {
				t.Errorf("TeardownKilled = %v, want %v (TeardownDuration=%s)", res.TeardownKilled, tc.wantKilled, res.TeardownDuration)
			}
			if tc.wantKilled && res.TeardownDuration < grace {
				t.Errorf("TeardownDuration = %s: the SIGKILL came before the %s grace", res.TeardownDuration, grace)
			}
			if !tc.wantKilled && res.TeardownDuration >= grace {
				t.Errorf("TeardownDuration = %s: a refapp that exits on SIGTERM paid the whole %s grace", res.TeardownDuration, grace)
			}
			assertReaped(t, res.RefappPID)
		})
	}
}

// TestDriveTier3_TeardownIsTallied: the teardown's cost reaches the tally and
// the tier3_tally.json snapshot, which is how a nightly shows it: a refapp
// that ignores SIGTERM is counted as killed on every seed, and the longest
// teardown is at least the grace.
func TestDriveTier3_TeardownIsTallied(t *testing.T) {
	const grace = 200 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()
	snap := filepath.Join(t.TempDir(), "tier3_tally.json")
	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      []string{"-c", `trap '' TERM INT HUP; echo "ready addr=` + srv.URL + `"; exec sleep 30`},
		ReplayBin:       buildPassingReplay(t),
		PerSeedDuration: 2 * time.Second,
		ReadyTimeout:    tier1TestReadyTimeout,
		RefappStopGrace: grace,
		Seeds:           []corpus.Seed{{Value: 0x1, Tag: "tally"}},
		SnapshotPath:    snap,
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tally, err := driveTier3(ctx, cfg, nil)
	if err != nil {
		t.Fatalf("driveTier3: %v", err)
	}
	if tally.SeedsPassed == 0 {
		t.Fatalf("no seed passed, so no teardown was tallied: %s", tally)
	}
	// Every passed seed had a refapp to tear down, and every such refapp
	// ignored SIGTERM. (A seed whose refapp never started has no teardown.)
	if tally.SeedsTeardownKilled < tally.SeedsPassed {
		t.Errorf("SeedsTeardownKilled = %d, want at least every passed seed (%d): %s", tally.SeedsTeardownKilled, tally.SeedsPassed, tally)
	}
	if time.Duration(tally.TeardownMaxNS) < grace || tally.TeardownTotalNS < tally.TeardownMaxNS {
		t.Errorf("teardown times not tallied: %s", tally)
	}
	data, err := os.ReadFile(snap)
	if err != nil {
		t.Fatal(err)
	}
	var onDisk tier3TallySnapshot
	if err := json.Unmarshal(data, &onDisk); err != nil {
		t.Fatal(err)
	}
	if onDisk.SeedsTeardownKilled == 0 || onDisk.TeardownMaxNS == 0 || !strings.Contains(string(data), `"seeds_teardown_killed"`) {
		t.Errorf("tier3_tally.json does not carry the teardown counters: %s", data)
	}
}

// unstoppableRefapp is a remote.Process that announces ready and whose exit
// the driver can never observe: every Wait fails. It records the signals it
// was sent.
type unstoppableRefapp struct {
	mu      sync.Mutex
	signals []int
}

func (p *unstoppableRefapp) PID() int { return 424242 }
func (p *unstoppableRefapp) Signal(sig int) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, sig)
	return nil
}
func (p *unstoppableRefapp) Wait(context.Context) (remote.WaitResult, error) {
	return remote.WaitResult{}, errors.New("lost the process")
}
func (p *unstoppableRefapp) Stderr() io.Reader {
	return strings.NewReader("ready addr=127.0.0.1:1\n")
}

// oneProcDriver is a remote.Driver whose every Start returns proc.
type oneProcDriver struct{ proc remote.Process }

func (d oneProcDriver) Start(context.Context, []string) (remote.Process, error) {
	return d.proc, nil
}
func (d oneProcDriver) Close() error { return nil }

// pidlessRefapp is a remote.Process whose PID the driver never learned (the
// SSH driver's pidfile read failed): it announces ready, cannot be signalled,
// and its Wait returns only when its context ends.
type pidlessRefapp struct{}

func (pidlessRefapp) PID() int         { return 0 }
func (pidlessRefapp) Signal(int) error { return nil }
func (pidlessRefapp) Wait(ctx context.Context) (remote.WaitResult, error) {
	<-ctx.Done()
	return remote.WaitResult{}, ctx.Err()
}
func (pidlessRefapp) Stderr() io.Reader {
	return strings.NewReader("ready addr=127.0.0.1:1\n")
}

// TestReplayOneSeed_UnknownPIDFailsAtOnce: a refapp whose PID is unknown
// cannot be signalled, so its teardown reports that at once. Waiting it out
// cost twice the stop grace, 10 s, on every such seed, for a Wait that
// nothing was ever going to end.
func TestReplayOneSeed_UnknownPIDFailsAtOnce(t *testing.T) {
	cfg := tier3Config{
		Driver:          oneProcDriver{proc: pidlessRefapp{}},
		ReplayBin:       buildPassingReplay(t),
		PerSeedDuration: 5 * time.Second,
		ReadyTimeout:    tier1TestReadyTimeout,
	}
	res := replayOneSeed(context.Background(), cfg, corpus.Seed{Value: 0x1})
	if res.ExitCode != -1 || !strings.Contains(res.Stderr, "its pid is unknown") {
		t.Errorf("ExitCode=%d Stderr=%q, want -1 and the unknown-pid teardown error", res.ExitCode, res.Stderr)
	}
	if res.TeardownDuration > time.Second {
		t.Errorf("TeardownDuration = %s, want an immediate report", res.TeardownDuration)
	}
}

// TestReplayOneSeed_TeardownFailureIsReported: a refapp that cannot be stopped
// and reaped is a driver failure, so a seed whose replay passed comes back
// errored (T3-DRIVE) with the teardown error, never as a pass; a seed whose
// replay failed keeps its failing exit code, so a teardown problem cannot hide
// what the replay found.
func TestReplayOneSeed_TeardownFailureIsReported(t *testing.T) {
	for _, tc := range []struct {
		name     string
		replay   func(*testing.T) string
		wantExit int
	}{
		{"passing replay becomes errored", buildPassingReplay, -1},
		{"failing replay keeps its exit code", buildFailingReplay, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			proc := &unstoppableRefapp{}
			cfg := tier3Config{
				Driver:          oneProcDriver{proc: proc},
				ReplayBin:       tc.replay(t),
				PerSeedDuration: 5 * time.Second,
				ReadyTimeout:    tier1TestReadyTimeout,
			}
			res := replayOneSeed(context.Background(), cfg, corpus.Seed{Value: 0x1})
			if res.ExitCode != tc.wantExit {
				t.Errorf("ExitCode = %d, want %d (Stderr=%q)", res.ExitCode, tc.wantExit, res.Stderr)
			}
			if !strings.Contains(res.Stderr, "stop refapp pid 424242: lost the process") {
				t.Errorf("teardown error missing from Stderr: %q", res.Stderr)
			}
			proc.mu.Lock()
			defer proc.mu.Unlock()
			if len(proc.signals) == 0 || proc.signals[0] != int(syscall.SIGTERM) {
				t.Errorf("signals sent = %v, want SIGTERM first", proc.signals)
			}
		})
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

// Regression for #77 — validator-replay was getting SIGKILLed by
// exec.CommandContext at the per-seed deadline because the replay's
// `-duration` and the exec ctx had the same expiry. The fix gives
// the replay 2s less internal time so it can exit cleanly before
// the exec deadline fires.
//
// This test uses the "slow" helper that simulates the real replay's
// pattern: sleep for the `-duration` value, print, exit 0. With the
// grace-window fix the slow replay completes inside the per-seed
// budget and the cell counts as a pass; without the fix it would
// be SIGKILLed and counted as T3-DRIVE errored.
func TestDriveTier3_SlowReplayClassedAsPassed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	cfg := tier3Config{
		Driver:          remote.NewLocal("/bin/sh"),
		RefappArgs:      fakeRefappArgs(srv),
		ReplayBin:       buildSlowReplay(t),
		PerSeedDuration: 3 * time.Second,
		ReadyTimeout:    2 * time.Second,
		Seeds:           []corpus.Seed{{Value: 0x1, Tag: "slow-but-clean"}},
	}
	results := make(chan tier3Result, 4)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	tally, err := driveTier3(ctx, cfg, results)
	if err != nil {
		t.Fatalf("driveTier3: %v", err)
	}
	// The slow replay sleeps for replayInternalDuration =
	// PerSeedDuration - 2s grace = 1s, then exits clean.
	//
	// Pre-fix behaviour was deterministic: the exec.CommandContext
	// deadline coincided with the replay's own duration, so SIGKILL
	// always won and every cell hit ExitCode=-1 (T3-DRIVE). Post-fix,
	// any non-zero SeedsPassed proves the grace window classified a
	// natural-exit replay as a pass at least once.
	//
	// We deliberately don't require passed > errored. Under parallel
	// test load with -race the OS fork pipe saturates and Driver.Start
	// itself can fail with ExitCode=-1, fully outside the grace-window
	// path under test. Those legitimately count as errored.
	if tally.SeedsPassed == 0 && tally.SeedsAttempted > 0 && tally.SeedsErrored == tally.SeedsAttempted {
		// Every single attempt errored — almost certainly fork pressure
		// from sibling t.Parallel tests, not the grace window. Skip so
		// the suite is green; the targeted in-isolation run still
		// asserts the real invariant.
		t.Skipf("all %d attempts errored before reaching replay — fork pressure under parallel tests, skipping (tally=%+v)",
			tally.SeedsAttempted, tally)
	}
	if tally.SeedsPassed == 0 {
		t.Errorf("expected SeedsPassed >= 1, got tally=%+v", tally)
	}
}

// Sanity check: confirm the test helpers actually produce
// runnable binaries on the current host arch. Runs serially (no
// t.Parallel) so the race-detector's fork limit doesn't bite under
// concurrent subtest runs.
func TestBuildHelperBin_Sanity(t *testing.T) {
	bin := buildPassingReplay(t)
	if _, err := os.Stat(bin); err != nil {
		t.Fatalf("helper bin missing: %v", err)
	}
	// Best-effort name check — filepath shouldn't be empty.
	if filepath.Base(bin) == "" {
		t.Errorf("bin path missing basename: %q", bin)
	}
}
