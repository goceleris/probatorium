package hostgate

import (
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"
)

// The fixtures below are collect.sh's output format, filled with the values
// the 2026-10-09 read-only investigation captured on the real hosts
// (evidence/msr1-rvtest-20261009: 01 ps and /proc, 04 ss, 06 PSI, 16 and 24
// for the msa2-server orphan). The ss and PSI lines are verbatim.

const rvtestSnapshot = "hostgate\tv1\n" +
	"host\tmsr1\n" +
	"epoch\t1791547200\n" +
	"psi\tio\tsome avg10=99.00 avg60=99.00 avg300=99.00 total=2436399339980\n" +
	"psi\tio\tfull avg10=97.97 avg60=98.02 avg300=98.05 total=2399333767658\n" +
	"ss\tLISTEN 0      4096                     127.0.0.1:40637      0.0.0.0:*    users:((\"rvtest\",pid=3671056,fd=40))\n" +
	"ss\tLISTEN 0      4096                     127.0.0.1:40637      0.0.0.0:*    users:((\"rvtest\",pid=3671056,fd=11))\n" +
	"ss\tLISTEN 0      4096                       0.0.0.0:22         0.0.0.0:*\n" +
	"ss\tLISTEN 0      4096   [fd7a:115c:a1e0::8d34:4727]:63735         [::]:*\n" +
	"proc\t3671056\t1\tmini\t2413380\trvtest\t/tmp/rvtest (deleted)\t/tmp\t./rvtest -bind=127.0.0.1:0 -engine=iouring\n" +
	"proc\t3851165\t1\tmini\t432000\tRunner.Listener\t/tmp/actions-runner-msr1/bin.2.337.0/Runner.Listener (deleted)\t/tmp/actions-runner-msr1\t/tmp/actions-runner-msr1/bin/Runner.Listener run\n" +
	"proc\t812\t1\troot\t2000000\tsshd\t/usr/sbin/sshd\t/\t/usr/sbin/sshd -D\n" +
	"end\tok\n"

const ginOrphanSnapshot = "hostgate\tv1\n" +
	"host\tmsa2-server\n" +
	"epoch\t1791547200\n" +
	"psi\tio\tsome avg10=0.00 avg60=0.00 avg300=0.00 total=14\n" +
	"ss\tLISTEN 0      65535                            *:8080             *:*    users:((\"gin\",pid=3941235,fd=4))\n" +
	"proc\t3941234\t1\tmini\t302219\tbash\t/usr/bin/bash\t/tmp/celeris-bench (deleted)\t/bin/bash -c [script elided]\n" +
	"proc\t3941235\t3941234\tmini\t302219\tgin\t/tmp/celeris-bench/competitors/gin (deleted)\t/tmp/celeris-bench (deleted)\t./competitors/gin -bind 0.0.0.0:8080 -engine h2c\n" +
	"proc\t3851165\t1\tmini\t432000\tRunner.Listener\t/tmp/actions-runner-msa2-server/bin/Runner.Listener\t/tmp/actions-runner-msa2-server\t/tmp/actions-runner-msa2-server/bin/Runner.Listener run\n" +
	"end\tok\n"

const idleSnapshot = "hostgate\tv1\n" +
	"host\tmsa2-client\n" +
	"epoch\t1791547200\n" +
	"psi\tio\tsome avg10=0.00 avg60=0.00 avg300=0.00 total=0\n" +
	"ss\tLISTEN 0      4096                       0.0.0.0:22         0.0.0.0:*\n" +
	"proc\t3851166\t1\tmini\t432000\tRunner.Listener\t/tmp/actions-runner-msa2-client/bin/Runner.Listener\t/tmp/actions-runner-msa2-client\t/tmp/actions-runner-msa2-client/bin/Runner.Listener run\n" +
	"end\tok\n"

func mustParse(t *testing.T, text string) Snapshot {
	t.Helper()
	s, err := Parse(strings.NewReader(text))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	return s
}

func rules(vs []Violation) map[string]int {
	m := map[string]int{}
	for _, v := range vs {
		m[v.Rule]++
	}
	return m
}

