package main

import (
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const celerisPkg = "github.com/goceleris/celeris/engine/iouring"

// syntheticSelfTestLogs writes, for every shard of the self-test plan, a log
// shaped like what that case produces on a runner. outcome overrides one
// case's body.
func syntheticSelfTestLogs(t *testing.T, dir string, p Plan, outcome map[string]func(Case) string) {
	t.Helper()
	for _, c := range p.Cases {
		for _, arch := range c.Arches {
			for s := 1; s <= c.Shards; s++ {
				body, exit := selfTestBody(c)
				if f, ok := outcome[c.Name]; ok {
					body = f(c)
				}
				ov := map[string]string{"celeris_sha": p.CelerisRef}
				writeShard(t, dir, c, arch, s, body, exit, ov)
			}
		}
	}
}

func verdictBlock(name, result string, subs ...string) string {
	var b strings.Builder
	fmt.Fprintf(&b, "=== RUN   %s\n", name)
	for _, s := range subs {
		fmt.Fprintf(&b, "=== RUN   %s/%s\n", name, s)
	}
	if result == "FAIL" {
		fmt.Fprintf(&b, "    x_test.go:1: something broke in %s\n", name)
	}
	fmt.Fprintf(&b, "--- %s: %s (0.00s)\n", result, name)
	for _, s := range subs {
		fmt.Fprintf(&b, "    --- %s: %s/%s (0.00s)\n", result, name, s)
	}
	return b.String()
}

func selfTestBody(c Case) (string, string) {
	var b strings.Builder
	b.WriteString("-test.shuffle 1\n")
	switch c.Name {
	case "good":
		for range c.Count {
			b.WriteString(verdictBlock("TestSockaddrString", "PASS", "ipv4", "ipv4-zero", "ipv6-loopback"))
			b.WriteString(verdictBlock("TestParseSendZCResult", "PASS", "unsupported-enosys"))
			b.WriteString(verdictBlock("TestUseSendZC", "PASS", "large-linked", "empty"))
			b.WriteString(verdictBlock("TestAbandonedResponseCountsAsSendPeerGone", "SKIP"))
		}
		b.WriteString("PASS\nok  \t" + celerisPkg + "\t1.0s\n")
		return b.String(), "0"
	case "fail":
		for range c.Count {
			for _, n := range celeris656 {
				b.WriteString(verdictBlock(n, "FAIL"))
			}
		}
		b.WriteString("FAIL\nFAIL\t" + celerisPkg + "\t0.01s\nFAIL\n")
		return b.String(), "1"
	case "skip":
		for _, n := range celeris656 {
			b.WriteString(verdictBlock(n, "SKIP"))
		}
		b.WriteString("PASS\nok  \t" + celerisPkg + "\t0.01s\n")
		return b.String(), "0"
	case "timeout":
		b.WriteString("=== RUN   TestAbandonedResponseCountsAsSendPeerGone\npanic: test timed out after 1s\n\trunning tests:\n\t\tTestAbandonedResponseCountsAsSendPeerGone (1s)\n\ngoroutine 1 [running]:\n")
		b.WriteString("FAIL\t" + celerisPkg + "\t1.1s\nFAIL\n")
		return b.String(), "1"
	case "none":
		b.WriteString("testing: warning: no tests to run\nPASS\nok  \t" + celerisPkg + "\t0.01s [no tests to run]\n")
		return b.String(), "0"
	}
	panic("unknown case " + c.Name)
}

func runSummarize(t *testing.T, p Plan, logs string) (int, string, map[string]any) {
	t.Helper()
	pj, _ := json.Marshal(p)
	out := t.TempDir()
	env := map[string]string{"STRESS_PLAN": string(pj), "STRESS_CELERIS_SHA": p.CelerisRef}
	var log strings.Builder
	code := cmdSummarize([]string{"-logs", logs, "-out", out}, &log, func(k string) string { return env[k] })
	var rep map[string]any
	if b, err := os.ReadFile(filepath.Join(out, "report.json")); err == nil {
		_ = json.Unmarshal(b, &rep)
	}
	md, _ := os.ReadFile(filepath.Join(out, "summary.md"))
	return code, log.String() + "\n" + string(md), rep
}

// The whole pull_request path on logs shaped like the runner's: every case
// as expected, so summarize exits 0, although three of the five cases FAIL.
func TestSummarizeSelfTestAllAsExpected(t *testing.T) {
	p := mustSelfTest(t)
	dir := t.TempDir()
	syntheticSelfTestLogs(t, dir, p, nil)
	code, out, rep := runSummarize(t, p, dir)
	if code != 0 {
		t.Fatalf("exit %d:\n%s", code, out)
	}
	if !strings.Contains(out, "5 of 5 case(s) as expected") {
		t.Errorf("summary does not say 5 of 5:\n%s", out)
	}
	if len(rep["cases"].([]any)) != 5 {
		t.Errorf("report.json cases: %v", rep["cases"])
	}
}

// RULE FIVE for the self-test: each way a case can come out wrong turns the
// run red.
func TestSummarizeSelfTestCatchesEachDeviation(t *testing.T) {
	p := mustSelfTest(t)
	for name, outcome := range map[string]map[string]func(Case) string{
		"skip counted as pass": {"skip": func(Case) string {
			var b strings.Builder
			for _, n := range celeris656 {
				b.WriteString(verdictBlock(n, "PASS"))
			}
			return b.String() + "PASS\nok  \t" + celerisPkg + "\t0.01s\n"
		}},
		"failure lost": {"fail": func(c Case) string { s, _ := selfTestBody(Case{Name: "skip", Count: 1}); return s }},
		"timeout lost": {"timeout": func(c Case) string { s, _ := selfTestBody(Case{Name: "none", Count: 1}); return s }},
		"good case short one run": {"good": func(c Case) string {
			s, _ := selfTestBody(Case{Name: "good", Count: c.Count - 1})
			return s
		}},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			syntheticSelfTestLogs(t, dir, p, outcome)
			if code, out, _ := runSummarize(t, p, dir); code != 1 {
				t.Errorf("exit %d, want 1:\n%s", code, out)
			}
		})
	}
	t.Run("missing shard", func(t *testing.T) {
		dir := t.TempDir()
		syntheticSelfTestLogs(t, dir, p, nil)
		if err := os.Remove(filepath.Join(dir, logName("good", "arm64", 2))); err != nil {
			t.Fatal(err)
		}
		if code, out, _ := runSummarize(t, p, dir); code != 1 || !strings.Contains(out, "MISSING") {
			t.Errorf("exit %d:\n%s", code, out)
		}
	})
}

