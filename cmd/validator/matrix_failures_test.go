package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

// The matrix is the release gate and it costs ~70 minutes of exclusive
// cluster time. probatorium#359: a run that stops early, or that stops
// late but says only "the first thing that went wrong", spends the whole
// window to report on part of the matrix.
//
// These tests drive [matrixRunner] with a stub cell runner, so the loop's
// policy -- what is recorded, what stops the run -- is judged without a
// cluster, a refapp binary or an orchestrator.

// cellWithWork is a cell that RAN: it sent traffic and its property loop
// reached verdicts. A failure on top of that is the oracle firing.
func cellWithWork(mc matrixCell) report.ValidationCellResult {
	return report.ValidationCellResult{
		Refapp: mc.Refapp,
		Engine: mc.Engine,
		Arch:   "amd64",
		Tier1: &report.Tier1Summary{
			RequestsSent:        120_000,
			PropertyEvaluations: 47,
		},
		PropertiesPassed: 3,
	}
}

// cellWithNoWork is what the orchestrator hands back when the refapp never
// came up: a named cell with an all-zero tally. The artifact convention is
// that evaluations == 0 means the cell did not run and
// refapp_stderr_tail.txt holds the reason.
func cellWithNoWork(mc matrixCell) report.ValidationCellResult {
	return report.ValidationCellResult{
		Refapp: mc.Refapp,
		Engine: mc.Engine,
		Arch:   "amd64",
		Tier1:  &report.Tier1Summary{},
	}
}

// The two fixtures must not agree, or every classification assertion below
// passes against code that cannot tell them apart.
func TestCellFixturesDiffer(t *testing.T) {
	mc := matrixCell{Refapp: "r", Engine: "e"}
	ran, dead := cellWithWork(mc), cellWithNoWork(mc)
	if ran.Tier1.RequestsSent == dead.Tier1.RequestsSent {
		t.Fatalf("fixtures coincide: both carry requests_sent=%d", ran.Tier1.RequestsSent)
	}
	if !matrixCellIsFinal(ran) {
		t.Fatal("cellWithWork must read as a cell that ran")
	}
	if matrixCellIsFinal(dead) {
		t.Fatal("cellWithNoWork must read as a cell that did not run")
	}
}

type stubCall struct {
	cell matrixCell
	idx  int
}

// newStubRunner returns a runner whose cells are driven by fn, plus the
// call log and the buffer that collects everything the run printed.
func newStubRunner(t *testing.T, plan []matrixCell,
	fn func(mc matrixCell, idx int) (report.ValidationCellResult, error),
) (*matrixRunner, *[]stubCall, *bytes.Buffer) {
	t.Helper()
	// Never let a real drain sentinel on the dev machine stop a unit test.
	t.Setenv("PROBATORIUM_DRAIN_FILE", filepath.Join(t.TempDir(), "no-drain"))
	var calls []stubCall
	var out bytes.Buffer
	r := &matrixRunner{
		cfg:       Config{OutDir: t.TempDir(), Target: "localhost", Arch: "amd64"},
		plan:      plan,
		startedAt: time.Now().UTC(),
		out:       &out,
		runCell: func(_ context.Context, mc matrixCell, idx int) (report.ValidationCellResult, error) {
			calls = append(calls, stubCall{cell: mc, idx: idx})
			return fn(mc, idx)
		},
	}
	return r, &calls, &out
}

func planOf(refapps ...string) []matrixCell {
	var plan []matrixCell
	for _, r := range refapps {
		for _, e := range []string{"iouring", "epoll", "std", "adaptive"} {
			plan = append(plan, matrixCell{Refapp: r, Engine: e})
		}
	}
	return plan
}

// readCells decodes the document the run persisted.
func readCells(t *testing.T, dir string) []map[string]any {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "validate-results.json"))
	if err != nil {
		t.Fatalf("read results: %v", err)
	}
	var doc struct {
		Validation struct {
			Cells []map[string]any `json:"cells"`
		} `json:"validation_results"`
	}
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse results: %v", err)
	}
	return doc.Validation.Cells
}

