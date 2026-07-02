//go:build mage

package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/goceleris/probatorium/report"
)

// Publish targets. Publish splits the latest bench results.json into the
// four-file docs tree (summary.json + timeseries.json.gz +
// histograms.json.gz + env.json under
// results/<version>/<yyyymmdd>/<arch>/), writes it into a goceleris/docs
// checkout, commits + pushes, then fires a TINY pointer
// repository_dispatch so the docs sync workflow can validate → index →
// refresh latest/. PublishValidate is a thin alias — validation rides
// inside the same summary.json (Document.validation_results) — so there
// is exactly one event type and one tree. BenchAndValidate composes the
// release gate.

// benchmarkPublishedEvent is the single canonical repository_dispatch
// event type the docs sync-benchmarks workflow listens for. It replaces
// the four mismatched strings the old design carried (celeris-bench /
// celeris-validate / results-updated / benchmark-updated): one producer
// string, one consumer string.
const benchmarkPublishedEvent = "benchmark-published"

// docsRepo is the GitHub owner/repo the tree is published to.
const docsRepo = "goceleris/docs"

// docsBranch is the docs default branch every publish pushes to.
const docsBranch = "main"

// defaultRunID is the canonical run for a date. The bench runs a single
// pass, so PUBLISH_RUN_ID is left unset and every publish is run-1. The
// env override is retained (a manual relabel can still set it) but the
// removed back-to-back loop no longer iterates run-1..run-N.
const defaultRunID = "run-1"

// archTag maps a Go GOARCH to the canonical on-disk / dispatch arch
// vocabulary. amd64 → x86_64; arm64 stays arm64. Everything downstream
// (tree dir, env.json, index.json, dispatch payload) uses the tag form
// so the docs site never has to know about Go's spelling.
func archTag(goarch string) string {
	switch goarch {
	case "amd64", "x86_64":
		return "x86_64"
	case "arm64", "aarch64":
		return "arm64"
	default:
		return goarch
	}
}

// archTagFromHostArchPair derives the publish-path arch tag from the
// document's own HostArchPair ("<GOOS>/<GOARCH>", e.g. "linux/amd64") — the
// same value BuildDocument computes from BENCH_TARGET via benchTargetArch,
// i.e. the arch of the benched SUT, not of the host running `mage Publish`.
//
// Using the document's self-reported arch (rather than re-deriving from
// runtime.GOARCH / BENCH_GOARCH at publish time) ties the on-disk path to
// the data's actual source of truth. Before this, meta.Arch fell back to
// runtime.GOARCH of the PUBLISHING host, which is msr1 (arm64, the cluster
// conductor pinned in benchmark-tier.yml) — not msa2-server (amd64, the
// actual SUT) — silently mis-filing every amd64 SUT run under "arm64" once
// the conductor was pinned to msr1 (see celeris v1.5.6 20260629 misfile).
func archTagFromHostArchPair(hostArchPair string) string {
	if _, goarch, ok := strings.Cut(hostArchPair, "/"); ok && goarch != "" {
		return archTag(goarch)
	}
	// Malformed/empty HostArchPair (e.g. very old result trees predating the
	// field) — fall back to the prior best-effort behaviour instead of
	// publishing under an empty/garbage arch segment.
	return archTag(benchTargetGOARCH())
}

