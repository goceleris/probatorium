package main

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/goceleris/probatorium/report"
)

// matrixRunner executes a resolved matrix plan, one cell at a time, and
// writes the run's single validate-results.json.
//
// It exists as a struct rather than a function body so the loop's POLICY --
// what is recorded, what stops the run -- is reachable from a test without a
// cluster, a refapp binary or an orchestrator. runCell is the seam:
// production binds it to [runMatrixCell]; tests bind it to a stub.
//
// The policy, in one line: a cell's failure is the cell's; the run's
// failure is the run's. probatorium#359.
type matrixRunner struct {
	cfg       Config
	plan      []matrixCell
	startedAt time.Time

	// runCell drives one cell. Returns the cell's result -- always, even
	// on failure, so a cell that produced nothing is still recorded as a
	// cell -- plus the error that describes what went wrong.
	runCell func(ctx context.Context, mc matrixCell, idx int) (report.ValidationCellResult, error)

	// maxConsecutiveFailures and maxTotalFailures bound the damage when
	// the matrix is failing wholesale. Zero means "resolve the default"
	// (see [resolveFailureCaps]); negative disables the cap.
	maxConsecutiveFailures int
	maxTotalFailures       int

	// stagingDir, when non-empty, is the refapp staging directory the
	// cells read their binaries from, checked before every cell. It was
	// readable when the run started (see [watchableStagingDir]), so
	// losing it means the cluster went out from under the run.
	stagingDir string

	// resume, when non-nil, carries the cells an earlier interrupted run
	// already made final. They are seeded into this run's document so the
	// absolute gate judges the whole soak rather than the remainder
	// (probatorium#376), and they are deliberately kept OUT of the ledger:
	// this run did not attempt them, their evidence directories are in the
	// OTHER run's OutDir, and the failure caps below bound THIS run's
	// wasted window. [matrixRunner.inheritedBad] is how their verdicts
	// still reach the exit status.
	resume *resumeMerge

	// out receives the progress lines and the end-of-run summary.
	// Defaults to os.Stderr.
	out io.Writer
}

// errMatrixFatal marks an error whose cause is the RUN's environment
// rather than the cell's subject: the transport to the host under test,
// the staging tree the cells are read from, the disk the evidence is
// written to. Continuing past one of these cannot produce evidence, so
// the remaining cells would burn the window to write nothing.
var errMatrixFatal = errors.New("fatal")

// fatalDriverError is the transport-loss case: a tier could not build its
// remote driver, so the validator cannot reach the host it is testing.
func fatalDriverError(why string) error {
	return fmt.Errorf("%w: remote driver unavailable: %s", errMatrixFatal, why)
}

// fatalStagingError is the lost-cluster case: the directory the refapp
// binaries were staged into is gone, so no later cell has a subject.
func fatalStagingError(dir string, err error) error {
	return fmt.Errorf("%w: refapp staging dir %s is unreadable: %v", errMatrixFatal, dir, err)
}

// fatalCause classifies one cell's error. The second return is the
// human-readable reason; the first says whether the RUN must stop.
//
// The split is deliberately narrow. Everything about the cell's subject --
// a refapp that will not start, an oracle that fires, a per-cell build
// failure, a cell that runs out of its own budget -- is recoverable: those
// are exactly the findings the matrix exists to collect, and one of them
// must never cost the other cells. Only the three conditions that make
// EVERY remaining cell meaningless are fatal:
//
//   - the remote driver is gone (no host to test),
//   - the refapp staging tree is gone (no subject to test),
//   - the disk is full (nowhere to record what was found).
//
// Getting this wrong in either direction is the risk: too fatal and one
// bad refapp costs the window again; too permissive and the run spends
// 70 minutes producing cells that measured nothing. Both fatal detections
// are structural (a recorded driver failure, an ENOSPC from the kernel),
// never a substring match on an error message, because the refapp's own
// dial failures carry the same words as the transport's.
func fatalCause(err error) (string, bool) {
	switch {
	case err == nil:
		return "", false
	case errors.Is(err, errMatrixFatal):
		return err.Error(), true
	case errors.Is(err, syscall.ENOSPC):
		return "fatal: out of disk space: " + err.Error(), true
	}
	return "", false
}

