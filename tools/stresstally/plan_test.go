package main

import (
	"encoding/json"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"testing"
)

func goodInputs() Inputs {
	return Inputs{
		CelerisRef: "main", Packages: "./engine/iouring", Run: "^TestDriverHTTPZeroOverhead$",
		Count: "5", Shards: "10", Arches: "both", Memlock: "8m", Race: "false", Timeout: "30m", Extra: "",
	}
}

func TestPlanAcceptsTheDocumentedExamples(t *testing.T) {
	for name, mod := range map[string]func(*Inputs){
		"defaults":         func(*Inputs) {},
		"full package":     func(in *Inputs) { in.Run = "" },
		"sha ref":          func(in *Inputs) { in.CelerisRef = "9f4d89b171db7838dbcc3ece2107191bc15b25f8" },
		"branch with dirs": func(in *Inputs) { in.CelerisRef = "fix/celeris-662-defer-accept" },
		"tag":              func(in *Inputs) { in.CelerisRef = "v1.6.0" },
		"pull ref":         func(in *Inputs) { in.CelerisRef = "refs/pull/674/head" },
		"many packages":    func(in *Inputs) { in.Packages = "./engine/iouring ./engine/epoll ./adaptive/... ./..." },
		"subtest regexp":   func(in *Inputs) { in.Run = `^TestA/(sub-1|sub\d+)$` },
		"raised memlock":   func(in *Inputs) { in.Memlock = "unlimited"; in.Race = "true" },
		"x86 only":         func(in *Inputs) { in.Arches = "x86" },
		"limits":           func(in *Inputs) { in.Count, in.Shards, in.Timeout = "1000", "20", "5h30m" },
		"root package":     func(in *Inputs) { in.Packages = ". ./engine/iouring" },
		"every extra": func(in *Inputs) {
			in.Extra = "-short -failfast -cpu=1,2,4 -parallel=8 -skip=^TestSlow$ -tags=integration -shuffle=off " +
				"CELERIS_REQUIRE_IOURING_WORKERS=1 WS484_CONNS=16 DRAIN583_REPS=3 GOTEST_BACKPRESSURE=1 CELERIS_REQUIRE_SYNACK0=1"
		},
	} {
		t.Run(name, func(t *testing.T) {
			in := goodInputs()
			mod(&in)
			p, err := planDispatch(in)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if len(p.Cases) != 1 || p.Cases[0].Name != "stress" || p.Cases[0].Expect.Verdict != "PASS" {
				t.Errorf("plan %+v", p)
			}
		})
	}
}

