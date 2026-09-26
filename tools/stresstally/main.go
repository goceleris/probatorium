// Command stresstally plans and judges the celeris stress workflow
// (.github/workflows/celeris-stress.yml), which runs celeris tests many times
// on GitHub-hosted x86 and arm64 runners to measure flakes and their rates.
//
//	stresstally plan        validate the dispatch inputs and emit the matrix
//	stresstally summarize   judge every case of a run from its shard logs
//	stresstally tally DIR   judge a directory of shard logs (local use)
//
// plan reads STRESS_EVENT, STRESS_RUN_ID and the dispatch inputs from
// IN_CELERIS_REF, IN_PACKAGES, IN_RUN, IN_COUNT, IN_SHARDS, IN_ARCHES,
// IN_MEMLOCK, IN_RACE, IN_TIMEOUT and IN_EXTRA, and writes the outputs
// `plan`, `matrix` and `celeris_ref` to $GITHUB_OUTPUT. Inputs arrive through
// the environment, never through the workflow's expression syntax.
//
// summarize reads the plan from STRESS_PLAN and the celeris commit every
// shard must have tested from STRESS_CELERIS_SHA, reads <case>__<arch>__<n>.log
// for every expected shard under -logs, and writes summary.md, report.json
// and tests.tsv to -out. It exits 0 only if every case came out as its plan
// expects: for a dispatch run, that the one case PASSed.
//
// The tally counts ONLY `--- PASS: `, `--- FAIL: ` and `--- SKIP: ` lines
// (subtests included). A SKIP is never a pass. A shard whose log has no
// verdict, a panic, a timeout, no trailer, or a test that ran without a
// verdict is UNPARSED, a shard with no log is MISSING, and a shard that ran
// in another shape than the one asked for is WRONG-SHAPE; each is its own
// class and fails the case.
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}
	var code int
	switch os.Args[1] {
	case "plan":
		code = cmdPlan(os.Stdout, os.Getenv)
	case "summarize":
		code = cmdSummarize(os.Args[2:], os.Stdout, os.Getenv)
	case "tally":
		code = cmdTally(os.Args[2:], os.Stdout)
	default:
		usage()
	}
	os.Exit(code)
}

func usage() {
	fmt.Fprintln(os.Stderr, "usage: stresstally plan | summarize -logs DIR -out DIR | tally [-case NAME] DIR")
	os.Exit(2)
}