// Failure caps. The matrix is ordered refapp-outer, engine-inner.
//
// defaultMaxConsecutiveFailures = 8 is two ENTIRE refapps with no
// survivor, on a four-engine plan. A defect scoped to one engine (the
// celeris#614 adaptive case: 8 of 32 cells, every fourth one) never
// produces more than ONE consecutive failure; a defect scoped to one
// refapp produces at most four. Eight in a row therefore means the
// failure is scoped to neither, i.e. it is environmental, and the
// remaining cells will repeat it rather than add to it. At the nightly's
// ~150 s cell budget the cap bounds that waste at ~20 minutes of a
// 70-minute window.
//
// The total cap is half the plan, and is deliberately NOT tighter. The
// biggest legitimate correlated failure this matrix has is the driver_*
// family: three of eight refapps, 12 of 32 cells, 37.5%, all of which go
// dark together when the fixture containers do not start. A cap at a
// third would abort that run and throw away the five healthy refapps --
// the exact harm probatorium#359 is about. Half sits above that and still
// stops a run whose verdict can no longer change.
const (
	defaultMaxConsecutiveFailures = 8
	envMaxConsecutiveFailures     = "PROBATORIUM_MATRIX_MAX_CONSECUTIVE_FAILURES"
	envMaxTotalFailures           = "PROBATORIUM_MATRIX_MAX_FAILED_CELLS"
)

// resolveFailureCaps returns the two caps for a plan of planLen cells.
// A negative value (from the env override) disables that cap; the
// override exists for the deliberate "run the whole thing anyway" case,
// e.g. characterising a known-broken tree.
func resolveFailureCaps(consecutiveEnv, totalEnv string, planLen int) (consecutive, total int) {
	consecutive = defaultMaxConsecutiveFailures
	if v, err := strconv.Atoi(strings.TrimSpace(consecutiveEnv)); err == nil && consecutiveEnv != "" {
		consecutive = v
	}
	total = planLen / 2
	if total < defaultMaxConsecutiveFailures {
		// A small plan (a dev smoke, a resumed tail) is worth finishing:
		// there is no window to protect and every cell is a data point.
		total = defaultMaxConsecutiveFailures
	}
	if v, err := strconv.Atoi(strings.TrimSpace(totalEnv)); err == nil && totalEnv != "" {
		total = v
	}
	return consecutive, total
}

// cellFailureReason flattens a driver message into one bounded line fit
// for the ledger. Driver messages embed the refapp's own stderr, so they
// arrive multi-line and can carry a whole stack trace; the ledger is a
// summary and the full text stays in the incident dossier.
func cellFailureReason(msg string) string {
	flat := strings.Join(strings.Fields(msg), " ")
	const max = 300
	if len(flat) > max {
		flat = flat[:max] + "..."
	}
	return flat
}

// cellOutcome is one cell's line in the run's ledger.
type cellOutcome struct {
	cell   matrixCell
	idx    int
	status report.ValidationCellStatus
	reason string
	dir    string
}

func (o cellOutcome) name() string { return o.cell.Refapp + "/" + o.cell.Engine }

// classifyCell decides what a finished cell was. The evidence test is
// [matrixCellIsFinal], the same predicate -matrix-resume-from uses to
// decide whether a cell need not be repeated -- so "this cell did not
// run" means one thing across the runner, the artifact and the resume.
func classifyCell(mc matrixCell, idx int, cell report.ValidationCellResult, err error) cellOutcome {
	out := cellOutcome{
		cell: mc,
		idx:  idx,
		dir:  cellOutDirName(idx, mc),
	}
	ran := matrixCellIsFinal(cell)
	switch {
	case err == nil && ran:
		out.status = report.ValidationCellOK
	case err == nil:
		// No error, no evidence. The orchestrator returned cleanly and
		// the cell still measured nothing -- a parked tier, a refapp that
		// never answered /debug/vars. It did not run.
		out.status = report.ValidationCellNotRun
		out.reason = "cell produced no requests and no property verdicts"
	case ran:
		out.status = report.ValidationCellFailed
		out.reason = err.Error()
	default:
		out.status = report.ValidationCellNotRun
		out.reason = err.Error()
	}
	return out
}