// Every input is checked before use, and anything that could reach a shell,
// name another option, or leave the allow-list is refused by name.
func TestPlanRefusesBadInputs(t *testing.T) {
	for name, c := range map[string]struct {
		mod  func(*Inputs)
		want string
	}{
		"ref as an option":     {func(in *Inputs) { in.CelerisRef = "--upload-pack=touch /tmp/x" }, "celeris_ref"},
		"ref with a command":   {func(in *Inputs) { in.CelerisRef = "main;id" }, "celeris_ref"},
		"ref substitution":     {func(in *Inputs) { in.CelerisRef = "$(id)" }, "celeris_ref"},
		"ref traversal":        {func(in *Inputs) { in.CelerisRef = "a/../b" }, "celeris_ref"},
		"ref refspec":          {func(in *Inputs) { in.CelerisRef = "main:refs/heads/x" }, "celeris_ref"},
		"empty ref":            {func(in *Inputs) { in.CelerisRef = "" }, "celeris_ref"},
		"absolute package":     {func(in *Inputs) { in.Packages = "/etc" }, "packages"},
		"package traversal":    {func(in *Inputs) { in.Packages = "./a/../../b" }, "packages"},
		"package option":       {func(in *Inputs) { in.Packages = "-exec=/bin/sh" }, "packages"},
		"remote package":       {func(in *Inputs) { in.Packages = "github.com/evil/x" }, "packages"},
		"no package":           {func(in *Inputs) { in.Packages = "  " }, "packages"},
		"duplicate package":    {func(in *Inputs) { in.Packages = "./a ./a" }, "packages"},
		"run with a space":     {func(in *Inputs) { in.Run = "TestA TestB" }, "run"},
		"run with a newline":   {func(in *Inputs) { in.Run = "TestA\nTestB" }, "run"},
		"run not a regexp":     {func(in *Inputs) { in.Run = "Test(" }, "run"},
		"run too long":         {func(in *Inputs) { in.Run = strings.Repeat("a", maxRunLen+1) }, "run"},
		"count zero":           {func(in *Inputs) { in.Count = "0" }, "count"},
		"count too big":        {func(in *Inputs) { in.Count = "1001" }, "count"},
		"count not a number":   {func(in *Inputs) { in.Count = "5; id" }, "count"},
		"shards zero":          {func(in *Inputs) { in.Shards = "0" }, "shards"},
		"shards too many":      {func(in *Inputs) { in.Shards = "21" }, "shards"},
		"arches":               {func(in *Inputs) { in.Arches = "all" }, "arches"},
		"memlock":              {func(in *Inputs) { in.Memlock = "64m" }, "memlock"},
		"race":                 {func(in *Inputs) { in.Race = "yes" }, "race"},
		"timeout zero":         {func(in *Inputs) { in.Timeout = "0s" }, "timeout"},
		"timeout too long":     {func(in *Inputs) { in.Timeout = "6h" }, "timeout"},
		"timeout unitless":     {func(in *Inputs) { in.Timeout = "10" }, "timeout"},
		"timeout days":         {func(in *Inputs) { in.Timeout = "1d" }, "timeout"},
		"extra exec":           {func(in *Inputs) { in.Extra = "-exec=/bin/sh" }, "-exec"},
		"extra toolexec":       {func(in *Inputs) { in.Extra = "-toolexec=x" }, "-toolexec"},
		"extra output file":    {func(in *Inputs) { in.Extra = "-o=/tmp/x" }, "-o=/tmp/x"},
		"extra json":           {func(in *Inputs) { in.Extra = "-json" }, "-json"},
		"extra count":          {func(in *Inputs) { in.Extra = "-count=5" }, "own input"},
		"extra run":            {func(in *Inputs) { in.Extra = "-run=X" }, "own input"},
		"extra v":              {func(in *Inputs) { in.Extra = "-v" }, "own input"},
		"extra race":           {func(in *Inputs) { in.Extra = "-race" }, "own input"},
		"extra twice":          {func(in *Inputs) { in.Extra = "-cpu=1 -cpu=2" }, "twice"},
		"extra shuffle on":     {func(in *Inputs) { in.Extra = "-shuffle=on" }, "-shuffle=on"},
		"extra bad skip":       {func(in *Inputs) { in.Extra = "-skip=(" }, "-skip"},
		"extra empty skip":     {func(in *Inputs) { in.Extra = "-skip=" }, "-skip"},
		"env GOFLAGS":          {func(in *Inputs) { in.Extra = "GOFLAGS=-toolexec=x" }, "GOFLAGS"},
		"env LD_PRELOAD":       {func(in *Inputs) { in.Extra = "LD_PRELOAD=/x.so" }, "LD_PRELOAD"},
		"env PATH":             {func(in *Inputs) { in.Extra = "PATH=/x" }, "PATH"},
		"env GITHUB":           {func(in *Inputs) { in.Extra = "GITHUB_ENV=/x" }, "GITHUB_ENV"},
		"env LD other":         {func(in *Inputs) { in.Extra = "LD_LIBRARY_PATH=/x" }, "LD_LIBRARY_PATH"},
		"env runner":           {func(in *Inputs) { in.Extra = "RUNNER_TEMP=/x" }, "RUNNER_TEMP"},
		"env cache mode":       {func(in *Inputs) { in.Extra = "ACTIONS_CACHE_MODE=write" }, "ACTIONS_CACHE_MODE"},
		"env no test reads it": {func(in *Inputs) { in.Extra = "FOO=1" }, "FOO"},
		"env unread knob":      {func(in *Inputs) { in.Extra = "WS999_CONNS=1" }, "WS999_CONNS"},
		"env GODEBUG":          {func(in *Inputs) { in.Extra = "GODEBUG=x" }, "GODEBUG"},
		"env empty celeris":    {func(in *Inputs) { in.Extra = "CELERIS_=1" }, "CELERIS_"},
		"env lower case":       {func(in *Inputs) { in.Extra = "celeris_x=1" }, "celeris_x"},
		"package dot dot":      {func(in *Inputs) { in.Packages = ".." }, "packages"},
		"package dot slash":    {func(in *Inputs) { in.Packages = "./." }, "packages"},
		"env value substitute": {func(in *Inputs) { in.Extra = "CELERIS_X=$(id)" }, "CELERIS_X"},
		"env value quote":      {func(in *Inputs) { in.Extra = "CELERIS_X='a'" }, "CELERIS_X"},
		"extra too many":       {func(in *Inputs) { in.Extra = strings.Repeat("-short ", maxExtraTokens+1) }, "tokens"},
	} {
		t.Run(name, func(t *testing.T) {
			in := goodInputs()
			c.mod(&in)
			_, err := planDispatch(in)
			if err == nil {
				t.Fatalf("accepted %+v", in)
			}
			if !strings.Contains(err.Error(), c.want) {
				t.Errorf("error %q does not name %q", err, c.want)
			}
		})
	}
}

