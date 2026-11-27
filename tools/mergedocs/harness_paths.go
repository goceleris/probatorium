package main

import (
	"fmt"
	"path"
	"sort"
	"strings"
	"sync"
)

// R3 scopes the harness-commit rule to what can change a measurement. The path
// lists are NOT in this file: they are the four fenced blocks of the R3 section
// of REFUSALS.md (r3-pin, r3-inert, r3-measurement, r3-non-measurement), parsed
// from the embedded text, so the list that is hashed into COMPOSITION.json is
// the list that is applied.

type pathClass int

const (
	classPin            pathClass = iota // celeris pin bump: accepted
	classInert                           // tests, docs: accepted
	classMeasurement                     // executes or configures a measurement: refused
	classNonMeasurement                  // readers, validators, composer, tools: accepted
	classUnknown                         // no list matches: refused
)

// HarnessLists are the parsed globs, in the order they are applied.
type HarnessLists struct {
	Pin, Inert, Measurement, NonMeasurement []string
}

var (
	harnessListsOnce sync.Once
	harnessListsVal  HarnessLists
)

// harnessLists parses REFUSALS.md once; a malformed file is a programming error
// (the tests parse it too), never a silently empty list.
func harnessLists() HarnessLists {
	harnessListsOnce.Do(func() {
		l, err := parseHarnessLists(refusalsMD)
		if err != nil {
			panic("mergedocs: REFUSALS.md R3 lists: " + err.Error())
		}
		harnessListsVal = l
	})
	return harnessListsVal
}

// parseHarnessLists reads the four ```r3-... fenced blocks. Blank lines and
// lines starting with # are skipped; every other line is one glob.
func parseHarnessLists(md []byte) (HarnessLists, error) {
	blocks := map[string][]string{}
	cur := ""
	for _, line := range strings.Split(string(md), "\n") {
		line = strings.TrimRight(line, " \t\r")
		switch {
		case cur == "" && strings.HasPrefix(line, "```r3-"):
			cur = strings.TrimPrefix(line, "```")
			if _, dup := blocks[cur]; dup {
				return HarnessLists{}, fmt.Errorf("list %s is given twice", cur)
			}
			blocks[cur] = []string{}
		case cur != "" && line == "```":
			cur = ""
		case cur != "":
			if line == "" || strings.HasPrefix(line, "#") {
				continue
			}
			if !validGlob(line) {
				return HarnessLists{}, fmt.Errorf("list %s: bad glob %q", cur, line)
			}
			blocks[cur] = append(blocks[cur], line)
		}
	}
	if cur != "" {
		return HarnessLists{}, fmt.Errorf("list %s is not closed", cur)
	}
	var l HarnessLists
	for name, dst := range map[string]*[]string{"r3-pin": &l.Pin, "r3-inert": &l.Inert, "r3-measurement": &l.Measurement, "r3-non-measurement": &l.NonMeasurement} {
		g, ok := blocks[name]
		if !ok || len(g) == 0 {
			return HarnessLists{}, fmt.Errorf("list %s is missing or empty", name)
		}
		*dst = g
	}
	return l, nil
}

// validGlob: segments for path.Match, with ** allowed only as a whole segment.
func validGlob(g string) bool {
	if g == "" || strings.HasPrefix(g, "/") || strings.HasSuffix(g, "/") {
		return false
	}
	for _, seg := range strings.Split(g, "/") {
		if seg == "" || seg == "." || seg == ".." {
			return false
		}
		if seg == "**" {
			continue
		}
		if strings.Contains(seg, "**") {
			return false
		}
		if _, err := path.Match(seg, ""); err != nil {
			return false
		}
	}
	return true
}

// globMatch matches a slash-separated name against a glob: ** is any number of
// directories (also none) and must be followed by something, so `a/**` matches
// everything below a/ but not a itself; other segments are path.Match patterns
// that never cross a slash. The match is case sensitive.
func globMatch(pattern, name string) bool {
	return matchSegs(strings.Split(pattern, "/"), strings.Split(name, "/"))
}

