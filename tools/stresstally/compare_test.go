package main

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// Each reference p-value is scipy.stats.fisher_exact([[a, b], [c, d]],
// alternative="two-sided"), cross-checked against the same sum computed in
// exact rational arithmetic. The first three rows are docs/STRESS.md
// example 1's figures: 5, 4 and 6 of 20 against 0 of 20.
func TestFisherMatchesScipy(t *testing.T) {
	for _, c := range []struct {
		a, b, c, d int
		p          float64
	}{
		{5, 15, 0, 20, 0.0471240471240471},
		{4, 16, 0, 20, 0.106029106029106},
		{6, 14, 0, 20, 0.0201960201960202},
		{6, 34, 0, 40, 0.0255466052934407},
		{5, 35, 0, 40, 0.0547427256288016},
		{0, 15, 2, 13, 0.482758620689655},
		{1, 37, 2, 35, 0.614794520547945},
		{1, 9, 0, 10, 1},
		{3, 17, 3, 17, 1},
		{10, 10, 2, 18, 0.0138141478519677},
		{0, 20, 0, 20, 1},
		{20, 0, 0, 20, 1.45088891038497e-11},
		{7, 13, 1, 19, 0.0435960435960436},
	} {
		if got := fisherTwoSided(c.a, c.b, c.c, c.d); !near(got, c.p) {
			t.Errorf("fisherTwoSided(%d, %d, %d, %d) = %.15g, want %.15g", c.a, c.b, c.c, c.d, got, c.p)
		}
	}
}

// armReport writes the stress-summary of one arm of a dispatch of TestFlaky:
// on each arch, fails[arch] of the shards fail (one FAIL in 3 iterations),
// the rest pass. It returns the directory, as gh run download leaves it.
func armReport(t *testing.T, sha, count, shards string, fails map[string]int, override map[string]map[string]string) string {
	t.Helper()
	p, c := dispatchPlan(t, count, shards)
	p.CelerisRef = sha
	p.ProbatoriumSHA = testProbatoriumSHA
	logs := t.TempDir()
	for _, arch := range c.Arches {
		for s := 1; s <= c.Shards; s++ {
			res := []string{"PASS", "PASS", "PASS"}
			if s <= fails[arch] {
				res[1] = "FAIL"
			}
			ov := map[string]string{"celeris_sha": sha}
			for k, v := range override[arch] {
				ov[k] = v
			}
			writeShard(t, logs, c, arch, s, body(res...), exitOf(res...), ov)
		}
	}
	out := t.TempDir()
	rep := judgeCase(c, logs, sha)
	if err := writeReports(out, p, sha, []CaseReport{rep}); err != nil {
		t.Fatal(err)
	}
	return out
}

const branchSHA = "fedcba9876543210fedcba9876543210fedcba98"

// setProbatorium rewrites an arm's report.json as if its run had come from
// probatorium commit sha ("" for a report that records none).
func setProbatorium(t *testing.T, dir, sha string) {
	t.Helper()
	b, err := os.ReadFile(filepath.Join(dir, "report.json"))
	if err != nil {
		t.Fatal(err)
	}
	var r runReport
	if err := json.Unmarshal(b, &r); err != nil {
		t.Fatal(err)
	}
	r.Plan.ProbatoriumSHA = sha
	if err := writeReports(dir, r.Plan, r.CelerisSHA, r.Cases); err != nil {
		t.Fatal(err)
	}
}

// The arms line up per test and arch, as failed / ran processes, with
// Fisher's exact p on those counts; iteration counts ride along.
func TestCompareSetsProcessesSideBySide(t *testing.T) {
	base := armReport(t, testSHA, "3", "20", map[string]int{"x86": 5, "arm64": 4}, nil)
	branch := armReport(t, branchSHA, "3", "20", nil, nil)
	var out strings.Builder
	if code := cmdCompare([]string{base, filepath.Join(branch, "report.json")}, &out); code != 0 {
		t.Fatalf("exit %d:\n%s", code, out.String())
	}
	s := out.String()
	for _, want := range []string{
		"base celeris " + testSHA + " vs branch celeris " + branchSHA,
		"both: probatorium " + testProbatoriumSHA + " packages ",
		"TestFlaky                                                    x86   5 / 20       0 / 20       0.04712 *  55/5/0                 60/0/0",
		"TestFlaky                                                    arm64 4 / 20       0 / 20       0.106      56/4/0                 60/0/0",
		"2 test/arch row(s) compared; 1 below p = 0.05",
		"for a familywise 0.05, hold each p to 0.05/2 = 0.025 (Bonferroni)",
	} {
		if !strings.Contains(s, want) {
			t.Errorf("comparison lacks %q:\n%s", want, s)
		}
	}
	if strings.Contains(s, "NOTE:") {
		t.Errorf("a clean pair has notes:\n%s", s)
	}
}