func TestPlanReportsEveryBadInputAtOnce(t *testing.T) {
	_, err := planDispatch(Inputs{CelerisRef: "-x", Packages: "/", Count: "0", Shards: "99", Arches: "?", Memlock: "?", Race: "?", Timeout: "?", Extra: "-exec=x"})
	if err == nil {
		t.Fatal("accepted")
	}
	for _, name := range []string{"celeris_ref", "packages", "count", "shards", "arches", "memlock", "race", "timeout", "extra"} {
		if !strings.Contains(err.Error(), name) {
			t.Errorf("error does not mention %s: %v", name, err)
		}
	}
}

func TestExtraSplitsFlagsEnvAndShuffle(t *testing.T) {
	in := goodInputs()
	in.Extra = "-short CELERIS_REQUIRE_IOURING_WORKERS=1 -shuffle=12345 -cpu=2"
	p, err := planDispatch(in)
	if err != nil {
		t.Fatal(err)
	}
	c := p.Cases[0]
	if !slices.Equal(c.Flags, []string{"-short", "-cpu=2"}) || !slices.Equal(c.Env, []string{"CELERIS_REQUIRE_IOURING_WORKERS=1"}) || c.Shuffle != "12345" {
		t.Errorf("flags %v env %v shuffle %q", c.Flags, c.Env, c.Shuffle)
	}
	for _, e := range p.Entries(7) {
		if e.Shuffle != "12345" {
			t.Errorf("entry %s/%d shuffle %q, want the one asked for", e.Arch, e.Shard, e.Shuffle)
		}
	}
}

