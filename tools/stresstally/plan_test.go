package main

import (
	"encoding/json"
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
		"every extra": func(in *Inputs) {
			in.Extra = "-short -failfast -cpu=1,2,4 -parallel=8 -skip=^TestSlow$ -tags=integration -shuffle=off " +
				"CELERIS_REQUIRE_IOURING_WORKERS=1 WS484_CONNS=16 DRAIN583_REPS=3"
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
		if e.Runner != runners[e.Arch] || (e.Arch == "x86") != (e.Runner == "ubuntu-latest") {
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
	p := planSelfTest()
	if !regexp.MustCompile(`^[0-9a-f]{40}$`).MatchString(p.CelerisRef) {
		t.Errorf("self-test ref %q is not a pinned commit", p.CelerisRef)
	}
	var names []string
	for _, c := range p.Cases {
		names = append(names, c.Name)
		if !slices.Equal(c.Arches, []string{"x86", "arm64"}) {
			t.Errorf("case %s arches %v, want both", c.Name, c.Arches)
		}
		// A self-test case must itself pass the dispatch validation, so it
		// exercises the same input space a dispatch can.
		in := Inputs{CelerisRef: p.CelerisRef, Packages: strings.Join(c.Packages, " "), Run: c.Run,
			Count: itoa(c.Count), Shards: itoa(c.Shards), Arches: "both", Memlock: c.Memlock,
			Race: boolStr(c.Race), Timeout: c.Timeout, Extra: strings.Join(append(append([]string{}, c.Flags...), c.Env...), " ")}
		if _, err := planDispatch(in); err != nil {
			t.Errorf("case %s is not a valid dispatch: %v", c.Name, err)
		}
	}
	if !slices.Equal(names, []string{"good", "fail", "skip", "timeout", "none"}) {
		t.Errorf("cases %v", names)
	}
	if n := len(p.Entries(1)); n != 12 {
		t.Errorf("%d shard jobs, want 12", n)
	}
}

func itoa(n int) string { b, _ := json.Marshal(n); return string(b) }
func boolStr(b bool) string {
	if b {
		return "true"
	}
	return "false"
}

// The plan crosses a job boundary as JSON; the expectations must survive it,
// including the difference between "no problems" and "don't care".
func TestPlanSurvivesJSON(t *testing.T) {
	p := planSelfTest()
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
	// The self-test runs on pull_request, whose ref is the merge ref.
	env := map[string]string{"STRESS_EVENT": "pull_request", "STRESS_REF": "refs/pull/1/merge", "STRESS_DEFAULT_BRANCH": "main"}
	var log strings.Builder
	if code := cmdPlan(&log, func(k string) string { return env[k] }); code != 0 {
		t.Errorf("pull_request: exit %d: %s", code, log.String())
	}
}
