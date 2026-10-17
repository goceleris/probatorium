// Package hostgate decides whether a cluster host is clean enough to start a
// tier on (probatorium#473, ask 3).
//
// The gate is REPORT-ONLY. It reads what hostgate/collect.sh printed on each
// host, parses it, applies a Policy, and says which hosts are not clean and
// why. It never signals, stops or removes anything, here or on a host: a
// process it names may belong to the maintainer (the msr1 "rvtest" was an
// agent's smoke test that nobody had asked about), and a person decides that.
//
// What it looks for, and which real incident each rule is the answer to:
//
//   - listener: something already bound to a port the harness is about to
//     use (the SUT port, its debug sidecar, the fixture backends). The gin
//     SUT that cancelled bench 37226284351 left on msa2-server held *:8080.
//   - stale-process: a process that is old and looks like it belongs to a
//     previous run, by harness binary name, by living in (or having lived in)
//     a /tmp/celeris-* directory, or by running a binary that was deleted
//     from /tmp. The msr1 rvtest listened on a random loopback port and had
//     no harness name; its deleted /tmp binary is what identifies it.
//   - io-pressure: idle IO pressure (PSI "some" avg60) at or above a
//     threshold. rvtest's 12 workers parked in io_cqring_wait held it at 99.
//   - no-data: the host could not be read, or its output did not parse. A gate
//     that cannot see a host must not pass it.
package hostgate

import (
	"bufio"
	"fmt"
	"io"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Rule names a violation's kind.
const (
	RuleListener     = "listener"
	RuleStaleProcess = "stale-process"
	RuleIOPressure   = "io-pressure"
	RuleNoPressure   = "no-pressure-reading"
	RuleNoData       = "no-data"
)

// Listener is one listening TCP socket. PID is 0 when ss could not attribute
// the socket (it can only do so for processes the caller may inspect).
type Listener struct {
	Addr string
	Port int
	PID  int
	Comm string
}

// Proc is one process the collector reported (kernel threads are not).
type Proc struct {
	PID, PPID int
	User      string
	Age       time.Duration
	Comm      string
	Exe       string // readlink /proc/pid/exe, "(deleted)" suffix kept
	Cwd       string // readlink /proc/pid/cwd, "(deleted)" suffix kept
	Args      string // first words only; shell scripts are elided (they carry secrets)
}

// PSI is the IO pressure reading.
type PSI struct {
	Present   bool
	SomeAvg60 float64
	FullAvg60 float64
}

// Snapshot is one host's parsed collector output.
type Snapshot struct {
	Host      string
	Epoch     int64
	Listeners []Listener
	Procs     []Proc
	IO        PSI
	Notes     []string
}

// Policy holds every threshold; DefaultPolicy documents the defaults.
type Policy struct {
	// GuardedPorts: any listener on one of these is a violation.
	GuardedPorts []int
	// MaxAge: a harness-looking process at least this old is stale.
	MaxAge time.Duration
	// MaxIOSomeAvg60 is the IO PSI "some" avg60 (percent) the host must stay
	// BELOW. Zero disables the check.
	MaxIOSomeAvg60 float64
	// RequirePSI turns a missing pressure reading into a violation.
	RequirePSI bool
}

// Harness identity, mirrored from ansible/cleanup.yml, which kills the same
// names. Keep the two in step.
var (
	harnessComms = []string{"runner", "loadgen", "observer", "validator", "validator-checker", "validator-replay", "conformance", "server"}
	// Anything the harness stages lives under one of these (inventory.yml:
	// bench_root, results_root).
	harnessPathPrefixes = []string{"/tmp/celeris-"}
	// Paths that are legitimately long-lived and often "(deleted)": the
	// GitHub Actions runner replaces its own bin directory when it updates.
	exemptPathPrefixes = []string{"/tmp/actions-runner-"}
)

// DefaultPolicy is the policy the tiers run with unless HOSTGATE_* says
// otherwise.
//
//   - Ports: 8080 (SUT, bench_port), 18089 (celeris debug sidecar,
//     bench_debug_port), 54321 / 63791 / 21211 (postgres, redis, memcached
//     fixtures; bench.yml starts them, so before a tier they are leftovers).
//   - MaxAge 1 h: the gate runs before anything of the tier starts and the
//     tiers share one concurrency group, so nothing of the harness can be
//     legitimately alive; the margin is for a previous run's last processes
//     still exiting.
//   - MaxIOSomeAvg60 25: an idle host reads 0.00; the conductor has just
//     installed Go and mage, which is the only legitimate source of a blip.
//     The incident read 99.
func DefaultPolicy() Policy {
	return Policy{
		GuardedPorts:   []int{8080, 18089, 54321, 63791, 21211},
		MaxAge:         time.Hour,
		MaxIOSomeAvg60: 25,
	}
}

// PolicyFromEnv applies HOSTGATE_MAX_IO_PSI (percent, 0 disables),
// HOSTGATE_MAX_AGE_HOURS, HOSTGATE_PORTS (csv) and HOSTGATE_REQUIRE_PSI
// (1/true/0/false) over DefaultPolicy. A malformed value is an error, never a
// silent default: a typo must not quietly relax the gate.
func PolicyFromEnv(getenv func(string) string) (Policy, error) {
	p := DefaultPolicy()
	if v := strings.TrimSpace(getenv("HOSTGATE_MAX_IO_PSI")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 || f > 100 {
			return p, fmt.Errorf("HOSTGATE_MAX_IO_PSI=%q: want a percentage 0-100", v)
		}
		p.MaxIOSomeAvg60 = f
	}
	if v := strings.TrimSpace(getenv("HOSTGATE_MAX_AGE_HOURS")); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || f < 0 {
			return p, fmt.Errorf("HOSTGATE_MAX_AGE_HOURS=%q: want hours >= 0", v)
		}
		p.MaxAge = time.Duration(f * float64(time.Hour))
	}
	if v := strings.TrimSpace(getenv("HOSTGATE_PORTS")); v != "" {
		p.GuardedPorts = nil
		for _, f := range strings.Split(v, ",") {
			n, err := strconv.Atoi(strings.TrimSpace(f))
			if err != nil || n < 1 || n > 65535 {
				return p, fmt.Errorf("HOSTGATE_PORTS=%q: %q is not a port", v, f)
			}
			p.GuardedPorts = append(p.GuardedPorts, n)
		}
	}
	if v := strings.TrimSpace(getenv("HOSTGATE_REQUIRE_PSI")); v != "" {
		b, err := strconv.ParseBool(v)
		if err != nil {
			return p, fmt.Errorf("HOSTGATE_REQUIRE_PSI=%q: want 1/0/true/false", v)
		}
		p.RequirePSI = b
	}
	return p, nil
}

