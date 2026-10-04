package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// A base-vs-branch comparison is two dispatches that differ in the celeris
// commit and in nothing else, down to the probatorium commit the workflow and
// this tool ran from. compare checks that from the two runs' own reports,
// then sets each test's per-process counts side by side, per arch, with
// Fisher's exact test on them.

// runReport is report.json as summarize and tally write it.
type runReport struct {
	Plan       Plan         `json:"plan"`
	CelerisSHA string       `json:"celeris_sha"`
	Cases      []CaseReport `json:"cases"`
}

// readReport reads a report.json, or the one in a directory (what
// `gh run download -n stress-summary -D DIR` leaves).
func readReport(path string) (runReport, error) {
	var r runReport
	if st, err := os.Stat(path); err == nil && st.IsDir() {
		path = filepath.Join(path, "report.json")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("%s: %v", path, err)
	}
	if len(r.Cases) != 1 {
		return r, fmt.Errorf("%s holds %d cases; compare takes the report of a dispatch, which has one", path, len(r.Cases))
	}
	// Without it, two arms that ran different workflows or tallies would
	// look alike; fail closed, as for any other part of the configuration.
	if r.Plan.ProbatoriumSHA == "" {
		return r, fmt.Errorf("%s records no probatorium commit (plan.probatorium_sha), so compare cannot tell whether both arms ran the same workflow and tool; "+
			"it takes the stress-summary of a run of this workflow", path)
	}
	return r, nil
}

// configDiffs lists every way the two arms differ, apart from the celeris
// commit, which is what a comparison varies: the probatorium commit the run
// came from (a change to the workflow or the tally between two dispatches is
// a second cause), then every input.
func configDiffs(base, branch runReport) []string {
	a, b := base.Cases[0].Config, branch.Cases[0].Config
	var d []string
	diff := func(what string, x, y any) {
		xs, ys := fmt.Sprint(x), fmt.Sprint(y)
		if xs != ys {
			d = append(d, fmt.Sprintf("%s: base %s, branch %s", what, xs, ys))
		}
	}
	diff("probatorium commit", base.Plan.ProbatoriumSHA, branch.Plan.ProbatoriumSHA)
	// Where and how the arms ran: a GitHub-hosted arm against a cluster arm,
	// or a stress arm against a timing arm, differs in machine and shape.
	diff("target", orDefault(a.Target, "github"), orDefault(b.Target, "github"))
	diff("mode", orDefault(a.Mode, "stress"), orDefault(b.Mode, "stress"))
	diff("cpus", a.CPUs, b.CPUs)
	diff("pmu", strconv.Quote(a.PMU), strconv.Quote(b.PMU))
	// By layout-free name: a pattern names the same package before and after
	// celeris#443's move, and shard.sh runs it where each arm's commit keeps it.
	diff("packages", layoutFreeAll(a.Packages), layoutFreeAll(b.Packages))
	diff("run", strconv.Quote(a.Run), strconv.Quote(b.Run))
	diff("count", a.Count, b.Count)
	diff("shards", a.Shards, b.Shards)
	diff("arches", a.Arches, b.Arches)
	diff("memlock", a.Memlock, b.Memlock)
	diff("race", a.Race, b.Race)
	diff("timeout", a.Timeout, b.Timeout)
	diff("flags", a.Flags, b.Flags)
	diff("env", a.Env, b.Env)
	diff("shuffle", strconv.Quote(a.Shuffle), strconv.Quote(b.Shuffle))
	return d
}

// celerisModule is the import path prefix of every celeris package.
const celerisModule = "github.com/goceleris/celeris/"

// layoutFree is a celeris package's name with celeris#443's move undone, so a
// package keys alike on both sides of it: a leading internal/ is dropped, and
// driver/X/internal/protocol reads as driver/X/protocol. It takes an import
// path (github.com/goceleris/celeris/internal/engine/iouring) or a pattern
// (./internal/engine/iouring, ./internal/engine/...) and leaves anything else
// as it is. Neither 28383e8 (before the move) nor ee8ae8b (the move) has both a
// package P and internal/P, but nothing keeps a later commit from adding one,
// so cmdCompare checks that no arm has two packages with one layout-free name
// before it joins rows by it (pkgpaths.sh probes the same candidates, the
// other way round).
func layoutFree(p string) string {
	var prefix string
	switch {
	case strings.HasPrefix(p, celerisModule):
		prefix = celerisModule
	case strings.HasPrefix(p, "./"):
		prefix = "./"
	default:
		return p
	}
	rest := strings.TrimPrefix(strings.TrimPrefix(p, prefix), "internal/")
	if m := driverInternalProtocolRe.FindStringSubmatch(rest); m != nil {
		rest = "driver/" + m[1] + "/protocol" + m[2]
	}
	return prefix + rest
}

var driverInternalProtocolRe = regexp.MustCompile(`^driver/([^/]+)/internal/protocol(/.*)?$`)