func TestParseReadsTheCollectorFormat(t *testing.T) {
	s := mustParse(t, rvtestSnapshot)
	if s.Host != "msr1" || s.Epoch != 1791547200 {
		t.Fatalf("host/epoch = %q/%d", s.Host, s.Epoch)
	}
	if !s.IO.Present || s.IO.SomeAvg60 != 99.0 || s.IO.FullAvg60 != 98.02 {
		t.Fatalf("io psi = %+v, want some 99.00 full 98.02", s.IO)
	}
	// 12 SO_REUSEPORT lines are two here; the parser keeps every socket line.
	var rv int
	for _, l := range s.Listeners {
		if l.PID == 3671056 {
			rv++
			if l.Port != 40637 || l.Comm != "rvtest" || l.Addr != "127.0.0.1" {
				t.Errorf("rvtest listener = %+v", l)
			}
		}
	}
	if rv != 2 {
		t.Errorf("rvtest listeners = %d, want 2", rv)
	}
	var ssh, v6 *Listener
	for i := range s.Listeners {
		switch s.Listeners[i].Port {
		case 22:
			ssh = &s.Listeners[i]
		case 63735:
			v6 = &s.Listeners[i]
		}
	}
	if ssh == nil || ssh.PID != 0 || ssh.Addr != "0.0.0.0" {
		t.Errorf("a socket ss could not attribute to a process must parse with pid 0: %+v", ssh)
	}
	if v6 == nil || v6.Addr != "[fd7a:115c:a1e0::8d34:4727]" {
		t.Errorf("ipv6 listener = %+v", v6)
	}
	p := s.Procs[0]
	if p.PID != 3671056 || p.Comm != "rvtest" || p.Exe != "/tmp/rvtest (deleted)" || p.Cwd != "/tmp" || p.Age != 2413380*time.Second {
		t.Errorf("proc = %+v", p)
	}
	ga := mustParse(t, ginOrphanSnapshot)
	if l := ga.Listeners[0]; l.Addr != "*" || l.Port != 8080 || l.PID != 3941235 || l.Comm != "gin" {
		t.Errorf("gin listener = %+v", l)
	}
}

func TestParseFailsClosed(t *testing.T) {
	for name, text := range map[string]string{
		"empty":             "",
		"no header":         "host\tmsr1\nend\tok\n",
		"wrong version":     "hostgate\tv9\nhost\tmsr1\nend\tok\n",
		"truncated":         strings.TrimSuffix(rvtestSnapshot, "end\tok\n"),
		"no host":           "hostgate\tv1\nepoch\t1\nend\tok\n",
		"bad proc age":      "hostgate\tv1\nhost\ta\nproc\t1\t1\tu\tNaN\tc\te\tw\ta\nend\tok\n",
		"short proc line":   "hostgate\tv1\nhost\ta\nproc\t1\t1\nend\tok\n",
		"bad psi":           "hostgate\tv1\nhost\ta\npsi\tio\tsome avg60=oops\nend\tok\n",
		"collector failure": "hostgate\tv1\nhost\ta\nfatal\tcannot read /proc\nend\tok\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := Parse(strings.NewReader(text)); err == nil {
				t.Errorf("Parse accepted %s output; a host the gate cannot read must fail the gate, not pass it", name)
			}
		})
	}
}