// dispatchPlan is a dispatch of one synthetic test on both arches, with the
// commit fixed so the shard headers match it.
func dispatchPlan(t *testing.T, count, shards string) (Plan, Case) {
	t.Helper()
	in := goodInputs()
	in.Run, in.Count, in.Shards = "^TestFlaky$", count, shards
	p, err := planDispatch(in)
	if err != nil {
		t.Fatal(err)
	}
	p.CelerisRef = testSHA
	return p, p.Cases[0]
}

// body is one process of TestFlaky: its verdicts in order, then the package
// result line go test prints for them.
func body(results ...string) string {
	var b strings.Builder
	failed := false
	for _, r := range results {
		b.WriteString(verdictBlock("TestFlaky", r))
		failed = failed || r == "FAIL"
	}
	if failed {
		b.WriteString("FAIL\nFAIL\t" + celerisPkg + "\t1s\nFAIL\n")
	} else {
		b.WriteString("PASS\nok  \t" + celerisPkg + "\t1s\n")
	}
	return b.String()
}

func exitOf(results ...string) string {
	for _, r := range results {
		if r == "FAIL" {
			return "1"
		}
	}
	return "0"
}

// A dispatch run: the one case must PASS; any failure turns it red and is
// listed with its first lines.
func TestSummarizeDispatch(t *testing.T) {
	p, c := dispatchPlan(t, "1", "2")
	for _, tc := range []struct {
		name  string
		shard map[int]string
		code  int
	}{{"all pass", map[int]string{1: "PASS", 2: "PASS"}, 0}, {"one fails", map[int]string{1: "PASS", 2: "FAIL"}, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, arch := range c.Arches {
				for s, res := range tc.shard {
					writeShard(t, dir, c, arch, s, body(res), exitOf(res), nil)
				}
			}
			code, out, _ := runSummarize(t, p, dir)
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			if tc.code == 1 && (!strings.Contains(out, "something broke in TestFlaky") ||
				!strings.Contains(out, "| `TestFlaky` | engine/iouring | x86 | 1 / 2 | 50.00% | 1.26% to 98.74% | 1 | 1 | 0 | 0 |")) {
				t.Errorf("the failure is not listed with its lines and per-process rate:\n%s", out)
			}
		})
	}
}