// RULE FIVE for the arm structure: two runs that differ in anything but the
// celeris commit are not a comparison, and compare says what differs.
func TestCompareRefusesAPairThatDiffersInMoreThanTheCommit(t *testing.T) {
	base := armReport(t, testSHA, "3", "4", nil, nil)
	for name, c := range map[string]struct {
		count, shards string
		want          string
	}{
		"count":  {"5", "4", "count: base 3, branch 5"},
		"shards": {"3", "6", "shards: base 4, branch 6"},
	} {
		t.Run(name, func(t *testing.T) {
			branch := armReport(t, branchSHA, c.count, c.shards, nil, nil)
			var out strings.Builder
			if code := cmdCompare([]string{base, branch}, &out); code != 2 || !strings.Contains(out.String(), c.want) {
				t.Errorf("exit %d, want 2 naming %q:\n%s", code, c.want, out.String())
			}
		})
	}
	// The same inputs from two probatorium commits (stress/runs moved between
	// the dispatches) ran two workflows and two tallies: refused. From the
	// same commit, the same pair is a comparison.
	t.Run("probatorium commit", func(t *testing.T) {
		branch := armReport(t, branchSHA, "3", "4", nil, nil)
		var out strings.Builder
		if code := cmdCompare([]string{base, branch}, &out); code != 0 {
			t.Fatalf("one probatorium commit in both arms: exit %d, want 0:\n%s", code, out.String())
		}
		const other = "76543210fedcba9876543210fedcba9876543210"
		setProbatorium(t, branch, other)
		out.Reset()
		want := "probatorium commit: base " + testProbatoriumSHA + ", branch " + other
		if code := cmdCompare([]string{base, branch}, &out); code != 2 || !strings.Contains(out.String(), want) {
			t.Errorf("exit %d, want 2 naming %q:\n%s", code, want, out.String())
		}
	})
	// A self-test report is not a dispatch.
	selfDir := t.TempDir()
	p := mustSelfTest(t)
	var reps []CaseReport
	for _, c := range p.Cases {
		reps = append(reps, CaseReport{Case: c.Name, Config: c})
	}
	if err := writeReports(selfDir, p, testSHA, reps); err != nil {
		t.Fatal(err)
	}
	var out strings.Builder
	if code := cmdCompare([]string{selfDir, base}, &out); code != 2 || !strings.Contains(out.String(), "holds 5 cases") {
		t.Errorf("a self-test report was compared: exit %d\n%s", code, out.String())
	}
}

// What qualifies a comparison is printed with it: an A/A pair, a kernel that
// differs between the arms, a shard that did not complete.
func TestCompareNotesWhatQualifiesIt(t *testing.T) {
	base := armReport(t, testSHA, "3", "2", nil, nil)
	same := armReport(t, testSHA, "3", "2", nil, nil)
	newKernel := armReport(t, branchSHA, "3", "2", nil, map[string]map[string]string{"arm64": {"kernel": "6.18.0-1001-azure"}})
	// A branch arm whose x86 shard 2 never uploaded a log.
	short := armReport(t, branchSHA, "3", "2", nil, nil)
	rep, err := readReport(short)
	if err != nil {
		t.Fatal(err)
	}
	rep.Cases[0].Arches[0].Complete--
	rep.Cases[0].Arches[0].Missing++
	if err := writeReports(short, rep.Plan, branchSHA, rep.Cases); err != nil {
		t.Fatal(err)
	}
	for name, c := range map[string]struct {
		branch, want string
	}{
		"A/A":        {same, "both arms tested celeris " + testSHA + ": this is an A/A comparison"},
		"kernel":     {newKernel, "arm64 ran on kernel 6.11.0-test in the base arm and 6.18.0-1001-azure in the branch arm"},
		"incomplete": {short, "branch: 1 of 2 x86 shard(s) did not complete (UNPARSED 0, MISSING 1, WRONG-SHAPE 0)"},
	} {
		t.Run(name, func(t *testing.T) {
			var out strings.Builder
			if code := cmdCompare([]string{base, c.branch}, &out); code != 0 || !strings.Contains(out.String(), "NOTE: "+c.want) {
				t.Errorf("exit %d, want a note %q:\n%s", code, c.want, out.String())
			}
		})
	}
}

// A report that records no probatorium commit (written before the plan
// recorded one, or by a local tally) cannot show that both arms ran the same
// workflow and tool, so compare refuses it, even when neither arm has one.
func TestCompareRefusesAReportWithoutTheProbatoriumCommit(t *testing.T) {
	for name, c := range map[string]struct{ base, branch string }{
		"branch lacks it": {testProbatoriumSHA, ""},
		"base lacks it":   {"", testProbatoriumSHA},
		"both lack it":    {"", ""},
	} {
		t.Run(name, func(t *testing.T) {
			base := armReport(t, testSHA, "3", "2", nil, nil)
			branch := armReport(t, branchSHA, "3", "2", nil, nil)
			setProbatorium(t, base, c.base)
			setProbatorium(t, branch, c.branch)
			var out strings.Builder
			if code := cmdCompare([]string{base, branch}, &out); code != 2 || !strings.Contains(out.String(), "records no probatorium commit") {
				t.Errorf("exit %d, want 2 naming the missing probatorium commit:\n%s", code, out.String())
			}
		})
	}
}

// The probatorium commit the plan records reaches both halves of the
// stress-summary: report.json, which compare reads, and the job summary.
func TestReportsRecordTheProbatoriumCommit(t *testing.T) {
	dir := armReport(t, testSHA, "3", "2", nil, nil)
	r, err := readReport(dir)
	if err != nil {
		t.Fatal(err)
	}
	if r.Plan.ProbatoriumSHA != testProbatoriumSHA {
		t.Errorf("report.json records probatorium commit %q, want %q", r.Plan.ProbatoriumSHA, testProbatoriumSHA)
	}
	md, err := os.ReadFile(filepath.Join(dir, "summary.md"))
	if err != nil {
		t.Fatal(err)
	}
	if want := "; probatorium commit `" + testProbatoriumSHA + "`."; !strings.Contains(string(md), want) {
		t.Errorf("summary.md lacks %q:\n%s", want, md)
	}
}