// A failing cell must not cost the cells after it, the run must still exit
// non-zero, and the end-of-run summary must name EVERY failure -- not the
// first one. Nightly 34818888908 recorded eight failures and its exit error
// mentioned one.
func TestMatrixRunReportsEveryFailedCellAndStillFails(t *testing.T) {
	plan := planOf("auth_jwt_csrf", "auth_session_ratelimit")
	// Cell 3 ran and its oracle fired; cell 7's refapp never started.
	r, calls, out := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		switch idx {
		case 3:
			return cellWithWork(mc), fmt.Errorf("cell run: validation: I-PANIC violated by tier-1-property: handler panic")
		case 7:
			return cellWithNoWork(mc), fmt.Errorf("cell run: validation: I-LIVENESS violated by tier-1-property: refapp process died mid-run: process exited unexpectedly (code=1)")
		}
		return cellWithWork(mc), nil
	})
	err := r.run(context.Background())

	if len(*calls) != len(plan) {
		t.Errorf("ran %d of %d cells: a failing cell stopped the ones after it", len(*calls), len(plan))
	}
	if err == nil {
		t.Fatal("run returned nil: the matrix is a gate, a failed cell must exit non-zero")
	}
	summary := out.String()
	for _, want := range []string{
		"auth_jwt_csrf/adaptive",          // idx 3, failed
		"auth_session_ratelimit/adaptive", // idx 7, never ran
		"I-PANIC",
		"I-LIVENESS",
	} {
		if !strings.Contains(summary, want) {
			t.Errorf("run output does not name %q:\n%s", want, summary)
		}
	}
	// The summary is also left beside the results, because the run log of a
	// 70-minute cluster tier is not where anyone finds anything.
	b, ferr := os.ReadFile(filepath.Join(r.cfg.OutDir, "cell-failures.txt"))
	if ferr != nil {
		t.Fatalf("cell-failures.txt: %v", ferr)
	}
	for _, want := range []string{"auth_jwt_csrf/adaptive", "auth_session_ratelimit/adaptive"} {
		if !strings.Contains(string(b), want) {
			t.Errorf("cell-failures.txt does not name %q:\n%s", want, b)
		}
	}
}

// A cell whose refapp would not start is a different fact from a cell whose
// oracle fired: the first says nothing about celeris' behaviour under load,
// the second is the finding. The document has to carry the difference --
// nothing else in the artifact does.
func TestMatrixRunSeparatesCellsThatFailedFromCellsThatNeverRan(t *testing.T) {
	plan := planOf("kitchen_sink")
	r, _, _ := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		switch idx {
		case 1:
			return cellWithWork(mc), fmt.Errorf("cell run: validation: I-MW-2 violated by tier-1-property: middleware order")
		case 2:
			return cellWithNoWork(mc), fmt.Errorf("cell run: validation: I-LIVENESS violated by tier-1-property: refapp process died mid-run")
		}
		return cellWithWork(mc), nil
	})
	_ = r.run(context.Background())

	cells := readCells(t, r.cfg.OutDir)
	if len(cells) != len(plan) {
		t.Fatalf("document has %d cells, want %d", len(cells), len(plan))
	}
	want := []string{"ok", "failed", "not_run", "ok"}
	for i, w := range want {
		got, _ := cells[i]["status"].(string)
		if got != w {
			t.Errorf("cell %d (%s/%s): status=%q, want %q", i, cells[i]["refapp"], cells[i]["engine"], got, w)
		}
	}
	if reason, _ := cells[2]["failure_reason"].(string); !strings.Contains(reason, "I-LIVENESS") {
		t.Errorf("cell 2 failure_reason = %q, want the refapp's cause", reason)
	}
	if reason, _ := cells[0]["failure_reason"].(string); reason != "" {
		t.Errorf("clean cell carries failure_reason %q", reason)
	}
}

// Out of disk is not a cell-level fact: the next cell cannot record its own
// evidence either, so continuing spends the window producing nothing.
func TestMatrixRunAbortsWhenTheDiskIsFull(t *testing.T) {
	plan := planOf("a", "b", "c")
	r, calls, out := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		if idx == 2 {
			return cellWithNoWork(mc), fmt.Errorf("mkdir cell out: %w", syscall.ENOSPC)
		}
		return cellWithWork(mc), nil
	})
	err := r.run(context.Background())
	if err == nil {
		t.Fatal("run returned nil after a fatal condition")
	}
	if len(*calls) != 3 {
		t.Errorf("ran %d cells, want 3: a full disk must stop the run at the cell that found it", len(*calls))
	}
	if !strings.Contains(out.String(), "fatal") {
		t.Errorf("run output does not call the abort fatal:\n%s", out.String())
	}
}