func (r *matrixRunner) log(format string, args ...any) {
	w := r.out
	if w == nil {
		w = os.Stderr
	}
	_, _ = fmt.Fprintf(w, format, args...)
}

// run iterates the plan and returns the aggregate exit error: non-nil
// whenever any cell did not pass, whatever else happened. The matrix is a
// gate; a failed cell is a failed run even though the run continued.
func (r *matrixRunner) run(ctx context.Context) error {
	maxConsecutive, maxTotal := r.maxConsecutiveFailures, r.maxTotalFailures
	if maxConsecutive == 0 || maxTotal == 0 {
		c, t := resolveFailureCaps(os.Getenv(envMaxConsecutiveFailures), os.Getenv(envMaxTotalFailures), len(r.plan))
		if maxConsecutive == 0 {
			maxConsecutive = c
		}
		if maxTotal == 0 {
			maxTotal = t
		}
	}

	// Seeded with the inherited cells, so every write from here on -- the
	// after-every-cell partial included -- is the merged document. That is
	// also what makes a resume OF a resume work: whatever kills this run
	// leaves behind a document carrying both halves.
	inherited := r.resume.cells()
	cells := make([]report.ValidationCellResult, 0, len(r.plan)+len(inherited))
	cells = append(cells, inherited...)
	ledger := make([]cellOutcome, 0, len(r.plan))
	attempted := 0
	consecutive := 0
	failures := 0
	var fatalErr error
	var abortReason string

	for i, mc := range r.plan {
		// Graceful drain (UPS power loss), checked at the TOP of the
		// iteration so the cell that was running has already completed and
		// been persisted. ops/power/cluster-power-event writes the sentinel
		// from apcupsd's onbattery hook. Unlike the cancellation break
		// below, this is not an error: the run stops deliberately, and
		// -resume-from finishes it later.
		if drainRequested() {
			abortReason = fmt.Sprintf("drain requested; stopping cleanly with %d cell(s) unrun", len(r.plan)-i)
			r.log("matrix: %s\n", abortReason)
			break
		}
		// The staging tree going missing is not a cell's problem: it is
		// the cluster's, and no later cell has a subject either. Checked
		// BEFORE the cell so the abort does not record a phantom failure
		// against a refapp that was never at fault.
		if r.stagingDir != "" {
			if _, err := os.ReadDir(r.stagingDir); err != nil {
				fatalErr = fatalStagingError(r.stagingDir, err)
				abortReason = fmt.Sprintf("%v — aborting with %d cell(s) unrun", fatalErr, len(r.plan)-i)
				r.log("matrix: %s\n", abortReason)
				break
			}
		}
		r.log("\nmatrix [%d/%d]: refapp=%s engine=%s\n",
			i+1, len(r.plan), mc.Refapp, mc.Engine)
		cell, err := r.runCell(ctx, mc, i)
		attempted++
		outcome := classifyCell(mc, i, cell, err)
		cell.Status = outcome.status
		cell.FailureReason = outcome.reason
		cells = append(cells, cell)
		ledger = append(ledger, outcome)
		if err != nil {
			r.log("matrix [%d/%d]: %v\n", i+1, len(r.plan), err)
		}

		// Persist after EVERY cell. Until now nothing was written until the
		// whole matrix finished, so a hard kill -- a power cut, an OOM, a
		// SIGKILL -- threw away every completed cell. A 24 h soak losing all
		// 24 h to a mains blip is precisely what the UPS work exists to
		// prevent, and a graceful cancel alone cannot cover it. The document
		// is small (24-48 cells) and this makes it parseable at all times.
		persistErr := writePartial(r.cfg, r.startedAt, cells, r.resume)
		if persistErr != nil {
			r.log("matrix: persist after cell %d: %v\n", i+1, persistErr)
		}

		// Fatal conditions, from the cell itself or from failing to record
		// it. Checked AFTER the cell is written down, so the evidence that
		// named the condition survives the abort.
		if why, fatal := fatalCause(err); fatal {
			fatalErr = err
			abortReason = fmt.Sprintf("%s — aborting with %d cell(s) unrun", why, len(r.plan)-i-1)
			r.log("matrix: %s\n", abortReason)
			break
		}
		if why, fatal := fatalCause(persistErr); fatal {
			fatalErr = persistErr
			abortReason = fmt.Sprintf("%s — aborting with %d cell(s) unrun", why, len(r.plan)-i-1)
			r.log("matrix: %s\n", abortReason)
			break
		}

		if ctx.Err() != nil {
			// Whole-run cancelled — emit what we have and bail.
			abortReason = "run cancelled"
			break
		}

		if outcome.status == report.ValidationCellOK {
			consecutive = 0
			continue
		}
		consecutive++
		failures++
		if maxConsecutive > 0 && consecutive >= maxConsecutive {
			abortReason = fmt.Sprintf("fatal: %d consecutive cell failures (cap %d) — the failure is scoped to neither an engine nor a refapp; aborting with %d cell(s) unrun",
				consecutive, maxConsecutive, len(r.plan)-i-1)
			r.log("matrix: %s\n", abortReason)
			break
		}
		if maxTotal > 0 && failures >= maxTotal {
			abortReason = fmt.Sprintf("fatal: %d of %d cells failed (cap %d) — the run's verdict cannot change; aborting with %d cell(s) unrun",
				failures, len(r.plan), maxTotal, len(r.plan)-i-1)
			r.log("matrix: %s\n", abortReason)
			break
		}
	}

	// Streaming coverage rollup. The WS/SSE slices only run where the
	// refapp routes the endpoint, so this table is the answer to "what
	// did the ws_*/sse_* counters actually measure this run?" — printed
	// to the run log and left beside the results for the same reason
	// the soak's 87.5%-absent rate needed digging out of the totals
	// (probatorium#300).
	coverage := report.FormatStreamingCoverage(report.StreamingCoverage(cells))
	r.log("\n%s", coverage)
	if err := os.WriteFile(filepath.Join(r.cfg.OutDir, "streaming-coverage.txt"), []byte(coverage), 0o644); err != nil {
		r.log("matrix: write streaming coverage: %v\n", err)
	}

	// The failure ledger: every cell that did not pass, with its reason and
	// where its evidence is. Printed to the run log AND left beside the
	// results, because the run log of a 70-minute cluster tier is not where
	// anyone finds anything.
	summary := r.formatSummary(ledger, attempted, abortReason)
	r.log("\n%s", summary)
	if err := os.WriteFile(filepath.Join(r.cfg.OutDir, "cell-failures.txt"), []byte(summary), 0o644); err != nil {
		r.log("matrix: write cell failures: %v\n", err)
	}

	// Run-wide property totals: the per-cell verdicts summed, with the
	// first violation message of every failed predicate keyed
	// "<refapp>/<engine>/<ID>" so the top-level document is readable
	// without descending into cells. On a resumed run the sum spans the
	// inherited cells too -- the document is the whole soak's verdict or it
	// is not a verdict the gate can use.
	doc := buildMatrixDoc(r.cfg, r.startedAt, cells, r.resume)
	var writeErr error
	if err := writeJSON(filepath.Join(r.cfg.OutDir, "validate-results.json"), doc); err != nil {
		r.log("matrix: write results: %v\n", err)
		writeErr = err
	}

	return r.exitError(ledger, attempted, fatalErr, writeErr)
}

