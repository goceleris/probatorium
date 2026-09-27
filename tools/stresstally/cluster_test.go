package main

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func clusterInputs() Inputs {
	in := goodInputs()
	in.CelerisRef, in.Shards, in.Timeout, in.Memlock, in.Target = testSHA, "3", "5m", "default", "cluster"
	return in
}

func timingInputs() Inputs {
	in := clusterInputs()
	in.Mode, in.CelerisRef, in.Shards, in.Timeout = "timing", "A="+testSHA+" B="+branchSHA, "4", "2m"
	return in
}

// A Williams design: every row a permutation; every arm first equally
// often; every ordered pair of different arms adjacent equally often.
func TestWilliamsIsCounterbalanced(t *testing.T) {
	for k := 2; k <= maxArms; k++ {
		rows := williams(k)
		want := k
		if k%2 == 1 {
			want = 2 * k
		}
		if len(rows) != want {
			t.Fatalf("k=%d: %d rows, want %d", k, len(rows), want)
		}
		first := map[int]int{}
		pairs := map[[2]int]int{}
		for _, r := range rows {
			if s := slices.Sorted(slices.Values(r)); !slices.Equal(s, func() []int {
				x := make([]int, k)
				for i := range x {
					x[i] = i
				}
				return x
			}()) {
				t.Fatalf("k=%d: row %v is not a permutation", k, r)
			}
			first[r[0]]++
			for i := 0; i+1 < k; i++ {
				pairs[[2]int{r[i], r[i+1]}]++
			}
		}
		for a := range k {
			if first[a] != len(rows)/k {
				t.Errorf("k=%d: arm %d first %d times, want %d", k, a, first[a], len(rows)/k)
			}
			for b := range k {
				if a != b && pairs[[2]int{a, b}] != len(rows)/k {
					t.Errorf("k=%d: %d directly before %d %d times, want %d", k, a, b, pairs[[2]int{a, b}], len(rows)/k)
				}
			}
		}
	}
	if got := williams(2); !slices.Equal(got[0], []int{0, 1}) || !slices.Equal(got[1], []int{1, 0}) {
		t.Errorf("two arms: %v, want AB then BA", got)
	}
}

func TestClusterPlanAccepts(t *testing.T) {
	for name, mod := range map[string]func(*Inputs){
		"stress":            func(*Inputs) {},
		"branch":            func(in *Inputs) { in.CelerisRef = "fix/celeris-662-defer-accept" },
		"tag":               func(in *Inputs) { in.CelerisRef = "v1.6.0" },
		"200 short shards":  func(in *Inputs) { in.Shards, in.Timeout = "200", "1m" },
		"8m asked":          func(in *Inputs) { in.Memlock = "8m" },
		"x86 only":          func(in *Inputs) { in.Arches = "x86" },
		"timing":            func(in *Inputs) { *in = timingInputs() },
		"timing A/A":        func(in *Inputs) { *in = timingInputs(); in.CelerisRef = "A=" + testSHA + " A2=" + testSHA },
		"timing 3 arms":     func(in *Inputs) { *in = timingInputs(); in.CelerisRef += " C=main"; in.Shards = "6" },
		"timing 4 arms":     func(in *Inputs) { *in = timingInputs(); in.CelerisRef += " C=main D=v1.5.8" },
		"timing options":    func(in *Inputs) { *in = timingInputs(); in.Timing = "cpus=2 pmu=required" },
		"timing no options": func(in *Inputs) { *in = timingInputs(); in.Timing = "" },
	} {
		t.Run(name, func(t *testing.T) {
			in := clusterInputs()
			mod(&in)
			p, err := planDispatch(in)
			if err != nil {
				t.Fatalf("refused: %v", err)
			}
			if !p.IsCluster() {
				t.Errorf("not a cluster plan: %+v", p)
			}
			if m := hostJobMinutes(p); m > maxClusterJobMinutes {
				t.Errorf("limit %d over %d", m, maxClusterJobMinutes)
			}
		})
	}
}

