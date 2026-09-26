package report

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

// faultcontrol.go judges the celeris#588 capture control (probatorium#351):
// a refapp run with PROBATORIUM_REFAPP_FAULT holds a lock that every
// request on one path needs, at a known instant, and this checker decides
// from the ARTIFACT ALONE whether the capture that landed for #588
// (probatorium#347) would have root-caused it: the right counter moved, a
// slow-fire record carries the instant, the leg that expired, the verbatim
// error and both addresses, and a dossier taken INSIDE the stall holds a
// goroutine dump that names the holder and its waiters.
//
// The ground truth is the refapp's own log line for each hold
// ("[fault] hold path=... hold=... start=..." / "[fault] release path=...
// end=... waiters=..."), which lands in the cell's refapp stderr tail. The
// checker never trusts the capture to find the fault; it checks the capture
// against the fault.

// The frames a goroutine dump shows inside a hold
// (validation/refapp/internal/debugvars/faults.go; its test pins them).
const (
	FaultHolderFrame = "debugvars.(*FaultHold).run"
	FaultWaiterFrame = "debugvars.(*FaultHold).wait"
)

// FaultInjection is one hold as the refapp logged it.
type FaultInjection struct {
	Path    string
	Hold    time.Duration
	Start   time.Time
	End     time.Time // zero when the cell ended inside the hold
	Waiters int64     // -1 when no release line was seen
}

var (
	faultHoldRE    = regexp.MustCompile(`\[fault\] hold path=(\S+) hold=(\S+) start=(\S+)`)
	faultReleaseRE = regexp.MustCompile(`\[fault\] release path=(\S+) end=(\S+) waiters=(\d+)`)
)

// ParseFaultLog extracts the holds from a refapp log, oldest first. The
// same line may arrive twice (the cell summary's stderr tail and the cell's
// refapp_stderr_tail.txt carry the same text); a hold is identified by its
// path and start instant and kept once.
func ParseFaultLog(lines []string) []FaultInjection {
	var out []FaultInjection
	idx := map[string]int{}
	seen := map[string]bool{}
	for _, l := range lines {
		if m := faultHoldRE.FindStringSubmatch(l); m != nil {
			if seen[m[1]+" "+m[3]] {
				continue
			}
			seen[m[1]+" "+m[3]] = true
			hold, _ := time.ParseDuration(m[2])
			start, _ := time.Parse(time.RFC3339Nano, m[3])
			idx[m[1]] = len(out)
			out = append(out, FaultInjection{Path: m[1], Hold: hold, Start: start, Waiters: -1})
			continue
		}
		if m := faultReleaseRE.FindStringSubmatch(l); m != nil {
			if i, ok := idx[m[1]]; ok {
				out[i].End, _ = time.Parse(time.RFC3339Nano, m[2])
				out[i].Waiters, _ = strconv.ParseInt(m[3], 10, 64)
			}
		}
	}
	return out
}

// FaultControlResult is the verdict on one matrix cell.
type FaultControlResult struct {
	Refapp, Engine, Arch string
	CellDir              string
	Injections           []FaultInjection
	// Findings is what the artifact says, in the words a reader would use
	// to root-cause the event; Failures are the checks that did not hold.
	Findings []string
	Failures []string
}

// Pass reports whether every check held.
func (r FaultControlResult) Pass() bool { return len(r.Failures) == 0 }

// faultClass is what one path's walker must show.
type faultClass struct {
	slice, total, timeout string // Tier1Summary map and keys
	outcome               string // SlowFire.Outcome of the failed fire
	minReadMs             int64  // the walker's read budget, less slack
	errPart               string // substring of SlowFire.Err
	predicate             string // the gated counter's record-only dossier
	stallPredicate        string // the in-stall dossier (taken while a read still waits)
	ring                  func(*Tier1Summary) []SlowFire
	counters              func(*Tier1Summary) map[string]int64
}

var faultClasses = map[string]faultClass{
	// The WS-torture handshake: 2 s budget for dial + write + 101.
	"/ws": {slice: "ws_torture", total: "ws_handshake_fail", timeout: "ws_handshake_fail_timeout",
		outcome: "handshake-fail-timeout", minReadMs: 1500, errPart: "timeout", predicate: "I-WS-HANDSHAKE", stallPredicate: "I-WS-STALL",
		ring: func(t *Tier1Summary) []SlowFire { return t.WSSlowReads }, counters: func(t *Tier1Summary) map[string]int64 { return t.WSTorture }},
	// The h2c-churn preamble (GET / with Upgrade: h2c): 20 s read budget.
	"/": {slice: "h2c_churn", total: "h2c_hang", timeout: "h2c_hang_timeout",
		outcome: "hang-timeout", minReadMs: 19000, errPart: "timeout", predicate: "I-H2C-HANG", stallPredicate: "I-H2C-STALL",
		ring: func(t *Tier1Summary) []SlowFire { return t.H2CSlowReads }, counters: func(t *Tier1Summary) map[string]int64 { return t.H2CChurn }},
}