// Publish writes the latest bench results into the docs tree and fires
// the pointer dispatch.
//
// Flow:
//  1. Resolve version (PUBLISH_VERSION or go.mod), date (UTC yyyymmdd),
//     arch (archTag of BENCH_GOARCH/runtime), run_id (run-1).
//  2. Read the newest results/<...>-bench-<ver>/results.json and its
//     sibling timeseries.json.gz.
//  3. report.SplitDocument + report.WriteTree into the docs checkout's
//     results/ dir, producing results/<ver>/<date>/<arch>/{4 files}.
//  4. Commit + push the cell (git path) or PUT each file (contents path).
//  5. Fire a ≤1 KB pointer repository_dispatch {version,arch,date,
//     run_id,path,commit}. The docs workflow owns index.json + latest/.
//
// Env knobs:
//
//	PUBLISH_VERSION=   override go.mod auto-detect (manual relabel).
//	PUBLISH_VIA=       "git" (default) commits into a docs checkout;
//	                   "contents" PUTs each file via the GitHub
//	                   contents API (CI with no local checkout).
//	PUBLISH_DRYRUN=1   split + write the tree to a local dir and stop:
//	                   no clone, no push, no dispatch. Prints the cell
//	                   path. Default output dir is ./results-publish
//	                   (override with PUBLISH_OUT).
//	PUBLISH_OUT=       dry-run output root (default ./results-publish).
//	DOCS_REPO_DIR=     path to an existing goceleris/docs working tree.
//	                   When set (git path) the tree is written there and
//	                   committed in place; otherwise a shallow clone is
//	                   made in a temp dir.
//	DOCS_TOKEN=        GitHub token with repo scope on goceleris/docs.
//	                   Falls back to `gh auth token`.
//	BENCH_PUBLISH_FORCE=1  publish despite integrity violations (missing
//	                   column rollups, >20% non-ok cells, dead-SUT
//	                   cells). The integrity summary always prints.
func Publish() error {
	meta, doc, tsGz, resultsDir, err := loadPublishInputs()
	if err != nil {
		return err
	}

	// Provenance single-source: the publish version (meta.Version) is
	// authoritative — it names the results tree. The embedded
	// BenchmarkConfig.CelerisVer can be stale ("dev") when the bench ran
	// before a tag resolved, which is exactly how published v1.5.5 shipped
	// celeris_version="dev" while the tree said v1.5.5. Overwrite so the
	// embedded value and the tree can never disagree.
	if meta.Version != "" {
		doc.BenchmarkConfig.CelerisVer = meta.Version
	}

	// Integrity gate (v3.9): refuse to ship a run that v3.8 proved can
	// look complete while being broken — a force-exited runner column,
	// a mostly-dead grid, or cells measured against a crashed SUT. The
	// summary prints either way; BENCH_PUBLISH_FORCE=1 overrides.
	if err := checkPublishIntegrity(resultsDir, doc); err != nil {
		return err
	}

	// Data-completeness gate: refuse to ship a document missing data that
	// must never be empty. This is the guard that was absent while
	// hdr_histogram_b64 / loadgen_cpu_p95 / framework_version silently
	// shipped empty for four releases (v1.5.2–v1.5.5). BENCH_PUBLISH_FORCE=1
	// overrides (e.g. backfilling an old run whose raw data predates a
	// metric). See checkDataCompleteness.
	if err := checkDataCompleteness(doc); err != nil {
		return err
	}

	// Dry-run: split + write locally, then stop before any network op.
	if os.Getenv("PUBLISH_DRYRUN") == "1" {
		outRoot := envOrDefault("PUBLISH_OUT", "results-publish")
		cell, err := report.WriteTree(filepath.Join(outRoot, "results"), doc, tsGz, meta)
		if err != nil {
			return fmt.Errorf("dry-run write tree: %w", err)
		}
		fmt.Printf("Dry-run: wrote tree to %s (no push, no dispatch)\n", cell)
		return nil
	}

	via := envOrDefault("PUBLISH_VIA", "git")
	var commit string
	switch via {
	case "git":
		commit, err = publishViaGit(meta, doc, tsGz)
	case "contents":
		commit, err = publishViaContents(meta, doc, tsGz)
	default:
		return fmt.Errorf("PUBLISH_VIA=%q not recognised (use git or contents)", via)
	}
	if err != nil {
		return err
	}

	return dispatchPointer(meta, commit)
}

// PublishValidate is retained for the BenchAndValidate sequencing and
// any caller that still invokes the target by name. Validation results
// are part of the canonical Document (validation_results) and therefore
// already land in summary.json — there is no separate validate tree or
// event type. It simply delegates to Publish.
func PublishValidate() error {
	return Publish()
}