// RULE FIVE for the per-process rate. Iterations of -count run inside one
// process and share its state, so the rate and its interval are per process
// (one package in one shard), and the iterations are counts only. Here one
// process of two fails in 2 of its 5 iterations: that is 1 failing process
// of 2 (50%, exact 95% 1.26% to 98.74%), and 2 FAIL of 10 iterations. An
// iteration rate would say 20.00% with an interval of 2.52% to 55.61%, as if
// the 10 iterations were independent trials; neither may appear.
func TestRatesArePerProcessNotPerIteration(t *testing.T) {
	p, c := dispatchPlan(t, "5", "2")
	dir := t.TempDir()
	flaky := []string{"PASS", "FAIL", "PASS", "FAIL", "PASS"}
	clean := []string{"PASS", "PASS", "PASS", "PASS", "PASS"}
	for _, arch := range c.Arches {
		writeShard(t, dir, c, arch, 1, body(flaky...), exitOf(flaky...), nil)
		writeShard(t, dir, c, arch, 2, body(clean...), exitOf(clean...), nil)
	}
	out := t.TempDir()
	pj, _ := json.Marshal(p)
	var log strings.Builder
	env := map[string]string{"STRESS_PLAN": string(pj), "STRESS_CELERIS_SHA": testSHA}
	if code := cmdSummarize([]string{"-logs", dir, "-out", out}, &log, func(k string) string { return env[k] }); code != 1 {
		t.Fatalf("exit %d, want 1 (a test failed):\n%s", code, log.String())
	}
	md, _ := os.ReadFile(filepath.Join(out, "summary.md"))
	tsv, _ := os.ReadFile(filepath.Join(out, "tests.tsv"))
	for _, want := range []string{
		"| `TestFlaky` | engine/iouring | x86 | 1 / 2 | 50.00% | 1.26% to 98.74% | 8 | 2 | 0 | 0 |",
		"| `TestFlaky` | engine/iouring | arm64 | 1 / 2 | 50.00% | 1.26% to 98.74% | 8 | 2 | 0 | 0 |",
	} {
		if !strings.Contains(string(md), want) {
			t.Errorf("summary.md lacks %q:\n%s", want, md)
		}
	}
	for _, bad := range []string{"20.00%", "2.52%", "55.61%"} {
		if strings.Contains(string(md), bad) || strings.Contains(log.String(), bad) {
			t.Errorf("an iteration rate or interval (%s) is reported", bad)
		}
	}
	lines := strings.Split(strings.TrimSpace(string(tsv)), "\n")
	if lines[0] != strings.TrimSuffix(tsvHeader, "\n") {
		t.Errorf("tests.tsv header %q", lines[0])
	}
	if want := "stress\t" + celerisPkg + "\tTestFlaky\tx86\t2\t1\t0.500000\t0.012579\t0.987421\t8\t2\t0\t0"; lines[1] != want {
		t.Errorf("tests.tsv x86 row\n got %q\nwant %q", lines[1], want)
	}
	var rep struct {
		Cases []CaseReport `json:"cases"`
	}
	b, _ := os.ReadFile(filepath.Join(out, "report.json"))
	if err := json.Unmarshal(b, &rep); err != nil {
		t.Fatal(err)
	}
	r := rep.Cases[0].Tests[0]
	if r.Processes != 2 || r.FailedProcesses != 1 || r.Pass != 8 || r.Fail != 2 || r.ProcFailRate == nil || *r.ProcFailRate != 0.5 {
		t.Errorf("report.json row %+v", r)
	}
}