var (
	ssLocal = regexp.MustCompile(`^LISTEN\s+\d+\s+\d+\s+(\S+)\s+\S+`)
	ssUser  = regexp.MustCompile(`\("([^"]*)",pid=(\d+),fd=\d+\)`)
)

// Parse reads collect.sh output. It is strict on purpose: the gate's one
// unforgivable error is passing a host it could not read, so a missing header,
// a missing end marker, a collector "fatal" line or any malformed record is an
// error.
func Parse(r io.Reader) (Snapshot, error) {
	var s Snapshot
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 64*1024), 1024*1024)
	n, header, ended := 0, false, false
	fail := func(format string, a ...any) (Snapshot, error) {
		return Snapshot{}, fmt.Errorf("line %d: %s", n, fmt.Sprintf(format, a...))
	}
	for sc.Scan() {
		n++
		line := strings.TrimRight(sc.Text(), "\r")
		if line == "" {
			continue
		}
		f := strings.Split(line, "\t")
		if n == 1 || !header {
			if len(f) != 2 || f[0] != "hostgate" || f[1] != "v1" {
				return fail("not hostgate v1 output (got %q)", truncate(line, 60))
			}
			header = true
			continue
		}
		if ended {
			return fail("data after the end marker")
		}
		switch f[0] {
		case "host":
			if len(f) != 2 || f[1] == "" {
				return fail("malformed host record")
			}
			s.Host = f[1]
		case "epoch":
			if len(f) != 2 {
				return fail("malformed epoch record")
			}
			e, err := strconv.ParseInt(f[1], 10, 64)
			if err != nil {
				return fail("epoch %q", f[1])
			}
			s.Epoch = e
		case "note":
			s.Notes = append(s.Notes, strings.Join(f[1:], " "))
		case "fatal":
			return fail("collector failed: %s", strings.Join(f[1:], " "))
		case "psi":
			if len(f) != 3 || f[1] != "io" {
				return fail("malformed psi record")
			}
			kind, avg60, err := parsePSI(f[2])
			if err != nil {
				return fail("psi %q: %v", truncate(f[2], 60), err)
			}
			switch kind {
			case "some":
				s.IO.Present, s.IO.SomeAvg60 = true, avg60
			case "full":
				s.IO.FullAvg60 = avg60
			}
		case "ss":
			if len(f) < 2 {
				return fail("malformed ss record")
			}
			ls, err := parseSS(strings.Join(f[1:], "\t"))
			if err != nil {
				return fail("%v", err)
			}
			s.Listeners = append(s.Listeners, ls...)
		case "proc":
			p, err := parseProc(line)
			if err != nil {
				return fail("%v", err)
			}
			s.Procs = append(s.Procs, p)
		case "end":
			if len(f) != 2 || f[1] != "ok" {
				return fail("collector did not finish cleanly: %q", line)
			}
			ended = true
		default:
			// A record this version does not know is ignored: a newer
			// collector may add some. Only malformed KNOWN records fail.
		}
	}
	if err := sc.Err(); err != nil {
		return Snapshot{}, err
	}
	if !header {
		return Snapshot{}, fmt.Errorf("no output")
	}
	if !ended {
		return Snapshot{}, fmt.Errorf("output ends without the end marker (truncated)")
	}
	if s.Host == "" {
		return Snapshot{}, fmt.Errorf("no host record")
	}
	return s, nil
}