// cmdPlan validates the inputs and writes the workflow outputs.
func cmdPlan(stdout io.Writer, getenv func(string) string) int {
	var plan Plan
	switch ev := getenv("STRESS_EVENT"); ev {
	case "workflow_dispatch":
		p, err := planDispatch(Inputs{
			CelerisRef: getenv("IN_CELERIS_REF"),
			Packages:   getenv("IN_PACKAGES"),
			Run:        getenv("IN_RUN"),
			Count:      getenv("IN_COUNT"),
			Shards:     getenv("IN_SHARDS"),
			Arches:     getenv("IN_ARCHES"),
			Memlock:    getenv("IN_MEMLOCK"),
			Race:       getenv("IN_RACE"),
			Timeout:    getenv("IN_TIMEOUT"),
			Extra:      getenv("IN_EXTRA"),
		})
		if err != nil {
			for _, line := range strings.Split(err.Error(), "\n") {
				say(stdout, "::error::%s\n", line)
			}
			return 2
		}
		plan = p
	case "pull_request":
		plan = planSelfTest()
	default:
		say(stdout, "::error::stresstally plan: event %q is not supported (workflow_dispatch or pull_request)\n", ev)
		return 2
	}

	var runID int64
	if s := getenv("STRESS_RUN_ID"); s != "" {
		n, err := strconv.ParseInt(s, 10, 64)
		if err != nil || n < 0 || n > 1<<50 {
			say(stdout, "::error::STRESS_RUN_ID %q is not a run id\n", s)
			return 2
		}
		runID = n
	}
	entries := plan.Entries(runID)
	planJSON, err := json.Marshal(plan)
	if err != nil {
		say(stdout, "::error::%v\n", err)
		return 2
	}
	matrixJSON, err := json.Marshal(map[string][]Entry{"include": entries})
	if err != nil {
		say(stdout, "::error::%v\n", err)
		return 2
	}

	say(stdout, "event %s; celeris ref %s; %d case(s), %d shard job(s)\n", plan.Event, plan.CelerisRef, len(plan.Cases), len(entries))
	for _, c := range plan.Cases {
		say(stdout, "  case %s: packages %q run %q count %d shards %d arches %v memlock %s race %t timeout %s flags %q env %q shuffle %q; expect %s\n",
			c.Name, strings.Join(c.Packages, " "), c.Run, c.Count, c.Shards, c.Arches, c.Memlock, c.Race, c.Timeout,
			strings.Join(c.Flags, " "), strings.Join(c.Env, " "), c.Shuffle, c.Expect.Verdict)
	}

	out := getenv("GITHUB_OUTPUT")
	if out == "" {
		say(stdout, "plan=%s\nmatrix=%s\nceleris_ref=%s\n", planJSON, matrixJSON, plan.CelerisRef)
		return 0
	}
	f, err := os.OpenFile(out, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		say(stdout, "::error::open GITHUB_OUTPUT: %v\n", err)
		return 2
	}
	// Single-line values only: JSON never contains a raw newline, and the
	// validated ref cannot contain one.
	_, err = fmt.Fprintf(f, "plan=%s\nmatrix=%s\nceleris_ref=%s\n", planJSON, matrixJSON, plan.CelerisRef)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err != nil {
		say(stdout, "::error::write GITHUB_OUTPUT: %v\n", err)
		return 2
	}
	return 0
}

// cmdSummarize judges every case of the plan and writes the reports.
func cmdSummarize(args []string, stdout io.Writer, getenv func(string) string) int {
	fs := flag.NewFlagSet("summarize", flag.ContinueOnError)
	logs := fs.String("logs", "logs", "directory holding every shard log")
	outDir := fs.String("out", "out", "directory for summary.md, report.json and tests.tsv")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	var plan Plan
	if err := json.Unmarshal([]byte(getenv("STRESS_PLAN")), &plan); err != nil || len(plan.Cases) == 0 {
		say(stdout, "::error::STRESS_PLAN is not a plan with at least one case (%v)\n", err)
		return 2
	}
	sha := getenv("STRESS_CELERIS_SHA")
	reports := make([]CaseReport, 0, len(plan.Cases))
	for _, c := range plan.Cases {
		reports = append(reports, judgeCase(c, *logs, sha))
	}
	if err := writeReports(*outDir, plan, sha, reports); err != nil {
		say(stdout, "::error::%v\n", err)
		return 2
	}
	return verdictText(stdout, plan, reports)
}

