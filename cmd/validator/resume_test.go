package main

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/report"
)

func writeDoc(t *testing.T, dir string, cells []report.ValidationCellResult) {
	t.Helper()
	writeDocAt(t, dir, &report.ValidationResults{Cells: cells})
}

func writeDocAt(t *testing.T, dir string, v *report.ValidationResults) {
	t.Helper()
	doc := report.Document{SchemaVersion: report.SchemaVersion, Validation: v}
	b, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "validate-results.json"), b, 0o644); err != nil {
		t.Fatal(err)
	}
}

// readMergedDoc reads back the document a matrix run wrote, failing loudly
// when there is none -- "the resumed run produced no verdict at all" is the
// pre-#376 behaviour and has to read as a failure, not as an empty result.
func readMergedDoc(t *testing.T, outDir string) report.Document {
	t.Helper()
	p := filepath.Join(outDir, "validate-results.json")
	b, err := os.ReadFile(p)
	if err != nil {
		t.Fatalf("no merged document at %s: %v", p, err)
	}
	var doc report.Document
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatalf("parse %s: %v", p, err)
	}
	if doc.Validation == nil {
		t.Fatalf("%s has no validation block", p)
	}
	return doc
}

// refappTree stages a source tree resolveMatrixPlan can discover: one
// directory per slug, each with a main.go.
//
// It is deliberately NOT a module, so any cell that actually reaches
// `go build` fails in about a second instead of standing a refapp up and
// load-testing it for a per-cell budget. These tests assert WHICH cells a
// resumed run attempts and what the merged document says -- never what a
// cell measures, which is every other test's job.
func refappTree(t *testing.T, slugs ...string) string {
	t.Helper()
	root := t.TempDir()
	for _, slug := range slugs {
		dir := filepath.Join(root, slug)
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "main.go"),
			[]byte("package main\n\nfunc main() {}\n"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return root
}

// finalCell builds a prior-run cell with EVERY tally at a distinct,
// non-default value, so a merge that drops or rebuilds a field cannot pass by
// landing on a coincidence. Status is the one the run that MEASURED it
// recorded (schema 5.12, probatorium#359) -- a prior document has one, and a
// merge that silently rewrote it would be the dishonesty this whole file is
// about.
func finalCell(refapp, engine, arch string, sent int64, passed, failed int) report.ValidationCellResult {
	return report.ValidationCellResult{
		Refapp:           refapp,
		Engine:           engine,
		Arch:             arch,
		Status:           report.ValidationCellOK,
		Tier1:            &report.Tier1Summary{RequestsSent: sent, Requests2xx: sent - 7},
		PropertiesPassed: passed,
		PropertiesFailed: failed,
		FailureSummaries: map[string]string{"I-MEM-1": refapp + "/" + engine + " slope 1071 B/s"},
	}
}

// midFlightCell is what an interrupted run leaves behind and what the matrix
// runner classifies as not_run: a named cell with an all-zero tally.
func midFlightCell(refapp, engine, arch string) report.ValidationCellResult {
	return report.ValidationCellResult{
		Refapp: refapp, Engine: engine, Arch: arch,
		Status:        report.ValidationCellNotRun,
		FailureReason: "cell produced no requests and no property verdicts",
	}
}

// matrixCfg is the smallest Config a matrix run needs when no cell will
// actually stand a refapp up.
func matrixCfg(outDir, arch string) Config {
	return Config{
		Target:   "msr1",
		Arch:     arch,
		Duration: 4 * time.Second,
		OutDir:   outDir,
	}
}

// A cell recorded but empty is what an interrupted run leaves behind.
// Treating it as done is how a resume would quietly hand the absolute gate a
// matrix that is short a cell.
func TestMatrixCellIsFinalRequiresEvidenceOfWork(t *testing.T) {
	if matrixCellIsFinal(report.ValidationCellResult{Refapp: "kitchen_sink", Engine: "epoll"}) {
		t.Error("an empty cell must NOT count as final")
	}
	if !matrixCellIsFinal(report.ValidationCellResult{
		Tier1: &report.Tier1Summary{RequestsSent: 1000},
	}) {
		t.Error("a cell that sent requests must count as final")
	}
	if !matrixCellIsFinal(report.ValidationCellResult{PropertiesPassed: 3}) {
		t.Error("a cell with property verdicts must count as final")
	}
	if !matrixCellIsFinal(report.ValidationCellResult{PropertiesFailed: 1}) {
		t.Error("a FAILED property is still a verdict; re-running would hide it")
	}
}

func TestPlanResumeSplitsThePlanAndCarriesTheVerdicts(t *testing.T) {
	dir := t.TempDir()
	writeDoc(t, dir, []report.ValidationCellResult{
		finalCell("kitchen_sink", "iouring", "arm64", 511, 9, 1),
		midFlightCell("kitchen_sink", "epoll", "arm64"), // recorded, never ran
		finalCell("observability", "std", "arm64", 733, 2, 0),
		finalCell("retired_refapp", "std", "arm64", 101, 1, 0), // not in this run's plan
	})
	plan := []matrixCell{
		{Refapp: "kitchen_sink", Engine: "iouring"},
		{Refapp: "kitchen_sink", Engine: "epoll"},
		{Refapp: "observability", Engine: "std"},
		{Refapp: "driver_redis", Engine: "iouring"},
	}
	remaining, merge, err := planResume(plan, dir, "arm64")
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	if len(merge.Inherited) != 2 {
		t.Fatalf("inherited=%d, want 2 (the two with evidence of work that are in the plan): %+v",
			len(merge.Inherited), merge.Inherited)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining=%d, want 2: %+v", len(remaining), remaining)
	}
	want := map[string]bool{"kitchen_sink/epoll": true, "driver_redis/iouring": true}
	for _, mc := range remaining {
		if !want[mc.Refapp+"/"+mc.Engine] {
			t.Errorf("unexpected remaining cell %s/%s", mc.Refapp, mc.Engine)
		}
	}
	// Inherited cells keep the prior verdict verbatim and are stamped.
	got := merge.Inherited[0]
	if got.Refapp != "kitchen_sink" || got.Engine != "iouring" {
		t.Fatalf("inherited[0] = %s/%s, want kitchen_sink/iouring (plan order)", got.Refapp, got.Engine)
	}
	if got.Tier1 == nil || got.Tier1.RequestsSent != 511 || got.PropertiesPassed != 9 || got.PropertiesFailed != 1 {
		t.Errorf("inherited cell lost its verdict: %+v", got)
	}
	if got.ResumedFrom != dir {
		t.Errorf("inherited cell is not stamped with its provenance: resumed_from=%q, want %q",
			got.ResumedFrom, dir)
	}
	if len(merge.NotInherited) != 2 {
		t.Errorf("NotInherited=%v, want the mid-flight cell and the out-of-plan one", merge.NotInherited)
	}
}

// THE DISHONESTY CASE. A cell that was mid-flight when the runner was lost is
// recorded with no requests and no verdict. It must be re-run, and the merged
// document must show this run's (failed) attempt -- never the prior entry
// dressed up as inherited work.
func TestResumeNeverInheritsAMidFlightCellAsPassing(t *testing.T) {
	prior := filepath.Join(t.TempDir(), "20260913T040535-soak-arm64")
	writeDoc(t, prior, []report.ValidationCellResult{
		finalCell("alpha", "std", "arm64", 4242, 11, 0),
		midFlightCell("bravo", "std", "arm64"), // mid-flight when the runner died
	})
	out := t.TempDir()
	cfg := matrixCfg(out, "arm64")
	// bravo's re-run fails in a second (refappTree is not a module); what
	// matters is that it IS re-run rather than inherited.
	m := MatrixConfig{
		Enabled:    true,
		Refapps:    "alpha,bravo",
		Engines:    "std",
		RefappRoot: refappTree(t, "alpha", "bravo"),
		ResumeFrom: prior,
	}
	if err := runMatrix(context.Background(), cfg, m); err == nil {
		t.Fatal("bravo was re-run and cannot have succeeded (refappTree is not a module); got nil error")
	}

	doc := readMergedDoc(t, out)
	cells := doc.Validation.Cells
	if len(cells) != 2 {
		t.Fatalf("merged document has %d cells, want 2 (alpha inherited + bravo re-run): %+v", len(cells), cells)
	}
	byKey := map[string]report.ValidationCellResult{}
	for _, c := range cells {
		byKey[c.Refapp+"/"+c.Engine] = c
	}
	alpha, ok := byKey["alpha/std"]
	if !ok {
		t.Fatal("the inherited cell alpha/std is missing from the merged document")
	}
	if alpha.ResumedFrom != prior || alpha.Tier1 == nil || alpha.Tier1.RequestsSent != 4242 {
		t.Errorf("alpha/std should be the prior verdict, stamped: %+v", alpha)
	}
	bravo, ok := byKey["bravo/std"]
	if !ok {
		t.Fatal("bravo/std is missing: the mid-flight cell was neither inherited nor re-run")
	}
	if bravo.ResumedFrom != "" {
		t.Errorf("bravo/std was mid-flight in the previous run and must NOT be marked inherited; "+
			"resumed_from=%q", bravo.ResumedFrom)
	}
	if bravo.Tier1 != nil || bravo.PropertiesPassed != 0 || bravo.PropertiesFailed != 0 {
		t.Errorf("bravo/std must carry THIS run's empty result, not the prior entry: %+v", bravo)
	}
	r := doc.Validation.Resume
	if r == nil {
		t.Fatal("the merged document carries no resume provenance; a reader cannot tell which " +
			"cells this run measured")
	}
	if r.InheritedCells != 1 || r.RanCells != 1 {
		t.Errorf("provenance says inherited=%d ran=%d, want 1 and 1", r.InheritedCells, r.RanCells)
	}
	if r.From != prior {
		t.Errorf("provenance From=%q, want %q", r.From, prior)
	}
	var sawMidFlight bool
	for _, why := range r.NotInherited {
		if strings.HasPrefix(why, "bravo/std:") {
			sawMidFlight = true
		}
	}
	if !sawMidFlight {
		t.Errorf("the mid-flight cell is not on the not-inherited record: %v", r.NotInherited)
	}
}

// A resume in which nothing is left to run must still emit the whole-soak
// verdict. Before #376 it returned early and wrote no document at all, so the
// gate read the previous run's directory or failed outright.
func TestResumeWithNothingLeftStillWritesTheWholeSoakVerdict(t *testing.T) {
	prior := filepath.Join(t.TempDir(), "20260913T040535-soak-arm64")
	writeDocAt(t, prior, &report.ValidationResults{
		StartedAt:  time.Date(2026, 9, 13, 4, 5, 35, 0, time.UTC),
		FinishedAt: time.Date(2026, 9, 14, 0, 10, 58, 0, time.UTC),
		Cells: []report.ValidationCellResult{
			finalCell("alpha", "std", "arm64", 1001, 3, 0),
			finalCell("bravo", "std", "arm64", 2002, 5, 1),
			finalCell("charlie", "std", "arm64", 3003, 7, 0),
		},
	})
	out := t.TempDir()
	cfg := matrixCfg(out, "arm64")
	m := MatrixConfig{
		Enabled:    true,
		Refapps:    "alpha,bravo,charlie",
		Engines:    "std",
		RefappRoot: refappTree(t, "alpha", "bravo", "charlie"),
		ResumeFrom: prior,
	}
	if err := runMatrix(context.Background(), cfg, m); err != nil {
		t.Fatalf("runMatrix: %v", err)
	}
	doc := readMergedDoc(t, out)
	if n := len(doc.Validation.Cells); n != 3 {
		t.Fatalf("merged document has %d cells, want 3: a resume with nothing left to run still "+
			"owes the gate the whole soak's verdict", n)
	}
	// Property totals are the union's, not this run's (which measured none).
	if doc.Validation.PropertiesPassed != 15 || doc.Validation.PropertiesFailed != 1 {
		t.Errorf("run-wide totals = %d passed / %d failed, want 15 / 1 (the union)",
			doc.Validation.PropertiesPassed, doc.Validation.PropertiesFailed)
	}
	r := doc.Validation.Resume
	if r == nil {
		t.Fatal("no resume provenance on a document that is entirely inherited")
	}
	if r.InheritedCells != 3 || r.RanCells != 0 {
		t.Errorf("provenance says inherited=%d ran=%d, want 3 and 0", r.InheritedCells, r.RanCells)
	}
	if !r.PriorStartedAt.Equal(time.Date(2026, 9, 13, 4, 5, 35, 0, time.UTC)) {
		t.Errorf("prior_started_at=%s, want the previous run's own start", r.PriorStartedAt)
	}
	if doc.Validation.StartedAt.Before(r.PriorFinishedAt) {
		t.Errorf("the merged document's started_at (%s) must be THIS run's, not the prior run's",
			doc.Validation.StartedAt)
	}
}

// THE OTHER DISHONESTY CASE. A document truncated by whatever killed the
// previous run cannot be resumed from: what it recorded is unknown. Running
// the remainder of a plan derived from it, and reporting success, is the
// failure mode probatorium#376 exists to remove.
func TestResumeFromATruncatedDocumentAborts(t *testing.T) {
	prior := t.TempDir()
	good, err := json.Marshal(report.Document{
		SchemaVersion: report.SchemaVersion,
		Validation: &report.ValidationResults{
			Cells: []report.ValidationCellResult{finalCell("alpha", "std", "arm64", 1001, 3, 0)},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	// Exactly what a SIGKILL mid-write leaves: a valid prefix.
	if err := os.WriteFile(filepath.Join(prior, "validate-results.json"), good[:len(good)/2], 0o644); err != nil {
		t.Fatal(err)
	}
	out := t.TempDir()
	m := MatrixConfig{
		Enabled:    true,
		Refapps:    "alpha,bravo",
		Engines:    "std",
		RefappRoot: refappTree(t, "alpha", "bravo"),
		ResumeFrom: prior,
	}
	err = runMatrix(context.Background(), matrixCfg(out, "arm64"), m)
	if err == nil {
		t.Fatal("a resume from a truncated document must abort; got nil")
	}
	if !strings.Contains(err.Error(), "resume from") {
		t.Errorf("the error must name the resume as the cause, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "validate-results.json")); statErr == nil {
		t.Error("the run wrote a results document anyway; an unreadable resume must abort BEFORE " +
			"any cell runs, or it reports a partial matrix as a verdict")
	}
}

// The missing-document case is the SAME accident wearing a different hat: the
// teardown purged results_root, or `resume_from: auto` selected the empty
// directory it had just created, or the path exists only on the other host of
// a two-arch run. Until #376 all three read as "nothing to skip" and ran the
// full matrix while printing "resume: 0 of 64 cells already final".
func TestResumeFromAMissingDocumentAborts(t *testing.T) {
	out := t.TempDir()
	m := MatrixConfig{
		Enabled:    true,
		Refapps:    "alpha",
		Engines:    "std",
		RefappRoot: refappTree(t, "alpha"),
		ResumeFrom: filepath.Join(t.TempDir(), "purged-by-the-teardown"),
	}
	err := runMatrix(context.Background(), matrixCfg(out, "arm64"), m)
	if err == nil {
		t.Fatal("a resume from a directory with no validate-results.json must abort; got nil")
	}
	if !strings.Contains(err.Error(), "resume from") {
		t.Errorf("the error must name the resume as the cause, got: %v", err)
	}
	if _, statErr := os.Stat(filepath.Join(out, "validate-results.json")); statErr == nil {
		t.Error("the run produced a document from a resume that never happened")
	}
}

// VALIDATE_TARGET=both hands ONE resume_from string to both hosts. Inheriting
// the other arch's verdicts would put them in front of ValidateDiff's
// arch-parity gate as though the parity had been measured on this host.
func TestResumeRejectsAnotherArchsDocument(t *testing.T) {
	prior := t.TempDir()
	writeDoc(t, prior, []report.ValidationCellResult{finalCell("alpha", "std", "arm64", 1001, 3, 0)})
	plan := []matrixCell{{Refapp: "alpha", Engine: "std"}}
	_, _, err := planResume(plan, prior, "amd64")
	if err == nil {
		t.Fatal("an arm64 document resumed on amd64 must abort; got nil")
	}
	if !strings.Contains(err.Error(), "arm64") || !strings.Contains(err.Error(), "amd64") {
		t.Errorf("the error must name both arches, got: %v", err)
	}
}

// A prior document that lists the same cell twice must contribute it once.
// The gate counts entries, so a merge that can double-count is a merge that
// can reach 64 without 64 cells having run.
func TestResumeInheritsADuplicatedCellOnlyOnce(t *testing.T) {
	prior := t.TempDir()
	writeDoc(t, prior, []report.ValidationCellResult{
		finalCell("alpha", "std", "arm64", 1001, 3, 0),
		finalCell("alpha", "std", "arm64", 9009, 4, 0),
	})
	remaining, merge, err := planResume([]matrixCell{{Refapp: "alpha", Engine: "std"}}, prior, "arm64")
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	if len(merge.Inherited) != 1 {
		t.Fatalf("inherited=%d, want 1: a duplicated entry must not inflate the cell count",
			len(merge.Inherited))
	}
	if len(remaining) != 0 {
		t.Fatalf("remaining=%d, want 0", len(remaining))
	}
	if merge.Inherited[0].Tier1.RequestsSent != 1001 {
		t.Errorf("the FIRST recorded verdict must win, got requests_sent=%d",
			merge.Inherited[0].Tier1.RequestsSent)
	}
	if len(merge.NotInherited) != 1 {
		t.Errorf("the dropped duplicate must be on the record: %v", merge.NotInherited)
	}
}

// A run with no -matrix-resume-from must carry no provenance at all: an
// ordinary document must never look like a merged one.
func TestNonResumeRunCarriesNoProvenance(t *testing.T) {
	doc := buildMatrixDoc(matrixCfg(t.TempDir(), "arm64"), time.Now().UTC(),
		[]report.ValidationCellResult{finalCell("alpha", "std", "arm64", 5, 1, 0)}, nil)
	if doc.Validation.Resume != nil {
		t.Errorf("a non-resume run emitted resume provenance: %+v", doc.Validation.Resume)
	}
	if doc.Validation.Cells[0].ResumedFrom != "" {
		t.Error("a cell this run measured must not be marked inherited")
	}
}

func TestDrainRequestedHonoursEnvOverride(t *testing.T) {
	dir := t.TempDir()
	sentinel := filepath.Join(dir, "drain-requested")
	t.Setenv("PROBATORIUM_DRAIN_FILE", sentinel)
	if drainRequested() {
		t.Fatal("no sentinel, but a drain was reported")
	}
	if err := os.WriteFile(sentinel, []byte("now\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if !drainRequested() {
		t.Fatal("sentinel present, but no drain reported")
	}
}

// ---------------------------------------------------------------------------
// probatorium#359 x probatorium#376. Two changes describe the same cell from
// different angles: #359 gives it a Status of ok / failed / not_run, and this
// one gives an inherited cell a ResumedFrom. A cell that reads "ok" while
// nothing in THIS run measured it is exactly the dishonesty the rest of this
// file guards against, so the pairing is decided here rather than left to
// whichever field a reader happens to look at first.
// ---------------------------------------------------------------------------

// The load-bearing invariant: not_run and not-final are the SAME predicate.
// classifyCell calls matrixCellIsFinal, and so does the resume, so a cell
// classified not_run can never be inherited -- it is always re-run. If either
// side ever grows its own notion of "did it run", this breaks.
func TestNotRunAndNotFinalAreTheSamePredicate(t *testing.T) {
	mc := matrixCell{Refapp: "alpha", Engine: "std"}
	cases := []struct {
		name string
		cell report.ValidationCellResult
		err  error
	}{
		{"clean cell", finalCell("alpha", "std", "arm64", 1001, 3, 0), nil},
		{"cell that ran then failed", finalCell("alpha", "std", "arm64", 1001, 0, 2), errors.New("oracle fired")},
		// matrixCellIsFinal has TWO arms and both have to be shared. Without
		// this row the test passes against a classifyCell that judges on
		// requests alone -- and such a cell would be not_run in the ledger
		// while the resume read it as final, i.e. inheritable as a cell that
		// never ran. (Verified: it is the row that fails that injection.)
		{"property verdicts but no requests", report.ValidationCellResult{
			Refapp: "alpha", Engine: "std", PropertiesFailed: 1,
		}, nil},
		{"empty cell, no error", report.ValidationCellResult{Refapp: "alpha", Engine: "std"}, nil},
		{"empty cell, refapp never started", report.ValidationCellResult{Refapp: "alpha", Engine: "std"}, errors.New("start refapp")},
		{"tier1 present but silent", report.ValidationCellResult{Refapp: "alpha", Engine: "std", Tier1: &report.Tier1Summary{}}, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := classifyCell(mc, 0, tc.cell, tc.err)
			isNotRun := got.status == report.ValidationCellNotRun
			if isNotRun == matrixCellIsFinal(tc.cell) {
				t.Fatalf("status=%q but matrixCellIsFinal=%v; the matrix runner and the resume "+
					"disagree about whether this cell ran, so a not_run cell could be inherited",
					got.status, matrixCellIsFinal(tc.cell))
			}
		})
	}
}

// ...and the consequence, end to end: a prior cell recorded not_run is never
// carried over, whatever its status says.
func TestResumeNeverInheritsANotRunCell(t *testing.T) {
	prior := t.TempDir()
	writeDoc(t, prior, []report.ValidationCellResult{
		finalCell("alpha", "std", "arm64", 1001, 3, 0),
		midFlightCell("bravo", "std", "arm64"),
	})
	plan := []matrixCell{{Refapp: "alpha", Engine: "std"}, {Refapp: "bravo", Engine: "std"}}
	remaining, merge, err := planResume(plan, prior, "arm64")
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	for _, c := range merge.Inherited {
		if c.Status == report.ValidationCellNotRun {
			t.Errorf("a not_run cell was inherited: %s/%s", c.Refapp, c.Engine)
		}
	}
	if len(remaining) != 1 || remaining[0].Refapp != "bravo" {
		t.Fatalf("the not_run cell must be re-run, got remaining=%+v", remaining)
	}
}

// A cell that RAN and then failed is the finding. It must be inherited with
// its verdict and its reason intact -- re-running it would hide it -- and it
// must still make the resumed run exit non-zero, because a resume that
// finishes its own cells cleanly over a soak that had real failures is not a
// passing soak.
func TestResumeInheritsAFailedCellAndStillFailsTheRun(t *testing.T) {
	failed := finalCell("bravo", "std", "amd64", 88_000, 1, 1)
	failed.Status = report.ValidationCellFailed
	failed.FailureReason = "cell run: I-MEM-1 violated: heap trough slope 1071 B/s"
	prior := t.TempDir()
	writeDoc(t, prior, []report.ValidationCellResult{
		finalCell("alpha", "std", "amd64", 1001, 3, 0),
		failed,
	})
	plan := []matrixCell{{Refapp: "alpha", Engine: "std"}, {Refapp: "bravo", Engine: "std"}}
	remaining, merge, err := planResume(plan, prior, "amd64")
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	if len(remaining) != 0 {
		t.Fatalf("a failed cell is a verdict and must not be re-run, got %+v", remaining)
	}
	var got *report.ValidationCellResult
	for i, c := range merge.Inherited {
		if c.Refapp == "bravo" {
			got = &merge.Inherited[i]
		}
	}
	if got == nil {
		t.Fatal("the failed cell was not inherited; re-running it would hide the finding")
	}
	if got.Status != report.ValidationCellFailed {
		t.Errorf("the inherited cell's status was rewritten to %q; the run that measured it said failed", got.Status)
	}
	if got.FailureReason != failed.FailureReason {
		t.Errorf("the inherited cell lost its reason: %q", got.FailureReason)
	}
	if got.ResumedFrom != prior {
		t.Errorf("the inherited cell is not stamped: resumed_from=%q", got.ResumedFrom)
	}

	// The run's exit status has to carry it, even with nothing left to run.
	r, _, out := newStubRunner(t, nil, func(mc matrixCell, _ int) (report.ValidationCellResult, error) {
		t.Fatalf("no cell should run, got %s/%s", mc.Refapp, mc.Engine)
		return report.ValidationCellResult{}, nil
	})
	r.resume = merge
	runErr := r.run(t.Context())
	if runErr == nil {
		t.Fatal("a resume carrying an inherited FAILED cell exited 0; the soak did not pass")
	}
	if !strings.Contains(runErr.Error(), "inherited") || !strings.Contains(runErr.Error(), "bravo/std") {
		t.Errorf("the exit error does not name the inherited failure: %v", runErr)
	}
	if strings.Contains(runErr.Error(), "attempted cell(s) did not pass") {
		t.Errorf("an inherited cell was counted as attempted by this run: %v", runErr)
	}
	if !strings.Contains(out.String(), "inherited") {
		t.Errorf("the end-of-run summary does not mention the inherited cells:\n%s", out.String())
	}
}

// An all-ok resume must exit 0 and the summary must still say how much of the
// merged document this run actually measured.
func TestResumeWithOnlyOKInheritedCellsPasses(t *testing.T) {
	prior := t.TempDir()
	writeDoc(t, prior, []report.ValidationCellResult{finalCell("alpha", "std", "amd64", 1001, 3, 0)})
	_, merge, err := planResume([]matrixCell{{Refapp: "alpha", Engine: "std"}}, prior, "amd64")
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	r, _, out := newStubRunner(t, nil, func(matrixCell, int) (report.ValidationCellResult, error) {
		t.Fatal("no cell should run")
		return report.ValidationCellResult{}, nil
	})
	r.resume = merge
	if err := r.run(t.Context()); err != nil {
		t.Fatalf("an all-ok resume must exit 0: %v", err)
	}
	if !strings.Contains(out.String(), "1 cell(s) inherited") {
		t.Errorf("the summary does not say what was inherited:\n%s", out.String())
	}
	cells := readCells(t, r.cfg.OutDir)
	if len(cells) != 1 {
		t.Fatalf("merged document has %d cells, want 1", len(cells))
	}
	if cells[0]["status"] != "ok" {
		t.Errorf("the inherited cell lost its recorded status: %v", cells[0]["status"])
	}
	if cells[0]["resumed_from"] != prior {
		t.Errorf("the inherited cell is not stamped in the document: %v", cells[0]["resumed_from"])
	}
}

// A prior document written before schema 5.12 records no status at all.
// "" must stay "" -- neither promoted to ok (which would claim a verdict
// nobody recorded) nor counted as a failure (which would make every resume
// from an older document fail).
func TestResumeDoesNotInventAStatusForAnOlderDocument(t *testing.T) {
	old := finalCell("alpha", "std", "amd64", 1001, 3, 0)
	old.Status = "" // pre-5.12
	prior := t.TempDir()
	writeDoc(t, prior, []report.ValidationCellResult{old})
	_, merge, err := planResume([]matrixCell{{Refapp: "alpha", Engine: "std"}}, prior, "amd64")
	if err != nil {
		t.Fatalf("planResume: %v", err)
	}
	if got := merge.Inherited[0].Status; got != "" {
		t.Errorf("status was invented for a pre-5.12 cell: %q", got)
	}
	r, _, _ := newStubRunner(t, nil, func(matrixCell, int) (report.ValidationCellResult, error) {
		t.Fatal("no cell should run")
		return report.ValidationCellResult{}, nil
	})
	r.resume = merge
	if err := r.run(t.Context()); err != nil {
		t.Fatalf("an unrecorded status must not fail the run: %v", err)
	}
}
