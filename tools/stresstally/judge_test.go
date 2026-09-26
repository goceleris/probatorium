package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

const (
	pkgA = "example.com/stresstally/samples/a"
	pkgB = "example.com/stresstally/samples/b"
)

func judgeOne(t *testing.T, c Case, body, exit string, override map[string]string) CaseReport {
	t.Helper()
	dir := t.TempDir()
	writeShard(t, dir, c, "x86", 1, body, exit, override)
	return judgeCase(c, dir, testSHA)
}

// The clean corpus: -count=2 -shuffle=42 over ./a ./b ./c. Every number
// below is read off the sample module, not off the parser.
func TestTallyCountsEveryVerdictIncludingSubtests(t *testing.T) {
	r := judgeOne(t, sampleCase("good"), corpus(t, "good"), "0", nil)
	if r.Verdict != "PASS" || !r.OK() || len(r.Problems) != 0 {
		t.Fatalf("verdict %s problems %v mismatches %v, want a clean PASS", r.Verdict, r.Problems, r.Mismatches)
	}
	for _, c := range []struct {
		pkg, name        string
		pass, fail, skip int
	}{
		{pkgA, "TestPass", 2, 0, 0},
		{pkgB, "TestPass", 2, 0, 0}, // same name, other package: its own row
		{pkgB, "TestOnlyB", 2, 0, 0},
		{pkgA, "TestSub", 2, 0, 0},
		{pkgA, "TestSub/one", 2, 0, 0},
		{pkgA, "TestSub/two_words", 2, 0, 0},
		{pkgA, "TestSub/two_words/deep", 2, 0, 0},
		{pkgA, "TestSub/skipper", 0, 0, 2}, // a skipped subtest of a passing test
		{pkgA, "TestSkip", 0, 0, 2},
		{pkgA, "TestParallel/p0", 2, 0, 0},
		{pkgA, "TestParallel/p2", 2, 0, 0},
		{pkgA, "Example", 1, 0, 0}, // go test runs an example once, whatever -count says
	} {
		got := row(t, r, c.pkg, c.name, "x86")
		if got.Pass != c.pass || got.Fail != c.fail || got.Skip != c.skip || got.NoVerdict != 0 {
			t.Errorf("%s %s: pass/fail/skip/noverdict %d/%d/%d/%d, want %d/%d/%d/0",
				c.pkg, c.name, got.Pass, got.Fail, got.Skip, got.NoVerdict, c.pass, c.fail, c.skip)
		}
	}
	// Counted by hand from the sample module: 19 rows in a (TestPass,
	// TestSub and its 4 subtests, TestSkip, TestFail, TestSubFail and its 2,
	// TestParallel and its 3, TestPanic, TestGoroutinePanic, TestSlow,
	// Example) and 2 in b. Two of them skip; each test ran twice and the
	// example once.
	if len(r.Tests) != 21 {
		t.Errorf("%d rows, want 21", len(r.Tests))
	}
	tot := r.Arches[0]
	if tot.Pass != 2*18+1 || tot.Skip != 2*2 || tot.Fail != 0 || tot.NoVerdict != 0 {
		t.Errorf("totals pass %d skip %d fail %d noverdict %d, want 37, 4, 0, 0", tot.Pass, tot.Skip, tot.Fail, tot.NoVerdict)
	}
}

// RULE: a SKIP is its own column and never a pass. A run of nothing but
// skips must not PASS.
func TestSkipIsNeverAPass(t *testing.T) {
	c := sampleCase("skiponly")
	c.Packages, c.Count = []string{"./a"}, 3
	r := judgeOne(t, c, corpus(t, "skiponly"), "0", nil)
	got := row(t, r, pkgA, "TestSkip", "x86")
	if got.Pass != 0 || got.Skip != 3 {
		t.Errorf("TestSkip pass %d skip %d, want 0 and 3", got.Pass, got.Skip)
	}
	if got.FailRate != nil {
		t.Errorf("a test that only skipped has a fail rate %v; it never ran", *got.FailRate)
	}
	if r.Verdict != "FAIL" || !slices.Equal(r.Problems, []string{problemNothingRan}) {
		t.Errorf("verdict %s problems %v, want FAIL [nothing-ran]", r.Verdict, r.Problems)
	}
	if r.Shards[0].Status != statusComplete {
		t.Errorf("shard %s %v; a skip-only log is complete, the CASE fails", r.Shards[0].Status, r.Shards[0].Reasons)
	}
}