// Every cluster rule is refused by name before anything reaches a host.
func TestClusterPlanRefuses(t *testing.T) {
	for name, c := range map[string]struct {
		mod  func(*Inputs)
		want string
	}{
		"pull ref":            {func(in *Inputs) { in.CelerisRef = "refs/pull/674/head" }, "pull ref"},
		"short pull ref":      {func(in *Inputs) { in.CelerisRef = "pull/674/head" }, "pull ref"},
		"... pattern":         {func(in *Inputs) { in.Packages = "./engine/..." }, "no ... pattern"},
		"too many shards":     {func(in *Inputs) { in.Shards = "201" }, "shards"},
		"over the limit":      {func(in *Inputs) { in.Shards, in.Timeout = "100", "5m" }, "480"},
		"bad target":          {func(in *Inputs) { in.Target = "msr1" }, "target"},
		"bad mode":            {func(in *Inputs) { in.Mode = "bench" }, "mode"},
		"timing options":      {func(in *Inputs) { in.Timing = "cpus=4" }, "only to mode timing"},
		"timing one arm":      {func(in *Inputs) { *in = timingInputs(); in.CelerisRef = "A=" + testSHA }, "2 to 4 arms"},
		"timing five arms":    {func(in *Inputs) { *in = timingInputs(); in.CelerisRef += " C=a D=b E=c" }, "2 to 4 arms"},
		"timing plain ref":    {func(in *Inputs) { *in = timingInputs(); in.CelerisRef = testSHA + " " + branchSHA }, "NAME=REF"},
		"timing name twice":   {func(in *Inputs) { *in = timingInputs(); in.CelerisRef = "A=" + testSHA + " A=" + branchSHA }, "named twice"},
		"timing lower name":   {func(in *Inputs) { *in = timingInputs(); in.CelerisRef = "a=" + testSHA + " B=" + branchSHA }, "NAME"},
		"timing pull arm":     {func(in *Inputs) { *in = timingInputs(); in.CelerisRef = "A=main B=refs/pull/1/head" }, "pull ref"},
		"timing bad arm ref":  {func(in *Inputs) { *in = timingInputs(); in.CelerisRef = "A=main B=-x" }, "arm B"},
		"timing odd blocks":   {func(in *Inputs) { *in = timingInputs(); in.Shards = "3" }, "multiple of 2"},
		"timing 3 arms x4":    {func(in *Inputs) { *in = timingInputs(); in.CelerisRef += " C=main" }, "multiple of 6"},
		"timing two packages": {func(in *Inputs) { *in = timingInputs(); in.Packages = "./a ./b" }, "exactly one package"},
		"timing cpus 0":       {func(in *Inputs) { *in = timingInputs(); in.Timing = "cpus=0" }, "cpus"},
		"timing cpus 17":      {func(in *Inputs) { *in = timingInputs(); in.Timing = "cpus=17" }, "cpus"},
		"timing pmu":          {func(in *Inputs) { *in = timingInputs(); in.Timing = "pmu=maybe" }, "pmu"},
		"timing unknown":      {func(in *Inputs) { *in = timingInputs(); in.Timing = "governor=performance" }, "governor=performance"},
		"timing twice":        {func(in *Inputs) { *in = timingInputs(); in.Timing = "cpus=2 cpus=4" }, "twice"},
		"timing on github":    {func(in *Inputs) { *in = timingInputs(); in.Target = "github" }, "needs target cluster"},
	} {
		t.Run(name, func(t *testing.T) {
			in := clusterInputs()
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

// The default target stays github, and its plan is what it always was: no
// cluster field in its JSON, and memlock default resolves to 8m.
func TestGithubPlanIsUnchanged(t *testing.T) {
	in := goodInputs()
	in.Memlock = "default"
	p, err := planDispatch(in)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(p)
	for _, k := range []string{`"target"`, `"mode"`, `"cpus"`, `"pmu"`, `"arms"`} {
		if strings.Contains(string(b), k) {
			t.Errorf("a github plan carries %s: %s", k, b)
		}
	}
	old := goodInputs() // memlock 8m, as every dispatch before this change
	q, _ := planDispatch(old)
	c, _ := json.Marshal(q)
	if string(b) != string(c) {
		t.Errorf("memlock default and memlock 8m plan differently on github:\n%s\n%s", b, c)
	}
	if p.IsCluster() || p.IsTiming() {
		t.Error("a github plan claims the cluster")
	}
}

func TestHostJobMinutes(t *testing.T) {
	in := clusterInputs()
	in.Packages, in.Shards, in.Timeout = "./a ./b", "3", "5m"
	p, err := planDispatch(in)
	if err != nil {
		t.Fatal(err)
	}
	if got, want := hostJobMinutes(p), clusterFixedMinutes+3*(2*5+clusterShardMinutes); got != want {
		t.Errorf("stress limit %d, want %d", got, want)
	}
	tp, err := planDispatch(timingInputs())
	if err != nil {
		t.Fatal(err)
	}
	// A timing adds the quiet wait and, per arm, 3 minutes to build its test
	// binary before the quiet check (cluster-host.sh prebuild_arms), so no
	// observation's budget carries a compile.
	if got, want := hostJobMinutes(tp), clusterFixedMinutes+timingQuietMinutes+2*3+8*(2+clusterShardMinutes); got != want {
		t.Errorf("timing limit %d, want %d (2 arms x 4 blocks, each arm prebuilt)", got, want)
	}
	for _, e := range tp.HostEntries(7) {
		if e.JobTimeout != hostJobMinutes(tp) || e.CPUs != defaultCPUs || e.PMU != "optional" || e.Mode != "timing" ||
			e.Cases != "A B" || e.FirstCase != "A" || e.MemlockLimit != "unlimited" {
			t.Errorf("host entry %+v", e)
		}
	}
}

// Three arms: six blocks in the mirrored Williams order, one seed per block.
func TestTimingSequenceThreeArms(t *testing.T) {
	in := timingInputs()
	in.CelerisRef, in.Shards = "A=main B=v1.5.8 C=fix/x", "6"
	p, err := planDispatch(in)
	if err != nil {
		t.Fatal(err)
	}
	seq := p.Sequence(5)
	if len(seq) != 18 {
		t.Fatalf("%d observations, want 18", len(seq))
	}
	var blocks []string
	for b := 0; b < 6; b++ {
		var row []string
		for _, tok := range seq[3*b : 3*b+3] {
			parts := strings.Split(tok, ":")
			if parts[1] != fmt.Sprint(b+1) || parts[2] != fmt.Sprint(5000+b+1) {
				t.Errorf("token %s: want block %d on seed %d", tok, b+1, 5000+b+1)
			}
			row = append(row, parts[0])
		}
		blocks = append(blocks, strings.Join(row, ""))
	}
	if want := []string{"ABC", "BCA", "CAB", "CBA", "ACB", "BAC"}; !slices.Equal(blocks, want) {
		t.Errorf("blocks %v, want %v", blocks, want)
	}
	// An explicit -shuffle holds for every observation.
	in.Extra = "-shuffle=off"
	p, _ = planDispatch(in)
	for _, tok := range p.Sequence(5) {
		if !strings.HasSuffix(tok, ":off") {
			t.Errorf("token %s ignores -shuffle=off", tok)
		}
	}
}

func TestParseCaseSHAs(t *testing.T) {
	p, _ := planDispatch(timingInputs())
	if m, err := parseCaseSHAs("A="+testSHA+" B="+branchSHA, p); err != nil || m["A"] != testSHA || m["B"] != branchSHA {
		t.Errorf("%v %v", m, err)
	}
	for _, bad := range []string{"", "A=" + testSHA, "A=" + testSHA + " B=main", "A=" + testSHA + " B=" + branchSHA + " C=" + testSHA, "A " + testSHA} {
		if _, err := parseCaseSHAs(bad, p); err == nil {
			t.Errorf("%q accepted", bad)
		}
	}
}

// The cluster's own shape: the log must come from the planned host, and a
// timing's log must carry the asked-for pinning and PMU rule.
func TestClusterShapeIsChecked(t *testing.T) {
	p, err := planDispatch(clusterInputs())
	if err != nil {
		t.Fatal(err)
	}
	c := p.Cases[0]
	c.Shards, c.Arches = 1, []string{"x86"}
	good := map[string]string{"target": "cluster", "host": "msa2-server", "memlock_limit": "unlimited",
		"memlock_in_force": "unlimited:unlimited", "memlock": "unlimited", "count": "5", "run": c.Run, "timeout": c.Timeout}
	for name, tc := range map[string]struct {
		override map[string]string
		status   string
	}{
		"right host":  {nil, statusComplete},
		"wrong host":  {map[string]string{"host": "msa2-client"}, statusWrongShape},
		"no target":   {map[string]string{"target": "<drop>"}, statusWrongShape},
		"github log":  {map[string]string{"target": "github"}, statusWrongShape},
		"8m in force": {map[string]string{"memlock_in_force": "8388608:8388608"}, statusWrongShape},
	} {
		t.Run(name, func(t *testing.T) {
			ov := maps2(good, tc.override)
			dir := t.TempDir()
			writeShard(t, dir, c, "x86", 1, body("PASS"), "0", ov)
			r := judgeCase(c, dir, testSHA)
			if r.Shards[0].Status != tc.status {
				t.Errorf("status %s, want %s: %v", r.Shards[0].Status, tc.status, r.Shards[0].Reasons)
			}
		})
	}
	tp, _ := planDispatch(timingInputs())
	tc := tp.Cases[0]
	tc.Shards, tc.Arches = 1, []string{"arm64"}
	timingGood := maps2(good, map[string]string{"host": "msr1", "mode": "timing", "cpus_asked": "4", "pmu_asked": "optional", "timeout": "2m"})
	for name, x := range map[string]struct {
		override map[string]string
		status   string
	}{
		"as asked":   {nil, statusComplete},
		"other cpus": {map[string]string{"cpus_asked": "2"}, statusWrongShape},
		"other pmu":  {map[string]string{"pmu_asked": "required"}, statusWrongShape},
		"not timing": {map[string]string{"mode": "stress"}, statusWrongShape},
	} {
		t.Run("timing "+name, func(t *testing.T) {
			dir := t.TempDir()
			writeShard(t, dir, tc, "arm64", 1, body("PASS")+obsLine(1, "A", 1, "abc", 5e6), "0", maps2(timingGood, x.override))
			r := judgeCase(tc, dir, testSHA)
			if r.Shards[0].Status != x.status {
				t.Errorf("status %s, want %s: %v", r.Shards[0].Status, x.status, r.Shards[0].Reasons)
			}
		})
	}
}

func maps2(a, b map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range a {
		out[k] = v
	}
	for k, v := range b {
		out[k] = v
	}
	return out
}

func obsLine(seq int, arm string, block int, binSHA string, wallNS float64) string {
	return fmt.Sprintf("stress-obs: seq=%d case=%s block=%d binary_sha256=%s exit=0 wall_ns=%.0f user_s=0.010 sys_s=0.002 "+
		"load1_before=0.05 load1_after=0.06 perf=absent instructions_u= cycles_u= task_clock_ms=\n", seq, arm, block, binSHA, wallNS)
}

// shard.sh's own lines after go test are data, never test output: they add
// no reason, no verdict and no failure, and they reach the report.
func TestObservationAndAfterLinesAreParsed(t *testing.T) {
	c := sampleCase("stress")
	c.Packages = []string{"./engine/iouring"}
	log := shardText(c, "x86", 1, body("PASS")+obsLine(3, "B", 2, "feed", 1.5e6)+"stress-after: loadavg=0.10,0.20,0.30\n", "0", nil)
	sl := parseShard(strings.NewReader(log))
	if len(sl.reasons) != 0 {
		t.Errorf("reasons %v", sl.reasons)
	}
	if len(sl.obs) != 1 || sl.obs[0]["seq"] != "3" || sl.obs[0]["binary_sha256"] != "feed" || sl.obs[0]["wall_ns"] != "1500000" {
		t.Errorf("obs %v", sl.obs)
	}
	if sl.after["loadavg"] != "0.10,0.20,0.30" {
		t.Errorf("after %v", sl.after)
	}
	if got := sl.results[testKey{celerisPkg, "TestFlaky"}]; got == nil || got.Pass != 1 {
		t.Errorf("results %v", sl.results)
	}
}

// A timing's summary: each arm judged against its own commit, the
// observations in run order with their planned positions, one single-case
// report per arm (so compare works on the arms), and a descriptive section.
func TestSummarizeTiming(t *testing.T) {
	p, err := planDispatch(timingInputs())
	if err != nil {
		t.Fatal(err)
	}
	p.ProbatoriumSHA = testProbatoriumSHA
	sha := map[string]string{"A": testSHA, "B": branchSHA}
	dir := t.TempDir()
	for _, arch := range []string{"x86", "arm64"} {
		for i, tok := range p.Sequence(42) {
			parts := strings.Split(tok, ":")
			arm, block := parts[0], parts[1]
			var c Case
			for _, x := range p.Cases {
				if x.Name == arm {
					c = x
				}
			}
			var n int
			_, _ = fmt.Sscan(block, &n)
			ov := map[string]string{"celeris_sha": sha[arm], "target": "cluster", "host": clusterHosts[arch], "mode": "timing",
				"cpus_asked": "4", "pmu_asked": "optional", "memlock_in_force": "unlimited:unlimited", "memlock_limit": "unlimited",
				"memlock": "unlimited", "shuffle": parts[2], "cpuset": "12,13,14,15"}
			wall := 1e6 * float64(10+i)
			writeShard(t, dir, c, arch, n, body("PASS")+obsLine(i+1, arm, n, "bin"+arm, wall), "0", ov)
		}
	}
	pj, _ := json.Marshal(p)
	out := t.TempDir()
	env := map[string]string{"STRESS_PLAN": string(pj), "STRESS_CELERIS_SHA": testSHA, "STRESS_CASE_SHAS": "A=" + testSHA + " B=" + branchSHA}
	var log strings.Builder
	if code := cmdSummarize([]string{"-logs", dir, "-out", out}, &log, func(k string) string { return env[k] }); code != 0 {
		t.Fatalf("exit %d:\n%s", code, log.String())
	}
	tsv, err := os.ReadFile(filepath.Join(out, "observations.tsv"))
	if err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(string(tsv)), "\n")
	if len(lines) != 1+16 {
		t.Fatalf("%d observation rows, want 16:\n%s", len(lines)-1, tsv)
	}
	var order []string
	for _, l := range lines[1:9] {
		f := strings.Split(l, "\t")
		if f[0] != "x86" || f[1] != "msa2-server" || f[18] != "12,13,14,15" || f[19] != statusComplete {
			t.Errorf("row %q", l)
		}
		order = append(order, f[4]+f[3]+"@"+f[5])
	}
	if want := []string{"A1@1", "B1@2", "B2@1", "A2@2", "A3@1", "B3@2", "B4@1", "A4@2"}; !slices.Equal(order, want) {
		t.Errorf("x86 order %v, want %v", order, want)
	}
	for _, arm := range []string{"A", "B"} {
		r, err := readReport(filepath.Join(out, "arms", arm))
		if err != nil {
			t.Fatalf("arm %s: %v", arm, err)
		}
		if r.CelerisSHA != sha[arm] || r.Cases[0].Verdict != "PASS" || r.Cases[0].Config.Mode != "timing" {
			t.Errorf("arm %s report: sha %s verdict %s mode %q", arm, r.CelerisSHA, r.Cases[0].Verdict, r.Cases[0].Config.Mode)
		}
	}
	var cmp strings.Builder
	if code := cmdCompare([]string{filepath.Join(out, "arms", "A"), filepath.Join(out, "arms", "B")}, &cmp); code != 0 {
		t.Errorf("the arms of one timing are not comparable: exit %d\n%s", code, cmp.String())
	}
	md, _ := os.ReadFile(filepath.Join(out, "summary.md"))
	for _, want := range []string{"Timing of 2 arm(s): A `" + testSHA + "`", "## Timing (descriptive only)", "Order on each host (arm and block): A1 B1 B2 A2",
		"on the bare-metal cluster", "### Host facts"} {
		if !strings.Contains(string(md), want) {
			t.Errorf("summary.md lacks %q:\n%s", want, md)
		}
	}
	// An arm's shard that tested the other arm's commit is WRONG-SHAPE.
	bad := filepath.Join(dir, logName("B", "arm64", 3))
	b, _ := os.ReadFile(bad)
	if err := os.WriteFile(bad, []byte(strings.Replace(string(b), "celeris_sha="+branchSHA, "celeris_sha="+testSHA, 1)), 0o644); err != nil {
		t.Fatal(err)
	}
	log.Reset()
	if code := cmdSummarize([]string{"-logs", dir, "-out", t.TempDir()}, &log, func(k string) string { return env[k] }); code != 1 || !strings.Contains(log.String(), "WRONG-SHAPE") {
		t.Errorf("an arm's shard on the other arm's commit: exit %d\n%s", code, log.String())
	}
	// A cluster plan without the per-case commits fails closed.
	delete(env, "STRESS_CASE_SHAS")
	log.Reset()
	if code := cmdSummarize([]string{"-logs", dir, "-out", t.TempDir()}, &log, func(k string) string { return env[k] }); code != 2 {
		t.Errorf("no STRESS_CASE_SHAS: exit %d, want 2\n%s", code, log.String())
	}
}

// guardRun builds a run as the API lists it.
func guardRun(id int64, path, status, event, title string) ghRun {
	return ghRun{ID: id, Name: filepath.Base(path), Path: path, Status: status, Event: event, DisplayTitle: title}
}

func TestGuardClassifiesTheGroup(t *testing.T) {
	const self = 100
	nightly := ".github/workflows/matrix-nightly-tier.yml"
	bench := ".github/workflows/benchmark-tier.yml"
	cases := map[string]struct {
		runs  []ghRun
		jobs  map[int64][]ghJob
		allow bool
		class string // of run 1, when there is one
	}{
		"empty":                {nil, nil, true, ""},
		"self only":            {[]ghRun{guardRun(self, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress [cluster/stress] main")}, nil, true, ""},
		"tier running":         {[]ghRun{guardRun(1, nightly, "in_progress", "workflow_dispatch", "Nightly Validation")}, nil, true, classHolder},
		"tier pending":         {[]ghRun{guardRun(1, bench, "pending", "workflow_dispatch", "Benchmark Tier")}, nil, false, classPending},
		"tier queued":          {[]ghRun{guardRun(1, bench, "queued", "workflow_dispatch", "Benchmark Tier")}, nil, false, classPending},
		"tier waiting":         {[]ghRun{guardRun(1, nightly, "waiting", "schedule", "Nightly Validation")}, nil, false, classPending},
		"tier completed":       {[]ghRun{guardRun(1, nightly, "completed", "workflow_dispatch", "Nightly Validation")}, nil, true, ""},
		"other workflow":       {[]ghRun{guardRun(1, ".github/workflows/test.yml", "pending", "push", "Test")}, nil, true, ""},
		"stress self-test":     {[]ghRun{guardRun(1, stressWorkflowPath, "in_progress", "pull_request", "Celeris Stress self-test #1")}, nil, true, classIgnored},
		"stress github":        {[]ghRun{guardRun(1, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress [github/stress] main")}, nil, true, classIgnored},
		"stress before guard":  {[]ghRun{guardRun(1, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress [cluster/stress] main")}, map[int64][]ghJob{1: {{Name: "plan", Status: "in_progress"}}}, false, classPending},
		"stress untitled":      {[]ghRun{guardRun(1, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress")}, nil, false, classPending},
		"stress guard refused": {[]ghRun{guardRun(1, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress [cluster/stress] main")}, map[int64][]ghJob{1: {{Name: "cluster-guard", Status: "completed", Conclusion: "failure"}}}, true, classIgnored},
		"stress in group, waiting": {[]ghRun{guardRun(1, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress [cluster/stress] main")},
			map[int64][]ghJob{1: {{Name: "cluster-guard", Status: "completed", Conclusion: "success"}, {Name: "cluster / bootstrap cluster runners", Status: "queued"}}}, false, classPending},
		"stress in group, running": {[]ghRun{guardRun(1, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress [cluster/timing] A=main B=x")},
			map[int64][]ghJob{1: {{Name: "cluster-guard", Status: "completed", Conclusion: "success"}, {Name: "cluster / bootstrap cluster runners", Status: "completed", Conclusion: "success"},
				{Name: "cluster / host x86 (msa2-server)", Status: "in_progress"}}}, true, classHolder},
		"holder and pending": {[]ghRun{guardRun(1, nightly, "in_progress", "workflow_dispatch", "Nightly Validation"), guardRun(2, bench, "pending", "workflow_dispatch", "Benchmark Tier")}, nil, false, classHolder},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			classes := classifyRuns(c.runs, c.jobs, self)
			ok, msg := guardDecision(classes)
			if ok != c.allow {
				t.Errorf("allow %v, want %v: %s", ok, c.allow, msg)
			}
			for _, x := range classes {
				if x.Run.ID == self {
					t.Error("the guard judged its own run")
				}
				if x.Run.ID == 1 && x.Class != c.class {
					t.Errorf("run 1 is %s, want %s (%s)", x.Class, c.class, x.Why)
				}
			}
			if !ok && !strings.Contains(msg, "run 2") && !strings.Contains(msg, "run 1") {
				t.Errorf("the refusal does not name the pending run: %s", msg)
			}
		})
	}
}

// cmdGuard fails closed: no listing, no decision.
func TestCmdGuard(t *testing.T) {
	var out strings.Builder
	if code := cmdGuard([]string{"-dir", t.TempDir(), "-self", "7"}, &out); code != 2 {
		t.Errorf("no runs listed: exit %d, want 2\n%s", code, out.String())
	}
	if code := cmdGuard([]string{"-dir", t.TempDir()}, &out); code != 2 {
		t.Errorf("no -self: exit %d, want 2", code)
	}
	write := func(dir, name string, v ...any) {
		var b strings.Builder
		for _, x := range v {
			j, _ := json.Marshal(x)
			b.Write(j)
			b.WriteString("\n")
		}
		if err := os.WriteFile(filepath.Join(dir, name), []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	allow := t.TempDir()
	write(allow, "runs-in_progress.jsonl", guardRun(7, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress [cluster/stress] main"),
		guardRun(8, ".github/workflows/matrix-race-tier.yml", "in_progress", "workflow_dispatch", "Race Validation"))
	write(allow, "runs-pending.jsonl")
	out.Reset()
	if code := cmdGuard([]string{"-dir", allow, "-self", "7"}, &out); code != 0 || !strings.Contains(out.String(), "allowed: nothing pending; run 8") {
		t.Errorf("exit %d\n%s", code, out.String())
	}
	if b, err := os.ReadFile(filepath.Join(allow, "decision.txt")); err != nil || !strings.Contains(string(b), "holder") {
		t.Errorf("decision.txt: %v %s", err, b)
	}
	refuse := t.TempDir()
	write(refuse, "runs-pending.jsonl", guardRun(9, ".github/workflows/matrix-weekend-tier.yml", "pending", "workflow_dispatch", "Weekend Soak"))
	write(refuse, "runs-in_progress.jsonl", guardRun(10, stressWorkflowPath, "in_progress", "workflow_dispatch", "Celeris Stress [cluster/stress] x"))
	write(refuse, "jobs-10.jsonl", ghJob{Name: "cluster-guard", Status: "completed", Conclusion: "success"},
		ghJob{Name: "cluster / host arm64 (msr1)", Status: "in_progress"})
	out.Reset()
	if code := cmdGuard([]string{"-dir", refuse, "-self", "7"}, &out); code != 1 || !strings.Contains(out.String(), "::error::refusing: run 9") {
		t.Errorf("exit %d\n%s", code, out.String())
	}
	broken := t.TempDir()
	if err := os.WriteFile(filepath.Join(broken, "runs-queued.jsonl"), []byte("{not json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := cmdGuard([]string{"-dir", broken, "-self", "7"}, &out); code != 2 {
		t.Errorf("unreadable listing: exit %d, want 2", code)
	}
}