// The rvtest case: the process listened on a random loopback port, its binary
// was deleted from /tmp, and it carries no harness name. Only two criteria can
// see it, and this test pins which.
func TestRvtestIsCaughtByPressureAndTheDeletedBinaryRuleNotByPorts(t *testing.T) {
	vs := Evaluate(mustParse(t, rvtestSnapshot), DefaultPolicy())
	got := rules(vs)
	if got[RuleIOPressure] != 1 {
		t.Errorf("io pressure 99%% must trip the gate: %v", vs)
	}
	if got[RuleStaleProcess] != 1 {
		t.Errorf("a 28-day-old process whose binary was deleted from /tmp must trip the gate: %v", vs)
	}
	if got[RuleListener] != 0 {
		t.Errorf("40637 is not a guarded port; a listener violation here means the guard list is wrong: %v", vs)
	}
	// The self-updated GitHub runner has a deleted exe under /tmp too, and is
	// meant to live for days. It must not be reported.
	for _, v := range vs {
		if strings.Contains(strings.Join(v.Lines, "\n"), "Runner.Listener") {
			t.Errorf("the runner's own listener was reported: %v", v)
		}
	}
	if len(vs) == 0 || !strings.Contains(strings.Join(vs[len(vs)-1].Lines, "\n")+strings.Join(vs[0].Lines, "\n"), "3671056") {
		t.Errorf("the violation must name the pid: %v", vs)
	}
}

// The orphan the cancelled bench 37226284351 left on msa2-server: caught by
// the port and by its deleted /tmp/celeris-bench directory, not by pressure.
func TestGinOrphanIsCaughtByPortAndBenchDirNotByPressure(t *testing.T) {
	vs := Evaluate(mustParse(t, ginOrphanSnapshot), DefaultPolicy())
	got := rules(vs)
	if got[RuleListener] != 1 {
		t.Errorf("*:8080 held by gin must trip the listener rule once: %v", vs)
	}
	if got[RuleStaleProcess] != 2 {
		t.Errorf("both the supervisor and the SUT live in a deleted /tmp/celeris-bench: %v", vs)
	}
	if got[RuleIOPressure] != 0 {
		t.Errorf("msa2-server PSI is 0.00: %v", vs)
	}
	text := Annotation(vs)
	for _, want := range []string{"msa2-server", "8080", "3941235", "3941234", "gin"} {
		if !strings.Contains(text, want) {
			t.Errorf("annotation lacks %q:\n%s", want, text)
		}
	}
}

func TestACleanHostPasses(t *testing.T) {
	for _, snap := range []string{idleSnapshot} {
		if vs := Evaluate(mustParse(t, snap), DefaultPolicy()); len(vs) != 0 {
			t.Errorf("idle host violated the gate: %v", vs)
		}
	}
}

func TestPressureThresholdIsStrictlyBelow(t *testing.T) {
	mk := func(v string) Snapshot {
		return mustParse(t, "hostgate\tv1\nhost\th\npsi\tio\tsome avg10=0.00 avg60="+v+" avg300=0.00 total=1\nend\tok\n")
	}
	p := DefaultPolicy()
	p.MaxIOSomeAvg60 = 10
	if vs := Evaluate(mk("9.99"), p); len(vs) != 0 {
		t.Errorf("9.99 < 10 must pass: %v", vs)
	}
	if vs := Evaluate(mk("10.00"), p); rules(vs)[RuleIOPressure] != 1 {
		t.Errorf("10.00 is not below 10 and must fail: %v", vs)
	}
	p.MaxIOSomeAvg60 = 0 // 0 disables the check
	if vs := Evaluate(mk("99.00"), p); len(vs) != 0 {
		t.Errorf("a threshold of 0 disables the pressure check: %v", vs)
	}
}

func TestAgeThresholdIsInclusive(t *testing.T) {
	mk := func(age int) Snapshot {
		return mustParse(t, fmt.Sprintf("hostgate\tv1\nhost\th\nproc\t9\t1\tmini\t%d\tvalidator\t/tmp/celeris-bench/validator\t/tmp/celeris-bench\t./validator\nend\tok\n", age))
	}
	p := DefaultPolicy()
	p.MaxAge = time.Hour
	if vs := Evaluate(mk(3599), p); len(vs) != 0 {
		t.Errorf("a harness process one second under the limit is a run still ending: %v", vs)
	}
	if vs := Evaluate(mk(3600), p); rules(vs)[RuleStaleProcess] != 1 {
		t.Errorf("a harness process at the limit must be reported: %v", vs)
	}
}