// FaultControlPaths is the set of paths the checker knows how to judge.
func FaultControlPaths() []string {
	var p []string
	for k := range faultClasses {
		p = append(p, k)
	}
	sort.Strings(p)
	return p
}

// recordSlack is how long after a hold's release a failed fire may still be
// recorded: a fire sent just before the release ends at its own deadline.
const recordSlack = 3 * time.Second

// CheckFaultControlCell judges one cell. cellDir is the cell's directory
// in the run artifact (it holds incidents/ and refapp_stderr_tail.txt);
// wantPaths are the paths the run's PROBATORIUM_REFAPP_FAULT named for this
// cell (nil for an un-injected cell, which must then show NONE of the
// events -- the false-positive control).
func CheckFaultControlCell(cellDir string, cell ValidationCellResult, wantPaths []string) FaultControlResult {
	r := FaultControlResult{Refapp: cell.Refapp, Engine: cell.Engine, Arch: cell.Arch, CellDir: cellDir}
	fail := func(f string, a ...any) { r.Failures = append(r.Failures, fmt.Sprintf(f, a...)) }
	note := func(f string, a ...any) { r.Findings = append(r.Findings, fmt.Sprintf(f, a...)) }
	t1 := cell.Tier1
	if t1 == nil {
		fail("no tier-1 summary: the walkers never ran")
		return r
	}
	lines := t1.RefappStderrTail
	if b, err := os.ReadFile(filepath.Join(cellDir, "refapp_stderr_tail.txt")); err == nil {
		lines = append(lines, strings.Split(string(b), "\n")...)
	}
	r.Injections = ParseFaultLog(lines)
	byPath := map[string]FaultInjection{}
	for _, inj := range r.Injections {
		byPath[inj.Path] = inj
	}
	dossiers, _ := filepath.Glob(filepath.Join(cellDir, "incidents", "*"))

	for _, path := range wantPaths {
		fc, known := faultClasses[path]
		if !known {
			fail("%s: no judged walker uses this path (known: %v)", path, FaultControlPaths())
			continue
		}
		inj, fired := byPath[path]
		if !fired {
			fail("%s: the refapp never logged the hold -- the fault did not fire (cell shorter than its offset, or the env never reached the refapp)", path)
			continue
		}
		end := inj.End
		if end.IsZero() {
			end = inj.Start.Add(inj.Hold)
		}
		cnt := fc.counters(t1)
		if cnt[fc.total] < 1 || cnt[fc.timeout] < 1 {
			fail("%s: %s.%s=%d %s=%d, want both >= 1: the gated counter did not see the injected stall",
				path, fc.slice, fc.total, cnt[fc.total], fc.timeout, cnt[fc.timeout])
		}
		// The per-event record: instant, leg, error, addresses.
		var in []SlowFire
		for _, sf := range fc.ring(t1) {
			ts, err := time.Parse(time.RFC3339Nano, sf.TS)
			if err != nil || sf.Outcome != fc.outcome {
				continue
			}
			if !ts.Before(inj.Start) && !ts.After(end.Add(recordSlack)) {
				in = append(in, sf)
			}
		}
		if len(in) == 0 {
			fail("%s: no %q record in the slow-fire ring between the hold's start %s and release+%s",
				path, fc.outcome, inj.Start.Format(time.RFC3339Nano), recordSlack)
		} else {
			sf := in[0]
			switch {
			case sf.ReadMs < fc.minReadMs:
				fail("%s: first record's read_ms=%d < %d: the READ leg is not the one that expired", path, sf.ReadMs, fc.minReadMs)
			case !strings.Contains(sf.Err, fc.errPart):
				fail("%s: first record's err=%q does not name the deadline", path, sf.Err)
			case sf.LocalAddr == "" || sf.RemoteAddr == "":
				fail("%s: first record carries no socket addresses (local=%q remote=%q)", path, sf.LocalAddr, sf.RemoteAddr)
			case sf.ValidatorSkewMs != 0:
				fail("%s: first record says the validator itself stalled %d ms: the stall is not attributable to the server", path, sf.ValidatorSkewMs)
			}
			note("%s: %d %q record(s) inside the hold; first at +%s after its start: dial %d ms, write %d ms, read %d ms, err %q, %s -> %s, validator skew 0",
				path, len(in), fc.outcome, tsSince(sf.TS, inj.Start), sf.DialMs, sf.WriteMs, sf.ReadMs, sf.Err, sf.LocalAddr, sf.RemoteAddr)
		}
		// The dossiers. The gated counter's own record-only dossier must
		// exist (it is what the gate's failure points at), and at least one
		// dossier -- that one, or the in-stall one taken while a read was
		// still waiting -- must have been observed INSIDE the hold with a
		// goroutine dump that names both the holder and a waiter: that is
		// the dump from which the stall can be root-caused. A dossier taken
		// after the release shows a healthy process and names nothing.
		gated := findDossier(dossiers, fc.predicate)
		if gated == "" {
			fail("%s: no incidents/*-%s dossier", path, fc.predicate)
		}
		var inside []string
		for _, d := range dossiers {
			base := filepath.Base(d)
			if !strings.HasSuffix(base, "-"+fc.predicate) && !strings.HasSuffix(base, "-"+fc.stallPredicate) {
				continue
			}
			obs := dossierObservedAt(d)
			stacks, serr := os.ReadFile(filepath.Join(d, "goroutine-stacks.txt"))
			holder := strings.Count(string(stacks), FaultHolderFrame+"(")
			waiters := strings.Count(string(stacks), FaultWaiterFrame+"(")
			in := !obs.IsZero() && !obs.Before(inj.Start) && !obs.After(end)
			// The hold's own log line must be in the dossier's stderr tail
			// for the dossier to tie its dump to the ground truth. Judged
			// only on the dossiers that could root-cause THIS hold: one
			// taken before it (on the event-loop engines a hold on another
			// path parks the loops and stalls this path's walker too, the
			// #588 control at 3a62131) predates the line by construction.
			tail, terr := os.ReadFile(filepath.Join(d, "refapp_stderr_tail.txt"))
			tailOK := terr == nil && strings.Contains(string(tail), "[fault] hold path="+path+" ")
			switch {
			case serr != nil:
				note("%s: dossier %s has no goroutine-stacks.txt (%v)", path, base, serr)
			case !in:
				note("%s: dossier %s observed at %s, outside the hold [+0, +%s]: its dump (%d holder / %d waiter frame(s)) cannot name the stall",
					path, base, obs.Sub(inj.Start).Round(time.Millisecond), end.Sub(inj.Start).Round(time.Millisecond), holder, waiters)
			case holder < 1 || waiters < 1:
				note("%s: dossier %s observed inside the hold but its dump shows %d holder / %d waiter frame(s)", path, base, holder, waiters)
			case !tailOK:
				note("%s: dossier %s names the stall inside the hold, but its refapp_stderr_tail.txt lacks the hold line (err=%v): it cannot be tied to the injected hold",
					path, base, terr)
			default:
				inside = append(inside, base)
				note("%s: dossier %s observed at +%s after the hold's start: its goroutine dump shows %d request goroutine(s) blocked in %s and the holder asleep in %s -- the stall is a held lock on %s",
					path, base, obs.Sub(inj.Start).Round(time.Millisecond), waiters, FaultWaiterFrame, FaultHolderFrame, path)
			}
			if _, err := os.Stat(filepath.Join(d, "core.skipped")); err != nil {
				fail("%s: dossier %s has no core.skipped marker: a record-only incident must not have paused the refapp", path, base)
			}
		}
		if len(inside) == 0 {
			fail("%s: no %s or %s dossier was observed inside the hold with a goroutine dump naming holder and waiter and the hold line in its stderr tail: the stall is not root-causable from this artifact",
				path, fc.predicate, fc.stallPredicate)
		}
	}

	// False-positive control: every judged path the run did NOT inject must
	// show none of its events.
	injected := map[string]bool{}
	for _, p := range wantPaths {
		injected[p] = true
	}
	for _, path := range FaultControlPaths() {
		if injected[path] {
			continue
		}
		fc := faultClasses[path]
		cnt := fc.counters(t1)
		if cnt[fc.total] != 0 {
			fail("%s (not injected): %s.%s=%d, want 0", path, fc.slice, fc.total, cnt[fc.total])
		}
		if d := findDossier(dossiers, fc.predicate); d != "" {
			fail("%s (not injected): unexpected dossier %s", path, filepath.Base(d))
		}
		for _, sf := range fc.ring(t1) {
			if sf.Outcome == fc.outcome {
				fail("%s (not injected): a %q record at %s", path, fc.outcome, sf.TS)
				break
			}
		}
	}
	return r
}

func findDossier(dirs []string, predicate string) string {
	for _, d := range dirs {
		if strings.HasSuffix(filepath.Base(d), "-"+predicate) {
			return d
		}
	}
	return ""
}

func dossierObservedAt(dir string) time.Time {
	b, err := os.ReadFile(filepath.Join(dir, "incident.json"))
	if err != nil {
		return time.Time{}
	}
	var inc struct {
		ObservedAt string `json:"observed_at"`
	}
	if json.Unmarshal(b, &inc) != nil {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339Nano, inc.ObservedAt)
	return t
}

func tsSince(ts string, origin time.Time) time.Duration {
	t, err := time.Parse(time.RFC3339Nano, ts)
	if err != nil {
		return 0
	}
	return t.Sub(origin).Round(time.Millisecond)
}