// cmdTally judges a directory of logs without a plan: every log in it is a
// shard, grouped into cases by file name. Nothing can be MISSING and nothing
// is expected beyond PASS; it is for looking at logs by hand.
func cmdTally(args []string, stdout io.Writer) int {
	fs := flag.NewFlagSet("tally", flag.ContinueOnError)
	only := fs.String("case", "", "judge only this case")
	outDir := fs.String("out", "", "also write summary.md, report.json and tests.tsv here")
	if err := fs.Parse(args); err != nil || fs.NArg() != 1 {
		say(stdout, "%v\n", "usage: stresstally tally [-case NAME] [-out DIR] DIR")
		return 2
	}
	dir := fs.Arg(0)
	// Logs may sit flat in dir or one artifact directory deep, the way
	// `gh run download -p 'stress-log-*'` leaves them.
	byFile := map[string]string{}
	var paths []string
	err := filepath.WalkDir(dir, func(p string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if ok, _ := filepath.Match("*__*__*.log", d.Name()); ok && !d.IsDir() {
			if _, dup := byFile[d.Name()]; !dup {
				byFile[d.Name()] = p
				paths = append(paths, p)
			}
		}
		return nil
	})
	if err != nil || len(paths) == 0 {
		say(stdout, "no <case>__<arch>__<shard>.log files under %s (%v)\n", dir, err)
		return 2
	}
	plan := Plan{Event: "local"}
	byName := map[string]*Case{}
	var names []string
	for _, p := range paths {
		parts := strings.Split(strings.TrimSuffix(filepath.Base(p), ".log"), "__")
		n, err := strconv.Atoi(parts[len(parts)-1])
		if len(parts) != 3 || err != nil || (*only != "" && parts[0] != *only) {
			continue
		}
		h := parseShardFile(p).header
		c := byName[parts[0]]
		if c == nil {
			count, _ := strconv.Atoi(h["count"])
			c = &Case{Name: parts[0], Packages: strings.Fields(h["packages"]), Run: h["run"], Count: count,
				Memlock: memlockLabel(h["memlock_limit"]), Race: h["race"] == "true", Timeout: h["timeout"],
				Flags: strings.Fields(h["flags"]), Env: strings.Fields(h["env"]), Expect: Expect{Verdict: "PASS"}}
			if c.Flags == nil {
				c.Flags = []string{}
			}
			if c.Env == nil {
				c.Env = []string{}
			}
			byName[parts[0]] = c
			names = append(names, parts[0])
		}
		if !slices.Contains(c.Arches, parts[1]) {
			c.Arches = append(c.Arches, parts[1])
		}
		c.Shards = max(c.Shards, n)
	}
	slices.Sort(names)
	for _, n := range names {
		plan.Cases = append(plan.Cases, *byName[n])
	}
	var reports []CaseReport
	for _, c := range plan.Cases {
		reports = append(reports, judgeCaseFrom(c, func(name string) string {
			if p, ok := byFile[name]; ok {
				return p
			}
			return filepath.Join(dir, name)
		}, ""))
	}
	if *outDir != "" {
		if err := writeReports(*outDir, plan, "", reports); err != nil {
			say(stdout, "%v\n", err)
			return 2
		}
	}
	return verdictText(stdout, plan, reports)
}

func memlockLabel(limit string) string {
	for k, v := range memlocks {
		if v == limit {
			return k
		}
	}
	return limit
}

func writeReports(dir string, plan Plan, sha string, reports []CaseReport) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	var md strings.Builder
	ok := 0
	for _, r := range reports {
		if r.OK() {
			ok++
		}
	}
	say(&md, "# celeris stress: %d of %d case(s) as expected\n\n", ok, len(reports))
	if plan.Event == "pull_request" {
		md.WriteString("This pull request run is the workflow's self-test: each case has a fixed configuration and a fixed expected outcome, " +
			"including the cases that must FAIL. The run is green only when every case comes out exactly as expected.\n\n")
	}
	say(&md, "celeris ref `%s`, commit `%s`.\n\n", inline(plan.CelerisRef), inline(sha))
	for _, r := range reports {
		r.Markdown(&md)
	}
	if err := os.WriteFile(filepath.Join(dir, "summary.md"), []byte(md.String()), 0o644); err != nil {
		return err
	}
	j, err := json.MarshalIndent(map[string]any{"plan": plan, "celeris_sha": sha, "cases": reports}, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(dir, "report.json"), j, 0o644); err != nil {
		return err
	}
	var tsv strings.Builder
	tsv.WriteString(tsvHeader)
	for _, r := range reports {
		r.TSV(&tsv)
	}
	return os.WriteFile(filepath.Join(dir, "tests.tsv"), []byte(tsv.String()), 0o644)
}

// verdictText prints every case and returns the exit status: 0 only if
// every case came out as expected.
func verdictText(w io.Writer, plan Plan, reports []CaseReport) int {
	bad := 0
	for _, r := range reports {
		r.Text(w)
		if !r.OK() {
			bad++
			say(w, "::error::case %s: %s\n", r.Case, strings.Join(r.Mismatches, "; "))
		}
	}
	say(w, "stress summary: %d of %d case(s) as expected\n", len(reports)-bad, len(reports))
	if bad > 0 {
		return 1
	}
	return 0
}
