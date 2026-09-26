package main

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
)

// Case problems. A case PASSes only with none of them.
const (
	problemFailures   = "test-failures"
	problemUnparsed   = "unparsed-shards"
	problemMissing    = "missing-shards"
	problemWrongShape = "wrong-shape-shards"
	problemNothingRan = "nothing-ran"
)

// Expect is what a case's summary must show. A dispatch run expects only
// Verdict PASS. The self-test cases pin more: the exact problem set, exact
// per-test counts on every arch, and the status and reasons of every shard.
type Expect struct {
	Verdict string `json:"verdict"`
	// Problems, when non-nil, is the exact set of problems (an empty
	// slice means none). No omitempty: nil and empty must survive JSON.
	Problems     []string               `json:"problems"`
	Tests        map[string]CountExpect `json:"tests,omitempty"`
	ShardStatus  string                 `json:"shard_status,omitempty"`
	ShardReasons []string               `json:"shard_reasons,omitempty"`
}

// CountExpect constrains one test's counts on each arch, summed over shards
// and packages: "" is anything, "N" exactly N, ">=N" at least N.
type CountExpect struct {
	Pass      string `json:"pass,omitempty"`
	Fail      string `json:"fail,omitempty"`
	Skip      string `json:"skip,omitempty"`
	NoVerdict string `json:"no_verdict,omitempty"`
}

// ShardReport is one shard as the summary shows it.
type ShardReport struct {
	Arch       string   `json:"arch"`
	Shard      int      `json:"shard"`
	Log        string   `json:"log"`
	Status     string   `json:"status"`
	Reasons    []string `json:"reasons"`
	Notes      []string `json:"notes,omitempty"`
	Exit       string   `json:"exit"`
	Elapsed    string   `json:"elapsed_s"`
	Pass       int      `json:"pass"`
	Fail       int      `json:"fail"`
	Skip       int      `json:"skip"`
	NoVerdict  int      `json:"no_verdict"`
	Races      int      `json:"data_races"`
	TimedOut   []string `json:"timed_out,omitempty"`
	Shuffle    string   `json:"shuffle"`
	Kernel     string   `json:"kernel"`
	Image      string   `json:"image"`
	Memlock    string   `json:"memlock_in_force"`
	Nproc      string   `json:"nproc"`
	CelerisSHA string   `json:"celeris_sha"`
	GoVersion  string   `json:"go_version"`
}

// TestRow is one test (subtests included) on one arch.
type TestRow struct {
	Package   string   `json:"package"`
	Name      string   `json:"name"`
	Arch      string   `json:"arch"`
	Pass      int      `json:"pass"`
	Fail      int      `json:"fail"`
	Skip      int      `json:"skip"`
	NoVerdict int      `json:"no_verdict"`
	FailRate  *float64 `json:"fail_rate"` // fail / (pass + fail); nil when it never ran
	CILow     *float64 `json:"ci95_low"`
	CIHigh    *float64 `json:"ci95_high"`
}

// ArchTotal sums one arch of a case.
type ArchTotal struct {
	Arch       string   `json:"arch"`
	Shards     int      `json:"shards"`
	Complete   int      `json:"complete"`
	Unparsed   int      `json:"unparsed"`
	Missing    int      `json:"missing"`
	WrongShape int      `json:"wrong_shape"`
	Tests      int      `json:"tests"`
	Pass       int      `json:"pass"`
	Fail       int      `json:"fail"`
	Skip       int      `json:"skip"`
	NoVerdict  int      `json:"no_verdict"`
	Kernels    []string `json:"kernels"`
	Images     []string `json:"images"`
}

// CaseReport is the judged result of one case.
type CaseReport struct {
	Case       string        `json:"case"`
	Config     Case          `json:"config"`
	CelerisSHA string        `json:"celeris_sha"`
	Verdict    string        `json:"verdict"`
	Problems   []string      `json:"problems"`
	Mismatches []string      `json:"mismatches"`
	Arches     []ArchTotal   `json:"arches"`
	Shards     []ShardReport `json:"shards"`
	Tests      []TestRow     `json:"tests"`
	Failures   []Failure     `json:"failures"`
}

// OK reports whether the case came out as expected.
func (r CaseReport) OK() bool { return len(r.Mismatches) == 0 }

func logName(caseName, arch string, shard int) string {
	return fmt.Sprintf("%s__%s__%d.log", caseName, arch, shard)
}

// judgeCase reads every expected shard log of a case from dir and judges it.
// celerisSHA, when set, is the commit every shard must have tested.
func judgeCase(c Case, dir, celerisSHA string) CaseReport {
	return judgeCaseFrom(c, func(name string) string { return filepath.Join(dir, name) }, celerisSHA)
}