// inheritedBad returns the inherited cells whose RECORDED status is neither
// ok nor absent. They are not in the ledger -- this run did not attempt
// them -- but the merged document asserts them, so the run's exit status
// has to. A resumed run that finishes its own cells cleanly over a soak
// that had three real failures is not a passing soak.
//
// Two statuses cannot appear here, and both are worth saying out loud:
// [report.ValidationCellNotRun], because the matrix classifies not_run with
// the same [matrixCellIsFinal] the resume uses to decide a cell need not be
// repeated, so such a cell is always re-run rather than carried over; and
// "" from a document older than schema 5.12, which records no verdict at
// all and must not be read as either pass or fail.
func (r *matrixRunner) inheritedBad() []report.ValidationCellResult {
	var bad []report.ValidationCellResult
	for _, c := range r.resume.cells() {
		if c.Status != "" && c.Status != report.ValidationCellOK {
			bad = append(bad, c)
		}
	}
	return bad
}

// exitError composes the process's exit error. Non-nil whenever a cell did
// not pass, whatever else happened: the matrix is the release gate and a
// resilient run must not become an advisory one.
func (r *matrixRunner) exitError(ledger []cellOutcome, attempted int, fatalErr, writeErr error) error {
	bad := badCells(ledger)
	// Inherited cells the PREVIOUS run already judged as not passing. The
	// document this run writes asserts them, so this run's exit status must
	// too -- otherwise resuming a soak that had three real failures over
	// eight clean remaining cells exits 0 and reads as a recovered soak.
	// Reported as its own clause, never folded into "attempted": this run
	// attempted none of them.
	inheritedBad := r.inheritedBad()
	var inheritedErr error
	if len(inheritedBad) > 0 {
		names := make([]string, 0, len(inheritedBad))
		for _, c := range inheritedBad {
			names = append(names, c.Refapp+"/"+c.Engine+" ("+string(c.Status)+")")
		}
		inheritedErr = fmt.Errorf("matrix: %d inherited cell(s) did not pass in the run they were measured in (%s): %s",
			len(inheritedBad), r.resume.From, strings.Join(names, ", "))
	}
	switch {
	case len(bad) > 0:
		names := make([]string, 0, len(bad))
		for _, o := range bad {
			names = append(names, o.name())
		}
		failed, notRun := countByStatus(bad)
		err := fmt.Errorf("matrix: %d of %d attempted cell(s) did not pass (%d failed, %d never ran): %s — reasons in %s",
			len(bad), attempted, failed, notRun, strings.Join(names, ", "),
			filepath.Join(r.cfg.OutDir, "cell-failures.txt"))
		return errors.Join(err, inheritedErr, fatalErr)
	case inheritedErr != nil:
		return errors.Join(inheritedErr, fatalErr)
	case fatalErr != nil:
		return fatalErr
	case writeErr != nil:
		return writeErr
	}
	return nil
}