func TestFailIsCountedAndListedWithItsFirstLines(t *testing.T) {
	c := sampleCase("fail")
	c.Packages, c.Count = []string{"./a"}, 1
	r := judgeOne(t, c, corpus(t, "fail"), "1", nil)
	if r.Verdict != "FAIL" || !slices.Equal(r.Problems, []string{problemFailures}) || r.Shards[0].Status != statusComplete {
		t.Fatalf("verdict %s problems %v shard %s %v", r.Verdict, r.Problems, r.Shards[0].Status, r.Shards[0].Reasons)
	}
	for name, want := range map[string][2]int{
		"TestFail": {0, 1}, "TestSubFail": {0, 1}, "TestSubFail/bad": {0, 1}, "TestSubFail/ok": {1, 0}, "TestPass": {1, 0},
	} {
		if got := row(t, r, pkgA, name, "x86"); got.Pass != want[0] || got.Fail != want[1] {
			t.Errorf("%s pass %d fail %d, want %v", name, got.Pass, got.Fail, want)
		}
	}
	fr := row(t, r, pkgA, "TestFail", "x86")
	if fr.FailRate == nil || *fr.FailRate != 1 || !near(*fr.CILow, 0.025) || *fr.CIHigh != 1 {
		t.Errorf("TestFail rate/CI %s %s, want 100%% and 2.50%% to 100.00%% (1 of 1)", pct(fr.FailRate), ci(fr))
	}
	lines := map[string]string{}
	for _, f := range r.Failures {
		lines[f.Name] = strings.Join(f.Lines, "\n")
		if f.Package != pkgA || f.Arch != "x86" || f.Shard != 1 || f.Shuffle == "" {
			t.Errorf("failure %s attributed to %q %s/%d shuffle %q", f.Name, f.Package, f.Arch, f.Shard, f.Shuffle)
		}
	}
	for name, want := range map[string]string{
		"TestFail":        "boom: want 1 got 2",
		"TestSubFail/bad": "sub failed",
		"TestSubFail":     "sub failed",
	} {
		if !strings.Contains(lines[name], want) {
			t.Errorf("first failure lines of %s = %q, want them to include %q", name, lines[name], want)
		}
	}
	if strings.Contains(lines["TestFail"], "--- ") || strings.Contains(lines["TestFail"], "=== ") {
		t.Errorf("excerpt keeps go test's own marker lines: %q", lines["TestFail"])
	}
}

// A panic inside a test: the FAIL verdict it produced counts, the rest of
// that package never ran, so the shard is UNPARSED. The next package's
// results still count.
func TestPanicMakesTheShardUnparsed(t *testing.T) {
	c := sampleCase("panic")
	c.Count = 1
	r := judgeOne(t, c, corpus(t, "panic"), "1", nil)
	s := r.Shards[0]
	if s.Status != statusUnparsed || !hasReason(s.Reasons, reasonPanic) {
		t.Fatalf("shard %s %v, want UNPARSED with %s", s.Status, s.Reasons, reasonPanic)
	}
	if got := row(t, r, pkgA, "TestPanic", "x86"); got.Fail != 1 {
		t.Errorf("TestPanic fail %d, want 1", got.Fail)
	}
	if got := row(t, r, pkgB, "TestOnlyB", "x86"); got.Pass != 1 {
		t.Errorf("TestOnlyB (package b, after the panic) pass %d, want 1", got.Pass)
	}
	if !slices.ContainsFunc(r.Failures, func(f Failure) bool {
		return f.Name == "(panic)" && f.Package == pkgA && strings.HasPrefix(f.Lines[0], "panic: kaboom")
	}) {
		t.Errorf("no (panic) excerpt in package a: %+v", r.Failures)
	}
	if r.Verdict != "FAIL" || !slices.Contains(r.Problems, problemUnparsed) {
		t.Errorf("verdict %s problems %v", r.Verdict, r.Problems)
	}
}