// judgeCaseFrom is judgeCase with the log paths given by locate.
func judgeCaseFrom(c Case, locate func(name string) string, celerisSHA string) CaseReport {
	rep := CaseReport{Case: c.Name, Config: c, CelerisSHA: celerisSHA, Problems: []string{}, Mismatches: []string{}}
	type rowKey struct{ pkg, name, arch string }
	rows := map[rowKey]*counts{}

	for _, arch := range c.Arches {
		for shard := 1; shard <= c.Shards; shard++ {
			name := logName(c.Name, arch, shard)
			sr := ShardReport{Arch: arch, Shard: shard, Log: name, Reasons: []string{}}
			path := locate(name)
			if _, err := os.Stat(path); errors.Is(err, fs.ErrNotExist) {
				sr.Status = statusMissing
				sr.Reasons = append(sr.Reasons, "no log: the shard job never uploaded one")
				rep.Shards = append(rep.Shards, sr)
				continue
			}
			sl := parseShardFile(path)
			h := sl.header
			sr.Exit, sr.Elapsed, sr.Races, sr.TimedOut, sr.Notes = sl.exit, sl.elapsed, sl.races, sl.timedOut, sl.notes
			sr.Shuffle, sr.Kernel, sr.Image, sr.Memlock = h["shuffle"], h["kernel"], h["image"], h["memlock_in_force"]
			sr.Nproc, sr.CelerisSHA, sr.GoVersion = h["nproc"], h["celeris_sha"], h["go_version"]
			sr.Reasons = append(sr.Reasons, sl.reasons...)
			shape := checkShape(h, c, arch, shard, celerisSHA)
			switch {
			case sl.refused != "" || sl.exit == "refused" || (len(shape) > 0 && !sl.hasReason(reasonNoHeader)):
				sr.Status = statusWrongShape
				for _, m := range shape {
					sr.Reasons = append(sr.Reasons, reasonShape+": "+m)
				}
			case len(sl.reasons) > 0:
				sr.Status = statusUnparsed
			default:
				sr.Status = statusComplete
			}
			// A wrong-shape shard ran a different experiment: its verdicts
			// are listed per shard but kept out of the case's tallies.
			if sr.Status != statusWrongShape {
				for k, v := range sl.results {
					rk := rowKey{k.Package, k.Name, arch}
					if rows[rk] == nil {
						rows[rk] = &counts{}
					}
					rows[rk].Pass += v.Pass
					rows[rk].Fail += v.Fail
					rows[rk].Skip += v.Skip
					rows[rk].NoVerdict += v.NoVerdict
				}
				for _, f := range sl.failures {
					f.Arch, f.Shard, f.Shuffle = arch, shard, h["shuffle"]
					rep.Failures = append(rep.Failures, f)
				}
			}
			for _, v := range sl.results {
				sr.Pass += v.Pass
				sr.Fail += v.Fail
				sr.Skip += v.Skip
				sr.NoVerdict += v.NoVerdict
			}
			rep.Shards = append(rep.Shards, sr)
		}
	}

	for k, v := range rows {
		row := TestRow{Package: k.pkg, Name: k.name, Arch: k.arch, Pass: v.Pass, Fail: v.Fail, Skip: v.Skip, NoVerdict: v.NoVerdict}
		if n := v.Pass + v.Fail; n > 0 {
			rate := float64(v.Fail) / float64(n)
			lo, hi := clopperPearson(v.Fail, n)
			row.FailRate, row.CILow, row.CIHigh = &rate, &lo, &hi
		}
		rep.Tests = append(rep.Tests, row)
	}
	slices.SortFunc(rep.Tests, func(a, b TestRow) int {
		return cmpStrings(a.Package, b.Package, a.Name, b.Name, archOrder(a.Arch), archOrder(b.Arch))
	})

	// Per-arch totals and the case's problems.
	var pass, fail int
	for _, arch := range c.Arches {
		t := ArchTotal{Arch: arch, Kernels: []string{}, Images: []string{}}
		for _, s := range rep.Shards {
			if s.Arch != arch {
				continue
			}
			t.Shards++
			switch s.Status {
			case statusComplete:
				t.Complete++
			case statusUnparsed:
				t.Unparsed++
			case statusMissing:
				t.Missing++
			case statusWrongShape:
				t.WrongShape++
			}
			if s.Kernel != "" && !slices.Contains(t.Kernels, s.Kernel) {
				t.Kernels = append(t.Kernels, s.Kernel)
			}
			if s.Image != "" && !slices.Contains(t.Images, s.Image) {
				t.Images = append(t.Images, s.Image)
			}
		}
		for _, r := range rep.Tests {
			if r.Arch != arch {
				continue
			}
			t.Tests++
			t.Pass += r.Pass
			t.Fail += r.Fail
			t.Skip += r.Skip
			t.NoVerdict += r.NoVerdict
		}
		pass += t.Pass
		fail += t.Fail
		rep.Arches = append(rep.Arches, t)
		addIf := func(cond bool, p string) {
			if cond && !slices.Contains(rep.Problems, p) {
				rep.Problems = append(rep.Problems, p)
			}
		}
		addIf(t.Fail > 0, problemFailures)
		addIf(t.Unparsed > 0, problemUnparsed)
		addIf(t.Missing > 0, problemMissing)
		addIf(t.WrongShape > 0, problemWrongShape)
	}
	if pass+fail == 0 {
		rep.Problems = append(rep.Problems, problemNothingRan)
	}
	slices.Sort(rep.Problems)
	rep.Verdict = "PASS"
	if len(rep.Problems) > 0 {
		rep.Verdict = "FAIL"
	}
	rep.Mismatches = checkExpect(rep, c.Expect)
	return rep
}

