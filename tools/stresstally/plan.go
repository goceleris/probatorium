package main

import (
	"errors"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Plan is what one run of the stress workflow will do: one or more cases,
// each a go test configuration run on some shards of some architectures, and
// what each case's summary must show for the run to pass.
//
// A workflow_dispatch run has exactly one case, "stress", built from the
// dispatch inputs and expected to PASS. A pull_request run has the fixed
// self-test cases in selfTestCases instead: every pull request that touches
// the workflow or this tool proves again, on real runner logs, that a clean
// run passes and that a failure, a skip-only run, a timeout and a run of
// nothing are each reported as what they are.
type Plan struct {
	Event      string `json:"event"`
	CelerisRef string `json:"celeris_ref"`
	Cases      []Case `json:"cases"`
}

// Case is one go test configuration.
type Case struct {
	Name     string   `json:"name"`
	Purpose  string   `json:"purpose,omitempty"`
	Packages []string `json:"packages"`
	Run      string   `json:"run"`
	Count    int      `json:"count"`
	Shards   int      `json:"shards"`
	Arches   []string `json:"arches"`
	Memlock  string   `json:"memlock"`
	Race     bool     `json:"race"`
	Timeout  string   `json:"timeout"`
	// Flags are the allow-listed go test flags from the extra input; Env
	// the allow-listed NAME=VALUE settings. Shuffle is empty for the default
	// per-shard seed, else the -shuffle value the extra input asked for.
	Flags   []string `json:"flags"`
	Env     []string `json:"env"`
	Shuffle string   `json:"shuffle,omitempty"`
	Expect  Expect   `json:"expect"`
}

// Entry is one shard job: an element of the workflow's matrix include list.
// Every value a job step reads comes from here, already validated.
type Entry struct {
	Case         string `json:"case"`
	Arch         string `json:"arch"`
	Runner       string `json:"runner"`
	Shard        int    `json:"shard"`
	Shuffle      string `json:"shuffle"`
	Packages     string `json:"packages"`
	Run          string `json:"run"`
	Count        int    `json:"count"`
	Memlock      string `json:"memlock"`
	MemlockLimit string `json:"memlock_limit"`
	Race         string `json:"race"`
	Timeout      string `json:"timeout"`
	Flags        string `json:"flags"`
	Env          string `json:"env"`
	JobTimeout   int    `json:"job_timeout"`
}

// Inputs are the raw workflow_dispatch inputs, exactly as typed.
type Inputs struct {
	CelerisRef, Packages, Run, Count, Shards, Arches, Memlock, Race, Timeout, Extra string
}

// runners maps an architecture to its GitHub-hosted runner label. x86 uses
// the label celeris's own ci.yml uses, so a flake measured here is measured
// on the image celeris CI runs; arm64 has no "latest" alias, so it names the
// image. Both are free for public repositories. Each shard records the image
// it got (ImageOS/ImageVersion, uname), and the summary prints them per arch.
var runners = map[string]string{
	"x86":   "ubuntu-latest",
	"arm64": "ubuntu-24.04-arm",
}

// memlocks maps the memlock input to the value handed to prlimit. 8m is the
// GitHub-hosted default and the shape celeris CI's unit job runs in; it is
// set explicitly anyway, because the arm64 image's default is not documented.
var memlocks = map[string]string{
	"8m":        "8388608",
	"128m":      "134217728",
	"unlimited": "unlimited",
}

const (
	maxRunLen      = 2048
	maxPackages    = 16
	maxExtraTokens = 16
	maxCount       = 1000
	maxShards      = 20
	minTimeout     = time.Second
	// A GitHub-hosted job may run 6 h. The job limit is the go test timeout
	// plus jobSlack (checkout, toolchain, compiling with -race), so go test
	// always times out first and prints its goroutine dump.
	maxTimeout = 5*time.Hour + 30*time.Minute
	jobSlack   = 20
)

var (
	// A branch, a tag, a full commit sha or refs/pull/N/head: what
	// actions/checkout and `git fetch` accept. The first character is
	// alphanumeric so the value can never be read as an option.
	refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	// A package pattern relative to the celeris module root: ./..., ./a/b,
	// ./a/b/... . No component may start with '.', so ".." cannot appear.
	pkgRe = regexp.MustCompile(`^\./(\.\.\.|[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*(/\.\.\.)?)$`)
	// A -run or -skip regex: printable ASCII without spaces. It reaches go
	// test as one argv element, so no character here can reach a shell.
	regexCharsRe = regexp.MustCompile(`^[!-~]*$`)
	numRe        = regexp.MustCompile(`^[0-9]{1,4}$`)
	cpuRe        = regexp.MustCompile(`^-cpu=[1-9][0-9]{0,2}(,[1-9][0-9]{0,2}){0,7}$`)
	parallelRe   = regexp.MustCompile(`^-parallel=[1-9][0-9]{0,2}$`)
	tagsRe       = regexp.MustCompile(`^-tags=[A-Za-z0-9_.,]{1,128}$`)
	shuffleRe    = regexp.MustCompile(`^-shuffle=(off|[0-9]{1,18})$`)
	timeoutRe    = regexp.MustCompile(`^([0-9]{1,4}h)?([0-9]{1,4}m)?([0-9]{1,5}s)?$`)
	// Environment settings for the celeris tests' own knobs, and nothing
	// else: CELERIS_* (CELERIS_REQUIRE_IOURING_WORKERS=1 turns an
	// environment skip into a failure, the way celeris CI runs its
	// skipping-forbidden steps) and issue-numbered test knobs such as
	// WS484_CONNS or DRAIN583_REPS. GO*, LD_*, PATH and the runner's own
	// variables cannot match either name pattern.
	envRe = regexp.MustCompile(`^(CELERIS_[A-Z0-9_]{1,60}|[A-Z]{2,12}[0-9]{3}_[A-Z0-9_]{1,40})=[A-Za-z0-9_.,:/+-]{0,128}$`)
)

// Flags that have their own input; naming one in extra is refused rather
// than silently overriding the input.
var ownInputFlags = []string{"-run", "-count", "-timeout", "-race", "-v"}

// planDispatch validates every workflow_dispatch input and returns the plan
// for a run of one case, "stress", that must PASS.
func planDispatch(in Inputs) (Plan, error) {
	var errs []error
	bad := func(format string, a ...any) { errs = append(errs, fmt.Errorf(format, a...)) }

	ref := strings.TrimSpace(in.CelerisRef)
	switch {
	case !refRe.MatchString(ref):
		bad("celeris_ref %q: want a branch, a tag or a full commit sha ([A-Za-z0-9._/-], starting with a letter or digit, at most 200 characters)", in.CelerisRef)
	case strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.HasSuffix(ref, "/") ||
		strings.HasSuffix(ref, ".") || strings.HasSuffix(ref, ".lock"):
		bad("celeris_ref %q is not a valid git ref name", in.CelerisRef)
	}

	pkgs := strings.Fields(in.Packages)
	switch {
	case len(pkgs) == 0:
		bad("packages: name at least one package, e.g. ./engine/iouring")
	case len(pkgs) > maxPackages:
		bad("packages: %d patterns, at most %d", len(pkgs), maxPackages)
	}
	for _, p := range pkgs {
		if !pkgRe.MatchString(p) {
			bad("packages: %q is not a package pattern under the celeris module (./dir, ./dir/..., or ./...)", p)
		}
	}
	if hasDup(pkgs) {
		bad("packages: a pattern is listed twice")
	}

	run := strings.TrimSpace(in.Run)
	if err := checkRegex("run", run); err != nil {
		errs = append(errs, err)
	}

	count, err := boundedInt("count", in.Count, 1, maxCount)
	if err != nil {
		errs = append(errs, err)
	}
	shards, err := boundedInt("shards", in.Shards, 1, maxShards)
	if err != nil {
		errs = append(errs, err)
	}

	var arches []string
	switch strings.TrimSpace(in.Arches) {
	case "x86":
		arches = []string{"x86"}
	case "arm64":
		arches = []string{"arm64"}
	case "both":
		arches = []string{"x86", "arm64"}
	default:
		bad("arches %q: want x86, arm64 or both", in.Arches)
	}

	mem := strings.TrimSpace(in.Memlock)
	if _, ok := memlocks[mem]; !ok {
		bad("memlock %q: want 8m, 128m or unlimited", in.Memlock)
	}

	var race bool
	switch strings.TrimSpace(in.Race) {
	case "true":
		race = true
	case "false":
	default:
		bad("race %q: want true or false", in.Race)
	}

	timeout := strings.TrimSpace(in.Timeout)
	if _, err := checkTimeout(timeout); err != nil {
		errs = append(errs, err)
	}

	flags, env, shuffle, extraErrs := parseExtra(in.Extra)
	errs = append(errs, extraErrs...)

	if len(errs) > 0 {
		return Plan{}, errors.Join(errs...)
	}
	return Plan{
		Event:      "workflow_dispatch",
		CelerisRef: ref,
		Cases: []Case{{
			Name:     "stress",
			Packages: pkgs,
			Run:      run,
			Count:    count,
			Shards:   shards,
			Arches:   arches,
			Memlock:  mem,
			Race:     race,
			Timeout:  timeout,
			Flags:    flags,
			Env:      env,
			Shuffle:  shuffle,
			Expect:   Expect{Verdict: "PASS"},
		}},
	}, nil
}

func hasDup(s []string) bool {
	seen := map[string]bool{}
	for _, v := range s {
		if seen[v] {
			return true
		}
		seen[v] = true
	}
	return false
}

func checkRegex(name, re string) error {
	if len(re) > maxRunLen {
		return fmt.Errorf("%s: %d characters, at most %d", name, len(re), maxRunLen)
	}
	if !regexCharsRe.MatchString(re) {
		return fmt.Errorf("%s %q: only printable ASCII without spaces is accepted", name, re)
	}
	if _, err := regexp.Compile(re); err != nil {
		return fmt.Errorf("%s %q is not a valid Go regexp: %v", name, re, err)
	}
	return nil
}

func boundedInt(name, s string, lo, hi int) (int, error) {
	s = strings.TrimSpace(s)
	if !numRe.MatchString(s) {
		return 0, fmt.Errorf("%s %q: want a whole number from %d to %d", name, s, lo, hi)
	}
	n, _ := strconv.Atoi(s)
	if n < lo || n > hi {
		return 0, fmt.Errorf("%s %d: want %d to %d", name, n, lo, hi)
	}
	return n, nil
}

func checkTimeout(s string) (time.Duration, error) {
	if s == "" || !timeoutRe.MatchString(s) {
		return 0, fmt.Errorf("timeout %q: want a go test duration such as 30m, 90s or 1h30m", s)
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return 0, fmt.Errorf("timeout %q: %v", s, err)
	}
	if d < minTimeout || d > maxTimeout {
		return 0, fmt.Errorf("timeout %s: want %s to %s (a hosted job may run 6h)", s, minTimeout, maxTimeout)
	}
	return d, nil
}

// parseExtra splits the extra input into allow-listed go test flags and
// allow-listed environment settings. Every token must match one rule
// exactly; anything else is refused by name.
func parseExtra(extra string) (flags, env []string, shuffle string, errs []error) {
	toks := strings.Fields(extra)
	if len(toks) > maxExtraTokens {
		return nil, nil, "", []error{fmt.Errorf("extra: %d tokens, at most %d", len(toks), maxExtraTokens)}
	}
	seen := map[string]bool{}
	for _, tok := range toks {
		name, _, _ := strings.Cut(tok, "=")
		if seen[name] {
			errs = append(errs, fmt.Errorf("extra: %s is given twice", name))
			continue
		}
		seen[name] = true
		switch {
		case slices.Contains(ownInputFlags, name):
			errs = append(errs, fmt.Errorf("extra: %s has its own input (or is always set); use that", name))
		case tok == "-short", tok == "-failfast":
			flags = append(flags, tok)
		case cpuRe.MatchString(tok), parallelRe.MatchString(tok), tagsRe.MatchString(tok):
			flags = append(flags, tok)
		case strings.HasPrefix(tok, "-skip="):
			re := strings.TrimPrefix(tok, "-skip=")
			if re == "" {
				errs = append(errs, errors.New("extra: -skip= needs a regexp"))
			} else if err := checkRegex("extra -skip", re); err != nil {
				errs = append(errs, err)
			} else {
				flags = append(flags, tok)
			}
		case shuffleRe.MatchString(tok):
			shuffle = strings.TrimPrefix(tok, "-shuffle=")
		case envRe.MatchString(tok):
			env = append(env, tok)
		default:
			errs = append(errs, fmt.Errorf("extra: %q is not allowed (allowed: -short -failfast -cpu=N[,N] -parallel=N -skip=REGEXP -tags=LIST -shuffle=off|N, and CELERIS_*=VALUE or issue-numbered knobs like WS484_CONNS=16)", tok))
		}
	}
	return flags, env, shuffle, errs
}

// Entries expands a plan into the shard jobs of the matrix. The shuffle seed
// is the run id times 100 plus the shard number: every shard of a run tests a
// different order, the same shard number on x86 and arm64 tests the SAME
// order (so an arch difference is not an order difference), and the seed in
// each shard's log reproduces its order with -shuffle=<seed>.
func (p Plan) Entries(runID int64) []Entry {
	var out []Entry
	for _, c := range p.Cases {
		d, _ := checkTimeout(c.Timeout)
		job := int(math.Ceil(d.Minutes())) + jobSlack
		for _, arch := range c.Arches {
			for s := 1; s <= c.Shards; s++ {
				shuffle := c.Shuffle
				if shuffle == "" {
					shuffle = strconv.FormatInt(runID*100+int64(s), 10)
				}
				out = append(out, Entry{
					Case:         c.Name,
					Arch:         arch,
					Runner:       runners[arch],
					Shard:        s,
					Shuffle:      shuffle,
					Packages:     strings.Join(c.Packages, " "),
					Run:          c.Run,
					Count:        c.Count,
					Memlock:      c.Memlock,
					MemlockLimit: memlocks[c.Memlock],
					Race:         strconv.FormatBool(c.Race),
					Timeout:      c.Timeout,
					Flags:        strings.Join(c.Flags, " "),
					Env:          strings.Join(c.Env, " "),
					JobTimeout:   job,
				})
			}
		}
	}
	return out
}

// selfTestSHA pins the celeris commit the pull_request self-test runs, so the
// self-test judges this tool and workflow, not whatever celeris main is that
// day. It is celeris main as of 2026-09-26 (celeris#687 merged).
const selfTestSHA = "9f4d89b171db7838dbcc3ece2107191bc15b25f8"

// The celeris#656 tests need two io_uring workers. At an 8 MiB memlock
// RLIMIT_MEMLOCK funds one, so they skip, and CELERIS_REQUIRE_IOURING_WORKERS=1
// turns that skip into a failure (engine/iouring/init_failure_leak_linux_test.go).
// That makes them a deterministic SKIP and a deterministic FAIL on any runner.
var celeris656 = []string{
	"TestListenClosesListenSocketsWhenEveryWorkerRingSetupFails",
	"TestListenClosesListenSocketWhenOneWorkerRingSetupFails",
	"TestListenClosesListenSocketRingAndEventfdWhenInitialSubmitFails",
}

// selfTestCases is the fixed configuration of a pull_request run. Each case
// is small; each must come out exactly as its Expect says.
func selfTestCases() []Case {
	both := []string{"x86", "arm64"}
	pkg := []string{"./engine/iouring"}
	run656 := "^(" + strings.Join(celeris656, "|") + ")$"
	each := func(c CountExpect, names ...string) map[string]CountExpect {
		m := map[string]CountExpect{}
		for _, n := range names {
			m[n] = c
		}
		return m
	}
	good := each(CountExpect{Pass: "6", Fail: "0", Skip: "0", NoVerdict: "0"},
		"TestSockaddrString", "TestSockaddrString/ipv4", "TestSockaddrString/ipv6-loopback",
		"TestParseSendZCResult", "TestUseSendZC", "TestUseSendZC/large-linked")
	good["TestAbandonedResponseCountsAsSendPeerGone"] = CountExpect{Pass: "0", Fail: "0", Skip: "6", NoVerdict: "0"}
	return []Case{
		{
			Name: "good",
			Purpose: "pure, deterministic tests with subtests, under -race, 3 runs on each of 2 shards per arch: " +
				"every one must PASS exactly 6 times per arch; one more test skips under -short and must show as SKIP, not PASS",
			Packages: pkg,
			Run:      "^(TestSockaddrString|TestParseSendZCResult|TestUseSendZC|TestAbandonedResponseCountsAsSendPeerGone)$",
			Count:    3, Shards: 2, Arches: both, Memlock: "8m", Race: true, Timeout: "5m",
			Flags:  []string{"-short"},
			Expect: Expect{Verdict: "PASS", Problems: []string{}, Tests: good, ShardStatus: statusComplete},
		},
		{
			Name:     "fail",
			Purpose:  "the celeris#656 tests at 8 MiB with CELERIS_REQUIRE_IOURING_WORKERS=1: each must FAIL once per arch, and the case must FAIL",
			Packages: pkg, Run: run656,
			Count: 1, Shards: 1, Arches: both, Memlock: "8m", Timeout: "5m",
			Env: []string{"CELERIS_REQUIRE_IOURING_WORKERS=1"},
			Expect: Expect{Verdict: "FAIL", Problems: []string{problemFailures},
				Tests: each(CountExpect{Pass: "0", Fail: "1", Skip: "0", NoVerdict: "0"}, celeris656...), ShardStatus: statusComplete},
		},
		{
			Name:     "skip",
			Purpose:  "the same tests at 8 MiB without the require variable: each must SKIP, and a run in which nothing but skips happened must not PASS",
			Packages: pkg, Run: run656,
			Count: 1, Shards: 1, Arches: both, Memlock: "8m", Timeout: "5m",
			Expect: Expect{Verdict: "FAIL", Problems: []string{problemNothingRan},
				Tests: each(CountExpect{Pass: "0", Fail: "0", Skip: "1", NoVerdict: "0"}, celeris656...), ShardStatus: statusComplete},
		},
		{
			Name: "timeout",
			Purpose: "a test that churns for 1 s and then sleeps 300 ms, under a 1 s go test timeout: the panic must make every shard UNPARSED " +
				"(timeout), with the test counted as run without a verdict",
			Packages: pkg, Run: "^TestAbandonedResponseCountsAsSendPeerGone$",
			Count: 1, Shards: 1, Arches: both, Memlock: "8m", Timeout: "1s",
			Expect: Expect{Verdict: "FAIL", Problems: []string{problemNothingRan, problemUnparsed},
				Tests:       map[string]CountExpect{"TestAbandonedResponseCountsAsSendPeerGone": {Pass: "0", Fail: "0", Skip: "0", NoVerdict: "1"}},
				ShardStatus: statusUnparsed, ShardReasons: []string{reasonTimeout}},
		},
		{
			Name:     "none",
			Purpose:  "a -run that matches no test: go test exits 0, and the shard must still be UNPARSED (no verdict lines), never a pass",
			Packages: pkg, Run: "^TestStressSelfTestMatchesNoTest$",
			Count: 1, Shards: 1, Arches: both, Memlock: "8m", Timeout: "5m",
			Expect: Expect{Verdict: "FAIL", Problems: []string{problemNothingRan, problemUnparsed},
				ShardStatus: statusUnparsed, ShardReasons: []string{reasonNoVerdicts}},
		},
	}
}

func planSelfTest() Plan {
	return Plan{Event: "pull_request", CelerisRef: selfTestSHA, Cases: selfTestCases()}
}
