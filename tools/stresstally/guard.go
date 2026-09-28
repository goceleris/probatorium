package main

import (
	"bufio"
	"cmp"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The cluster guard. Every workflow that runs on the cluster holds the
// matrix-tier-cluster concurrency group, which GitHub lets hold ONE running
// run and ONE pending run: a third run queued into the group cancels the
// pending one. A stress run on the cluster joins the group from its `cluster`
// job, so the moment that job is created it would evict whatever was
// pending, a release tier included. The guard runs just before, outside the
// group, and refuses the run if anything is pending there.
//
// It reads what `gh api` returned (the workflow step lists every run that is
// not completed, and the jobs of every other stress run among them) and
// classifies each run:
//
//   - a run of a workflow in clusterGroupWorkflows: in_progress holds the
//     group; any other open status (queued, pending, waiting, requested,
//     action_required) is counted as pending. A queued run may already hold
//     the group; counting it as pending can only make the guard refuse more,
//     never evict.
//   - another run of this workflow: a pull_request self-test or a run whose
//     title names the github target never joins the group. Otherwise its jobs
//     decide: its guard not finished yet = racing us = pending; its guard
//     refused or skipped = never joins; guard passed and a job of its cluster
//     call started = holder; guard passed and none started = pending.
//
// Any pending run refuses this one. The decision, every run and why, is
// printed and written to decision.txt next to the inputs.

// clusterGroupWorkflows are the workflow files that declare the
// matrix-tier-cluster group themselves (matrix-pr-tier only for a labelled
// same-repository pull request; counted anyway). celeris-stress.yml is not
// listed: its runs join from a job and are classified by their jobs.
// TestStressClusterGuardKnowsEveryClusterWorkflow keeps this list equal to
// the files that name the group.
var clusterGroupWorkflows = []string{
	".github/workflows/benchmark-tier.yml",
	".github/workflows/matrix-checkptr-tier.yml",
	".github/workflows/matrix-nightly-tier.yml",
	".github/workflows/matrix-pr-tier.yml",
	".github/workflows/matrix-race-tier.yml",
	".github/workflows/matrix-weekend-tier.yml",
}

const (
	stressWorkflowPath = ".github/workflows/celeris-stress.yml"
	// The caller jobs' names in celeris-stress.yml. GitHub names a called
	// workflow's jobs "<caller job name> / <job name>".
	guardJobName      = "cluster-guard"
	clusterCallPrefix = "cluster / "
)

// ghRun is the part of a workflow run (GET /repos/{o}/{r}/actions/runs) the
// guard reads.
type ghRun struct {
	ID           int64  `json:"id"`
	Name         string `json:"name"`
	DisplayTitle string `json:"display_title"`
	Path         string `json:"path"`
	Event        string `json:"event"`
	Status       string `json:"status"`
	CreatedAt    string `json:"created_at"`
}

// ghJob is the part of a job (GET .../runs/{id}/jobs) the guard reads.
type ghJob struct {
	Name       string `json:"name"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}

const (
	classHolder  = "holder"
	classPending = "pending"
	classIgnored = "ignored"
)

// runClass is one run as the guard judged it.
type runClass struct {
	Run   ghRun
	Class string
	Why   string
}

// githubTitleRe is the run-name celeris-stress.yml gives a github dispatch.
var githubTitleRe = regexp.MustCompile(`^Celeris Stress \[github/`)

// classifyRuns judges every open run but self.
func classifyRuns(runs []ghRun, jobs map[int64][]ghJob, self int64) []runClass {
	var out []runClass
	seen := map[int64]bool{}
	for _, r := range runs {
		if r.ID == self || seen[r.ID] || r.Status == "completed" {
			continue
		}
		seen[r.ID] = true
		switch {
		case slices.Contains(clusterGroupWorkflows, r.Path):
			if r.Status == "in_progress" {
				out = append(out, runClass{r, classHolder, "a cluster workflow in progress holds the group"})
			} else {
				out = append(out, runClass{r, classPending, "a cluster workflow in status " + r.Status + " waits in the group (or is about to)"})
			}
		case r.Path == stressWorkflowPath:
			out = append(out, classifyStressRun(r, jobs[r.ID]))
		}
	}
	slices.SortFunc(out, func(a, b runClass) int { return cmp.Compare(a.Run.ID, b.Run.ID) })
	return out
}

func classifyStressRun(r ghRun, jobs []ghJob) runClass {
	if r.Event == "pull_request" {
		return runClass{r, classIgnored, "a pull_request self-test never reaches the cluster"}
	}
	if githubTitleRe.MatchString(r.DisplayTitle) {
		return runClass{r, classIgnored, "a github-target dispatch never joins the group"}
	}
	var guard *ghJob
	started := false
	for i, j := range jobs {
		if j.Name == guardJobName {
			guard = &jobs[i]
		}
		if strings.HasPrefix(j.Name, clusterCallPrefix) && (j.Status == "in_progress" || j.Status == "completed") {
			started = true
		}
	}
	switch {
	case guard == nil || guard.Status != "completed":
		return runClass{r, classPending, "another stress run whose guard has not finished: it may be entering the group now"}
	case guard.Conclusion != "success":
		return runClass{r, classIgnored, "its guard concluded " + guard.Conclusion + ": it never joins the group"}
	case started:
		return runClass{r, classHolder, "a stress run whose cluster jobs have started holds the group"}
	default:
		return runClass{r, classPending, "a stress run past its guard whose cluster jobs have not started waits in the group"}
	}
}

// guardDecision allows the run only when nothing is pending in the group.
func guardDecision(classes []runClass) (bool, string) {
	var pending, holders []string
	for _, c := range classes {
		desc := fmt.Sprintf("run %d (%s, %s)", c.Run.ID, c.Run.Name, c.Run.Status)
		switch c.Class {
		case classPending:
			pending = append(pending, desc)
		case classHolder:
			holders = append(holders, desc)
		}
	}
	if len(pending) > 0 {
		return false, fmt.Sprintf("refusing: %s pending in matrix-tier-cluster; entering the group now would cancel it. "+
			"Dispatch again once the pending slot is free", strings.Join(pending, ", "))
	}
	if len(holders) > 0 {
		return true, fmt.Sprintf("allowed: nothing pending; %s holds the group, so this run will wait as the pending one "+
			"(a later tier dispatch cancels THIS run, never the other way round)", strings.Join(holders, ", "))
	}
	return true, "allowed: matrix-tier-cluster is empty"
}

// readJSONL reads one JSON object per line (what `gh api --jq '.x[]'` prints).
func readJSONL[T any](path string) ([]T, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	var out []T
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 16<<20)
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var v T
		if err := json.Unmarshal([]byte(line), &v); err != nil {
			return nil, fmt.Errorf("%s:%d: %v", path, n, err)
		}
		out = append(out, v)
	}
	return out, sc.Err()
}

// cmdGuard reads runs-*.jsonl and jobs-<id>.jsonl from -dir and decides.
// Exit 0 allows, 1 refuses, 2 cannot tell (which refuses too: fail closed).
func cmdGuard(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("guard", flag.ContinueOnError)
	dir := fs.String("dir", "guard", "directory holding runs-*.jsonl and jobs-<run id>.jsonl")
	self := fs.Int64("self", 0, "this run's id")
	if err := fs.Parse(args); err != nil || *self <= 0 {
		say(stdout, "::error::usage: stresstally guard -dir DIR -self RUN_ID\n")
		return 2
	}
	files, _ := filepath.Glob(filepath.Join(*dir, "runs-*.jsonl"))
	if len(files) == 0 {
		say(stdout, "::error::no runs-*.jsonl under %s: cannot tell whether a run is pending in matrix-tier-cluster; refusing\n", *dir)
		return 2
	}
	var runs []ghRun
	for _, f := range files {
		rs, err := readJSONL[ghRun](f)
		if err != nil {
			say(stdout, "::error::%v; refusing\n", err)
			return 2
		}
		runs = append(runs, rs...)
	}
	jobs := map[int64][]ghJob{}
	jobFiles, _ := filepath.Glob(filepath.Join(*dir, "jobs-*.jsonl"))
	for _, f := range jobFiles {
		id, err := strconv.ParseInt(strings.TrimSuffix(strings.TrimPrefix(filepath.Base(f), "jobs-"), ".jsonl"), 10, 64)
		if err != nil {
			continue
		}
		js, err := readJSONL[ghJob](f)
		if err != nil {
			say(stdout, "::error::%v; refusing\n", err)
			return 2
		}
		jobs[id] = js
	}
	classes := classifyRuns(runs, jobs, *self)
	ok, msg := guardDecision(classes)
	var b strings.Builder
	say(&b, "matrix-tier-cluster guard for run %d: %d open run(s) listed\n", *self, len(runs))
	for _, c := range classes {
		say(&b, "  %-8s run %d %s [%s, created %s] %q: %s\n", c.Class, c.Run.ID, c.Run.Path, c.Run.Status, c.Run.CreatedAt, c.Run.DisplayTitle, c.Why)
	}
	say(&b, "%s\n", msg)
	say(stdout, "%s", b.String())
	if err := os.WriteFile(filepath.Join(*dir, "decision.txt"), []byte(b.String()), 0o644); err != nil {
		say(stdout, "::error::write decision.txt: %v; refusing\n", err)
		return 2
	}
	if !ok {
		say(stdout, "::error::%s\n", msg)
		return 1
	}
	return 0
}