// A panic on a goroutine the test started: no verdict for the running test.
func TestGoroutinePanicLeavesARunWithoutAVerdict(t *testing.T) {
	c := sampleCase("gpanic")
	c.Packages, c.Count = []string{"./a"}, 1
	r := judgeOne(t, c, corpus(t, "gpanic"), "1", nil)
	s := r.Shards[0]
	for _, want := range []string{reasonPanic, reasonRanWithoutVerdict, reasonPkgFailNoTest} {
		if !hasReason(s.Reasons, want) {
			t.Errorf("reasons %v lack %s", s.Reasons, want)
		}
	}
	if got := row(t, r, pkgA, "TestGoroutinePanic", "x86"); got.NoVerdict != 1 || got.Pass+got.Fail+got.Skip != 0 {
		t.Errorf("TestGoroutinePanic %+v, want exactly one run without a verdict", got)
	}
	if got := row(t, r, pkgA, "TestPass", "x86"); got.Pass != 1 {
		t.Errorf("TestPass (before the panic) pass %d, want 1", got.Pass)
	}
}

func TestTimeoutIsUnparsedAndNamesTheTest(t *testing.T) {
	c := sampleCase("timeout")
	c.Packages, c.Count = []string{"./a"}, 1
	r := judgeOne(t, c, corpus(t, "timeout"), "1", nil)
	s := r.Shards[0]
	if s.Status != statusUnparsed || !hasReason(s.Reasons, reasonTimeout) || hasReason(s.Reasons, reasonPanic) {
		t.Fatalf("shard %s %v, want UNPARSED with %s (and not the generic %s)", s.Status, s.Reasons, reasonTimeout, reasonPanic)
	}
	if !slices.Equal(s.TimedOut, []string{"TestSlow"}) {
		t.Errorf("timed out in %v, want [TestSlow]", s.TimedOut)
	}
	if got := row(t, r, pkgA, "TestSlow", "x86"); got.NoVerdict != 1 {
		t.Errorf("TestSlow %+v, want one run without a verdict", got)
	}
	if got := row(t, r, pkgA, "TestPass", "x86"); got.Pass != 1 {
		t.Errorf("TestPass pass %d, want 1: results before the timeout still count", got.Pass)
	}
}

// go test exits 0 when -run matches nothing. That is not a pass.
func TestNoVerdictsIsUnparsedNotAPass(t *testing.T) {
	c := sampleCase("none")
	c.Count = 1
	r := judgeOne(t, c, corpus(t, "none"), "0", nil)
	s := r.Shards[0]
	if s.Status != statusUnparsed || !hasReason(s.Reasons, reasonNoVerdicts) {
		t.Fatalf("shard %s %v, want UNPARSED no-verdicts", s.Status, s.Reasons)
	}
	if r.Verdict != "FAIL" || !slices.Equal(r.Problems, []string{problemNothingRan, problemUnparsed}) {
		t.Errorf("verdict %s problems %v", r.Verdict, r.Problems)
	}
}

// A log cut short (job killed, upload of a partial file): what was printed
// still counts, the shard is UNPARSED, and nothing is a pass by default.
func TestTruncatedLogIsUnparsed(t *testing.T) {
	c := sampleCase("trunc")
	body := corpus(t, "good")
	cut := strings.Index(body, "=== RUN   TestSub\n")
	if cut < 0 {
		t.Fatal("corpus changed: no TestSub")
	}
	body = body[:cut+len("=== RUN   TestSub\n=== RUN   TestSub/one\n")]
	r := judgeOne(t, c, body, "", nil) // no trailer
	s := r.Shards[0]
	for _, want := range []string{reasonNoTrailer, reasonUnterminated, reasonRanWithoutVerdict} {
		if !hasReason(s.Reasons, want) {
			t.Errorf("reasons %v lack %s", s.Reasons, want)
		}
	}
	if s.Status != statusUnparsed {
		t.Errorf("status %s, want UNPARSED", s.Status)
	}
	if got := row(t, r, "(no package result)", "TestSkip", "x86"); got.Skip != 1 {
		t.Errorf("TestSkip before the cut: %+v, want skip 1", got)
	}
}