// A process in which the test only skipped, or only ran without a verdict,
// is not a process that ran it; one in which it failed several times is one
// failing process.
func TestProcessCountsIgnoreSkipsAndCountAFailingProcessOnce(t *testing.T) {
	c := sampleCase("procs")
	c.Packages, c.Count, c.Shards = []string{"./a"}, 3, 3
	dir := t.TempDir()
	three := func(res string) string {
		return strings.Repeat(verdictBlock("TestX", res), 3)
	}
	writeShard(t, dir, c, "x86", 1, three("FAIL")+"FAIL\nFAIL\t"+pkgA+"\t1s\nFAIL\n", "1", nil)
	writeShard(t, dir, c, "x86", 2, three("SKIP")+"PASS\nok  \t"+pkgA+"\t1s\n", "0", nil)
	writeShard(t, dir, c, "x86", 3, three("PASS")+"PASS\nok  \t"+pkgA+"\t1s\n", "0", nil)
	r := judgeCase(c, dir, testSHA)
	got := row(t, r, pkgA, "TestX", "x86")
	if got.Processes != 2 || got.FailedProcesses != 1 || got.Fail != 3 || got.Skip != 3 || got.Pass != 3 {
		t.Errorf("TestX %+v, want 1 failing process of 2 (the skip-only process is not one), iterations pass 3 fail 3 skip 3", got)
	}
}

// RULE FIVE for the arch check: a test that reached a verdict on one arch and
// never on the other fails the case (arch-gap) and is named; a test that
// skipped on both is no gap, and a one-arch case has none.
func TestATestThatRanOnOneArchOnlyIsAnArchGap(t *testing.T) {
	c := sampleCase("gap")
	c.Packages, c.Count, c.Arches = []string{"./a"}, 1, []string{"x86", "arm64"}
	both := func(x86, arm string) CaseReport {
		dir := t.TempDir()
		ok := "PASS\nok  \t" + pkgA + "\t1s\n"
		writeShard(t, dir, c, "x86", 1, verdictBlock("TestPass", "PASS")+verdictBlock("TestX", x86)+ok, "0", nil)
		writeShard(t, dir, c, "arm64", 1, verdictBlock("TestPass", "PASS")+verdictBlock("TestX", arm)+ok, "0", nil)
		return judgeCase(c, dir, testSHA)
	}
	r := both("PASS", "SKIP")
	if r.Verdict != "FAIL" || !slices.Equal(r.Problems, []string{problemArchGap}) {
		t.Fatalf("verdict %s problems %v, want FAIL [arch-gap]", r.Verdict, r.Problems)
	}
	if len(r.ArchGaps) != 1 || !strings.Contains(r.ArchGaps[0], "TestX") || !strings.Contains(r.ArchGaps[0], "never on arm64") {
		t.Errorf("arch gaps %q, want TestX never on arm64", r.ArchGaps)
	}
	var md strings.Builder
	r.Markdown(&md)
	if !strings.Contains(md.String(), "**Arch gap:** TestX") {
		t.Errorf("the summary does not name the gap:\n%s", md.String())
	}
	if r := both("SKIP", "SKIP"); !r.OK() || len(r.ArchGaps) != 0 || r.Verdict != "PASS" {
		t.Errorf("a test skipped on both arches: verdict %s problems %v gaps %v", r.Verdict, r.Problems, r.ArchGaps)
	}
	if r := both("FAIL", "PASS"); slices.Contains(r.Problems, problemArchGap) {
		t.Errorf("a test that failed on one arch and passed on the other ran on both: %v", r.Problems)
	}
	one := c
	one.Arches = []string{"x86"}
	dir := t.TempDir()
	writeShard(t, dir, one, "x86", 1, verdictBlock("TestX", "PASS")+"PASS\nok  \t"+pkgA+"\t1s\n", "0", nil)
	if r := judgeCase(one, dir, testSHA); len(r.ArchGaps) != 0 || r.Verdict != "PASS" {
		t.Errorf("one arch: verdict %s gaps %v", r.Verdict, r.ArchGaps)
	}
}