// Seeds: every shard of a run a different order; the same shard number on
// both arches the same order.
func TestEntriesShardAndSeed(t *testing.T) {
	in := goodInputs()
	in.Shards, in.Timeout = "3", "1h30m"
	p, err := planDispatch(in)
	if err != nil {
		t.Fatal(err)
	}
	es := p.Entries(35504173390)
	if len(es) != 6 {
		t.Fatalf("%d entries, want 3 shards x 2 arches", len(es))
	}
	seeds := map[string]string{}
	for _, e := range es {
		if e.Runner != runners[e.Arch] || (e.Arch == "x86") != (e.Runner == "ubuntu-24.04") {
			t.Errorf("entry %s runs on %s", e.Arch, e.Runner)
		}
		if strings.Contains(e.Runner, "self-hosted") || strings.Contains(e.Runner, "celeris-cluster") {
			t.Errorf("a cluster runner label: %s", e.Runner)
		}
		key := string(rune('0' + e.Shard))
		if prev, ok := seeds[key]; ok && prev != e.Shuffle {
			t.Errorf("shard %d seeds differ across arches: %s vs %s", e.Shard, prev, e.Shuffle)
		}
		seeds[key] = e.Shuffle
		if e.JobTimeout != 90+jobSlack {
			t.Errorf("job timeout %d, want %d", e.JobTimeout, 90+jobSlack)
		}
		if e.MemlockLimit != "8388608" || e.Race != "false" || e.Packages != "./engine/iouring" {
			t.Errorf("entry %+v", e)
		}
	}
	if seeds["1"] != "3550417339001" || seeds["1"] == seeds["2"] || seeds["2"] == seeds["3"] {
		t.Errorf("seeds %v", seeds)
	}
}

func TestJobTimeoutStaysUnderTheHostedLimit(t *testing.T) {
	in := goodInputs()
	in.Timeout = "5h30m"
	p, _ := planDispatch(in)
	for _, e := range p.Entries(1) {
		if e.JobTimeout > 360 {
			t.Errorf("job timeout %d exceeds the 360 minutes a hosted job may run", e.JobTimeout)
		}
	}
}

func TestSelfTestPlan(t *testing.T) {
	p := mustSelfTest(t)
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(p.CelerisRef) {
		t.Errorf("self-test ref %q is not a pinned commit", p.CelerisRef)
	}
	var names []string
	for _, c := range p.Cases {
		names = append(names, c.Name)
		if !slices.Equal(c.Arches, []string{"x86", "arm64"}) {
			t.Errorf("case %s arches %v, want both", c.Name, c.Arches)
		}
	}
	if !slices.Equal(names, []string{"good", "fail", "skip", "timeout", "none"}) {
		t.Errorf("cases %v", names)
	}
	if n := len(p.Entries(1)); n != 12 {
		t.Errorf("%d shard jobs, want 12", n)
	}
}

// The self-test is planned by the dispatch code: every case is exactly what
// planDispatch makes of its IN_* inputs, with only the name, purpose and
// expectation added. The fail case's environment setting, for instance,
// exists only as the IN_EXTRA string until parseExtra splits it out.
func TestSelfTestTakesTheDispatchPath(t *testing.T) {
	p := mustSelfTest(t)
	for i, st := range selfTests() {
		d, err := planDispatch(inputsFrom(func(k string) string { return st.inputs[k] }))
		if err != nil {
			t.Fatalf("case %s: %v", st.name, err)
		}
		want := d.Cases[0]
		want.Name, want.Purpose, want.Expect = st.name, st.purpose, st.expect
		got, _ := json.Marshal(p.Cases[i])
		exp, _ := json.Marshal(want)
		if string(got) != string(exp) {
			t.Errorf("case %s\n got %s\nwant %s", st.name, got, exp)
		}
	}
	fail := p.Cases[1]
	if fail.Name != "fail" || !slices.Equal(fail.Env, []string{"CELERIS_REQUIRE_IOURING_WORKERS=1"}) || fail.Count != 2 {
		t.Errorf("fail case %+v: want count 2 and the require variable parsed out of IN_EXTRA", fail)
	}
	// Every self-test input is a key the workflow passes, and no other.
	want := []string{"IN_ARCHES", "IN_CELERIS_REF", "IN_COUNT", "IN_EXTRA", "IN_MEMLOCK", "IN_PACKAGES", "IN_RACE", "IN_RUN", "IN_SHARDS", "IN_TIMEOUT"}
	for _, st := range selfTests() {
		if keys := slices.Sorted(maps.Keys(st.inputs)); !slices.Equal(keys, want) {
			t.Errorf("case %s inputs %v, want exactly %v", st.name, keys, want)
		}
	}
}