func badCells(ledger []cellOutcome) []cellOutcome {
	var bad []cellOutcome
	for _, o := range ledger {
		if o.status != report.ValidationCellOK {
			bad = append(bad, o)
		}
	}
	return bad
}

func countByStatus(ledger []cellOutcome) (failed, notRun int) {
	for _, o := range ledger {
		switch o.status {
		case report.ValidationCellFailed:
			failed++
		case report.ValidationCellNotRun:
			notRun++
		case report.ValidationCellOK:
		}
	}
	return failed, notRun
}

// hasCapturedOutput reports whether path holds refapp output worth sending
// a reader to. The tail writer always emits a header line, so "the file
// exists" is not the question -- "is there a line under the header" is.
// An unreadable file is treated as nothing, because to the reader it is.
func hasCapturedOutput(path string) bool {
	b, err := os.ReadFile(path)
	if err != nil {
		return false
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		return true
	}
	return false
}

// hasDossier reports whether dir holds at least one incident dossier. For a
// cell that never became ready this is where the cause actually lands: the
// T1-DRIVE incident message carries the refapp's own stderr (PR-tier run
// 34920929477: "driver_memcached: probe: dial tcp 127.0.0.1:21211: connect:
// connection refused").
func hasDossier(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}

// evidenceFor names the best evidence a cell actually left behind, or says
// plainly that it left none. Order is by how directly the artefact answers
// "why did this cell not run":
//
//  1. refapp_stderr_tail.txt -- the refapp's own last words;
//  2. incidents/ -- the T1-DRIVE dossier, whose message embeds that stderr
//     when the refapp died before the tail was written;
//  3. nothing -- said out loud, because the alternative is a path the
//     reader follows to an empty file.
func (r *matrixRunner) evidenceFor(o cellOutcome) string {
	cellDir := filepath.Join(r.cfg.OutDir, o.dir)
	if tail := filepath.Join(cellDir, "refapp_stderr_tail.txt"); hasCapturedOutput(tail) {
		return "evidence: " + filepath.Join(o.dir, "refapp_stderr_tail.txt")
	}
	if incidents := filepath.Join(cellDir, "incidents"); hasDossier(incidents) {
		return "evidence: " + filepath.Join(o.dir, "incidents") +
			" (no refapp output was captured; the driver incident carries what it said)"
	}
	return "no refapp output was captured and no incident was recorded: " +
		"the reason above is all this cell produced"
}