func TestHarnessNamesAndPathsAreMatched(t *testing.T) {
	old := 7200
	for name, tc := range map[string]struct {
		line string
		want bool
	}{
		"comm in the cleanup.yml list":   {fmt.Sprintf("proc\t9\t1\tmini\t%d\tvalidator-checker\t/usr/local/bin/x\t/home/mini\tx", old), true},
		"comm server":                    {fmt.Sprintf("proc\t9\t1\tmini\t%d\tserver\t/opt/y\t/\ty", old), true},
		"exe under a live bench dir":     {fmt.Sprintf("proc\t9\t1\tmini\t%d\tbun\t/tmp/celeris-bench/bun/bin/bun\t/tmp/celeris-bench\tbun run dist/server", old), true},
		"cwd under results dir":          {fmt.Sprintf("proc\t9\t1\tmini\t%d\tpython3\t/usr/bin/python3.12\t/tmp/celeris-results/x\tpython -m uvicorn", old), true},
		"deleted binary under tmp":       {fmt.Sprintf("proc\t9\t1\tmini\t%d\tfoo\t/tmp/foo (deleted)\t/home/mini\t./foo", old), true},
		"deleted binary outside tmp":     {fmt.Sprintf("proc\t9\t1\tmini\t%d\tfoo\t/usr/bin/foo (deleted)\t/\tfoo", old), false},
		"live binary under tmp, unknown": {fmt.Sprintf("proc\t9\t1\tmini\t%d\tfoo\t/tmp/foo\t/tmp\t./foo", old), false},
		"runner dir is exempt":           {fmt.Sprintf("proc\t9\t1\tmini\t%d\tnode\t/tmp/actions-runner-msr1/externals/node24/bin/node (deleted)\t/tmp/actions-runner-msr1/_work\tnode x", old), false},
		"unrelated system process":       {fmt.Sprintf("proc\t9\t1\troot\t%d\tcron\t/usr/sbin/cron\t/\tcron -f", old), false},
		"celeris- tool in /usr/local":    {fmt.Sprintf("proc\t9\t1\troot\t%d\tceleris-host-samp\t/usr/bin/bash\t/\t/bin/bash /usr/local/sbin/celeris-host-sampler", old), false},
	} {
		t.Run(name, func(t *testing.T) {
			s := mustParse(t, "hostgate\tv1\nhost\th\n"+tc.line+"\nend\tok\n")
			got := rules(Evaluate(s, DefaultPolicy()))[RuleStaleProcess] == 1
			if got != tc.want {
				t.Errorf("flagged=%v want %v", got, tc.want)
			}
		})
	}
}

func TestListenerGuardIgnoresUnguardedPortsAndSharesOneViolationPerProcess(t *testing.T) {
	s := mustParse(t, "hostgate\tv1\nhost\th\n"+
		"ss\tLISTEN 0 4096 0.0.0.0:8080 0.0.0.0:* users:((\"gin\",pid=5,fd=3))\n"+
		"ss\tLISTEN 0 4096 0.0.0.0:8080 0.0.0.0:* users:((\"gin\",pid=5,fd=4))\n"+
		"ss\tLISTEN 0 4096 [::]:18089 [::]:* users:((\"gin\",pid=5,fd=5))\n"+
		"ss\tLISTEN 0 4096 127.0.0.1:9999 0.0.0.0:* users:((\"x\",pid=6,fd=5))\n"+
		"ss\tLISTEN 0 4096 0.0.0.0:54321 0.0.0.0:*\n"+ // no process info (not root): still a guarded port
		"end\tok\n")
	vs := Evaluate(s, DefaultPolicy())
	if n := rules(vs)[RuleListener]; n != 2 {
		t.Errorf("want one violation for pid 5 (ports 8080 and 18089 folded) and one for the unattributed 54321 listener, got %d: %v", n, vs)
	}
}

func TestMissingPressureIsNotedUnlessRequired(t *testing.T) {
	s := mustParse(t, "hostgate\tv1\nhost\th\nnote\tno /proc/pressure/io\nend\tok\n")
	p := DefaultPolicy()
	if vs := Evaluate(s, p); len(vs) != 0 {
		t.Errorf("a kernel without PSI cannot be gated on it: %v", vs)
	}
	p.RequirePSI = true
	if vs := Evaluate(s, p); rules(vs)[RuleNoPressure] != 1 {
		t.Errorf("RequirePSI must turn the missing reading into a violation: %v", vs)
	}
}

