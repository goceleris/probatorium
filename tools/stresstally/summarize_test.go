package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		for _, n := range celeris656 {
			b.WriteString(verdictBlock(n, "FAIL"))
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
	p := planSelfTest()
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
	p := planSelfTest()
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

// A dispatch run: the one case must PASS; any failure turns it red and is
// listed with its first lines.
func TestSummarizeDispatch(t *testing.T) {
	p, err := planDispatch(goodInputs())
	if err != nil {
		t.Fatal(err)
	}
	p.CelerisRef = testSHA
	c := p.Cases[0]
	c.Shards = 2
	p.Cases[0] = c
	body := func(result string) string {
		return verdictBlock("TestDriverHTTPZeroOverhead", result) + map[string]string{
			"PASS": "PASS\nok  \t" + celerisPkg + "\t1s\n",
			"FAIL": "FAIL\nFAIL\t" + celerisPkg + "\t1s\nFAIL\n",
		}[result]
	}
	for _, tc := range []struct {
		name  string
		shard map[int]string
		code  int
	}{{"all pass", map[int]string{1: "PASS", 2: "PASS"}, 0}, {"one fails", map[int]string{1: "PASS", 2: "FAIL"}, 1}} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for _, arch := range c.Arches {
				for s, res := range tc.shard {
					exit := map[string]string{"PASS": "0", "FAIL": "1"}[res]
					writeShard(t, dir, c, arch, s, body(res), exit, nil)
				}
			}
			code, out, _ := runSummarize(t, p, dir)
			if code != tc.code {
				t.Fatalf("exit %d, want %d:\n%s", code, tc.code, out)
			}
			if tc.code == 1 && (!strings.Contains(out, "something broke in TestDriverHTTPZeroOverhead") ||
				!strings.Contains(out, "| `TestDriverHTTPZeroOverhead` | engine/iouring | x86 | 1 | 1 | 0 | 0 | 50.00% | 1.26% to 98.74% |")) {
				t.Errorf("the failure is not listed with its lines and rate:\n%s", out)
			}
		})
	}
}