// RULE FIVE for the environment check: a kernel or OS release that differs
// between the arches, or more than one within an arch, is flagged (the job
// log gets a ::warning::, the summary a Warning line); the separately built
// images of one release (ubuntu24 and ubuntu24-arm64, different versions)
// are not. It never changes the verdict.
func TestKernelOrImageMismatchBetweenArchesIsFlagged(t *testing.T) {
	c := sampleCase("env")
	c.Packages, c.Count, c.Shards, c.Arches = []string{"./a"}, 1, 2, []string{"x86", "arm64"}
	ok := verdictBlock("TestPass", "PASS") + "PASS\nok  \t" + pkgA + "\t1s\n"
	x86 := map[string]string{"kernel": "6.17.0-1022-azure", "image": "ubuntu24 20260920.314.1"}
	arm := map[string]string{"kernel": "6.17.0-1022-azure", "image": "ubuntu24-arm64 20260920.129.1"}
	judge := func(arm1, arm2 map[string]string) CaseReport {
		dir := t.TempDir()
		writeShard(t, dir, c, "x86", 1, ok, "0", x86)
		writeShard(t, dir, c, "x86", 2, ok, "0", x86)
		writeShard(t, dir, c, "arm64", 1, ok, "0", arm1)
		writeShard(t, dir, c, "arm64", 2, ok, "0", arm2)
		return judgeCase(c, dir, testSHA)
	}
	if r := judge(arm, arm); len(r.Warnings) != 0 || r.Verdict != "PASS" {
		t.Errorf("same kernel and release on both arches: warnings %q verdict %s", r.Warnings, r.Verdict)
	}
	with := func(m map[string]string, k, v string) map[string]string {
		n := maps.Clone(m)
		n[k] = v
		return n
	}
	for name, tc := range map[string]struct {
		arm1, arm2 map[string]string
		want       string
	}{
		"kernel between arches":  {with(arm, "kernel", "6.14.0-1017-azure"), with(arm, "kernel", "6.14.0-1017-azure"), "kernel differs between the arches"},
		"release between arches": {with(arm, "image", "ubuntu22-arm64 20260920.1.1"), with(arm, "image", "ubuntu22-arm64 20260920.1.1"), "OS image differs between the arches"},
		"two kernels on arm64":   {arm, with(arm, "kernel", "6.14.0-1017-azure"), "the arm64 shards ran on 2 kernels"},
		"two images on arm64":    {arm, with(arm, "image", "ubuntu24-arm64 20260927.1.1"), "the arm64 shards ran on 2 runner images"},
	} {
		t.Run(name, func(t *testing.T) {
			r := judge(tc.arm1, tc.arm2)
			if !slices.ContainsFunc(r.Warnings, func(w string) bool { return strings.Contains(w, tc.want) }) {
				t.Errorf("warnings %q lack %q", r.Warnings, tc.want)
			}
			if r.Verdict != "PASS" || !r.OK() {
				t.Errorf("a warning changed the verdict: %s %v", r.Verdict, r.Mismatches)
			}
			var log strings.Builder
			verdictText(&log, Plan{}, []CaseReport{r})
			if !strings.Contains(log.String(), "::warning::case env: "+tc.want) {
				t.Errorf("no ::warning:: annotation:\n%s", log.String())
			}
			var md strings.Builder
			r.Markdown(&md)
			if !strings.Contains(md.String(), "**Warning:** "+tc.want) {
				t.Errorf("the summary does not show the warning:\n%s", md.String())
			}
		})
	}
}

// Text from a shard's log comes from the code under test, which may be
// anyone's pull request: in the job summary it renders as text, never as a
// link or HTML. A test name keeps its brackets inside its code span.
func TestSummaryRendersLogTextAsText(t *testing.T) {
	c := sampleCase("inert")
	c.Packages, c.Count = []string{"./a"}, 1
	dir := t.TempDir()
	body := verdictBlock("TestPass", "PASS", "[a](b)") + "signal: killed [click](https://example.invalid/) <img src=x>\n"
	writeShard(t, dir, c, "x86", 1, body, "1", nil)
	var md strings.Builder
	judgeCase(c, dir, testSHA).Markdown(&md)
	s := md.String()
	for _, want := range []string{`\[click\](https://example.invalid/) &lt;img src=x&gt;`, "| `TestPass/[a](b)` |"} {
		if !strings.Contains(s, want) {
			t.Errorf("summary lacks %q:\n%s", want, s)
		}
	}
	for _, bad := range []string{"[click](", "<img"} {
		if strings.Contains(s, bad) {
			t.Errorf("summary renders %q from the log as Markdown or HTML:\n%s", bad, s)
		}
	}
}