// checkDataCompleteness fails the publish when the merged Document is missing
// data that must never ship empty. It is the structural guard that was absent
// while hdr_histogram_b64, loadgen_cpu_p95, and framework_version silently
// shipped 0/52 — and started_at shipped as the zero time — across FOUR
// releases (v1.5.2–v1.5.5). A "looks complete but is empty" document now fails
// here at release time instead of being discovered four releases later.
//
// BENCH_PUBLISH_FORCE=1 overrides (same escape hatch as the integrity gate),
// e.g. to backfill an old run whose RAW data predates a metric (the v1.5.5
// histograms can be backfilled, but that run never captured loadgen_cpu_p95).
func checkDataCompleteness(doc *report.Document) error {
	var problems []string

	bc := doc.BenchmarkConfig
	if bc.StartedAt.IsZero() {
		problems = append(problems, "benchmark_config.started_at is the zero time (cluster-merge never set it)")
	}
	if bc.FinishedAt.IsZero() {
		problems = append(problems, "benchmark_config.finished_at is the zero time")
	}
	if !bc.StartedAt.IsZero() && !bc.FinishedAt.IsZero() && bc.FinishedAt.Before(bc.StartedAt) {
		problems = append(problems, "benchmark_config.finished_at precedes started_at")
	}

	celerisSeen := false
	for _, b := range doc.Benchmarks {
		// Every cell, every framework: identity + the headline metric must
		// be present (these are projected verbatim from the registry / the
		// always-emitted aggregate, so empty here means a real drop).
		if b.Name == "" {
			problems = append(problems, "a benchmark row has an empty name")
		}
		for field, v := range map[string]string{"framework": b.Framework, "language": b.Language, "category": b.Category} {
			if v == "" {
				problems = append(problems, fmt.Sprintf("%s: %s is empty", b.Name, field))
			}
		}
		if len(b.SaturationModeRPS) == 0 {
			problems = append(problems, fmt.Sprintf("%s: saturation_mode_rps is empty (no scenario produced a number)", b.Name))
		}

		// The framework UNDER TEST must carry its full per-cell data — this
		// is the exact set that silently shipped empty. Competitor rows are
		// exempt (their version/CPU/histogram coverage is best-effort).
		if b.Framework == "celeris" {
			celerisSeen = true
			if b.FrameworkVersion == "" {
				problems = append(problems, fmt.Sprintf("%s: framework_version is empty (celeris is the framework under test)", b.Name))
			}
			if countNonEmptyStr(b.HdrHistogramB64) == 0 {
				problems = append(problems, fmt.Sprintf("%s: hdr_histogram_b64 has no non-empty entries (histogram dropped)", b.Name))
			}
			if countNonZeroF(b.LoadgenCPUP95) == 0 {
				problems = append(problems, fmt.Sprintf("%s: loadgen_cpu_p95 has no non-zero entries (self-CPU sampler off?)", b.Name))
			}
		}
	}
	if !celerisSeen {
		problems = append(problems, "no celeris column in the document — the framework under test is missing entirely")
	}

	if len(problems) == 0 {
		return nil
	}
	msg := fmt.Sprintf("publish data-completeness gate FAILED (%d issue(s)):\n  - %s", len(problems), strings.Join(problems, "\n  - "))
	if os.Getenv("BENCH_PUBLISH_FORCE") == "1" {
		fmt.Fprintf(os.Stderr, "WARNING: %s\n(shipping anyway: BENCH_PUBLISH_FORCE=1)\n", msg)
		return nil
	}
	return fmt.Errorf("%s\n(set BENCH_PUBLISH_FORCE=1 to deliberately ship a known-incomplete run)", msg)
}

func countNonEmptyStr(m map[string]string) int {
	n := 0
	for _, v := range m {
		if v != "" {
			n++
		}
	}
	return n
}

func countNonZeroF(m map[string]float64) int {
	n := 0
	for _, v := range m {
		if v != 0 {
			n++
		}
	}
	return n
}

// loadPublishInputs resolves the run metadata and reads the newest bench
// results.json + its timeseries sidecar. Shared by every publish path.
// The fourth return is the run dir the results came from, so the
// integrity gate can audit its column rollups + raw payloads.
func loadPublishInputs() (report.SplitMeta, *report.Document, []byte, string, error) {
	version := os.Getenv("PUBLISH_VERSION")
	if version == "" {
		v, err := celerisVersion()
		if err != nil {
			return report.SplitMeta{}, nil, nil, "", err
		}
		version = v
	}

	resultsPath, err := latestBenchResults(version)
	if err != nil {
		// Fall back to the most recent run regardless of version: the
		// intent of Publish without a fresh same-version bench is "ship
		// what I have."
		resultsPath, err = latestBenchResults("")
		if err != nil {
			return report.SplitMeta{}, nil, nil, "", fmt.Errorf("no bench results to publish: %w", err)
		}
	}

	data, err := os.ReadFile(resultsPath)
	if err != nil {
		return report.SplitMeta{}, nil, nil, "", fmt.Errorf("read %s: %w", resultsPath, err)
	}
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return report.SplitMeta{}, nil, nil, "", fmt.Errorf("parse %s: %w", resultsPath, err)
	}

	// timeseries.json.gz lives next to results.json (both emit paths
	// write it there). Absent is fine — older runs predate the sidecar.
	var tsGz []byte
	tsPath := filepath.Join(filepath.Dir(resultsPath), report.TimeseriesFile)
	if b, err := os.ReadFile(tsPath); err == nil {
		tsGz = b
	}

	now := time.Now().UTC()
	// BENCH_START_DATE (if set) pins the Publish to the bench's start
	// date, so a single pass that crosses midnight UTC lands all cells
	// under the same date. Falls back to the per-Publish timestamp
	// otherwise (the behaviour for a one-shot `mage Publish` invocation).
	dateStr := os.Getenv("BENCH_START_DATE")
	if dateStr == "" {
		dateStr = now.Format("20060102")
	}
	meta := report.SplitMeta{
		Version:        version,
		Arch:           archTagFromHostArchPair(doc.HostArchPair),
		Date:           dateStr,
		RunID:          envOrDefault("PUBLISH_RUN_ID", defaultRunID),
		GitSHA:         gitRefOr(),
		GitRef:         os.Getenv("GITHUB_REF"),
		CelerisVersion: version,
		LoadgenVersion: goModRequireVersion("github.com/goceleris/loadgen"),
		GeneratedAt:    now,
	}
	return meta, &doc, tsGz, filepath.Dir(resultsPath), nil
}