// If every cell is failing the same way, the run has already answered the
// question; the rest of the window is spent re-proving it. Eight in a row is
// two whole refapps with no survivor -- past any engine- or refapp-scoped
// defect.
func TestMatrixRunAbortsAfterEightConsecutiveFailures(t *testing.T) {
	plan := planOf("a", "b", "c", "d", "e", "f") // 24 cells
	r, calls, out := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		if idx == 0 {
			return cellWithWork(mc), nil
		}
		return cellWithNoWork(mc), fmt.Errorf("cell run: refapp process died mid-run")
	})
	err := r.run(context.Background())
	if err == nil {
		t.Fatal("run returned nil with 23 failing cells")
	}
	if len(*calls) != 9 { // 1 clean + 8 consecutive failures
		t.Errorf("ran %d cells, want 9 (1 clean + the 8-failure cap)", len(*calls))
	}
	if !strings.Contains(out.String(), "consecutive") {
		t.Errorf("run output does not say why it stopped:\n%s", out.String())
	}
}

// A matrix whose failures are spread out rather than consecutive still
// stops once half of it has failed: the verdict cannot change any more.
//
// The pattern here fails two cells in every three, so the consecutive cap
// (8) never fires and only the total cap (half of 24 = 12) can stop the
// run. Its twelfth failure is cell 17, six cells short of the plan -- if
// the cap did not bite, the run would reach cell 23.
func TestMatrixRunAbortsOnceHalfTheMatrixHasFailed(t *testing.T) {
	plan := planOf("a", "b", "c", "d", "e", "f") // 24 cells, cap = 12
	r, calls, out := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		if idx%3 == 0 {
			return cellWithWork(mc), nil
		}
		return cellWithNoWork(mc), fmt.Errorf("cell run: refapp process died mid-run")
	})
	if err := r.run(context.Background()); err == nil {
		t.Fatal("run returned nil with half the matrix failing")
	}
	if len(*calls) != 18 {
		t.Errorf("ran %d cells, want 18 (the twelfth failure is cell 17)", len(*calls))
	}
	if !strings.Contains(out.String(), "12 of 24") {
		t.Errorf("run output does not name the abort reason:\n%s", out.String())
	}
}

// Losing the transport to the host under test is not a cell-level fact:
// the validator cannot reach the machine any more, so every remaining cell
// would park and come back empty. Thirty empty cells are worse than an
// abort -- they look like data.
func TestMatrixRunAbortsWhenTheTransportToTheHostIsLost(t *testing.T) {
	plan := planOf("a", "b", "c") // 12 cells
	r, calls, out := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		if idx == 1 {
			return cellWithNoWork(mc), fatalDriverError(
				"ssh: handshake failed: read tcp 10.0.0.2:41234->10.0.0.1:22: connection reset by peer")
		}
		return cellWithWork(mc), nil
	})
	err := r.run(context.Background())
	if err == nil {
		t.Fatal("run returned nil after the transport was lost")
	}
	if len(*calls) != 2 {
		t.Errorf("ran %d cells, want 2: a lost transport must stop the run at the cell that found it", len(*calls))
	}
	if !strings.Contains(err.Error(), "remote driver unavailable") {
		t.Errorf("exit error %q does not name the fatal condition", err)
	}
	summary := out.String()
	// The cells the abort never reached are a third fact, distinct from
	// "failed" and from "never ran": no verdict was formed on them at all.
	if !strings.Contains(summary, "never attempted (10)") {
		t.Errorf("summary does not account for the unreached cells:\n%s", summary)
	}
	if !strings.Contains(summary, "-matrix-resume-from=") {
		t.Errorf("summary does not say how to finish the run:\n%s", summary)
	}
}

// A refapp that will not start is the opposite call: it is exactly the
// finding the other cells are there to put in context (celeris#614 hit
// eight of thirty-two cells and the other twenty-four were clean), so it
// must never be fatal.
func TestRefappFailuresAreNotFatal(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("cell run: validation: I-LIVENESS violated by tier-1-property: refapp process died mid-run: process exited unexpectedly (code=1)"),
		fmt.Errorf("cell run: validation: I-PANIC violated by tier-1-property: handler panic"),
		fmt.Errorf("matrix: build kitchen_sink: exit status 1"),
		fmt.Errorf("orchestrator: validation: load markov /tmp/x.yaml: no such file"),
		context.DeadlineExceeded,
	} {
		if why, fatal := fatalCause(err); fatal {
			t.Errorf("fatalCause(%v) = %q, true; a cell-level failure must not stop the run", err, why)
		}
	}
	// ... and the fatal set is not empty, or the test above proves nothing.
	for _, err := range []error{
		fatalDriverError("ssh: handshake failed"),
		fatalStagingError("/tmp/celeris-bench/refapps", os.ErrNotExist),
		fmt.Errorf("persist: %w", syscall.ENOSPC),
	} {
		if _, fatal := fatalCause(err); !fatal {
			t.Errorf("fatalCause(%v) = false; the run cannot continue past this", err)
		}
	}
}

