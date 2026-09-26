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
// self-test cases in selfTests instead, each planned from its own dispatch
// inputs by planDispatch: every pull request that touches the workflow or
// this tool proves again, on real runner logs, that a clean run passes and
// that a failure, a skip-only run, a timeout and a run of nothing are each
// reported as what they are.
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

// runners maps an architecture to its GitHub-hosted runner label. Both name
// the same Ubuntu release, so the two arches run the same OS generation; a
// floating alias (ubuntu-latest) could move x86 to a newer image while arm64
// stays, and an arch difference would then be an image difference. celeris
// CI runs on ubuntu-latest, which is ubuntu-24.04 today: when that alias
// moves, change both labels here together. Both are free for public
// repositories. Each shard records the image and kernel it got, and the
// summary warns when they differ between the arches.
var runners = map[string]string{
	"x86":   "ubuntu-24.04",
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
	// The timeout input is go test's -timeout, which go test applies to
	// EACH test binary (one per package) on its own, not to the whole
	// command; binaries run up to -p (GOMAXPROCS) at a time. So a shard of n
	// packages can legitimately run up to n timeouts end to end. The job
	// limit (jobMinutes) is therefore n timeouts plus jobSlack (checkout,
	// toolchain, compiling with -race), capped at the 360 minutes a
	// GitHub-hosted job may run; with a `...` pattern n is unknown and the
	// limit is the cap.
	maxTimeout    = 5*time.Hour + 30*time.Minute
	jobSlack      = 20
	maxJobMinutes = 360
)

var (
	// A branch, a tag, a full commit sha or refs/pull/N/head: what
	// actions/checkout and `git fetch` accept. The first character is
	// alphanumeric so the value can never be read as an option.
	refRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,199}$`)
	// A package pattern relative to the celeris module root: . (the root
	// package itself, where celeris keeps most of its adaptive and server
	// tests), ./..., ./a/b, ./a/b/... . No component may start with '.', so
	// ".." cannot appear.
	pkgRe = regexp.MustCompile(`^(\.|\./(\.\.\.|[A-Za-z0-9_][A-Za-z0-9_.-]*(/[A-Za-z0-9_][A-Za-z0-9_.-]*)*(/\.\.\.)?))$`)
	// A -run or -skip regex: printable ASCII without spaces. It reaches go
	// test as one argv element, so no character here can reach a shell.
	regexCharsRe = regexp.MustCompile(`^[!-~]*$`)
	numRe        = regexp.MustCompile(`^[0-9]{1,4}$`)
	cpuRe        = regexp.MustCompile(`^-cpu=[1-9][0-9]{0,2}(,[1-9][0-9]{0,2}){0,7}$`)
	parallelRe   = regexp.MustCompile(`^-parallel=[1-9][0-9]{0,2}$`)
	tagsRe       = regexp.MustCompile(`^-tags=[A-Za-z0-9_.,]{1,128}$`)
	shuffleRe    = regexp.MustCompile(`^-shuffle=(off|[0-9]{1,18})$`)
	timeoutRe    = regexp.MustCompile(`^([0-9]{1,4}h)?([0-9]{1,4}m)?([0-9]{1,5}s)?$`)
	// An environment setting in extra: NAME=VALUE, the name upper case (so
	// it can never be read as a flag), the value from a character set no
	// shell or go test flag parser gives a meaning to.
	envTokRe   = regexp.MustCompile(`^[A-Z][A-Z0-9_]*=`)
	envNameRe  = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,63}$`)
	envValueRe = regexp.MustCompile(`^[A-Za-z0-9_.,:/+-]{0,128}$`)
)