// The environment allow-list is what celeris's tests read, and nothing in it
// may choose a binary or a library or steer the runner.
func TestCelerisTestEnvIsSafe(t *testing.T) {
	if !slices.IsSorted(celerisTestEnv) || len(slices.Compact(slices.Clone(celerisTestEnv))) != len(celerisTestEnv) {
		t.Errorf("celerisTestEnv must be sorted and without duplicates: %v", celerisTestEnv)
	}
	for _, n := range celerisTestEnv {
		if !envNameRe.MatchString(n) || envRefused(n) != "" || strings.HasPrefix(n, celerisEnvPrefix) {
			t.Errorf("%s: not a plain name, refused, or in the CELERIS_ namespace (accepted by prefix)", n)
		}
		if err := checkEnv(n + "=1"); err != nil {
			t.Errorf("%s=1 is refused: %v", n, err)
		}
	}
	for _, n := range []string{"PATH", "LD_PRELOAD", "LD_LIBRARY_PATH", "LD_AUDIT"} {
		if envRefused(n) == "" {
			t.Errorf("%s is not refused by name", n)
		}
	}
}

// go test's -timeout bounds each test binary, one per package: the job limit
// allows one timeout per package, and the hosted maximum when a `...`
// pattern hides how many packages there are.
func TestJobMinutesAllowOneTimeoutPerPackage(t *testing.T) {
	for _, c := range []struct {
		pkgs    []string
		timeout string
		want    int
	}{
		{[]string{"./engine/iouring"}, "30m", 30 + jobSlack},
		{[]string{"./engine/iouring", "./engine/epoll", "."}, "30m", 3*30 + jobSlack},
		{[]string{"./engine/iouring"}, "1s", 1 + jobSlack},
		{[]string{"./engine/iouring"}, "90s", 2 + jobSlack},
		{[]string{"./engine/iouring"}, "5h30m", 330 + jobSlack},
		{[]string{"./a", "./b"}, "5h30m", maxJobMinutes},
		{[]string{"./engine/iouring", "./adaptive/..."}, "5m", maxJobMinutes},
		{[]string{"./..."}, "1s", maxJobMinutes},
	} {
		got := jobMinutes(Case{Packages: c.pkgs, Timeout: c.timeout})
		if got != c.want {
			t.Errorf("packages %v timeout %s: job limit %d, want %d", c.pkgs, c.timeout, got, c.want)
		}
		if got > maxJobMinutes {
			t.Errorf("job limit %d over the hosted maximum", got)
		}
	}
}

// The plan crosses a job boundary as JSON; the expectations must survive it,
// including the difference between "no problems" and "don't care".
func TestPlanSurvivesJSON(t *testing.T) {
	p := mustSelfTest(t)
	b, err := json.Marshal(p)
	if err != nil {
		t.Fatal(err)
	}
	var q Plan
	if err := json.Unmarshal(b, &q); err != nil {
		t.Fatal(err)
	}
	if q.Cases[0].Expect.Problems == nil || len(q.Cases[0].Expect.Problems) != 0 {
		t.Errorf("good case problems after JSON: %#v, want an empty non-nil slice", q.Cases[0].Expect.Problems)
	}
	d, _ := planDispatch(goodInputs())
	b, _ = json.Marshal(d)
	var e Plan
	_ = json.Unmarshal(b, &e)
	if e.Cases[0].Expect.Problems != nil {
		t.Errorf("dispatch problems after JSON: %#v, want nil", e.Cases[0].Expect.Problems)
	}
}

