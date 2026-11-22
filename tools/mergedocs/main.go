// Command mergedocs composes the two result documents of ONE architecture (the
// E1 candidate+twin dispatch and the frameworks dispatch) into the one results
// directory that `mage Publish` publishes as one cell.
//
//	go run ./tools/mergedocs --candidate DIR --peers DIR --out DIR \
//	    [--meta-candidate FILE] [--meta-peers FILE] [--anchors gnet-h1,fiber-h1,stdhttp-h1] \
//	    [--twin separate|strip|keep] [--date YYYYMMDD] [--expect-columns FILE]
//
// It is a standalone tool: no mage target, no workflow and no cluster head
// calls it. The refusal list is REFUSALS.md (frozen by sha256). It recomputes
// no rank, ratio or tally; the only numbers it adds are the anchor drift and the
// twin comparison it records in COMPOSITION.json and twin-comparison.json.
package main

import (
	"errors"
	"flag"
	"fmt"
	"os"
	"strings"
)

func main() { os.Exit(run(os.Args[1:], os.Stdout, os.Stderr)) }

func run(args []string, stdout, stderr *os.File) int {
	if len(args) > 0 && args[0] == "install" {
		return runInstall(args[1:], stdout, stderr)
	}
	fs := flag.NewFlagSet("mergedocs", flag.ContinueOnError)
	fs.SetOutput(stderr)
	var (
		cand    = fs.String("candidate", "", "run dir (or downloaded artifact dir) of the candidate+twin dispatch (d1)")
		peers   = fs.String("peers", "", "run dir (or artifact dir) of the frameworks dispatch (d2)")
		out     = fs.String("out", "", "output directory (must not exist or be empty)")
		mc      = fs.String("meta-candidate", "", "meta file of d1: JSON or key=value (harness_commit|headSha|head, profile, run_id, kernel, boot_id)")
		mp      = fs.String("meta-peers", "", "meta file of d2")
		anchors = fs.String("anchors", strings.Join(DefaultAnchors, ","), "drift anchors measured in both dispatches")
		twin    = fs.String("twin", "separate", "v1.5.11 twin columns: separate (strip + comparison sidecar), strip, keep")
		date    = fs.String("date", "", "publish date yyyymmdd, REQUIRED, the same for both architectures of the release")
		harness = fs.String("harness-repo", "", "probatorium checkout used to prove that two different harness commits differ only by the celeris pin (go.mod/go.sum)")
		expect  = fs.String("expect-columns", "", "file listing the columns the merged document must hold (one per line)")
		showRef = fs.Bool("print-refusals", false, "print REFUSALS.md and its sha256 and exit")
	)
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *showRef {
		fmt.Fprintf(stdout, "%s\nsha256 %s\n", refusalsMD, refusalsSHA256())
		return 0
	}
	if *cand == "" || *peers == "" || *out == "" {
		fmt.Fprintln(stderr, "usage: mergedocs --candidate DIR --peers DIR --out DIR (see -h)")
		return 64
	}
	opt := Options{Twin: *twin, Date: *date}
	if *harness != "" {
		opt.HarnessDiff = gitHarnessDiff(*harness)
	}
	for _, a := range strings.Split(*anchors, ",") {
		if a = strings.TrimSpace(a); a != "" {
			opt.Anchors = append(opt.Anchors, a)
		}
	}
	if *expect != "" {
		b, err := os.ReadFile(*expect)
		if err != nil {
			fmt.Fprintln(stderr, err)
			return 64
		}
		for _, l := range strings.Split(string(b), "\n") {
			if l = strings.TrimSpace(l); l != "" && !strings.HasPrefix(l, "#") {
				opt.ExpectColumns = append(opt.ExpectColumns, l)
			}
		}
	}
	c, err := LoadDispatch("candidate", *cand, *mc)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 64
	}
	p, err := LoadDispatch("peers", *peers, *mp)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 64
	}
	res, err := Compose(c, p, opt)
	var re *RefusalError
	if errors.As(err, &re) {
		fmt.Fprintln(stderr, re)
		fmt.Fprintf(stderr, "mergedocs: %d refusal(s) (%s); nothing written; refusal list sha256 %s\n", len(re.Items), strings.Join(re.IDs(), ","), refusalsSHA256())
		return 2
	}
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 64
	}
	if err := WriteOutput(res, *out); err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	printSummary(stdout, stderr, res, *out)
	return 0
}

func printSummary(stdout, stderr *os.File, res *Result, out string) {
	c := res.Comp
	fmt.Fprintf(stdout, "merged %d columns (%s) -> %s\n", c.ColumnsN, c.Arch, out)
	fmt.Fprintf(stdout, "anchors %s: max_anchor_drift %.4f (%s); tie margins per scenario: %d scenarios, widest %.4f\n",
		strings.Join(c.Anchors.Names, ","), c.MaxAnchorDrift, c.Anchors.MaxDriftAnchor, len(c.TieMarginByScenario), widestMargin(c))
	fmt.Fprintf(stdout, "twin %s: %d column(s)\n", c.Twin.Mode, len(c.Twin.Columns))
	fmt.Fprintf(stdout, "publish as ONE cell on date %s:\n  PUBLISH_RESULTS=%s/results.json BENCH_START_DATE=%s PUBLISH_RUN_ID=run-1 PUBLISH_VERSION=<version> mage publish\n", c.Publish.Date, out, c.Publish.Date)
	for _, w := range c.Warnings {
		fmt.Fprintf(stderr, "WARNING: %s\n", w)
	}
	if c.Anchors.DriftWarning {
		fmt.Fprintf(stderr, "\n############################################################\n"+
			"# ANCHOR DRIFT ABOVE %.0f%% on %s: cross-dispatch comparisons\n"+
			"# are not same-run. widest tie margin is %.4f. Tell the release bot.\n"+
			"############################################################\n", driftWarnThreshold*100, c.Arch, widestMargin(c))
	}
}

func widestMargin(c *Composition) float64 {
	w := 0.0
	for _, v := range c.TieMarginByScenario {
		if v > w {
			w = v
		}
	}
	return w
}

// runInstall: mergedocs install --composed DIR --docs DOCS_CLONE --version vX.Y.Z
func runInstall(args []string, stdout, stderr *os.File) int {
	fs := flag.NewFlagSet("mergedocs install", flag.ContinueOnError)
	fs.SetOutput(stderr)
	composed := fs.String("composed", "", "the composed output directory")
	docs := fs.String("docs", "", "goceleris/docs checkout in which mage Publish wrote the cell")
	version := fs.String("version", "", "published version (the PUBLISH_VERSION of mage Publish)")
	if err := fs.Parse(args); err != nil {
		return 64
	}
	if *composed == "" || *docs == "" || *version == "" {
		fmt.Fprintln(stderr, "usage: mergedocs install --composed DIR --docs DIR --version vX.Y.Z")
		return 64
	}
	cell, err := Install(*composed, *docs, *version)
	if err != nil {
		fmt.Fprintln(stderr, err)
		return 1
	}
	fmt.Fprintf(stdout, "installed composition.json (+ twin.json) into %s; commit them in the docs checkout\n", cell)
	return 0
}