// celerisTestEnv lists every environment variable celeris's own tests read,
// outside the CELERIS_ namespace, at celeris 9f4d89b171db7838dbcc3ece2107191bc15b25f8.
// It was built by hand from
//
//	git grep -nE 'os\.(Getenv|LookupEnv)\(' 9f4d89b -- '*_test.go'
//
// resolving every name passed through a constant or a helper (envInt,
// envInt589). Knobs in the CELERIS_ namespace are accepted by prefix instead
// (celerisEnvPrefix), because a branch under test adds its own (the #674
// branch reads CELERIS_REQUIRE_SYNACK0). A knob outside that namespace that
// a later celeris adds must be added here, with the grep that found it.
// Nothing in this list can change which binary or library runs: PATH and
// LD_* are refused by name before the list is consulted (envRefused).
var celerisTestEnv = []string{
	"CHAOS_CONC",                   // test/conformance/memcached/cluster_failover_test.go
	"CHAOS_DURATION",               // test/conformance/memcached/cluster_failover_test.go
	"CHAOS_P99_CEILING_MS",         // test/conformance/memcached/cluster_failover_test.go
	"DEBUG_TOKEN",                  // middleware/debug/example_test.go
	"DRAIN583_REPS",                // internal/sockopts/drain_recv_tcp_linux_test.go
	"GOTEST_BACKPRESSURE",          // engine/epoll/backpressure_test.go: =1 runs a test skipped on CI as nondeterministic
	"PPROF_TOKEN",                  // middleware/pprof/example_test.go
	"SOAK_CLIENTS",                 // middleware/websocket/soak_test.go
	"SOAK_DURATION",                // middleware/websocket/soak_test.go
	"TESTING_STRICT_ALLOC_BUDGETS", // driver/*, middleware/* alloc_guard tests
	"WS482_BP",                     // middleware/websocket/pause_cancel_linux_test.go
	"WS482_BURSTS",
	"WS482_BURST_BYTES",
	"WS482_CONNS",
	"WS484_BP", // middleware/websocket/inbound_sequence_linux_test.go
	"WS484_BURSTS",
	"WS484_BURST_FRAMES",
	"WS484_CONNS",
	"WS583_BP", // middleware/websocket/server_close_drain_linux_test.go
	"WS583_CELLS",
	"WS583_CLIENT_RCVBUF",
	"WS583_CONNS",
	"WS583_ECHO_FRAMES",
	"WS583_POSTPAUSE_BYTES",
}

const celerisEnvPrefix = "CELERIS_"

// envRefused reports why a variable may never be set, whatever the lists
// say: PATH and LD_* choose which binaries and libraries run, and the
// runner's own GITHUB_*, RUNNER_* and ACTIONS_* variables steer the job
// (file commands, the runtime token, the cache mode).
func envRefused(name string) string {
	switch {
	case name == "PATH" || strings.HasPrefix(name, "LD_"):
		return "it chooses which binaries or libraries run"
	case strings.HasPrefix(name, "GITHUB_"), strings.HasPrefix(name, "RUNNER_"), strings.HasPrefix(name, "ACTIONS_"):
		return "it belongs to the runner"
	}
	return ""
}

// checkEnv validates one NAME=VALUE token of the extra input.
func checkEnv(tok string) error {
	name, value, _ := strings.Cut(tok, "=")
	switch {
	case !envNameRe.MatchString(name):
		return fmt.Errorf("extra: %q is not an environment variable name", name)
	case envRefused(name) != "":
		return fmt.Errorf("extra: %s may not be set: %s", name, envRefused(name))
	case !strings.HasPrefix(name, celerisEnvPrefix) && !slices.Contains(celerisTestEnv, name):
		return fmt.Errorf("extra: %s is not a variable celeris's tests read (allowed: CELERIS_*, or one of %s)",
			name, strings.Join(celerisTestEnv, " "))
	case strings.HasPrefix(name, celerisEnvPrefix) && len(name) == len(celerisEnvPrefix):
		return fmt.Errorf("extra: %s names no variable", name)
	case !envValueRe.MatchString(value):
		return fmt.Errorf("extra: the value of %s may use only A-Z a-z 0-9 _ . , : / + - (at most 128)", name)
	}
	return nil
}

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
		case envTokRe.MatchString(tok):
			if err := checkEnv(tok); err != nil {
				errs = append(errs, err)
			} else {
				env = append(env, tok)
			}
		default:
			errs = append(errs, fmt.Errorf("extra: %q is not allowed (allowed: -short -failfast -cpu=N[,N] -parallel=N -skip=REGEXP -tags=LIST -shuffle=off|N, "+
				"and NAME=VALUE for a variable celeris's tests read: CELERIS_*, GOTEST_BACKPRESSURE, WS484_CONNS, ...)", tok))
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
		job := jobMinutes(c)
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