// cellRelPath is the repo-root-relative path of a published cell, the
// value the pointer dispatch and the contents API both key on. It mirrors
// WriteTree's on-disk layout via the shared report.CellRelDir helper. The
// run-id segment keeps an alternate run-K (a manual relabel) from
// overwriting run-1's tree; the single-pass bench only ever writes run-1.
func cellRelPath(meta report.SplitMeta) string {
	return filepath.ToSlash(filepath.Join("results", report.CellRelDir(meta)))
}

// publishNonOKThreshold is the max tolerated fraction of non-OK cells in
// a published Document. Past it the run is more hole than data (v3.8:
// one crashed column alone produced 23 dnf cells) and publishing it
// would put rows with no numbers on the docs site.
const publishNonOKThreshold = 0.20

// publishIntegrity is the pre-publish audit collected from the bench run
// dir + merged Document. Pure data — the rule evaluation lives in
// violations() so tests can pin both halves separately.
type publishIntegrity struct {
	ResultsDir string

	// MissingRollups lists column dirs (relative to ResultsDir) without
	// the runner's results.json rollup — the runner force-exited
	// mid-column and ingest reconstructed the cells from per-cell JSONs
	// (provenanceReconstructed in mage_bench.go).
	MissingRollups []string

	// Reconstructed counts raw cells carrying the reconstruction
	// provenance mark — the cell-level mirror of MissingRollups.
	Reconstructed int

	// StatusCounts buckets every (server, scenario) cell of the merged
	// Document by reduced status; TotalCells is the sum.
	StatusCounts map[report.CellStatus]int
	TotalCells   int

	// ServerDied lists "server/scenario — error" for per-run records
	// whose reason is a dead SUT (the runner-synthesised "server-down:"
	// pre-probe and "server-died-mid-cell:" post-probe markers). A cell
	// that recovered on a rerun still appears here — its suspect status
	// keeps the evidence, and publishing it needs an explicit force.
	ServerDied []string
}

// collectPublishIntegrity audits resultsDir + the merged Document.
// Structural inputs (column dirs, raw payloads) are best-effort: an old
// or hand-assembled run dir without them simply contributes nothing,
// and the Document-level cell census still applies.
func collectPublishIntegrity(resultsDir string, doc *report.Document) publishIntegrity {
	pi := publishIntegrity{
		ResultsDir:   resultsDir,
		StatusCounts: map[report.CellStatus]int{},
	}

	// Column rollups: every <TS>-bench-<host>/<RR>-<comp>/ dir the
	// playbook produced must carry the runner's own results.json. The
	// dir matching mirrors aggregatePerCellResults.
	if entries, err := os.ReadDir(resultsDir); err == nil {
		for _, e := range entries {
			if !e.IsDir() || !strings.Contains(e.Name(), "-bench-") {
				continue
			}
			cols, err := os.ReadDir(filepath.Join(resultsDir, e.Name()))
			if err != nil {
				continue
			}
			for _, c := range cols {
				if !c.IsDir() {
					continue
				}
				parts := strings.SplitN(c.Name(), "-", 2)
				if len(parts) != 2 {
					continue
				}
				if _, err := parseRunIndex(parts[0]); err != nil {
					continue
				}
				if _, err := os.Stat(filepath.Join(resultsDir, e.Name(), c.Name(), "results.json")); err != nil {
					pi.MissingRollups = append(pi.MissingRollups, filepath.Join(e.Name(), c.Name()))
				}
			}
		}
	}
	sort.Strings(pi.MissingRollups)

	// Cell census from the Document: a scenario is one cell per server;
	// CellStatuses carries every non-OK outcome, headline maps the OK
	// (and suspect) ones. Union the two key sets so a suspect cell —
	// present in both — counts once, under its flag.
	for _, b := range doc.Benchmarks {
		cells := map[string]report.CellStatus{}
		for sc := range b.SaturationModeRPS {
			cells[sc] = report.CellOK
		}
		for sc, st := range b.CellStatuses {
			cells[sc] = report.CellStatus(st)
		}
		for _, st := range cells {
			pi.StatusCounts[st]++
			pi.TotalCells++
		}
	}

	// Dead-SUT evidence + reconstruction marks live on the per-run raw
	// records (the Document carries statuses, not error strings).
	if entries, err := os.ReadDir(filepath.Join(resultsDir, "raw")); err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".json") {
				continue
			}
			data, err := os.ReadFile(filepath.Join(resultsDir, "raw", e.Name()))
			if err != nil {
				continue
			}
			var payload struct {
				Cells []cellRecord `json:"cells"`
			}
			if json.Unmarshal(data, &payload) != nil {
				continue
			}
			for _, c := range payload.Cells {
				if c.Provenance != "" {
					pi.Reconstructed++
				}
				st := report.CellStatus(c.Status)
				if st == "" {
					st = report.ClassifyCellError(c.Error)
				}
				if st != report.CellDNF && st != report.CellSuspect {
					continue
				}
				if strings.HasPrefix(c.Error, "server-died") || strings.HasPrefix(c.Error, "server-down:") {
					pi.ServerDied = append(pi.ServerDied,
						fmt.Sprintf("%s/%s — %s", c.Competitor, c.Scenario, c.Error))
				}
			}
		}
	}
	sort.Strings(pi.ServerDied)
	return pi
}