func parsePSI(raw string) (kind string, avg60 float64, err error) {
	w := strings.Fields(raw)
	if len(w) == 0 || (w[0] != "some" && w[0] != "full") {
		return "", 0, fmt.Errorf("want some/full")
	}
	for _, kv := range w[1:] {
		if v, ok := strings.CutPrefix(kv, "avg60="); ok {
			avg60, err = strconv.ParseFloat(v, 64)
			if err != nil {
				return "", 0, fmt.Errorf("avg60 %q", v)
			}
			return w[0], avg60, nil
		}
	}
	return "", 0, fmt.Errorf("no avg60")
}

func parseSS(raw string) ([]Listener, error) {
	m := ssLocal.FindStringSubmatch(strings.TrimSpace(raw))
	if m == nil {
		return nil, fmt.Errorf("unrecognised ss line %q", truncate(raw, 80))
	}
	local := m[1]
	i := strings.LastIndex(local, ":")
	if i < 0 {
		return nil, fmt.Errorf("ss address %q has no port", local)
	}
	port, err := strconv.Atoi(local[i+1:])
	if err != nil {
		return nil, fmt.Errorf("ss port in %q", local)
	}
	addr := local[:i]
	if j := strings.Index(addr, "%"); j >= 0 { // 127.0.0.53%lo
		addr = addr[:j]
	}
	users := ssUser.FindAllStringSubmatch(raw, -1)
	if len(users) == 0 {
		return []Listener{{Addr: addr, Port: port}}, nil
	}
	var out []Listener
	for _, u := range users {
		pid, _ := strconv.Atoi(u[2])
		out = append(out, Listener{Addr: addr, Port: port, PID: pid, Comm: u[1]})
	}
	return out, nil
}

func parseProc(line string) (Proc, error) {
	f := strings.SplitN(line, "\t", 9)
	if len(f) != 9 {
		return Proc{}, fmt.Errorf("proc record has %d fields, want 9", len(f))
	}
	pid, e1 := strconv.Atoi(f[1])
	ppid, e2 := strconv.Atoi(f[2])
	age, e3 := strconv.ParseInt(f[4], 10, 64)
	if e1 != nil || e2 != nil || e3 != nil || age < 0 {
		return Proc{}, fmt.Errorf("proc record has a non-numeric pid/ppid/age: %q", truncate(line, 80))
	}
	return Proc{PID: pid, PPID: ppid, User: f[3], Age: time.Duration(age) * time.Second,
		Comm: f[5], Exe: f[6], Cwd: f[7], Args: f[8]}, nil
}

// Violation is one reason a host is not clean. Lines is the evidence, one
// line per process or socket, safe to print.
type Violation struct {
	Host    string
	Rule    string
	Summary string
	Lines   []string
}

func (p Proc) line() string {
	return fmt.Sprintf("pid=%d ppid=%d user=%s age=%s comm=%s exe=%s cwd=%s args=%s",
		p.PID, p.PPID, p.User, p.Age.Round(time.Minute), p.Comm, p.Exe, p.Cwd, p.Args)
}

