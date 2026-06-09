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

// defaultRunID is the canonical run for a date. Phase 5's back-to-back
// loop (BenchTier, #167) overrides this per pass via PUBLISH_RUN_ID
// (run-1..run-N); absent that env, every publish is run-1.
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
func Publish() error {
	meta, doc, tsGz, err := loadPublishInputs()
	if err != nil {
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

// loadPublishInputs resolves the run metadata and reads the newest bench
// results.json + its timeseries sidecar. Shared by every publish path.
func loadPublishInputs() (report.SplitMeta, *report.Document, []byte, error) {
	version := os.Getenv("PUBLISH_VERSION")
	if version == "" {
		v, err := celerisVersion()
		if err != nil {
			return report.SplitMeta{}, nil, nil, err
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
			return report.SplitMeta{}, nil, nil, fmt.Errorf("no bench results to publish: %w", err)
		}
	}

	data, err := os.ReadFile(resultsPath)
	if err != nil {
		return report.SplitMeta{}, nil, nil, fmt.Errorf("read %s: %w", resultsPath, err)
	}
	var doc report.Document
	if err := json.Unmarshal(data, &doc); err != nil {
		return report.SplitMeta{}, nil, nil, fmt.Errorf("parse %s: %w", resultsPath, err)
	}

	// timeseries.json.gz lives next to results.json (both emit paths
	// write it there). Absent is fine — older runs predate the sidecar.
	var tsGz []byte
	tsPath := filepath.Join(filepath.Dir(resultsPath), report.TimeseriesFile)
	if b, err := os.ReadFile(tsPath); err == nil {
		tsGz = b
	}

	now := time.Now().UTC()
	// BENCH_START_DATE (if set) pins every Publish in a back-to-back
	// iteration to the bench's start date, so a run that crosses
	// midnight UTC lands all cells under the same date. Falls back
	// to the per-Publish timestamp otherwise (the legacy behaviour
	// for a one-shot `mage Publish` invocation).
	dateStr := os.Getenv("BENCH_START_DATE")
	if dateStr == "" {
		dateStr = now.Format("20060102")
	}
	meta := report.SplitMeta{
		Version:        version,
		Arch:           archTag(benchTargetGOARCH()),
		Date:           dateStr,
		RunID:          envOrDefault("PUBLISH_RUN_ID", defaultRunID),
		GitSHA:         gitRefOr(),
		GitRef:         os.Getenv("GITHUB_REF"),
		CelerisVersion: version,
		LoadgenVersion: goModRequireVersion("github.com/goceleris/loadgen"),
		GeneratedAt:    now,
	}
	return meta, &doc, tsGz, nil
}

// cellRelPath is the repo-root-relative path of a published cell, the
// value the pointer dispatch and the contents API both key on. It mirrors
// WriteTree's on-disk layout via the shared report.CellRelDir helper so a
// back-to-back run-K never overwrites run-1's flat tree.
func cellRelPath(meta report.SplitMeta) string {
	return filepath.ToSlash(filepath.Join("results", report.CellRelDir(meta)))
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
// commits an index.json update between two back-to-back publishes that
// reuse one DOCS_REPO_DIR checkout (every BenchTier run: run-K saturation
// then run-K rated, then the next run). publishViaGit's commit only touches
// the run-K results tree — never index.json, which the workflow owns — so
// rebasing our publish commit onto the freshly fetched remote head replays
// cleanly with no file conflict; we then retry the push. Bounded retries
// guard against a livelock if the remote keeps moving.
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