// The caps have to fire on the plans this project actually runs, and not
// fire on the correlated failure it actually has (driver_* going dark
// together is 12 of 32 cells).
func TestFailureCapDefaults(t *testing.T) {
	consecutive, total := resolveFailureCaps("", "", 32)
	if consecutive != 8 {
		t.Errorf("consecutive cap = %d, want 8", consecutive)
	}
	if total != 16 {
		t.Errorf("total cap = %d, want 16 (half of 32)", total)
	}
	if total <= 12 {
		t.Errorf("total cap %d would abort a run that lost only the driver_* refapps (12 of 32 cells)", total)
	}
	// A small plan is worth finishing whole: there is no window to protect.
	if _, small := resolveFailureCaps("", "", 4); small < 4 {
		t.Errorf("total cap for a 4-cell plan = %d, want >= 4", small)
	}
	// Overrides, including the deliberate "run it all anyway".
	if c, tt := resolveFailureCaps("3", "5", 32); c != 3 || tt != 5 {
		t.Errorf("overrides ignored: got %d/%d, want 3/5", c, tt)
	}
	if c, tt := resolveFailureCaps("-1", "-1", 32); c != -1 || tt != -1 {
		t.Errorf("cap disable ignored: got %d/%d", c, tt)
	}
}

// Losing the staging tree is the cluster going out from under the run: no
// later cell has a subject, so there is nothing left to measure.
func TestMatrixRunAbortsWhenTheStagingTreeDisappears(t *testing.T) {
	staging := t.TempDir()
	plan := planOf("a", "b", "c") // 12 cells
	r, calls, out := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		if idx == 0 {
			// The cluster teardown, a wiped /tmp, a remount.
			if err := os.RemoveAll(staging); err != nil {
				t.Fatalf("remove staging: %v", err)
			}
		}
		return cellWithWork(mc), nil
	})
	r.stagingDir = staging
	err := r.run(context.Background())
	if err == nil {
		t.Fatal("run returned nil after the staging tree vanished")
	}
	if len(*calls) != 1 {
		t.Errorf("ran %d cells, want 1: the check is before the cell, so no refapp is blamed for it", len(*calls))
	}
	if !strings.Contains(err.Error(), "staging dir") {
		t.Errorf("exit error %q does not name the fatal condition", err)
	}
	if !strings.Contains(out.String(), "never attempted (11)") {
		t.Errorf("summary does not account for the unreached cells:\n%s", out.String())
	}
}

// ... and a staging dir that was never readable is NOT armed: pointing a
// dev run at a stale -matrix-bin-dir has always fallen back to building
// each refapp from source, and must keep doing so.
func TestStagingWatchIsNotArmedForADirThatWasNeverThere(t *testing.T) {
	if got := watchableStagingDir(filepath.Join(t.TempDir(), "nope")); got != "" {
		t.Errorf("watchableStagingDir armed on a missing dir: %q", got)
	}
	if got := watchableStagingDir(""); got != "" {
		t.Errorf("watchableStagingDir armed on an empty dir: %q", got)
	}
	real := t.TempDir()
	if got := watchableStagingDir(real); got != real {
		t.Errorf("watchableStagingDir(%q) = %q, want it armed", real, got)
	}
}

