package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
)

// The only lines that count. A verdict is `--- PASS: `, `--- FAIL: ` or
// `--- SKIP: ` at the start of a line (indented for a subtest), then the test
// name, then " (" and the elapsed time. The name is taken up to that " (":
// go test never ends a verdict line at the name, so nothing is anchored there.
// A verdict glued to the end of a test's own unterminated output is NOT
// counted; it is reported (reasonGlued) and the test shows as run without a
// verdict, because a tally that guessed would be guessing.
var (
	verdictRe  = regexp.MustCompile(`^\s*--- (PASS|FAIL|SKIP): (\S+) \(`)
	gluedRe    = regexp.MustCompile(`\S--- (PASS|FAIL|SKIP): (\S+) \(`)
	runRe      = regexp.MustCompile(`^=== RUN   (\S+)`)
	markerRe   = regexp.MustCompile(`^=== (?:RUN|NAME|CONT|PAUSE)\s+(\S+)`)
	okPkgRe    = regexp.MustCompile("^ok  \t(\\S+)")
	failPkgRe  = regexp.MustCompile("^FAIL\t(\\S+)")
	noFilesRe  = regexp.MustCompile("^\\?   \t(\\S+)\t\\[no test files\\]")
	brokenRe   = regexp.MustCompile("^FAIL\t(\\S+) \\[(build|setup) failed\\]")
	panicRe    = regexp.MustCompile(`^panic: `)
	timeoutRe2 = regexp.MustCompile(`^panic: test timed out after `)
	fatalRe    = regexp.MustCompile(`^fatal error: `)
	signalRe   = regexp.MustCompile(`^signal: `)
	raceRe     = regexp.MustCompile(`^WARNING: DATA RACE`)
	runningRe  = regexp.MustCompile(`^\t\t(\S+) \(`)
	trailerRe  = regexp.MustCompile(`^stress-trailer: exit=(\S+) elapsed_s=([0-9]+)$`)
)

// Shard statuses. Only a complete shard is a clean one; every other status
// makes its case fail, and none of them is ever a pass.
const (
	statusComplete   = "complete"
	statusUnparsed   = "UNPARSED"
	statusMissing    = "MISSING"
	statusWrongShape = "WRONG-SHAPE"
)

// Reasons a shard is UNPARSED (or WRONG-SHAPE).
const (
	reasonNoHeader          = "no-header"
	reasonNoTrailer         = "no-trailer"
	reasonNoVerdicts        = "no-verdicts"
	reasonPanic             = "panic"
	reasonTimeout           = "timeout"
	reasonFatal             = "fatal-error"
	reasonSignal            = "killed-by-signal"
	reasonBuildFailed       = "build-failed"
	reasonUnterminated      = "no-package-result"
	reasonRanWithoutVerdict = "ran-without-verdict"
	reasonVerdictWithoutRun = "verdict-without-run"
	reasonGlued             = "glued-verdict"
	reasonPkgFailNoTest     = "package-failed-without-a-failing-test"
	reasonExitMismatch      = "exit-status-disagrees"
	reasonUnreadable        = "unreadable"
	reasonRefused           = "refused"
	reasonShape             = "shape-mismatch"
)

type testKey struct {
	Package, Name string
}

type counts struct {
	Pass, Fail, Skip, NoVerdict int
}

// Failure is the first failure lines of one FAIL verdict (or of a panic).
type Failure struct {
	Package string   `json:"package"`
	Name    string   `json:"name"`
	Arch    string   `json:"arch"`
	Shard   int      `json:"shard"`
	Shuffle string   `json:"shuffle"`
	Lines   []string `json:"lines"`
}

// shardLog is everything read from one shard's log.
type shardLog struct {
	header   map[string]string
	exit     string // go test's exit status from the trailer; "" if none
	elapsed  string
	refused  string
	results  map[testKey]*counts
	reasons  []string
	notes    []string
	failures []Failure
	races    int
	timedOut []string // tests running when go test's timeout fired
}

func (s *shardLog) reason(r string, detail ...string) {
	msg := r
	if len(detail) > 0 && detail[0] != "" {
		msg = r + ": " + strings.Join(detail, " ")
	}
	if !slices.Contains(s.reasons, msg) {
		s.reasons = append(s.reasons, msg)
	}
}

func (s *shardLog) hasReason(r string) bool {
	for _, x := range s.reasons {
		if x == r || strings.HasPrefix(x, r+":") {
			return true
		}
	}
	return false
}

const (
	maxLineBytes   = 64 << 10 // a longer line is kept truncated
	ringLines      = 400      // how far back a failure excerpt may reach
	excerptLines   = 30
	panicContext   = 16
	excerptsPerKey = 1
)

// parseShardFile reads one shard log. A missing file is the caller's problem;
// an unreadable one is an UNPARSED shard.
func parseShardFile(path string) *shardLog {
	f, err := os.Open(path)
	if err != nil {
		s := newShardLog()
		s.reason(reasonUnreadable, err.Error())
		return s
	}
	defer func() { _ = f.Close() }()
	return parseShard(f)
}

