package main

import (
	"fmt"
	"io"
	"regexp"
	"strings"
)

// maxSummaryBytes keeps the markdown under the 1 MiB a job summary step may
// write. The full per-test table is always in tests.tsv and report.json.
const maxSummaryBytes = 900 << 10

// say writes to the job log or a report being built. A failed write there
// is not a verdict, so its error is dropped here, in one place.
func say(w io.Writer, format string, a ...any) { _, _ = fmt.Fprintf(w, format, a...) }

func pct(p *float64) string {
	if p == nil {
		return "n/a"
	}
	return fmt.Sprintf("%.2f%%", *p*100)
}

func ci(r TestRow) string {
	if r.CILow == nil {
		return "n/a (never ran)"
	}
	return fmt.Sprintf("%s to %s", pct(r.CILow), pct(r.CIHigh))
}

var ansiRe = regexp.MustCompile("\x1b\\[[0-9;?]*[A-Za-z]")

// clean makes a log line safe inside a fenced block and a table cell.
func clean(s string) string {
	s = ansiRe.ReplaceAllString(s, "")
	s = strings.Map(func(r rune) rune {
		if r == '\t' {
			return ' '
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
	s = strings.ReplaceAll(s, "```", "'''")
	if len(s) > 300 {
		s = s[:300] + " [...]"
	}
	return s
}

func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "`", "'").Replace(clean(s))
}

// interesting is a row the summary lists up front: it failed, it skipped, or
// it ran without a verdict.
func interesting(r TestRow) bool { return r.Fail > 0 || r.Skip > 0 || r.NoVerdict > 0 }

func testTable(w *strings.Builder, rows []TestRow, limit int) int {
	w.WriteString("| test | package | arch | pass | fail | skip | no verdict | fail rate | 95% CI (exact) |\n")
	w.WriteString("|---|---|---|--:|--:|--:|--:|--:|---|\n")
	n := 0
	for _, r := range rows {
		if w.Len() > limit {
			break
		}
		say(w, "| `%s` | %s | %s | %d | %d | %d | %d | %s | %s |\n",
			cell(r.Name), cell(shortPkg(r.Package)), r.Arch, r.Pass, r.Fail, r.Skip, r.NoVerdict, pct(r.FailRate), ci(r))
		n++
	}
	return n
}

func shortPkg(p string) string {
	return strings.TrimPrefix(p, "github.com/goceleris/celeris/")
}

// Markdown renders one case for the job summary.
func (r CaseReport) Markdown(w *strings.Builder) {
	c := r.Config
	state := "as expected"
	if !r.OK() {
		state = "NOT as expected"
	}
	say(w, "## Case `%s`: %s (expected %s, %s)\n\n", c.Name, r.Verdict, c.Expect.Verdict, state)
	if c.Purpose != "" {
		say(w, "%s\n\n", clean(c.Purpose))
	}
	if len(r.Problems) > 0 {
		say(w, "Problems: %s\n\n", strings.Join(r.Problems, ", "))
	}
	for _, m := range r.Mismatches {
		say(w, "- **Mismatch:** %s\n", cell(m))
	}
	if len(r.Mismatches) > 0 {
		w.WriteString("\n")
	}
	say(w, "celeris `%s`; packages `%s`; -run `%s`; %d run(s) x %d shard(s) per arch; memlock %s; race %t; timeout %s",
		cell(r.CelerisSHA), cell(strings.Join(c.Packages, " ")), cell(c.Run), c.Count, c.Shards, c.Memlock, c.Race, c.Timeout)
	if len(c.Flags) > 0 {
		say(w, "; flags `%s`", cell(strings.Join(c.Flags, " ")))
	}
	if len(c.Env) > 0 {
		say(w, "; env `%s`", cell(strings.Join(c.Env, " ")))
	}
	w.WriteString("\n\n")

	w.WriteString("| arch | shards | complete | UNPARSED | MISSING | WRONG-SHAPE | tests | pass | fail | skip | no verdict | kernel | image |\n")
	w.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|---|\n")
	for _, t := range r.Arches {
		say(w, "| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s | %s |\n",
			t.Arch, t.Shards, t.Complete, t.Unparsed, t.Missing, t.WrongShape, t.Tests, t.Pass, t.Fail, t.Skip, t.NoVerdict,
			cell(strings.Join(t.Kernels, ", ")), cell(strings.Join(t.Images, ", ")))
	}
	w.WriteString("\nSKIP is its own column and never counts as a pass; the fail rate is fail / (pass + fail).\n\n")

	w.WriteString("### Shards\n\n| arch | shard | status | exit | pass | fail | skip | no verdict | shuffle | elapsed | reasons |\n")
	w.WriteString("|---|--:|---|--:|--:|--:|--:|--:|--:|--:|---|\n")
	for _, s := range r.Shards {
		reasons := strings.Join(s.Reasons, "; ")
		if len(s.TimedOut) > 0 {
			reasons += " (timed out in " + strings.Join(s.TimedOut, ", ") + ")"
		}
		if s.Races > 0 {
			reasons += fmt.Sprintf(" (%d DATA RACE report(s))", s.Races)
		}
		say(w, "| %s | %d | %s | %s | %d | %d | %d | %d | %s | %ss | %s |\n",
			s.Arch, s.Shard, s.Status, cell(s.Exit), s.Pass, s.Fail, s.Skip, s.NoVerdict, cell(s.Shuffle), cell(s.Elapsed), cell(reasons))
	}
	w.WriteString("\n")

	var flagged []TestRow
	for _, t := range r.Tests {
		if interesting(t) {
			flagged = append(flagged, t)
		}
	}
	if len(flagged) > 0 {
		w.WriteString("### Tests that failed, skipped or ran without a verdict\n\n")
		testTable(w, flagged, maxSummaryBytes)
		w.WriteString("\n")
	}

	if len(r.Failures) > 0 {
		w.WriteString("### First failure lines\n\n")
		for _, f := range r.Failures {
			if w.Len() > maxSummaryBytes {
				w.WriteString("(more in report.json)\n\n")
				break
			}
			say(w, "<details><summary><code>%s</code> in %s, %s shard %d (-shuffle=%s)</summary>\n\n```text\n",
				cell(f.Name), cell(shortPkg(f.Package)), f.Arch, f.Shard, cell(f.Shuffle))
			for _, l := range f.Lines {
				w.WriteString(clean(l) + "\n")
			}
			w.WriteString("```\n\n</details>\n\n")
		}
	}

	say(w, "<details><summary>All %d test rows</summary>\n\n", len(r.Tests))
	if n := testTable(w, r.Tests, maxSummaryBytes); n < len(r.Tests) {
		say(w, "\n%d more rows in tests.tsv (the job summary is capped).\n", len(r.Tests)-n)
	}
	w.WriteString("\n</details>\n\n")
}

// Text renders one case for the job log.
func (r CaseReport) Text(w io.Writer) {
	c := r.Config
	say(w, "== case %s: verdict %s (expected %s)", c.Name, r.Verdict, c.Expect.Verdict)
	if len(r.Problems) > 0 {
		say(w, "; problems: %s", strings.Join(r.Problems, ", "))
	}
	say(w, "\n")
	for _, t := range r.Arches {
		say(w, "   %-5s shards %d (complete %d, UNPARSED %d, MISSING %d, WRONG-SHAPE %d); tests %d: pass %d, fail %d, skip %d, no verdict %d; kernel %s; image %s\n",
			t.Arch, t.Shards, t.Complete, t.Unparsed, t.Missing, t.WrongShape, t.Tests, t.Pass, t.Fail, t.Skip, t.NoVerdict,
			strings.Join(t.Kernels, ","), strings.Join(t.Images, ","))
	}
	for _, s := range r.Shards {
		if s.Status != statusComplete {
			say(w, "   shard %s/%d %s: %s\n", s.Arch, s.Shard, s.Status, strings.Join(s.Reasons, "; "))
		}
	}
	say(w, "   %-60s %-5s %6s %6s %6s %6s  %-8s %s\n", "test", "arch", "pass", "fail", "skip", "noverd", "rate", "95% CI")
	for _, t := range r.Tests {
		if !interesting(t) {
			continue
		}
		say(w, "   %-60s %-5s %6d %6d %6d %6d  %-8s %s\n", t.Name, t.Arch, t.Pass, t.Fail, t.Skip, t.NoVerdict, pct(t.FailRate), ci(t))
	}
	for _, f := range r.Failures {
		say(w, "   --- first failure lines of %s (%s, %s shard %d, -shuffle=%s):\n", f.Name, shortPkg(f.Package), f.Arch, f.Shard, f.Shuffle)
		for _, l := range f.Lines {
			say(w, "       %s\n", clean(l))
		}
	}
	for _, m := range r.Mismatches {
		say(w, "   MISMATCH: %s\n", m)
	}
}

// TSV renders every test row of the case.
func (r CaseReport) TSV(w io.Writer) {
	for _, t := range r.Tests {
		f := func(p *float64) string {
			if p == nil {
				return ""
			}
			return fmt.Sprintf("%.6f", *p)
		}
		say(w, "%s\t%s\t%s\t%s\t%d\t%d\t%d\t%d\t%s\t%s\t%s\n",
			r.Case, t.Package, t.Name, t.Arch, t.Pass, t.Fail, t.Skip, t.NoVerdict, f(t.FailRate), f(t.CILow), f(t.CIHigh))
	}
}

const tsvHeader = "case\tpackage\ttest\tarch\tpass\tfail\tskip\tno_verdict\tfail_rate\tci95_low\tci95_high\n"