func hasPrefixAny(s string, prefixes []string) bool {
	for _, p := range prefixes {
		if strings.HasPrefix(s, p) {
			return true
		}
	}
	return false
}

func isHarnessComm(comm string) bool {
	for _, n := range harnessComms {
		// /proc truncates comm to 15 bytes ("validator-check").
		if comm == n || (len(comm) == 15 && strings.HasPrefix(n, comm)) {
			return true
		}
	}
	return false
}

// staleReasons says why a process looks like a previous run's, or nil.
func staleReasons(p Proc, pol Policy) []string {
	if p.Age < pol.MaxAge {
		return nil
	}
	if hasPrefixAny(p.Exe, exemptPathPrefixes) || hasPrefixAny(p.Cwd, exemptPathPrefixes) {
		return nil
	}
	var why []string
	if isHarnessComm(p.Comm) {
		why = append(why, "harness binary name "+p.Comm)
	}
	if hasPrefixAny(p.Exe, harnessPathPrefixes) {
		why = append(why, "binary in "+p.Exe)
	}
	if hasPrefixAny(p.Cwd, harnessPathPrefixes) {
		why = append(why, "working directory "+p.Cwd)
	}
	if strings.HasPrefix(p.Exe, "/tmp/") && strings.HasSuffix(p.Exe, " (deleted)") {
		why = append(why, "binary deleted from /tmp")
	}
	return why
}

// Evaluate applies the policy to one snapshot. Violations come out in a fixed
// order (listeners, processes by pid, pressure) so output is stable.
func Evaluate(s Snapshot, pol Policy) []Violation {
	var out []Violation

	guarded := map[int]bool{}
	for _, p := range pol.GuardedPorts {
		guarded[p] = true
	}
	type group struct {
		comm  string
		ports []string
	}
	groups := map[string]*group{}
	var order []string
	for _, l := range s.Listeners {
		if !guarded[l.Port] {
			continue
		}
		key := fmt.Sprintf("pid:%d", l.PID)
		if l.PID == 0 {
			key = fmt.Sprintf("unattributed:%s:%d", l.Addr, l.Port)
		}
		g := groups[key]
		if g == nil {
			g = &group{comm: l.Comm}
			groups[key] = g
			order = append(order, key)
		}
		entry := fmt.Sprintf("%s:%d", l.Addr, l.Port)
		if !contains(g.ports, entry) {
			g.ports = append(g.ports, entry)
		}
	}
	byPID := map[int]Proc{}
	for _, p := range s.Procs {
		byPID[p.PID] = p
	}
	for _, key := range order {
		g := groups[key]
		line := "listening on " + strings.Join(g.ports, ", ")
		sum := "an unexpected listener holds " + strings.Join(g.ports, ", ")
		if pid, ok := strings.CutPrefix(key, "pid:"); ok {
			n, _ := strconv.Atoi(pid)
			line = fmt.Sprintf("pid=%d comm=%s %s", n, g.comm, line)
			if p, ok := byPID[n]; ok {
				line += fmt.Sprintf(" (age=%s exe=%s cwd=%s)", p.Age.Round(time.Minute), p.Exe, p.Cwd)
			}
			sum = fmt.Sprintf("pid %d (%s) holds %s", n, g.comm, strings.Join(g.ports, ", "))
		} else {
			line = "a process ss could not attribute (run as root to see it) is " + line
		}
		out = append(out, Violation{Host: s.Host, Rule: RuleListener, Summary: sum, Lines: []string{line}})
	}

	procs := append([]Proc(nil), s.Procs...)
	sort.Slice(procs, func(i, j int) bool { return procs[i].PID < procs[j].PID })
	var stale []string
	for _, p := range procs {
		if why := staleReasons(p, pol); len(why) > 0 {
			stale = append(stale, p.line()+" [reason: "+strings.Join(why, "; ")+"]")
		}
	}
	for _, l := range stale {
		out = append(out, Violation{Host: s.Host, Rule: RuleStaleProcess,
			Summary: "a process from a previous run is still alive", Lines: []string{l}})
	}

	switch {
	case s.IO.Present && pol.MaxIOSomeAvg60 > 0 && s.IO.SomeAvg60 >= pol.MaxIOSomeAvg60:
		out = append(out, Violation{Host: s.Host, Rule: RuleIOPressure,
			Summary: fmt.Sprintf("idle IO pressure is %.2f%% (some avg60), limit is below %.2f%%", s.IO.SomeAvg60, pol.MaxIOSomeAvg60),
			Lines:   []string{fmt.Sprintf("/proc/pressure/io some avg60=%.2f full avg60=%.2f; threads parked in io_cqring_wait count as iowait, see the process list above", s.IO.SomeAvg60, s.IO.FullAvg60)}})
	case !s.IO.Present && pol.RequirePSI:
		out = append(out, Violation{Host: s.Host, Rule: RuleNoPressure,
			Summary: "no /proc/pressure/io reading and HOSTGATE_REQUIRE_PSI is set", Lines: s.Notes})
	}
	return out
}