// The ledger points a reader at a failed cell's evidence. PR-tier run
// 34920929477 is why this test exists: every not_run cell's line ended
// "evidence: cell-NN-.../refapp_stderr_tail.txt" and not one of those
// files was there. A pointer to nothing is worse than no pointer — it is
// the same shape as an oracle that counts an error and discards it, and
// it lands on exactly the cells whose reason the reader cannot get any
// other way.
func TestLedgerNeverNamesEvidenceThatIsNotThere(t *testing.T) {
	plan := planOf("driver_memcached")
	r, _, out := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		return cellWithNoWork(mc), fmt.Errorf("cell run: validation: I-LIVENESS violated by tier-1-property: refapp never became ready")
	})
	// The cell directories exist (the orchestrator creates them) and hold
	// nothing that names a cause: this is the state the run above left on
	// disk, reproduced.
	for i, mc := range plan {
		if err := os.MkdirAll(filepath.Join(r.cfg.OutDir, cellOutDirName(i, mc)), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.run(context.Background())

	summary := out.String()
	if strings.Contains(summary, "refapp_stderr_tail.txt") {
		t.Errorf("the ledger names a stderr tail that does not exist:\n%s", summary)
	}
	if !strings.Contains(summary, "no refapp output was captured") {
		t.Errorf("the ledger does not say the evidence is missing; it just goes quiet:\n%s", summary)
	}
}

// ... and when the capture DID happen, the pointer must still be there:
// a rule that never names a file is as useless as one that always does.
func TestLedgerNamesEvidenceThatExists(t *testing.T) {
	plan := planOf("driver_memcached")
	r, _, out := newStubRunner(t, plan, func(mc matrixCell, idx int) (report.ValidationCellResult, error) {
		return cellWithNoWork(mc), fmt.Errorf("cell run: validation: I-LIVENESS violated by tier-1-property: refapp process died mid-run")
	})
	for i, mc := range plan {
		dir := filepath.Join(r.cfg.OutDir, cellOutDirName(i, mc))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		// A real capture: the header the writer always emits, plus the
		// refapp's own last words.
		body := "# refapp stdout+stderr tail: 1 line(s), last 80 kept\n" +
			"2026/09/15 04:58:13 driver_memcached: probe: dial tcp 127.0.0.1:21211: connect: connection refused\n"
		if err := os.WriteFile(filepath.Join(dir, "refapp_stderr_tail.txt"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	_ = r.run(context.Background())
	if !strings.Contains(out.String(), "refapp_stderr_tail.txt") {
		t.Errorf("the ledger dropped a pointer to evidence that is there:\n%s", out.String())
	}
}

// A file that holds only the writer's header is the same nothing as an
// absent file: "0 line(s)" tells the reader the process produced no
// output, which the ledger can say itself without a detour through the
// filesystem.
func TestHeaderOnlyCaptureCountsAsNoEvidence(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "refapp_stderr_tail.txt")
	if err := os.WriteFile(path, []byte("# refapp stdout+stderr tail: 0 line(s), last 80 kept\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if hasCapturedOutput(path) {
		t.Error("a header-only capture was treated as evidence")
	}
	if err := os.WriteFile(path, []byte("# refapp stdout+stderr tail: 1 line(s), last 80 kept\nboom\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !hasCapturedOutput(path) {
		t.Error("a capture with a real line was treated as nothing")
	}
	if hasCapturedOutput(filepath.Join(dir, "absent.txt")) {
		t.Error("a missing file was treated as evidence")
	}
}

// "cell produced no requests and no property verdicts" is what all eight
// not_run cells of PR-tier run 34920929477 recorded as their reason. It is
// true and it is useless: the cause the orchestrator actually held was
// "driver_memcached: probe: dial tcp 127.0.0.1:21211: connect: connection
// refused". When the driver knows why a cell never came up, that is the
// reason the ledger should carry — the summary line then answers the
// question without the reader opening anything.
func TestNotRunCellCarriesTheDriverCauseWhenThereIsOne(t *testing.T) {
	reason := cellFailureReason(
		"tier1: refapp not ready: EOF\n--- refapp stderr ---\n" +
			"2026/09/15 04:58:13 driver_memcached: probe: dial tcp 127.0.0.1:21211: connect: connection refused\n")
	for _, want := range []string{"refapp not ready", "connection refused", "21211"} {
		if !strings.Contains(reason, want) {
			t.Errorf("reason %q drops %q", reason, want)
		}
	}
	// One line: the ledger prints it inline, and a driver message that
	// embeds the refapp's stderr arrives with newlines in it.
	if strings.Contains(reason, "\n") {
		t.Errorf("reason spans lines and would break the ledger: %q", reason)
	}
	// Bounded: a refapp that printed a stack trace before dying must not
	// push the rest of the summary off the screen. The full text stays in
	// the incident dossier.
	long := cellFailureReason("tier1: refapp not ready: " + strings.Repeat("x", 4000))
	if len(long) > 320 {
		t.Errorf("reason is %d chars; it is a summary line, not the dossier", len(long))
	}
	if !strings.HasSuffix(long, "...") {
		t.Errorf("truncation is not marked: %q", long[len(long)-20:])
	}
	// No cause recorded means no invention.
	if got := cellFailureReason(""); got != "" {
		t.Errorf("cellFailureReason(\"\") = %q, want empty", got)
	}
}