// violations evaluates the publish-refusal rules against the audit.
// Empty means the run may ship.
func (pi publishIntegrity) violations() []string {
	var v []string
	if n := len(pi.MissingRollups); n > 0 {
		v = append(v, fmt.Sprintf("%d column(s) missing their results.json rollup (runner died mid-column; cells reconstructed from per-cell JSONs)", n))
	}
	if pi.TotalCells > 0 {
		nonOK := pi.TotalCells - pi.StatusCounts[report.CellOK]
		if frac := float64(nonOK) / float64(pi.TotalCells); frac > publishNonOKThreshold {
			v = append(v, fmt.Sprintf("%.0f%% of cells are non-ok (%d/%d, threshold %.0f%%)",
				frac*100, nonOK, pi.TotalCells, publishNonOKThreshold*100))
		}
	}
	if n := len(pi.ServerDied); n > 0 {
		v = append(v, fmt.Sprintf("%d cell(s) measured against a dead SUT (server-down / server-died-mid-cell)", n))
	}
	return v
}

// renderPublishIntegrity formats the audit as fixed-width text (not
// markdown) so it reads cleanly in a CI log. Printed on every publish,
// violations or not.
func renderPublishIntegrity(pi publishIntegrity) string {
	var sb strings.Builder
	fmt.Fprintf(&sb, "\n=== Publish integrity: %s ===\n", pi.ResultsDir)
	fmt.Fprintf(&sb, "  cells: %d total", pi.TotalCells)
	for _, st := range []report.CellStatus{report.CellOK, report.CellSuspect, report.CellDNF, report.CellNotApplicable} {
		if n := pi.StatusCounts[st]; n > 0 {
			fmt.Fprintf(&sb, "  %s=%d", st, n)
		}
	}
	sb.WriteString("\n")
	if pi.Reconstructed > 0 {
		fmt.Fprintf(&sb, "  reconstructed cells: %d (from columns that lost their rollup)\n", pi.Reconstructed)
	}
	for _, m := range pi.MissingRollups {
		fmt.Fprintf(&sb, "  MISSING ROLLUP  %s\n", m)
	}
	for _, s := range pi.ServerDied {
		fmt.Fprintf(&sb, "  SERVER DIED     %s\n", s)
	}
	if v := pi.violations(); len(v) > 0 {
		for _, msg := range v {
			fmt.Fprintf(&sb, "  VIOLATION       %s\n", msg)
		}
	} else {
		sb.WriteString("  integrity: ok\n")
	}
	return sb.String()
}

// checkPublishIntegrity prints the integrity summary and refuses the
// publish when any violation is present, unless BENCH_PUBLISH_FORCE=1
// (which still prints everything, then proceeds loudly).
func checkPublishIntegrity(resultsDir string, doc *report.Document) error {
	pi := collectPublishIntegrity(resultsDir, doc)
	fmt.Print(renderPublishIntegrity(pi))
	v := pi.violations()
	if len(v) == 0 {
		return nil
	}
	if os.Getenv("BENCH_PUBLISH_FORCE") == "1" {
		fmt.Printf("  BENCH_PUBLISH_FORCE=1 — publishing despite %d violation(s)\n", len(v))
		return nil
	}
	return fmt.Errorf("publish integrity: refusing to publish %s: %s (set BENCH_PUBLISH_FORCE=1 to override)",
		resultsDir, strings.Join(v, "; "))
}