// checkShape compares a shard's header with what the plan asked for.
func checkShape(h map[string]string, c Case, arch string, shard int, celerisSHA string) []string {
	var bad []string
	want := func(key, value string) {
		if got, ok := h[key]; !ok || got != value {
			bad = append(bad, fmt.Sprintf("%s is %q, want %q", key, got, value))
		}
	}
	want("case", c.Name)
	want("arch", arch)
	want("shard", strconv.Itoa(shard))
	limit := memlocks[c.Memlock]
	want("memlock_in_force", limit+":"+limit)
	want("count", strconv.Itoa(c.Count))
	want("race", strconv.FormatBool(c.Race))
	want("timeout", c.Timeout)
	want("run", c.Run)
	want("packages", strings.Join(c.Packages, " "))
	want("flags", strings.Join(c.Flags, " "))
	want("env", strings.Join(c.Env, " "))
	want("machine", map[string]string{"x86": "x86_64", "arm64": "aarch64"}[arch])
	if celerisSHA != "" {
		want("celeris_sha", celerisSHA)
	}
	if c.Shuffle != "" {
		want("shuffle", c.Shuffle)
	}
	return bad
}

// checkExpect lists every way a case report differs from its expectation.
func checkExpect(r CaseReport, e Expect) []string {
	out := []string{}
	if r.Verdict != e.Verdict {
		out = append(out, fmt.Sprintf("verdict is %s, expected %s", r.Verdict, e.Verdict))
	}
	if e.Problems != nil {
		want := slices.Sorted(slices.Values(e.Problems))
		if !slices.Equal(r.Problems, want) {
			out = append(out, fmt.Sprintf("problems are %v, expected exactly %v", r.Problems, want))
		}
	}
	names := slices.Sorted(func(yield func(string) bool) {
		for n := range e.Tests {
			if !yield(n) {
				return
			}
		}
	})
	for _, name := range names {
		ce := e.Tests[name]
		for _, arch := range r.Config.Arches {
			var got counts
			for _, t := range r.Tests {
				if t.Name == name && t.Arch == arch {
					got.Pass += t.Pass
					got.Fail += t.Fail
					got.Skip += t.Skip
					got.NoVerdict += t.NoVerdict
				}
			}
			for _, f := range []struct {
				what string
				got  int
				want string
			}{{"PASS", got.Pass, ce.Pass}, {"FAIL", got.Fail, ce.Fail}, {"SKIP", got.Skip, ce.Skip}, {"no-verdict", got.NoVerdict, ce.NoVerdict}} {
				if !countMatches(f.got, f.want) {
					out = append(out, fmt.Sprintf("%s on %s: %s count is %d, expected %s", name, arch, f.what, f.got, f.want))
				}
			}
		}
	}
	for _, s := range r.Shards {
		if e.ShardStatus != "" && s.Status != e.ShardStatus {
			out = append(out, fmt.Sprintf("shard %s/%d is %s, expected %s (reasons: %s)", s.Arch, s.Shard, s.Status, e.ShardStatus, strings.Join(s.Reasons, "; ")))
		}
		for _, want := range e.ShardReasons {
			if !slices.ContainsFunc(s.Reasons, func(x string) bool { return x == want || strings.HasPrefix(x, want+":") }) {
				out = append(out, fmt.Sprintf("shard %s/%d lacks the reason %q (reasons: %s)", s.Arch, s.Shard, want, strings.Join(s.Reasons, "; ")))
			}
		}
	}
	return out
}

func countMatches(got int, want string) bool {
	switch {
	case want == "":
		return true
	case strings.HasPrefix(want, ">="):
		n, err := strconv.Atoi(strings.TrimPrefix(want, ">="))
		return err == nil && got >= n
	default:
		n, err := strconv.Atoi(want)
		return err == nil && got == n
	}
}

func archOrder(a string) string {
	if a == "x86" {
		return "0"
	}
	return "1" + a
}

func cmpStrings(pairs ...string) int {
	for i := 0; i+1 < len(pairs); i += 2 {
		if c := strings.Compare(pairs[i], pairs[i+1]); c != 0 {
			return c
		}
	}
	return 0
}