func matchSegs(pat, name []string) bool {
	for len(pat) > 0 {
		if pat[0] == "**" {
			rest := pat[1:]
			if len(rest) == 0 {
				return len(name) > 0 // a/** matches below a, not a
			}
			for i := 0; i <= len(name); i++ {
				if matchSegs(rest, name[i:]) {
					return true
				}
			}
			return false
		}
		if len(name) == 0 {
			return false
		}
		if ok, err := path.Match(pat[0], name[0]); err != nil || !ok {
			return false
		}
		pat, name = pat[1:], name[1:]
	}
	return len(name) == 0
}

func firstMatch(globs []string, name string) (string, bool) {
	for _, g := range globs {
		if globMatch(g, name) {
			return g, true
		}
	}
	return "", false
}

// cleanRelative: git prints clean slash-separated relative paths; anything else
// cannot be classified safely.
func cleanRelative(name string) bool {
	return name != "" && !strings.HasPrefix(name, "/") && path.Clean(name) == name && name != "."
}

// classifyHarnessPath returns the class of one changed path and the glob that
// decided it ("" for classUnknown).
func classifyHarnessPath(name string) (pathClass, string) {
	if !cleanRelative(name) {
		return classUnknown, ""
	}
	l := harnessLists()
	if g, ok := firstMatch(l.Pin, name); ok {
		return classPin, g
	}
	if g, ok := firstMatch(l.Inert, name); ok {
		return classInert, g
	}
	if g, ok := firstMatch(l.Measurement, name); ok {
		return classMeasurement, g
	}
	if g, ok := firstMatch(l.NonMeasurement, name); ok {
		return classNonMeasurement, g
	}
	return classUnknown, ""
}

// HarnessDiff is what COMPOSITION.json records about two different, accepted
// harness commits: no file that can change a measurement differs.
type HarnessDiff struct {
	FilesChanged   int      `json:"files_changed"`
	PinBumps       []string `json:"pin_bumps,omitempty"`
	NonMeasurement []string `json:"non_measurement,omitempty"`
}

// maxNamed bounds the paths a refusal prints; the rest is counted.
const maxNamed = 12

// judgeHarnessDiff classifies every changed path. The reason is "" when none of
// them is a measurement path or an unknown path; otherwise it names the
// offenders (all of them, up to maxNamed) with the glob that refused each.
func judgeHarnessDiff(names []string) (HarnessDiff, string) {
	if len(names) == 0 {
		return HarnessDiff{}, "no file differs (the commits have identical trees)"
	}
	rec := HarnessDiff{FilesChanged: len(names)}
	var bad []string
	for _, n := range names {
		c, g := classifyHarnessPath(n)
		switch c {
		case classPin:
			rec.PinBumps = append(rec.PinBumps, n)
		case classInert, classNonMeasurement:
			rec.NonMeasurement = append(rec.NonMeasurement, n)
		case classMeasurement:
			bad = append(bad, fmt.Sprintf("%s (measurement path %s)", n, g))
		default:
			bad = append(bad, fmt.Sprintf("%s (no listed path matches it; unknown paths are refused)", n))
		}
	}
	sort.Strings(rec.PinBumps)
	sort.Strings(rec.NonMeasurement)
	if len(bad) == 0 {
		return rec, ""
	}
	sort.Strings(bad)
	total, more := len(bad), ""
	if total > maxNamed {
		more = fmt.Sprintf("; and %d more", total-maxNamed)
		bad = bad[:maxNamed]
	}
	return HarnessDiff{}, fmt.Sprintf("%d changed file(s) can change a measurement or are not classified, only celeris pin bumps and non-measurement paths may differ: %s%s", total, strings.Join(bad, "; "), more)
}

// classifyHarnessDiff returns "" when the diff is acceptable under R3.
func classifyHarnessDiff(names []string) string {
	_, why := judgeHarnessDiff(names)
	return why
}
