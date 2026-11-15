package main

import (
	"os"
	"os/exec"
	"regexp"
	"strings"
	"testing"
)

// The line contracts with l756e-validate.py (the same expressions are pinned there).
var (
	affinityLineRE   = regexp.MustCompile(`^\[affinity\] env=(unset|[0-9]+-[0-9]+) range=(none|[0-9]+-[0-9]+) tids=[0-9]+ set=[0-9]+ verified=[0-9]+ allowed=(na|[0-9,-]+)$`)
	gomaxprocsLineRE = regexp.MustCompile(`^\[gomaxprocs\] env=(unset|[0-9]+) runtime=[0-9]+ numcpu=[0-9]+$`)
)

func TestParseAffinity(t *testing.T) {
	good := map[string][2]int{"0-13": {0, 13}, "0-0": {0, 0}, "3-3": {3, 3}, "0-1023": {0, 1023}, "10-15": {10, 15}}
	for raw, want := range good {
		lo, hi, err := parseAffinity(raw)
		if err != nil || lo != want[0] || hi != want[1] {
			t.Errorf("parseAffinity(%q) = %d,%d,%v want %v", raw, lo, hi, err, want)
		}
	}
	for _, raw := range []string{"", "0", "13-0", "0-1024", "-1-3", "0-13,15", "00-13", "0-013", " 0-13", "0-13 ", "a-b", "0x0-13", "0:13", "0-13-14"} {
		if _, _, err := parseAffinity(raw); err == nil {
			t.Errorf("parseAffinity(%q) accepted", raw)
		}
	}
}

func TestAffinityMask(t *testing.T) {
	m := affinityMask(0, 13)
	if m[0] != 1<<14-1 {
		t.Errorf("0-13 mask = %#x", m[0])
	}
	m = affinityMask(62, 66)
	if m[0] != 3<<62 || m[1] != 7 {
		t.Errorf("62-66 mask = %#x %#x", m[0], m[1])
	}
	for i := 2; i < 16; i++ {
		if m[i] != 0 {
			t.Errorf("word %d = %#x", i, m[i])
		}
	}
}

func TestL756ELineFormats(t *testing.T) {
	un := affinityState{allowed: "0-15", tids: 12, set: 0}
	if got := un.line(); got != "[affinity] env=unset range=none tids=12 set=0 verified=0 allowed=0-15" || !affinityLineRE.MatchString(got) {
		t.Errorf("unset line %q", got)
	}
	set := affinityState{env: "0-13", envSet: true, lo: 0, hi: 13, tids: 11, set: 11, checked: 11, allowed: "0-13"}
	if got := set.line(); got != "[affinity] env=0-13 range=0-13 tids=11 set=11 verified=11 allowed=0-13" || !affinityLineRE.MatchString(got) {
		t.Errorf("set line %q", got)
	}
	if got := (affinityState{}).line(); !strings.HasSuffix(got, "allowed=na") || !affinityLineRE.MatchString(got) {
		t.Errorf("na line %q", got)
	}
	t.Setenv("GOMAXPROCS", "20")
	if got := gomaxprocsLine(); !gomaxprocsLineRE.MatchString(got) || !strings.HasPrefix(got, "[gomaxprocs] env=20 runtime=") {
		t.Errorf("gomaxprocs line %q", got)
	}
}

// A malformed value is fatal at start, never a silent default (child process: log.Fatalf).
func TestAffinityFatalOnBadValue(t *testing.T) {
	if os.Getenv("L756E_AFFINITY_FATAL_CHILD") == "1" {
		mustApplyAffinityKnob()
		return
	}
	for _, raw := range []string{"", "13-0", "0-13,15", "x"} {
		cmd := exec.Command(os.Args[0], "-test.run=TestAffinityFatalOnBadValue")
		cmd.Env = append(os.Environ(), "L756E_AFFINITY_FATAL_CHILD=1", affinityEnv+"="+raw)
		if err := cmd.Run(); err == nil {
			t.Errorf("%s=%q did not fail at start", affinityEnv, raw)
		}
	}
}

// Source order of server.go: both knobs before celeris.New, the lines after the [workers] line, the sampler last.
func TestServerOrderL756E(t *testing.T) {
	b, err := os.ReadFile("server.go")
	if err != nil {
		t.Fatal(err)
	}
	s := string(b)
	idx := func(sub string) int {
		i := strings.Index(s, sub)
		if i < 0 {
			t.Fatalf("server.go has no %q", sub)
		}
		return i
	}
	wk, af, nw := idx("mustApplyWorkersKnob(&cfg)"), idx("mustApplyAffinityKnob()"), idx("celeris.New(cfg)")
	wkL, afL, gpL, st := idx("log.Print(wk.line())"), idx("log.Print(af.line())"), idx("log.Print(gomaxprocsLine())"), idx("mustStartSchedSampler()")
	if !(wk < af && af < nw) {
		t.Errorf("order: workers knob %d, affinity knob %d, celeris.New %d", wk, af, nw)
	}
	if !(nw < wkL && wkL < afL && afL < gpL && gpL < st) {
		t.Errorf("order: celeris.New %d, [workers] %d, [affinity] %d, [gomaxprocs] %d, sampler %d", nw, wkL, afL, gpL, st)
	}
}