// publishViaGit writes the tree into a goceleris/docs checkout and
// pushes it, returning the pushed commit SHA. It deliberately does NOT
// touch index.json or latest/ — the docs workflow is the single writer
// of the manifest, so concurrent per-arch publishes never race on it.
func publishViaGit(meta report.SplitMeta, doc *report.Document, tsGz []byte) (string, error) {
	token, err := resolveDocsToken()
	if err != nil {
		return "", err
	}

	repoDir := os.Getenv("DOCS_REPO_DIR")
	cleanup := func() {}
	if repoDir == "" {
		dir, err := os.MkdirTemp("", "celeris-docs-")
		if err != nil {
			return "", fmt.Errorf("temp docs dir: %w", err)
		}
		cleanup = func() { _ = os.RemoveAll(dir) }
		repoDir = dir
		if err := cloneDocs(repoDir, token); err != nil {
			cleanup()
			return "", err
		}
	}
	defer cleanup()

	if _, err := report.WriteTree(filepath.Join(repoDir, "results"), doc, tsGz, meta); err != nil {
		return "", fmt.Errorf("write tree: %w", err)
	}
	rel := cellRelPath(meta)
	fmt.Printf("Publishing %s → %s...\n", rel, docsRepo)

	if err := runGit(repoDir, "add", rel); err != nil {
		return "", err
	}
	msg := fmt.Sprintf("bench(%s/%s/%s/%s): publish", meta.Version, meta.Date, meta.Arch, meta.RunID)
	if err := runGit(repoDir, "-c", "user.name=celeris-bot",
		"-c", "user.email=bot@goceleris.dev",
		"commit", "-m", msg); err != nil {
		return "", err
	}
	if err := pushDocsHEAD(repoDir, token); err != nil {
		return "", err
	}

	sha, err := gitHeadSHA(repoDir)
	if err != nil {
		return "", err
	}
	fmt.Println("Pushed.")
	return sha, nil
}

// publishViaContents writes each of the four files through the GitHub
// contents API (base64 PUT), no local git required. Returns the SHA of
// the last commit the API created. Used when CI has a token but no
// checkout.
func publishViaContents(meta report.SplitMeta, doc *report.Document, tsGz []byte) (string, error) {
	token, err := resolveDocsToken()
	if err != nil {
		return "", err
	}

	// Stage the tree in a temp dir so we reuse the one WriteTree code
	// path, then PUT each produced file.
	staging, err := os.MkdirTemp("", "celeris-contents-")
	if err != nil {
		return "", fmt.Errorf("temp staging dir: %w", err)
	}
	defer func() { _ = os.RemoveAll(staging) }()

	cell, err := report.WriteTree(filepath.Join(staging, "results"), doc, tsGz, meta)
	if err != nil {
		return "", fmt.Errorf("write tree: %w", err)
	}
	rel := cellRelPath(meta)
	fmt.Printf("Publishing %s → %s (contents API)...\n", rel, docsRepo)

	files := []string{report.SummaryFile, report.HistogramsFile, report.EnvFile}
	if tsGz != nil {
		files = append(files, report.TimeseriesFile)
	}
	var lastSHA string
	for _, f := range files {
		body, err := os.ReadFile(filepath.Join(cell, f))
		if err != nil {
			return "", fmt.Errorf("read staged %s: %w", f, err)
		}
		sha, err := putContents(token, rel+"/"+f, body, meta)
		if err != nil {
			return "", fmt.Errorf("PUT %s: %w", f, err)
		}
		lastSHA = sha
	}
	fmt.Println("Uploaded.")
	return lastSHA, nil
}

// putContents creates-or-updates a single file in the docs repo via the
// contents API and returns the resulting commit SHA. When the path
// already exists the existing blob SHA is fetched first (the API
// requires it for an update).
func putContents(token, repoPath string, body []byte, meta report.SplitMeta) (string, error) {
	apiPath := fmt.Sprintf("/repos/%s/contents/%s", docsRepo, repoPath)

	// Look up an existing blob SHA (ignore failure — absent means create).
	existingSHA := ""
	if out, err := ghAPI(token, "GET", apiPath, nil); err == nil {
		var resp struct {
			SHA string `json:"sha"`
		}
		if json.Unmarshal(out, &resp) == nil {
			existingSHA = resp.SHA
		}
	}

	payload := map[string]any{
		"message": fmt.Sprintf("bench(%s/%s/%s/%s): %s", meta.Version, meta.Date, meta.Arch, meta.RunID, filepath.Base(repoPath)),
		"content": base64.StdEncoding.EncodeToString(body),
	}
	if existingSHA != "" {
		payload["sha"] = existingSHA
	}
	in, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	out, err := ghAPI(token, "PUT", apiPath, in)
	if err != nil {
		return "", err
	}
	var resp struct {
		Commit struct {
			SHA string `json:"sha"`
		} `json:"commit"`
	}
	if err := json.Unmarshal(out, &resp); err != nil {
		return "", err
	}
	return resp.Commit.SHA, nil
}