// formatSummary renders the end-of-run ledger: what passed, what failed,
// what never ran, what was never attempted, and where each answer lives.
func (r *matrixRunner) formatSummary(ledger []cellOutcome, attempted int, abortReason string) string {
	var b strings.Builder
	bad := badCells(ledger)
	failed, notRun := countByStatus(bad)
	fmt.Fprintf(&b, "matrix cell summary: %d planned, %d attempted, %d ok, %d failed, %d never ran, %d never attempted\n",
		len(r.plan), attempted, attempted-len(bad), failed, notRun, len(r.plan)-attempted)
	// On a resume those counts are about the REMAINDER. The document is
	// about the whole soak, and so is the gate that reads it, so say both.
	if inherited := r.resume.cells(); len(inherited) > 0 {
		fmt.Fprintf(&b, "  resumed: %d cell(s) inherited from %s, not re-measured here; %d cell(s) in the merged document\n",
			len(inherited), r.resume.From, len(inherited)+attempted)
		for _, c := range r.inheritedBad() {
			fmt.Fprintf(&b, "  %-8s %-40s inherited — evidence is in %s\n",
				c.Status, c.Refapp+"/"+c.Engine, r.resume.From)
			if c.FailureReason != "" {
				fmt.Fprintf(&b, "           %s\n", c.FailureReason)
			}
		}
		for _, why := range r.resume.NotInherited {
			fmt.Fprintf(&b, "  not inherited: %s\n", why)
		}
	}
	if abortReason != "" {
		fmt.Fprintf(&b, "  run stopped early: %s\n", abortReason)
	}
	if len(bad) == 0 {
		fmt.Fprintf(&b, "  every attempted cell ran and passed\n")
	}
	for _, o := range bad {
		// Keyed "<refapp>/<engine>", the same way the document keys a
		// cell's failure summaries, so one grep finds both.
		fmt.Fprintf(&b, "  %-8s %-40s %s\n", o.status, o.name(), o.dir)
		fmt.Fprintf(&b, "           %s\n", o.reason)
		if o.status == report.ValidationCellNotRun {
			// The cell measured nothing, so its tally says nothing: the
			// evidence is whatever the refapp said on its way down. Name
			// only what is actually there. A pointer to a file that does
			// not exist -- which is what every not_run cell of PR-tier run
			// 34920929477 got -- teaches the reader less than the summary
			// line already did, and costs them a detour to find that out.
			fmt.Fprintf(&b, "           %s\n", r.evidenceFor(o))
		}
	}
	// Cells the abort never reached are a different fact again: no verdict
	// was formed on them at all. The absolute gate fails the run for them
	// via VALIDATE_GATE_EXPECT_CELLS; naming them here is what makes that
	// count legible.
	if attempted < len(r.plan) {
		var unrun []string
		for _, mc := range r.plan[attempted:] {
			unrun = append(unrun, mc.Refapp+"/"+mc.Engine)
		}
		fmt.Fprintf(&b, "  never attempted (%d): %s\n", len(unrun), strings.Join(unrun, ", "))
		fmt.Fprintf(&b, "  resume them with: -matrix-resume-from=%s\n", r.cfg.OutDir)
	}
	return b.String()
}