func newShardLog() *shardLog {
	return &shardLog{header: map[string]string{}, results: map[testKey]*counts{}}
}

type pendingVerdict struct {
	name, result string
}

// parseShard reads a shard log: the stress-header block, go test's output,
// and the trailer, which must be the last non-empty line.
func parseShard(r io.Reader) *shardLog {
	s := newShardLog()
	br := bufio.NewReaderSize(r, 1<<16)

	var (
		headerState  = 0 // 0 reading the header, 1 header complete, -1 header missing or broken
		lastNonEmpty string
		verdicts     int
		pending      []pendingVerdict
		pendingRuns  = map[string]int{}
		ring         []string
		panicLeft    int
		panicLines   []string
		panicLabel   string
		inRunning    bool
	)
	endPanic := func() {
		if len(panicLines) > 0 {
			s.failures = append(s.failures, Failure{Name: panicLabel, Lines: panicLines})
		}
		panicLeft, panicLines, inRunning = 0, nil, false
	}
	startPanic := func(label, line string) {
		endPanic()
		panicLeft, panicLines, panicLabel = panicContext, []string{line}, label
	}
	flush := func(pkg string) {
		endPanic()
		for _, v := range pending {
			c := s.counts(testKey{pkg, v.name})
			switch v.result {
			case "PASS":
				c.Pass++
			case "FAIL":
				c.Fail++
			case "SKIP":
				c.Skip++
			}
			pendingRuns[v.name]--
		}
		for name, n := range pendingRuns {
			switch {
			case n > 0:
				s.counts(testKey{pkg, name}).NoVerdict += n
				s.reason(reasonRanWithoutVerdict)
			case n < 0:
				s.reason(reasonVerdictWithoutRun)
			}
		}
		pending = nil
		clear(pendingRuns)
		for i := range s.failures {
			if s.failures[i].Package == "" {
				s.failures[i].Package = pkg
			}
		}
	}

	for {
		line, err := readLine(br)
		if err != nil && line == "" {
			if err != io.EOF {
				s.reason(reasonUnreadable, err.Error())
			}
			break
		}
		line = strings.TrimSuffix(line, "\r")
		if strings.TrimSpace(line) != "" {
			lastNonEmpty = line
		}

		if headerState == 0 {
			if rest, ok := strings.CutPrefix(line, "stress-header: "); ok {
				k, v, _ := strings.Cut(rest, "=")
				s.header[k] = v
				continue
			}
			headerState = -1
			if line == "stress-header-end" && len(s.header) > 0 {
				headerState = 1
				continue
			}
		}
		if rest, ok := strings.CutPrefix(line, "stress-refused: "); ok {
			s.refused = rest
			continue
		}
		if strings.HasPrefix(line, "stress-trailer: ") {
			continue // judged below, and only if it is the last line
		}

		// The lines after a panic are its excerpt; a timeout also names
		// the tests it interrupted.
		if panicLeft > 0 && len(panicLines) > 0 && !okPkgRe.MatchString(line) && !failPkgRe.MatchString(line) {
			panicLines = append(panicLines, line)
			panicLeft--
			if strings.TrimSpace(line) == "running tests:" {
				inRunning = true
			} else if inRunning {
				if m := runningRe.FindStringSubmatch(line); m != nil {
					s.timedOut = append(s.timedOut, m[1])
				} else {
					inRunning = false
				}
			}
			if panicLeft == 0 {
				endPanic()
			}
		}

		switch {
		case verdictRe.MatchString(line):
			m := verdictRe.FindStringSubmatch(line)
			verdicts++
			pending = append(pending, pendingVerdict{name: m[2], result: m[1]})
			if m[1] == "FAIL" {
				s.addFailure(m[2], excerpt(ring, m[2]))
			}
		case runRe.MatchString(line):
			pendingRuns[runRe.FindStringSubmatch(line)[1]]++
		case brokenRe.MatchString(line):
			m := brokenRe.FindStringSubmatch(line)
			s.reason(reasonBuildFailed, m[1])
			s.failures = append(s.failures, Failure{Package: m[1], Name: "(build failed)", Lines: buildErrors(ring, m[1])})
			flush(m[1])
		case okPkgRe.MatchString(line):
			pkg := okPkgRe.FindStringSubmatch(line)[1]
			if strings.Contains(line, "[no tests to run]") {
				s.notes = append(s.notes, "no tests to run in "+pkg)
			}
			flush(pkg)
		case failPkgRe.MatchString(line):
			pkg := failPkgRe.FindStringSubmatch(line)[1]
			if !slices.ContainsFunc(pending, func(v pendingVerdict) bool { return v.result == "FAIL" }) {
				s.reason(reasonPkgFailNoTest, pkg)
			}
			flush(pkg)
		case noFilesRe.MatchString(line):
			s.notes = append(s.notes, "no test files in "+noFilesRe.FindStringSubmatch(line)[1])
		case timeoutRe2.MatchString(line):
			s.reason(reasonTimeout)
			startPanic("(timeout)", line)
		case panicRe.MatchString(line):
			s.reason(reasonPanic)
			startPanic("(panic)", line)
		case fatalRe.MatchString(line):
			s.reason(reasonFatal)
			startPanic("(fatal error)", line)
		case signalRe.MatchString(line):
			s.reason(reasonSignal, strings.TrimPrefix(line, "signal: "))
		case raceRe.MatchString(line):
			s.races++
		}
		if m := gluedRe.FindStringSubmatch(line); m != nil && !verdictRe.MatchString(line) {
			s.reason(reasonGlued, m[2])
		}

		ring = append(ring, line)
		if len(ring) > ringLines {
			ring = ring[len(ring)-ringLines:]
		}
	}

	if headerState != 1 {
		s.reason(reasonNoHeader)
	}
	if len(pending) > 0 || len(pendingRuns) > 0 {
		// Test output that no package result line closed: go test (or
		// its log) was cut off.
		s.reason(reasonUnterminated)
		flush("(no package result)")
	}
	endPanic()
	for i := range s.failures {
		if s.failures[i].Package == "" {
			s.failures[i].Package = "(no package result)"
		}
	}

	if m := trailerRe.FindStringSubmatch(lastNonEmpty); m != nil {
		s.exit, s.elapsed = m[1], m[2]
	} else {
		s.reason(reasonNoTrailer)
	}
	if s.exit == "refused" || s.refused != "" {
		s.reason(reasonRefused, s.refused)
	}
	if verdicts == 0 && s.refused == "" {
		s.reason(reasonNoVerdicts)
	}

	// go test's own exit status must agree with what the verdicts say.
	fails := 0
	for _, c := range s.results {
		fails += c.Fail
	}
	if s.exit != "" && s.exit != "refused" {
		code, err := strconv.Atoi(s.exit)
		switch {
		case err != nil:
			s.reason(reasonExitMismatch, "unreadable exit status "+s.exit)
		case code == 0 && fails > 0:
			s.reason(reasonExitMismatch, fmt.Sprintf("exit 0 with %d FAIL verdict(s)", fails))
		case code != 0 && fails == 0 && len(s.reasons) == 0:
			s.reason(reasonExitMismatch, fmt.Sprintf("exit %d without a FAIL verdict", code))
		}
	}
	return s
}