// A test that prints without a newline glues go test's verdict onto its own
// output. The strict rule does not count it; the shard says why.
func TestGluedVerdictIsReportedNotCounted(t *testing.T) {
	c := sampleCase("glued")
	c.Packages, c.Count = []string{"./a"}, 1
	r := judgeOne(t, c, corpus(t, "glued"), "0", nil)
	s := r.Shards[0]
	if s.Status != statusUnparsed || !hasReason(s.Reasons, reasonGlued) || !hasReason(s.Reasons, reasonRanWithoutVerdict) {
		t.Fatalf("shard %s %v", s.Status, s.Reasons)
	}
	if got := row(t, r, pkgA, "TestNoNewline", "x86"); got.Pass != 0 || got.NoVerdict != 1 {
		t.Errorf("TestNoNewline %+v, want pass 0 and one run without a verdict", got)
	}
}

func TestBuildFailureIsUnparsedWithTheCompilerOutput(t *testing.T) {
	c := sampleCase("build")
	c.Packages, c.Count = []string{"./b", "./broken"}, 1
	r := judgeOne(t, c, corpus(t, "build"), "1", nil)
	s := r.Shards[0]
	if s.Status != statusUnparsed || !hasReason(s.Reasons, reasonBuildFailed) {
		t.Fatalf("shard %s %v", s.Status, s.Reasons)
	}
	if !slices.ContainsFunc(r.Failures, func(f Failure) bool {
		return f.Name == "(build failed)" && strings.Contains(strings.Join(f.Lines, "\n"), "undefined: undefinedOnPurpose")
	}) {
		t.Errorf("no compiler output in the failures: %+v", r.Failures)
	}
	if got := row(t, r, pkgB, "TestOnlyB", "x86"); got.Pass != 1 {
		t.Errorf("package b still ran: %+v", got)
	}
}

func TestDataRaceFailsTheTestAndIsCounted(t *testing.T) {
	c := sampleCase("race")
	c.Packages, c.Count, c.Race = []string{"./racy"}, 1, true
	r := judgeOne(t, c, corpus(t, "race"), "1", nil)
	s := r.Shards[0]
	if s.Status != statusComplete || s.Races != 1 {
		t.Fatalf("shard %s %v races %d, want complete with 1 race report", s.Status, s.Reasons, s.Races)
	}
	got := row(t, r, "example.com/stresstally/samples/racy", "TestRacy", "x86")
	if got.Fail != 1 {
		t.Errorf("TestRacy %+v, want fail 1", got)
	}
	if !strings.Contains(strings.Join(r.Failures[0].Lines, "\n"), "WARNING: DATA RACE") {
		t.Errorf("race excerpt %q", r.Failures[0].Lines)
	}
}

// go test's exit status must agree with the verdicts in both directions.
func TestExitStatusMustAgreeWithTheVerdicts(t *testing.T) {
	c := sampleCase("exit")
	r := judgeOne(t, c, corpus(t, "good"), "1", nil)
	if !hasReason(r.Shards[0].Reasons, reasonExitMismatch) || r.Shards[0].Status != statusUnparsed {
		t.Errorf("clean output with exit 1: %s %v", r.Shards[0].Status, r.Shards[0].Reasons)
	}
	c.Packages, c.Count = []string{"./a"}, 1
	r = judgeOne(t, c, corpus(t, "fail"), "0", nil)
	if !hasReason(r.Shards[0].Reasons, reasonExitMismatch) {
		t.Errorf("FAIL verdicts with exit 0: %v", r.Shards[0].Reasons)
	}
}