// dispatchPointer fires the tiny repository_dispatch the docs workflow
// listens for. The payload is always ≤1 KB — a pointer to the pushed
// cell, never the results themselves.
func dispatchPointer(meta report.SplitMeta, commit string) error {
	token, err := resolveDocsToken()
	if err != nil {
		return err
	}
	payload := map[string]any{
		"event_type": benchmarkPublishedEvent,
		"client_payload": map[string]any{
			"version": meta.Version,
			"arch":    meta.Arch,
			"date":    meta.Date,
			"run_id":  meta.RunID,
			"path":    cellRelPath(meta),
			"commit":  commit,
		},
	}
	in, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := ghAPI(token, "POST", "/repos/"+docsRepo+"/dispatches", in); err != nil {
		return fmt.Errorf("gh api dispatch: %w", err)
	}
	fmt.Println("Dispatched benchmark-published pointer.")
	return nil
}

// cloneDocs shallow-clones the docs repo into dir using the token.
func cloneDocs(dir, token string) error {
	fmt.Printf("Cloning %s (shallow)...\n", docsRepo)
	return runGit("", "clone", "--depth", "1", authRemote(token), dir)
}

// pushDocsHEAD pushes HEAD to the docs branch, recovering from the
// non-fast-forward rejection that happens when the docs-sync workflow
// commits an index.json update concurrently with our publish.
// publishViaGit's commit only touches the results tree — never
// index.json, which the workflow owns — so rebasing our publish commit
// onto the freshly fetched remote head replays cleanly with no file
// conflict; we then retry the push. Bounded retries guard against a
// livelock if the remote keeps moving.
func pushDocsHEAD(repoDir, token string) error {
	const maxAttempts = 6
	remote := authRemote(token)
	var lastErr error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		if err := runGit(repoDir, "push", remote, "HEAD:"+docsBranch); err == nil {
			return nil
		} else {
			lastErr = err
		}
		// Rejected (the workflow moved the branch): re-base our publish commit
		// onto the new remote head and retry the push.
		if err := runGit(repoDir, "fetch", remote, docsBranch); err != nil {
			return fmt.Errorf("docs push: fetch before rebase (attempt %d/%d): %w", attempt, maxAttempts, err)
		}
		if err := runGit(repoDir, "rebase", "FETCH_HEAD"); err != nil {
			_ = runGit(repoDir, "rebase", "--abort")
			return fmt.Errorf("docs push: rebase onto remote head (attempt %d/%d): %w", attempt, maxAttempts, err)
		}
	}
	return fmt.Errorf("docs push: still rejected after %d rebase attempts (remote kept moving): %w", maxAttempts, lastErr)
}

// authRemote builds an https remote URL with the token embedded so git
// push/clone authenticates without an interactive prompt. The token is
// never printed (runGit logs only the subcommand, not the URL).
func authRemote(token string) string {
	return fmt.Sprintf("https://x-access-token:%s@github.com/%s.git", token, docsRepo)
}

// runGit runs a git subcommand in dir (cwd when dir==""). Output streams
// to the process stdio. The argv is logged with any token-bearing URL
// redacted so a CI log never leaks the credential.
func runGit(dir string, args ...string) error {
	cmd := exec.Command("git", args...)
	cmd.Dir = dir
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("git %s: %w", redactArgs(args), err)
	}
	return nil
}

// gitHeadSHA returns the HEAD commit SHA of the repo at dir.
func gitHeadSHA(dir string) (string, error) {
	cmd := exec.Command("git", "rev-parse", "HEAD")
	cmd.Dir = dir
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	return strings.TrimSpace(string(out)), nil
}

// redactArgs joins git argv for logging, masking any token-bearing
// https remote so credentials never reach a log.
func redactArgs(args []string) string {
	cp := make([]string, len(args))
	for i, a := range args {
		if strings.Contains(a, "x-access-token:") {
			cp[i] = "https://x-access-token:***@github.com/" + docsRepo + ".git"
			continue
		}
		cp[i] = a
	}
	return strings.Join(cp, " ")
}

