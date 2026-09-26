package main

import (
	"fmt"
	"html"
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

// ci is the exact 95% interval of the per-process fail rate.
func ci(r TestRow) string {
	if r.ProcCILow == nil {
		return "n/a (no process ran it)"
	}
	return fmt.Sprintf("%s to %s", pct(r.ProcCILow), pct(r.ProcCIHigh))
}

// procs is "failed / ran" in processes.
func procs(r TestRow) string { return fmt.Sprintf("%d / %d", r.FailedProcesses, r.Processes) }

// rateNote explains the two kinds of count every table shows.
const rateNote = "A process is one test binary: one package in one shard. Its -count iterations run inside that one process " +
	"and share its state, so they are not independent trials; the fail rate and its exact (Clopper-Pearson) 95% interval " +
	"are per process: processes in which the test failed at least once, over processes in which it reached a PASS or FAIL. " +
	"pass, fail, skip and no verdict count iterations (verdict lines) and carry no interval. SKIP is its own column and never counts as a pass.\n\n"

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

// inline makes a value safe inside a code span outside a table.
func inline(s string) string { return strings.ReplaceAll(clean(s), "`", "'") }

// cell makes a value safe in a table cell or a line of the summary. Much of
// it comes from the log of the code under test, which may be anyone's pull
// request, so link and HTML syntax is escaped and renders as text.
func cell(s string) string {
	return strings.NewReplacer("|", `\|`, "`", "'", "[", `\[`, "]", `\]`, "<", "&lt;", ">", "&gt;").Replace(clean(s))
}

// codeCell makes a value safe in a code span inside a table cell, where
// Markdown and HTML are inert already and a backslash escape would show.
func codeCell(s string) string {
	return strings.NewReplacer("|", `\|`, "`", "'").Replace(clean(s))
}

// interesting is a row the summary lists up front: it failed, it skipped, or
// it ran without a verdict.
func interesting(r TestRow) bool { return r.Fail > 0 || r.Skip > 0 || r.NoVerdict > 0 }

func testTable(w *strings.Builder, rows []TestRow, limit int) int {
	w.WriteString("| test | package | arch | processes failed / ran | per-process fail rate | 95% CI (exact) | pass | fail | skip | no verdict |\n")
	w.WriteString("|---|---|---|--:|--:|---|--:|--:|--:|--:|\n")
	n := 0
	for _, r := range rows {
		if w.Len() > limit {
			break
		}
		say(w, "| `%s` | %s | %s | %s | %s | %s | %d | %d | %d | %d |\n",
			codeCell(r.Name), cell(shortPkg(r.Package)), r.Arch, procs(r), pct(r.ProcFailRate), ci(r), r.Pass, r.Fail, r.Skip, r.NoVerdict)
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
	for _, g := range r.ArchGaps {
		say(w, "- **Arch gap:** %s\n", cell(g))
	}
	if len(r.ArchGaps) > 0 {
		w.WriteString("\n")
	}
	for _, x := range r.Warnings {
		say(w, "- **Warning:** %s\n", cell(x))
	}
	if len(r.Warnings) > 0 {
		w.WriteString("\n")
	}
	say(w, "celeris `%s`; packages `%s`; -run `%s`; %d run(s) x %d shard(s) per arch; memlock %s; race %t; timeout %s per test binary",
		inline(r.CelerisSHA), inline(strings.Join(c.Packages, " ")), inline(c.Run), c.Count, c.Shards, c.Memlock, c.Race, c.Timeout)
	if len(c.Flags) > 0 {
		say(w, "; flags `%s`", inline(strings.Join(c.Flags, " ")))
	}
	if len(c.Env) > 0 {
		say(w, "; env `%s`", inline(strings.Join(c.Env, " ")))
	}
	w.WriteString("\n\n")

	w.WriteString("| arch | shards | complete | UNPARSED | MISSING | WRONG-SHAPE | tests | pass | fail | skip | no verdict | kernel | image |\n")
	w.WriteString("|---|--:|--:|--:|--:|--:|--:|--:|--:|--:|--:|---|---|\n")
	for _, t := range r.Arches {
		say(w, "| %s | %d | %d | %d | %d | %d | %d | %d | %d | %d | %d | %s | %s |\n",
			t.Arch, t.Shards, t.Complete, t.Unparsed, t.Missing, t.WrongShape, t.Tests, t.Pass, t.Fail, t.Skip, t.NoVerdict,
			cell(strings.Join(t.Kernels, ", ")), cell(strings.Join(t.Images, ", ")))
	}
	w.WriteString("\n" + rateNote)

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
				html.EscapeString(clean(f.Name)), html.EscapeString(clean(shortPkg(f.Package))), f.Arch, f.Shard, html.EscapeString(clean(f.Shuffle)))
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
	for _, g := range r.ArchGaps {
		say(w, "   ARCH GAP: %s\n", g)
	}
	for _, x := range r.Warnings {
		say(w, "   WARNING: %s\n", x)
	}
	say(w, "   %-60s %-5s %-11s %-8s %-18s %6s %6s %6s %6s\n", "test", "arch", "procs f/ran", "rate", "95% CI (process)", "pass", "fail", "skip", "noverd")
	for _, t := range r.Tests {
		if !interesting(t) {
			continue
		}
		say(w, "   %-60s %-5s %-11s %-8s %-18s %6d %6d %6d %6d\n", t.Name, t.Arch, procs(t), pct(t.ProcFailRate), ci(t), t.Pass, t.Fail, t.Skip, t.NoVerdict)
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

// TSV renders every test row of the case: the per-process counts, rate and
// interval first, then the iteration counts.
func (r CaseReport) TSV(w io.Writer) {
	for _, t := range r.Tests {
		f := func(p *float64) string {
			if p == nil {
				return ""
			}
			return fmt.Sprintf("%.6f", *p)
		}
		say(w, "%s\t%s\t%s\t%s\t%d\t%d\t%s\t%s\t%s\t%d\t%d\t%d\t%d\n",
			r.Case, t.Package, t.Name, t.Arch, t.Processes, t.FailedProcesses, f(t.ProcFailRate), f(t.ProcCILow), f(t.ProcCIHigh),
			t.Pass, t.Fail, t.Skip, t.NoVerdict)
	}
}

const tsvHeader = "case\tpackage\ttest\tarch\tprocesses\tfailed_processes\tprocess_fail_rate\tprocess_ci95_low\tprocess_ci95_high\tpass\tfail\tskip\tno_verdict\n"