// jobMinutes is a shard job's timeout-minutes. go test's -timeout bounds
// each test binary, one per package, and a shard runs one binary per package
// the patterns match; so the longest a shard's go test can legitimately run
// is one timeout per package (fewer when -p runs them side by side, never
// more). With a `...` pattern the number of packages is not known here, and
// the job gets the hosted maximum.
func jobMinutes(c Case) int {
	d, err := checkTimeout(c.Timeout)
	if err != nil {
		return maxJobMinutes
	}
	per := int(math.Ceil(d.Minutes()))
	for _, p := range c.Packages {
		if strings.HasSuffix(p, "...") {
			return maxJobMinutes
		}
	}
	return min(maxJobMinutes, len(c.Packages)*per+jobSlack)
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

// selfTest is one case of the pull_request self-test. It is written as the
// dispatch inputs a person would type, keyed exactly as the workflow passes
// them to `stresstally plan` (IN_*), and it is planned by the same code a
// dispatch is: inputsFrom, then planDispatch, then Entries. Only its name,
// its purpose and its expected outcome are added afterwards. So every pull
// request that touches the workflow or this tool runs the dispatch path's
// input handling and validation on a runner, not a parallel copy of it.
type selfTest struct {
	name, purpose string
	inputs        map[string]string
	expect        Expect
}

// selfTestInputs is a dispatch of the pinned celeris commit's
// ./engine/iouring on both arches at celeris CI's 8 MiB memlock.
func selfTestInputs(run, count, shards, race, timeout, extra string) map[string]string {
	return map[string]string{
		"IN_CELERIS_REF": selfTestSHA, "IN_PACKAGES": "./engine/iouring", "IN_RUN": run,
		"IN_COUNT": count, "IN_SHARDS": shards, "IN_ARCHES": "both", "IN_MEMLOCK": "8m",
		"IN_RACE": race, "IN_TIMEOUT": timeout, "IN_EXTRA": extra,
	}
}

// selfTests is the fixed configuration of a pull_request run. Each case is
// small; each must come out exactly as its expectation says, counts of
// iterations and of processes included. A process is one test binary: one
// package in one shard.
func selfTests() []selfTest {
	run656 := "^(" + strings.Join(celeris656, "|") + ")$"
	each := func(c CountExpect, names ...string) map[string]CountExpect {
		m := map[string]CountExpect{}
		for _, n := range names {
			m[n] = c
		}
		return m
	}
	good := each(CountExpect{Pass: "6", Fail: "0", Skip: "0", NoVerdict: "0", Processes: "2", FailedProcesses: "0"},
		"TestSockaddrString", "TestSockaddrString/ipv4", "TestSockaddrString/ipv6-loopback",
		"TestParseSendZCResult", "TestUseSendZC", "TestUseSendZC/large-linked")
	good["TestAbandonedResponseCountsAsSendPeerGone"] = CountExpect{Pass: "0", Fail: "0", Skip: "6", NoVerdict: "0", Processes: "0", FailedProcesses: "0"}
	return []selfTest{
		{
			name: "good",
			purpose: "pure, deterministic tests with subtests, under -race, 3 runs in each of 2 shards per arch: " +
				"every one must PASS exactly 6 times in 2 processes per arch; one more test skips under -short and must show as SKIP, not PASS",
			inputs: selfTestInputs("^(TestSockaddrString|TestParseSendZCResult|TestUseSendZC|TestAbandonedResponseCountsAsSendPeerGone)$",
				"3", "2", "true", "5m", "-short"),
			expect: Expect{Verdict: "PASS", Problems: []string{}, Tests: good, ShardStatus: statusComplete},
		},
		{
			name: "fail",
			purpose: "the celeris#656 tests at 8 MiB with CELERIS_REQUIRE_IOURING_WORKERS=1, 2 runs in one process per arch: " +
				"each must FAIL twice per arch, in 1 failing process of 1, and the case must FAIL",
			inputs: selfTestInputs(run656, "2", "1", "false", "5m", "CELERIS_REQUIRE_IOURING_WORKERS=1"),
			expect: Expect{Verdict: "FAIL", Problems: []string{problemFailures},
				Tests:       each(CountExpect{Pass: "0", Fail: "2", Skip: "0", NoVerdict: "0", Processes: "1", FailedProcesses: "1"}, celeris656...),
				ShardStatus: statusComplete},
		},
		{
			name:    "skip",
			purpose: "the same tests at 8 MiB without the require variable: each must SKIP, and a run in which nothing but skips happened must not PASS",
			inputs:  selfTestInputs(run656, "1", "1", "false", "5m", ""),
			expect: Expect{Verdict: "FAIL", Problems: []string{problemNothingRan},
				Tests:       each(CountExpect{Pass: "0", Fail: "0", Skip: "1", NoVerdict: "0", Processes: "0", FailedProcesses: "0"}, celeris656...),
				ShardStatus: statusComplete},
		},
		{
			name: "timeout",
			purpose: "a test that churns for 1 s and then sleeps 300 ms, under a 1 s go test timeout: the panic must make every shard UNPARSED " +
				"(timeout), with the test counted as run without a verdict",
			inputs: selfTestInputs("^TestAbandonedResponseCountsAsSendPeerGone$", "1", "1", "false", "1s", ""),
			expect: Expect{Verdict: "FAIL", Problems: []string{problemNothingRan, problemUnparsed},
				Tests: map[string]CountExpect{"TestAbandonedResponseCountsAsSendPeerGone": {
					Pass: "0", Fail: "0", Skip: "0", NoVerdict: "1", Processes: "0", FailedProcesses: "0"}},
				ShardStatus: statusUnparsed, ShardReasons: []string{reasonTimeout}},
		},
		{
			name:    "none",
			purpose: "a -run that matches no test: go test exits 0, and the shard must still be UNPARSED (no verdict lines), never a pass",
			inputs:  selfTestInputs("^TestStressSelfTestMatchesNoTest$", "1", "1", "false", "5m", ""),
			expect: Expect{Verdict: "FAIL", Problems: []string{problemNothingRan, problemUnparsed},
				ShardStatus: statusUnparsed, ShardReasons: []string{reasonNoVerdicts}},
		},
	}
}

// planSelfTest plans every self-test case through the dispatch path.
func planSelfTest() (Plan, error) {
	p := Plan{Event: "pull_request", CelerisRef: selfTestSHA}
	for _, st := range selfTests() {
		d, err := planDispatch(inputsFrom(func(k string) string { return st.inputs[k] }))
		if err != nil {
			return Plan{}, fmt.Errorf("self-test case %s is not a valid dispatch: %w", st.name, err)
		}
		if d.CelerisRef != p.CelerisRef || len(d.Cases) != 1 {
			return Plan{}, fmt.Errorf("self-test case %s planned %d case(s) of celeris %s", st.name, len(d.Cases), d.CelerisRef)
		}
		c := d.Cases[0]
		c.Name, c.Purpose, c.Expect = st.name, st.purpose, st.expect
		p.Cases = append(p.Cases, c)
	}
	return p, nil
}