func contains(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}

// Result is the gate's verdict over a set of hosts.
type Result struct {
	Checked    []string
	Violations []Violation
	Snapshots  map[string]Snapshot
}

// OK reports whether every host passed.
func (r Result) OK() bool { return len(r.Violations) == 0 }

// Hosts returns the sorted, de-duplicated names of the hosts that failed.
func (r Result) Hosts() []string {
	seen := map[string]bool{}
	var out []string
	for _, v := range r.Violations {
		if !seen[v.Host] {
			seen[v.Host] = true
			out = append(out, v.Host)
		}
	}
	sort.Strings(out)
	return out
}

// CheckAll reads, parses and evaluates every host. read returns one host's raw
// collector output; a read or parse failure is itself a no-data violation.
func CheckAll(hosts []string, read func(host string) ([]byte, error), pol Policy) Result {
	res := Result{Checked: hosts, Snapshots: map[string]Snapshot{}}
	for _, h := range hosts {
		raw, err := read(h)
		if err != nil {
			res.Violations = append(res.Violations, Violation{Host: h, Rule: RuleNoData,
				Summary: "the host could not be read, so it cannot be vouched for", Lines: []string{err.Error()}})
			continue
		}
		s, err := Parse(strings.NewReader(string(raw)))
		if err != nil {
			res.Violations = append(res.Violations, Violation{Host: h, Rule: RuleNoData,
				Summary: "the host's gate output could not be parsed, so it cannot be vouched for", Lines: []string{err.Error()}})
			continue
		}
		s.Host = h
		res.Snapshots[h] = s
		res.Violations = append(res.Violations, Evaluate(s, pol)...)
	}
	return res
}

const (
	maxAnnotationLines = 30
	maxAnnotationBytes = 3500
)

// Annotation renders violations as ONE GitHub annotation message: newlines
// are %0A, the list is capped, and the cap is announced.
func Annotation(vs []Violation) string {
	if len(vs) == 0 {
		return ""
	}
	var lines []string
	cur := ""
	for _, v := range vs {
		if v.Host != cur {
			cur = v.Host
			lines = append(lines, fmt.Sprintf("%s:", v.Host))
		}
		lines = append(lines, fmt.Sprintf("  [%s] %s", v.Rule, v.Summary))
		for _, l := range v.Lines {
			lines = append(lines, "    "+l)
		}
	}
	lines = append(lines, "The gate only reports; it stopped and removed nothing. Identify the owner of each process before killing it.")
	shown := lines
	more := 0
	if len(shown) > maxAnnotationLines {
		more = len(shown) - (maxAnnotationLines - 1)
		last := shown[len(shown)-1]
		shown = append(append([]string(nil), shown[:maxAnnotationLines-2]...), fmt.Sprintf("  ... and %d more line(s), see the job log", more), last)
	}
	msg := escapeData(strings.Join(shown, "\n"))
	if len(msg) > maxAnnotationBytes {
		msg = msg[:maxAnnotationBytes] + escapeData("\n  ... truncated, see the job log")
	}
	return msg
}

// escapeData is GitHub's workflow-command data escaping.
func escapeData(s string) string {
	s = strings.ReplaceAll(s, "%", "%25")
	s = strings.ReplaceAll(s, "\r", "%0D")
	return strings.ReplaceAll(s, "\n", "%0A")
}

// WorkflowCommand is the `::error` line for a failed Result.
func WorkflowCommand(res Result) string {
	return fmt.Sprintf("::error title=Host gate failed on %s::%s", strings.Join(res.Hosts(), "%2C "), Annotation(res.Violations))
}