func (s *shardLog) counts(k testKey) *counts {
	c := s.results[k]
	if c == nil {
		c = &counts{}
		s.results[k] = c
	}
	return c
}

// addFailure keeps the first failure excerpt of each test in this shard. Its
// package is filled in when the package result line arrives.
func (s *shardLog) addFailure(name string, lines []string) {
	n := 0
	for _, f := range s.failures {
		if f.Name == name {
			n++
		}
	}
	if n < excerptsPerKey {
		s.failures = append(s.failures, Failure{Name: name, Lines: lines})
	}
}

// excerpt returns the first lines a failing test printed: the output after
// its most recent RUN/NAME/CONT line, without go test's own marker and
// verdict lines. If the marker scrolled out of the ring, the last lines
// before the verdict stand in.
func excerpt(ring []string, name string) []string {
	start := -1
	for i := len(ring) - 1; i >= 0; i-- {
		if m := markerRe.FindStringSubmatch(ring[i]); m != nil && m[1] == name {
			start = i + 1
			break
		}
	}
	span := ring
	if start >= 0 {
		span = ring[start:]
	} else if len(span) > excerptLines {
		span = span[len(span)-excerptLines:]
	}
	var out []string
	for _, l := range span {
		if markerRe.MatchString(l) || verdictRe.MatchString(l) {
			continue
		}
		out = append(out, l)
		if len(out) == excerptLines {
			break
		}
	}
	return out
}

// buildErrors returns the compiler output go test printed for pkg: the lines
// after its "# pkg" banner that are not another package's test output.
func buildErrors(ring []string, pkg string) []string {
	start := -1
	for i := len(ring) - 1; i >= 0; i-- {
		if strings.HasPrefix(ring[i], "# "+pkg) {
			start = i
			break
		}
	}
	if start < 0 {
		return nil
	}
	out := []string{ring[start]}
	for _, l := range ring[start+1:] {
		if strings.HasPrefix(l, "=== ") || verdictRe.MatchString(l) || okPkgRe.MatchString(l) ||
			failPkgRe.MatchString(l) || l == "PASS" || l == "FAIL" || strings.HasPrefix(l, "# ") {
			break
		}
		out = append(out, l)
		if len(out) == excerptLines {
			break
		}
	}
	return out
}

// readLine returns one line without its newline. A line longer than
// maxLineBytes is truncated rather than failing the whole log.
func readLine(br *bufio.Reader) (string, error) {
	var b []byte
	for {
		chunk, isPrefix, err := br.ReadLine()
		if len(b) < maxLineBytes {
			b = append(b, chunk[:min(len(chunk), maxLineBytes-len(b))]...)
		}
		if err != nil {
			return string(b), err
		}
		if !isPrefix {
			return string(b), nil
		}
	}
}