// layoutClash reports two different packages among tests that have one
// layout-free name, in sorted order.
func layoutClash(tests []TestRow) (string, string, bool) {
	seen := map[string]string{}
	for _, t := range tests {
		lf := layoutFree(t.Package)
		if q, ok := seen[lf]; ok && q != t.Package {
			return min(q, t.Package), max(q, t.Package), true
		}
		seen[lf] = t.Package
	}
	return "", "", false
}

func layoutFreeAll(ps []string) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = layoutFree(p)
	}
	return out
}

func orDefault(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

// CompareRow is one test on one arch in both arms.
type CompareRow struct {
	Package, Name, Arch string
	Base, Branch        TestRow
	InBase, InBranch    bool
	P                   *float64 // Fisher's exact, two-sided, on failed/ran processes; nil unless both arms ran it
}

// Comparison is the judged pair.
type Comparison struct {
	BaseSHA, BranchSHA string
	ProbatoriumSHA     string // the same in both arms
	Config             Case
	Notes              []string
	Rows               []CompareRow
}

// compareReports refuses a pair that is not a comparison of two commits under
// one configuration, and otherwise lines the arms up.
func compareReports(base, branch runReport) (Comparison, error) {
	a, b := base.Cases[0], branch.Cases[0]
	if d := configDiffs(base, branch); len(d) > 0 {
		return Comparison{}, errors.New("the two runs differ in more than the celeris commit, so a difference between them has more than one cause:\n  " +
			strings.Join(d, "\n  "))
	}
	cmp := Comparison{BaseSHA: a.CelerisSHA, BranchSHA: b.CelerisSHA, ProbatoriumSHA: base.Plan.ProbatoriumSHA, Config: a.Config, Notes: []string{}}
	note := func(format string, x ...any) { cmp.Notes = append(cmp.Notes, fmt.Sprintf(format, x...)) }
	if pa, pb := strings.Join(a.Config.Packages, " "), strings.Join(b.Config.Packages, " "); pa != pb {
		note("the arms name their packages differently (base %q, branch %q): the same packages under celeris#443's two layouts", pa, pb)
	}
	if a.CelerisSHA == b.CelerisSHA {
		note("both arms tested celeris %s: this is an A/A comparison, a control, not base against branch", a.CelerisSHA)
	}
	for _, arm := range []struct {
		name string
		r    CaseReport
	}{{"base", a}, {"branch", b}} {
		for _, t := range arm.r.Arches {
			if n := t.Shards - t.Complete; n > 0 {
				note("%s: %d of %d %s shard(s) did not complete (UNPARSED %d, MISSING %d, WRONG-SHAPE %d); their processes are fewer or missing",
					arm.name, n, t.Shards, t.Arch, t.Unparsed, t.Missing, t.WrongShape)
			}
		}
		for _, w := range arm.r.Warnings {
			note("%s: %s", arm.name, w)
		}
	}
	for _, ta := range a.Arches {
		for _, tb := range b.Arches {
			if ta.Arch != tb.Arch {
				continue
			}
			ka, kb := slices.Sorted(slices.Values(ta.Kernels)), slices.Sorted(slices.Values(tb.Kernels))
			if !slices.Equal(ka, kb) {
				note("%s ran on kernel %s in the base arm and %s in the branch arm: a difference there may be the kernel's",
					ta.Arch, strings.Join(ka, ", "), strings.Join(kb, ", "))
			}
			if ra, rb := releases(ta.Images), releases(tb.Images); !slices.Equal(ra, rb) {
				note("%s ran on OS image %s in the base arm and %s in the branch arm: a difference there may be the image's",
					ta.Arch, strings.Join(ta.Images, ", "), strings.Join(tb.Images, ", "))
			}
		}
	}

	// Rows join by layout-free package: an arm at a commit after celeris#443's
	// move runs internal/engine/iouring's tests, which are engine/iouring's.
	// An arm with two packages of one layout-free name (a commit with both P
	// and internal/P) would merge two packages' tests, so then rows join by
	// package as named.
	joinKey := layoutFree
	for _, arm := range []struct {
		name string
		r    CaseReport
	}{{"base", a}, {"branch", b}} {
		if p, q, ok := layoutClash(arm.r.Tests); ok {
			note("%s: packages %s and %s share the name %s once celeris#443's move is undone, so rows join by package as named: a package that moved between the arms is in one arm only",
				arm.name, shortPkg(p), shortPkg(q), shortPkg(layoutFree(p)))
			joinKey = func(p string) string { return p }
			break
		}
	}
	type key struct{ pkg, name, arch string }
	rows := map[key]*CompareRow{}
	var order []key
	moved := map[string]string{}
	add := func(t TestRow, isBase bool) {
		k := key{joinKey(t.Package), t.Name, t.Arch}
		r := rows[k]
		if r == nil {
			r = &CompareRow{Package: t.Package, Name: t.Name, Arch: t.Arch}
			rows[k] = r
			order = append(order, k)
		}
		if isBase {
			r.Base, r.InBase = t, true
		} else {
			r.Branch, r.InBranch = t, true
		}
		if r.InBase && r.InBranch && r.Base.Package != r.Branch.Package {
			moved[r.Base.Package] = r.Branch.Package
		}
	}
	for _, t := range a.Tests {
		add(t, true)
	}
	for _, t := range b.Tests {
		add(t, false)
	}
	for _, p := range slices.Sorted(maps.Keys(moved)) {
		note("package %s in the base arm is %s in the branch arm (celeris#443's move); their tests are compared as one package",
			shortPkg(p), shortPkg(moved[p]))
	}
	slices.SortFunc(order, func(x, y key) int {
		return cmpStrings(x.pkg, y.pkg, x.name, y.name, archOrder(x.arch), archOrder(y.arch))
	})
	for _, k := range order {
		r := rows[k]
		if r.Base.Processes > 0 && r.Branch.Processes > 0 {
			p := fisherTwoSided(r.Base.FailedProcesses, r.Base.Processes-r.Base.FailedProcesses,
				r.Branch.FailedProcesses, r.Branch.Processes-r.Branch.FailedProcesses)
			r.P = &p
		}
		cmp.Rows = append(cmp.Rows, *r)
	}
	return cmp, nil
}

// Text prints the comparison: the configuration both arms share, what
// qualifies it, and every test per arch.
func (c Comparison) Text(w io.Writer) {
	cfg := c.Config
	say(w, "base celeris %s vs branch celeris %s\n", c.BaseSHA, c.BranchSHA)
	say(w, "both: probatorium %s packages %q run %q count %d shards %d arches %v memlock %s race %t timeout %s flags %q env %q\n",
		c.ProbatoriumSHA, strings.Join(cfg.Packages, " "), cfg.Run, cfg.Count, cfg.Shards, cfg.Arches, cfg.Memlock, cfg.Race, cfg.Timeout,
		strings.Join(cfg.Flags, " "), strings.Join(cfg.Env, " "))
	for _, n := range c.Notes {
		say(w, "NOTE: %s\n", n)
	}
	say(w, "Per process (one package in one shard): failed / ran, then Fisher's exact test, two-sided, on those counts.\n")
	say(w, "%-60s %-5s %-12s %-12s %-10s %-22s %s\n", "test", "arch", "base f/ran", "branch f/ran", "p", "base pass/fail/skip", "branch pass/fail/skip")
	tested, below := 0, 0
	for _, r := range c.Rows {
		side := func(in bool, t TestRow) (string, string) {
			if !in {
				return "-", "not in this arm"
			}
			return procs(t), fmt.Sprintf("%d/%d/%d", t.Pass, t.Fail, t.Skip)
		}
		bp, bi := side(r.InBase, r.Base)
		cp, ci := side(r.InBranch, r.Branch)
		p := "n/a"
		if r.P != nil {
			tested++
			p = fmt.Sprintf("%.4g", *r.P)
			if *r.P < 0.05 {
				below++
				p += " *"
			}
		}
		say(w, "%-60s %-5s %-12s %-12s %-10s %-22s %s\n", r.Name, r.Arch, bp, cp, p, bi, ci)
	}
	say(w, "%d test/arch row(s) compared; %d below p = 0.05 (marked *). With %d comparisons about %.1f come out below 0.05 by chance alone; no correction is applied",
		tested, below, tested, 0.05*float64(tested))
	if tested > 1 {
		say(w, ": for a familywise 0.05, hold each p to 0.05/%d = %.4g (Bonferroni)", tested, 0.05/float64(tested))
	}
	say(w, ".\n")
}

// cmdCompare compares a base run with a branch run.
func cmdCompare(args []string, stdout io.Writer) int {
	if len(args) != 2 {
		say(stdout, "usage: stresstally compare BASE BRANCH (each a stress-summary directory or its report.json)\n")
		return 2
	}
	base, err := readReport(args[0])
	if err != nil {
		say(stdout, "::error::base: %v\n", err)
		return 2
	}
	branch, err := readReport(args[1])
	if err != nil {
		say(stdout, "::error::branch: %v\n", err)
		return 2
	}
	c, err := compareReports(base, branch)
	if err != nil {
		say(stdout, "::error::%v\n", err)
		return 2
	}
	c.Text(stdout)
	return 0
}

// fisherTwoSided is Fisher's exact test, two-sided, for the 2x2 table
// [[a, b], [c, d]] (arm one: a failed, b did not; arm two: c failed, d did
// not): the probability, under one rate for both arms and the margins held
// fixed, of a table no more probable than the one observed. Tables within a
// relative 1e-7 of the observed probability count as equally probable, as in
// scipy and R.
func fisherTwoSided(a, b, c, d int) float64 {
	n1, n2, k := a+b, c+d, a+c
	n := n1 + n2
	lchoose := func(n, k int) float64 {
		x, _ := math.Lgamma(float64(n + 1))
		y, _ := math.Lgamma(float64(k + 1))
		z, _ := math.Lgamma(float64(n - k + 1))
		return x - y - z
	}
	logp := func(x int) float64 { return lchoose(k, x) + lchoose(n-k, n1-x) - lchoose(n, n1) }
	obs := logp(a)
	sum := 0.0
	for x := max(0, k-n2); x <= min(k, n1); x++ {
		if lp := logp(x); lp <= obs+1e-7 {
			sum += math.Exp(lp)
		}
	}
	return min(sum, 1)
}