func TestPolicyFromEnv(t *testing.T) {
	env := map[string]string{
		"HOSTGATE_MAX_IO_PSI":    "25.5",
		"HOSTGATE_MAX_AGE_HOURS": "3",
		"HOSTGATE_PORTS":         "8080, 9000",
		"HOSTGATE_REQUIRE_PSI":   "1",
	}
	p, err := PolicyFromEnv(func(k string) string { return env[k] })
	if err != nil {
		t.Fatal(err)
	}
	if p.MaxIOSomeAvg60 != 25.5 || p.MaxAge != 3*time.Hour || !p.RequirePSI || len(p.GuardedPorts) != 2 || p.GuardedPorts[1] != 9000 {
		t.Errorf("policy = %+v", p)
	}
	d, err := PolicyFromEnv(func(string) string { return "" })
	if err != nil || fmt.Sprint(d) != fmt.Sprint(DefaultPolicy()) {
		t.Errorf("no env must give the default policy: %+v %v", d, err)
	}
	for k, v := range map[string]string{
		"HOSTGATE_MAX_IO_PSI":    "abc",
		"HOSTGATE_MAX_AGE_HOURS": "-1",
		"HOSTGATE_PORTS":         "80,notaport",
		"HOSTGATE_REQUIRE_PSI":   "maybe",
	} {
		_, err := PolicyFromEnv(func(n string) string {
			if n == k {
				return v
			}
			return ""
		})
		if err == nil {
			t.Errorf("%s=%q was accepted", k, v)
		}
	}
}

func TestAnnotationIsOneLineForGitHubAndBounded(t *testing.T) {
	var b strings.Builder
	b.WriteString("hostgate\tv1\nhost\tmsr1\n")
	for i := 0; i < 80; i++ {
		fmt.Fprintf(&b, "proc\t%d\t1\tmini\t99999\tserver\t/x\t/y\t./server\n", 1000+i)
	}
	b.WriteString("end\tok\n")
	vs := Evaluate(mustParse(t, b.String()), DefaultPolicy())
	text := Annotation(vs)
	if strings.Contains(text, "\n") {
		t.Errorf("an annotation message is one line (newlines are %%0A): %q", text)
	}
	if !strings.Contains(text, "%0A") || !strings.Contains(text, "more") {
		t.Errorf("long process lists are listed and then truncated with a count: %s", text)
	}
	if len(text) > 4000 {
		t.Errorf("annotation is %d bytes; GitHub truncates long messages", len(text))
	}
}

func TestCheckAllReportsEveryHostAndAnUnreadableHostFailsToo(t *testing.T) {
	read := func(host string) ([]byte, error) {
		switch host {
		case "msr1":
			return []byte(rvtestSnapshot), nil
		case "msa2-server":
			return []byte(ginOrphanSnapshot), nil
		case "msa2-client":
			return []byte(idleSnapshot), nil
		}
		return nil, errors.New("no output for " + host)
	}
	res := CheckAll([]string{"msr1", "msa2-server", "msa2-client", "ghost"}, read, DefaultPolicy())
	if res.OK() {
		t.Fatal("the gate passed")
	}
	by := map[string]int{}
	for _, v := range res.Violations {
		by[v.Host]++
	}
	if by["msr1"] != 2 || by["msa2-server"] != 3 || by["msa2-client"] != 0 || by["ghost"] != 1 {
		t.Errorf("violations by host = %v (%v)", by, res.Violations)
	}
	if got := res.Hosts(); strings.Join(got, ",") != "ghost,msa2-server,msr1" {
		t.Errorf("failing hosts = %v", got)
	}
	ok := CheckAll([]string{"msa2-client"}, read, DefaultPolicy())
	if !ok.OK() {
		t.Errorf("clean host failed: %v", ok.Violations)
	}
}