func TestLogWithoutHeaderIsUnparsed(t *testing.T) {
	dir := t.TempDir()
	c := sampleCase("nohdr")
	body := corpus(t, "good") + "stress-trailer: exit=0 elapsed_s=1\n"
	if err := os.WriteFile(filepath.Join(dir, logName(c.Name, "x86", 1)), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	r := judgeCase(c, dir, testSHA)
	if s := r.Shards[0]; s.Status != statusUnparsed || !hasReason(s.Reasons, reasonNoHeader) {
		t.Errorf("shard %s %v, want UNPARSED no-header", s.Status, s.Reasons)
	}
}

func TestMissingShardFailsTheCase(t *testing.T) {
	dir := t.TempDir()
	c := sampleCase("miss")
	c.Shards, c.Arches = 2, []string{"x86", "arm64"}
	writeShard(t, dir, c, "x86", 1, corpus(t, "good"), "0", nil)
	writeShard(t, dir, c, "x86", 2, corpus(t, "good"), "0", nil)
	writeShard(t, dir, c, "arm64", 1, corpus(t, "good"), "0", nil)
	r := judgeCase(c, dir, testSHA)
	if r.Verdict != "FAIL" || !slices.Equal(r.Problems, []string{problemMissing}) {
		t.Fatalf("verdict %s problems %v, want FAIL [missing-shards]", r.Verdict, r.Problems)
	}
	if r.Shards[3].Status != statusMissing || r.Shards[3].Arch != "arm64" || r.Shards[3].Shard != 2 {
		t.Errorf("shard 4 is %+v, want arm64/2 MISSING", r.Shards[3])
	}
	if got := row(t, r, pkgA, "TestPass", "x86"); got.Pass != 4 {
		t.Errorf("x86 TestPass pass %d, want 4 (2 shards x 2 runs)", got.Pass)
	}
}

// A shard that ran in another shape tested something else: it is listed,
// fails the case, and its verdicts stay out of the tallies.
func TestWrongShapeIsItsOwnClassAndNotTallied(t *testing.T) {
	for name, override := range map[string]map[string]string{
		"memlock not applied": {"memlock_in_force": "65536:65536"},
		"other commit":        {"celeris_sha": "ffffffffffffffffffffffffffffffffffffffff"},
		"other arch":          {"machine": "aarch64"},
		"other run regexp":    {"run": "^TestOther$"},
	} {
		t.Run(name, func(t *testing.T) {
			r := judgeOne(t, sampleCase("shape"), corpus(t, "good"), "0", override)
			s := r.Shards[0]
			if s.Status != statusWrongShape || !hasReason(s.Reasons, reasonShape) {
				t.Fatalf("shard %s %v, want WRONG-SHAPE", s.Status, s.Reasons)
			}
			if len(r.Tests) != 0 {
				t.Errorf("%d rows tallied from a wrong-shape shard", len(r.Tests))
			}
			if !slices.Contains(r.Problems, problemWrongShape) {
				t.Errorf("problems %v", r.Problems)
			}
		})
	}
}

func TestRefusedShardIsWrongShape(t *testing.T) {
	body := "stress-refused: memlock in force is 65536:65536, want 8388608:8388608\n"
	r := judgeOne(t, sampleCase("refused"), body, "refused", map[string]string{"memlock_in_force": "65536:65536"})
	if s := r.Shards[0]; s.Status != statusWrongShape || !hasReason(s.Reasons, reasonRefused) {
		t.Errorf("shard %s %v", s.Status, s.Reasons)
	}
}

// A test cannot make its own shard WRONG-SHAPE by printing the refusal.
func TestRefusalTextInTestOutputIsNotARefusal(t *testing.T) {
	body := "=== RUN   TestPass\nstress-refused: memlock\n--- PASS: TestPass (0.00s)\nPASS\nok  \t" + pkgA + "\t0.1s\n"
	c := sampleCase("fakerefusal")
	c.Count = 1
	r := judgeOne(t, c, body, "0", nil)
	if s := r.Shards[0]; s.Status != statusComplete || r.Verdict != "PASS" {
		t.Errorf("shard %s %v verdict %s", s.Status, s.Reasons, r.Verdict)
	}
}

func TestOverlongLineIsTruncatedNotFatal(t *testing.T) {
	body := corpus(t, "good")
	i := strings.Index(body, "=== RUN   TestPanic\n") + len("=== RUN   TestPanic\n")
	body = body[:i] + strings.Repeat("x", 3<<20) + "\n" + body[i:]
	r := judgeOne(t, sampleCase("long"), body, "0", nil)
	if !r.OK() {
		t.Errorf("mismatches %v reasons %v", r.Mismatches, r.Shards[0].Reasons)
	}
}

func TestExpectationsCatchEveryDeviation(t *testing.T) {
	c := sampleCase("expect")
	c.Packages, c.Count = []string{"./a"}, 1
	c.Expect = Expect{
		Verdict:     "FAIL",
		Problems:    []string{problemFailures},
		ShardStatus: statusComplete,
		Tests: map[string]CountExpect{
			"TestFail":        {Pass: "0", Fail: "1", Skip: "0", NoVerdict: "0"},
			"TestSkip":        {Pass: "0", Skip: ">=1"},
			"TestSubFail/bad": {Fail: "1"},
		},
	}
	if r := judgeOne(t, c, corpus(t, "fail"), "1", nil); !r.OK() {
		t.Fatalf("the matching log is reported as mismatching: %v", r.Mismatches)
	}
	for name, mutate := range map[string]func(*Case){
		"verdict":      func(c *Case) { c.Expect.Verdict = "PASS" },
		"problems":     func(c *Case) { c.Expect.Problems = []string{} },
		"fail count":   func(c *Case) { c.Expect.Tests["TestFail"] = CountExpect{Fail: "2"} },
		"skip as pass": func(c *Case) { c.Expect.Tests["TestSkip"] = CountExpect{Pass: ">=1"} },
		"shard status": func(c *Case) { c.Expect.ShardStatus = statusUnparsed },
		"reason":       func(c *Case) { c.Expect.ShardReasons = []string{reasonTimeout} },
		"absent test":  func(c *Case) { c.Expect.Tests["TestNeverRan"] = CountExpect{Pass: "1"} },
	} {
		t.Run(name, func(t *testing.T) {
			m := c
			m.Expect.Tests = map[string]CountExpect{}
			for k, v := range c.Expect.Tests {
				m.Expect.Tests[k] = v
			}
			mutate(&m)
			if r := judgeOne(t, m, corpus(t, "fail"), "1", nil); r.OK() {
				t.Errorf("the %s mutation of the expectation still matches", name)
			}
		})
	}
}

func TestCountMatches(t *testing.T) {
	for _, c := range []struct {
		got  int
		want string
		ok   bool
	}{{0, "", true}, {3, "3", true}, {3, "2", false}, {3, ">=3", true}, {2, ">=3", false}, {1, "x", false}} {
		if countMatches(c.got, c.want) != c.ok {
			t.Errorf("countMatches(%d, %q) = %v", c.got, c.want, !c.ok)
		}
	}
}

func TestMarkdownEscapesWhatALogCanContain(t *testing.T) {
	r := CaseReport{Case: "x", Config: sampleCase("x"), Tests: []TestRow{{Name: "TestA/a|b`c", Arch: "x86", Fail: 1}},
		Failures: []Failure{{Name: "TestA", Lines: []string{"```", "\x1b[31mred\x1b[0m", "</details>"}}}}
	var b strings.Builder
	r.Markdown(&b)
	out := b.String()
	for _, bad := range []string{"a|b", "\x1b", "````"} {
		if strings.Contains(out, bad) {
			t.Errorf("markdown contains %q:\n%s", bad, out)
		}
	}
	if strings.Count(out, "```") != 2 {
		t.Errorf("fences: %d, want the excerpt's own 2", strings.Count(out, "```"))
	}
}