// ghAPI invokes `gh api` with an explicit method + Authorization header
// and returns stdout. A nil body sends no input. stderr streams through
// so failures are visible.
func ghAPI(token, method, path string, body []byte) ([]byte, error) {
	args := []string{"api", "-X", method, path,
		"-H", "Accept: application/vnd.github+json",
		"-H", "Authorization: token " + token,
	}
	if body != nil {
		args = append(args, "--input", "-")
	}
	cmd := exec.Command("gh", args...)
	if body != nil {
		cmd.Stdin = bytes.NewReader(body)
	}
	cmd.Stderr = os.Stderr
	return cmd.Output()
}

// BenchAndValidate is the release-gate composition: Validate first
// (long-running invariant + property suite), then ValidateDiff when both
// arches ran, then a fresh Bench, then a single Publish. Failure at any
// gate short-circuits — a release that can't pass Validate has no
// business shipping a bench number, and a publish without a fresh bench
// is misleading.
//
// PublishValidate is gone as a separate dispatch: validation rides
// inside the Document the bench Publish ships, so one Publish covers
// both panels.
//
// Reuses every BENCH_*, VALIDATE_*, CELERIS_VERSION, CLUSTER_USE_LAN,
// PUBLISH_*, and DOCS_TOKEN knob from the underlying targets.
func BenchAndValidate() error {
	fmt.Println("=== BenchAndValidate: Validate ===")
	if err := Validate(); err != nil {
		return fmt.Errorf("validate: %w", err)
	}
	if envOrDefault("VALIDATE_TARGET", defaultClusterTarget) == "both" {
		fmt.Println("\n=== BenchAndValidate: ValidateDiff ===")
		if err := ValidateDiff(); err != nil {
			return fmt.Errorf("validate-diff: %w", err)
		}
	} else {
		fmt.Println("\n=== BenchAndValidate: ValidateDiff (skipped — VALIDATE_TARGET != both) ===")
	}
	fmt.Println("\n=== BenchAndValidate: Bench ===")
	if err := Bench(); err != nil {
		return fmt.Errorf("bench: %w", err)
	}
	fmt.Println("\n=== BenchAndValidate: Publish ===")
	if err := Publish(); err != nil {
		return fmt.Errorf("publish: %w", err)
	}
	fmt.Println("\n=== BenchAndValidate: complete ===")
	return nil
}

// publishPreflight checks, BEFORE a multi-hour bench runs, that a real docs
// publish CAN succeed: the version must resolve to a real tag/pseudo-version
// (not "dev") and the docs token must resolve (DOCS_TOKEN env or `gh auth
// token`). BenchTier calls this when BENCH_PUBLISH is on so a missing
// DOCS_DISPATCH_TOKEN secret / absent gh on the runner fails at minute 0
// instead of after the whole grid — the v1.5.5 run completed all 813 cells
// and then lost the publish because the token was unresolvable on the runner.
// A dry-run (PUBLISH_DRYRUN=1) neither clones nor pushes, so the token check
// is skipped there.
func publishPreflight() error {
	version := os.Getenv("PUBLISH_VERSION")
	if version == "" {
		v, _ := celerisVersion()
		version = v
	}
	if version == "" || version == "dev" {
		return fmt.Errorf("publish version resolves to %q: set PUBLISH_VERSION, or pin celeris in servers/celeris/go.mod, so the docs land under the real version instead of \"dev\"", version)
	}
	if os.Getenv("PUBLISH_DRYRUN") == "1" {
		return nil
	}
	if _, err := resolveDocsToken(); err != nil {
		return fmt.Errorf("docs token unresolved: set the DOCS_DISPATCH_TOKEN secret (a PAT with repo scope on %s) / DOCS_TOKEN env, or install gh on the runner: %w", docsRepo, err)
	}
	return nil
}

// resolveDocsToken returns the token used for the docs push + dispatch.
// DOCS_TOKEN env wins; falls back to `gh auth token`. We never log the
// token so a stray CI log dump doesn't leak credentials.
func resolveDocsToken() (string, error) {
	if t := os.Getenv("DOCS_TOKEN"); t != "" {
		return t, nil
	}
	out, err := exec.Command("gh", "auth", "token").Output()
	if err != nil {
		return "", fmt.Errorf("gh auth token (set DOCS_TOKEN env or run `gh auth login`): %w", err)
	}
	tok := strings.TrimSpace(string(out))
	if tok == "" {
		return "", fmt.Errorf("gh auth token returned empty (set DOCS_TOKEN env or run `gh auth login`)")
	}
	return tok, nil
}