func TestCmdPlanWritesSingleLineOutputs(t *testing.T) {
	dir := t.TempDir()
	out := filepath.Join(dir, "out")
	if err := os.WriteFile(out, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{
		"STRESS_EVENT": "workflow_dispatch", "STRESS_RUN_ID": "42", "GITHUB_OUTPUT": out,
		"STRESS_REF": "refs/heads/stress/runs", "STRESS_DEFAULT_BRANCH": "main",
		"IN_CELERIS_REF": "main", "IN_PACKAGES": "./engine/iouring", "IN_RUN": "^TestA$", "IN_COUNT": "2",
		"IN_SHARDS": "2", "IN_ARCHES": "both", "IN_MEMLOCK": "8m", "IN_RACE": "true", "IN_TIMEOUT": "10m", "IN_EXTRA": "-short",
	}
	var log strings.Builder
	if code := cmdPlan(&log, func(k string) string { return env[k] }); code != 0 {
		t.Fatalf("exit %d: %s", code, log.String())
	}
	b, _ := os.ReadFile(out)
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if len(lines) != 3 || !strings.HasPrefix(lines[0], "plan=") || !strings.HasPrefix(lines[1], "matrix=") || lines[2] != "celeris_ref=main" {
		t.Fatalf("outputs:\n%s", b)
	}
	var m struct{ Include []Entry }
	if err := json.Unmarshal([]byte(strings.TrimPrefix(lines[1], "matrix=")), &m); err != nil || len(m.Include) != 4 {
		t.Fatalf("matrix %v %v", m, err)
	}

	env["IN_EXTRA"] = "-exec=/bin/sh"
	log.Reset()
	if code := cmdPlan(&log, func(k string) string { return env[k] }); code != 2 || !strings.Contains(log.String(), "::error::") {
		t.Errorf("bad input: exit %d, log %q", code, log.String())
	}
	env["STRESS_EVENT"] = "push"
	log.Reset()
	if code := cmdPlan(&log, func(k string) string { return env[k] }); code != 2 {
		t.Errorf("push event: exit %d", code)
	}
}

// A dispatch on the default branch would run the code under test in the
// default branch's cache scope; it is refused before anything else, and the
// refusal fails closed.
func TestCmdPlanRefusesTheDefaultBranch(t *testing.T) {
	base := map[string]string{
		"STRESS_EVENT": "workflow_dispatch", "IN_CELERIS_REF": "main", "IN_PACKAGES": "./engine/iouring",
		"IN_COUNT": "1", "IN_SHARDS": "1", "IN_ARCHES": "x86", "IN_MEMLOCK": "8m", "IN_RACE": "false", "IN_TIMEOUT": "5m",
	}
	for name, c := range map[string]struct {
		ref, def string
		code     int
	}{
		"default branch":       {"refs/heads/main", "main", 2},
		"other default":        {"refs/heads/trunk", "trunk", 2},
		"no ref":               {"", "main", 2},
		"no default branch":    {"refs/heads/stress/runs", "", 2},
		"a branch":             {"refs/heads/stress/runs", "main", 0},
		"a branch named main2": {"refs/heads/main2", "main", 0},
		"a tag":                {"refs/tags/v1", "main", 0},
	} {
		t.Run(name, func(t *testing.T) {
			env := map[string]string{"STRESS_REF": c.ref, "STRESS_DEFAULT_BRANCH": c.def}
			for k, v := range base {
				env[k] = v
			}
			var log strings.Builder
			if code := cmdPlan(&log, func(k string) string { return env[k] }); code != c.code {
				t.Errorf("exit %d, want %d: %s", code, c.code, log.String())
			}
		})
	}
	// The self-test runs on pull_request, whose ref is the merge ref; it
	// meets the same refusal (which a real pull_request never trips) and
	// fails closed the same way.
	for name, c := range map[string]struct {
		ref  string
		code int
	}{
		"merge ref":      {"refs/pull/1/merge", 0},
		"default branch": {"refs/heads/main", 2},
		"no ref":         {"", 2},
	} {
		t.Run("pull_request "+name, func(t *testing.T) {
			env := map[string]string{"STRESS_EVENT": "pull_request", "STRESS_REF": c.ref, "STRESS_DEFAULT_BRANCH": "main"}
			var log strings.Builder
			if code := cmdPlan(&log, func(k string) string { return env[k] }); code != c.code {
				t.Errorf("exit %d, want %d: %s", code, c.code, log.String())
			}
		})
	}
}
