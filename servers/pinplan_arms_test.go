package servers_test

import (
	"bytes"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"testing"

	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
)

// THROWAWAY (ab/pinplan-v7). Pins the layout of the PINPLAN7 run (PLAN-pinplan7.md): the exact (scenario, column) pairs the dispatch glob selects, one cell (io256) per column, two arms on ONE
// celeris commit family (rulend = celeris 7af74c6 = #1085's head, default Workers and NO knob, the loops planned by the rx-port rule; vnv = celeris 45a90edf = 7af74c6 + the EXPERIMENT-ONLY
// CELERIS_AB_PLAN_CPUS knob with the CPU list 0-1,7,9-11 a CONSTANT of the module), the single vector position (the standing CPU 1), the position-balanced seeded schedule (8 reps, each arm leads 4),
// and that every column's registry pin equals the celeris require of its module.
const (
	fastGlob = "get-simple-256c/pinplan7-*-io256"

	wantFast = 16 // 8 reps x 2 arms x io256

	pinPin  = "v1.5.12-0.20261019091132-a193acb4e610" // pppin8_celeris (kept as the byte-for-byte reference of the module sources): celeris a193acb4, not a column of this run
	pinRule = "v1.5.12-0.20261020000038-7af74c614248" // rulend: celeris 7af74c6 = #1085 head (branch ab/rulend-20261020), no knob
	pinVnv  = "v1.5.12-0.20261020071651-45a90edfe27f" // vnv: celeris 45a90edf (branch ab/vnv-on-7af74c6) = 7af74c6 + the EXPERIMENT-ONLY knob
)

var (
	arms   = []string{"rulend", "vnv"}
	modOf  = map[string]string{"rulend": "pprulend_celeris", "vnv": "ppvnv7_celeris"}
	pinOf  = map[string]string{"rulend": pinRule, "vnv": pinVnv}
	fwOf   = map[string]string{"rulend": "celeris-pp-rulend", "vnv": "celeris-pp-vnv7"}
	cpusOf = map[string]string{"vnv": "0-1,7,9-11"} // the EXPERIMENT-ONLY CELERIS_AB_PLAN_CPUS list of the one knob arm (a constant of its module); rulend has no knob
)

func matched(t *testing.T, globs string) map[string][]string { // column -> scenarios
	t.Helper()
	got := map[string][]string{}
	for _, sc := range scenarios.Registry() {
		for name := range servers.Registry {
			for _, g := range strings.Split(globs, ",") {
				if ok, _ := path.Match(g, sc.Name()+"/"+name); ok {
					got[name] = append(got[name], sc.Name())
					break
				}
			}
		}
	}
	return got
}

func TestPinplan7GlobsSelectOneCellPerColumn(t *testing.T) {
	fast := matched(t, fastGlob)
	if len(fast) != wantFast {
		t.Fatalf("the dispatch glob selects %d columns, want %d", len(fast), wantFast)
	}
	for c, s := range fast {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1 (the stall captures assume one window per column)", c, len(s), s)
		}
	}
	// the dispatch glob must not catch the previous experiments' columns, and those must not be in this branch's registry
	for name := range servers.Registry {
		for _, old := range []string{"irqplace-", "pinplan-", "pinplan2-", "pinplan3-", "pinplan4-", "pinplan5-", "pinplan6-"} {
			if strings.HasPrefix(name, old) {
				t.Errorf("column %s of an earlier experiment is in this branch's registry", name)
			}
		}
	}
	for _, old := range []string{"get-simple-256c/pinplan2-001-r1-pin-io256", "post-4k/pinplan-001-r1-pin-post4k", "get-simple-256c/pinplan3-001-r1-p8-io256", "get-simple-256c/pinplan4-001-r1-vl2-io256",
		"get-simple-256c/pinplan5-001-r1-base-io256", "get-simple-256c/pinplan6-001-r1-vnv-io256", "get-simple-256c/pinplan6-065-r5-rulend-io256", "get-simple-512c/pinplan7-001-r1-vnv-io512",
		"get-simple-128c/pinplan7-001-r1-vnv-ad128"} {
		for _, one := range strings.Split(fastGlob, ",") {
			if ok, _ := path.Match(one, old); ok {
				t.Errorf("glob %q selects the other experiment's / other cell's column %q", one, old)
			}
		}
	}
}

type col struct {
	name, arm, cell string
	seq, rep        int
}

func ppCols(t *testing.T) []col {
	t.Helper()
	re := regexp.MustCompile(`^pinplan7-(\d{3})-r(\d)-(vnv|rulend)-(io256)$`)
	var fast []col
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "pinplan7-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the naming pinplan7-<seq>-r<rep>-<vnv|rulend>-io256", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		rep, _ := strconv.Atoi(m[2])
		fast = append(fast, col{name, m[3], m[4], seq, rep})
	}
	sort.Slice(fast, func(i, j int) bool { return fast[i].name < fast[j].name })
	return fast
}

func modDir(name string) string {
	return filepath.Join("..", servers.Registry[name].Bin.(servers.GoBinary).ModuleDir)
}

// checkSchedule verifies the dispatch: seq numbering 1..16 without gaps, 8 reps x 2 arm slots of the one cell, each rep holding both arms once, and the position balance registered in
// PLAN-pinplan7.md section 3: each arm leads exactly 2 of reps 1-4 and exactly 2 of reps 5-8 (so 4 of 8), no arm leads more than 2 consecutive reps, and the leaders are not strictly alternating.
func checkSchedule(t *testing.T, cols []col) {
	t.Helper()
	const reps = 8
	if len(cols) != reps*2 {
		t.Fatalf("%d columns, want %d", len(cols), reps*2)
	}
	for i, c := range cols { // sorted name order is the execution order: seq 1.. without gaps
		if c.seq != 1+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, 1+i)
		}
	}
	lead := make([]string, reps)
	for k := 0; k < reps; k++ {
		a, b := cols[2*k], cols[2*k+1]
		if a.rep != k+1 || b.rep != k+1 || a.arm == b.arm || a.cell != "io256" || b.cell != "io256" {
			t.Errorf("rep %d is not two different arms of io256: %s, %s", k+1, a.name, b.name)
		}
		lead[k] = a.arm
	}
	for _, half := range [][]string{lead[:4], lead[4:]} {
		n := 0
		for _, a := range half {
			if a == "rulend" {
				n++
			}
		}
		if n != 2 {
			t.Errorf("each arm must lead exactly 2 of every 4 reps, leaders %v", lead)
		}
	}
	same := false
	for k := 0; k+1 < reps; k++ {
		same = same || lead[k] == lead[k+1]
		if k+2 < reps && lead[k] == lead[k+1] && lead[k] == lead[k+2] {
			t.Errorf("arm %s leads reps %d-%d: more than 2 consecutive reps", lead[k], k+1, k+3)
		}
	}
	if !same {
		t.Errorf("the leaders are strictly alternating: %v", lead)
	}
}

func TestPinplan7Layout(t *testing.T) {
	fast := ppCols(t)
	checkSchedule(t, fast)
	for _, c := range fast {
		a := servers.Registry[c.name]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != "1" || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=1 (the vector stays on its standing CPU; nothing moves it; the loop CPUs are a constant of the module, not an env)", c.name, a.SUTEnv)
		}
		if filepath.Base(modDir(c.name)) != modOf[c.arm] || a.FrameworkVersion != pinOf[c.arm] || a.Framework != fwOf[c.arm] {
			t.Errorf("%s: module %s framework %s version %s, want %s %s %s", c.name, modDir(c.name), a.Framework, a.FrameworkVersion, modOf[c.arm], fwOf[c.arm], pinOf[c.arm])
		}
		gomod, err := os.ReadFile(filepath.Join(modDir(c.name), "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gomod), "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", c.name, a.FrameworkVersion, modDir(c.name))
		}
	}
	// the v6 V-NV (celeris 3d0ddb0a, built on main 5 commits older than RULE-D's) is NOT in this run: neither as a module in the tree nor as a pin in any column
	for _, gone := range []string{"ppvnv_celeris", "ppbase_celeris"} {
		if _, err := os.Stat(gone); err == nil {
			t.Errorf("servers/%s is in this branch (the v6 V-NV / BASE modules are not part of PINPLAN7)", gone)
		}
	}
	for _, c := range fast {
		if strings.Contains(servers.Registry[c.name].FrameworkVersion, "3d0ddb0a") || strings.Contains(servers.Registry[c.name].FrameworkVersion, "a193acb4") {
			t.Errorf("%s carries a pin of the v6 reference arms: %s", c.name, servers.Registry[c.name].FrameworkVersion)
		}
	}
}

// Both arms are copies of the committed servers/celeris except for go.mod/go.sum (the module line and the pin) and its debugvars_test.go; both additionally differ in server.go by the
// one `Workers: armWorkers,` line (and its comment) and in arm.go.
func TestPinplan7ArmModulesAreCopiesOfServersCeleris(t *testing.T) {
	ents, err := os.ReadDir("celeris")
	if err != nil {
		t.Fatal(err)
	}
	for _, arm := range arms {
		mod := modOf[arm]
		for _, e := range ents {
			if strings.HasSuffix(e.Name(), "_test.go") || e.Name() == "go.mod" || e.Name() == "go.sum" {
				continue
			}
			a, _ := os.ReadFile(filepath.Join("celeris", e.Name()))
			b, err := os.ReadFile(filepath.Join(mod, e.Name()))
			if err != nil {
				t.Fatal(err)
			}
			if e.Name() == "server.go" {
				b = []byte(stripArmLines(string(b)))
			}
			if !bytes.Equal(a, b) {
				t.Errorf("%s/%s differs from servers/celeris/%s", mod, e.Name(), e.Name())
			}
		}
		have, err := os.ReadDir(mod)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range have {
			if _, err := os.Stat(filepath.Join("celeris", e.Name())); err != nil {
				if e.Name() != "arm.go" {
					t.Errorf("%s has the extra file %s", mod, e.Name())
				}
			}
		}
	}
}

// stripArmLines removes the arm's own lines from server.go: the comment line that names the arm and the Workers line itself (the lines are the v3 ones:
// the three #1049-based modules reuse pppin8_celeris/server.go byte for byte).
func stripArmLines(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if strings.Contains(l, "AB ARM (throwaway, ab/pinplan-v3)") || strings.TrimSpace(l) == "Workers: armWorkers," {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

const knobEnv = "CELERIS_AB_PLAN_CPUS"

// The loop CPUs of a variant are a constant of its module (arm.go: `const armPlanCPUs = "<list>"` and an init() that os.Setenv's the knob from it, so
// a value the run's environment carries is overwritten and nothing can unset it); Config.Workers stays unset (armWorkers = 0) in every #1049-based
// arm; rulend never mentions the knob (7af74c6 does not have it); no other file of any module mentions it or reads the
// environment for a CPU list.
func TestPinplan7PlanKnobIsAModuleConstant(t *testing.T) {
	for _, arm := range arms {
		mod := modOf[arm]
		entries, _ := os.ReadDir(mod)
		for _, e := range entries {
			if !strings.HasSuffix(e.Name(), ".go") {
				continue
			}
			src, _ := os.ReadFile(filepath.Join(mod, e.Name()))
			s := string(src)
			if strings.Contains(s, "GOMAXPROCS") || strings.Contains(s, "CELERIS_WORKERS") || strings.Contains(s, "Getenv(\"CELERIS_") {
				t.Errorf("%s/%s reads or sets a worker count or a CPU list from the environment or the runtime", mod, e.Name())
			}
			if strings.Contains(s, knobEnv) && !(e.Name() == "arm.go" && cpusOf[arm] != "") {
				t.Errorf("%s/%s mentions %s: only the variants' arm.go may", mod, e.Name(), knobEnv)
			}
		}
		srv, _ := os.ReadFile(filepath.Join(mod, "server.go"))
		if n := strings.Count(string(srv), "Workers: armWorkers,"); n != 1 {
			t.Errorf("%s/server.go has %d `Workers: armWorkers,` lines, want exactly 1", mod, n)
		}
		src, err := os.ReadFile(filepath.Join(mod, "arm.go"))
		if err != nil {
			t.Fatal(err)
		}
		s := string(src)
		if n := strings.Count(s, "const armWorkers = 0\n"); n != 1 {
			t.Errorf("%s/arm.go: want exactly one `const armWorkers = 0` (Config.Workers unset in every #1049-based arm)", mod)
		}
		want := cpusOf[arm]
		if want == "" { // rulend: no knob at all
			if strings.Contains(s, "armPlanCPUs") || strings.Contains(s, "Setenv") {
				t.Errorf("%s/arm.go must not carry the knob", mod)
			}
			continue
		}
		if n := strings.Count(s, "const armPlanCPUs = \""+want+"\"\n"); n != 1 {
			t.Errorf("%s/arm.go: want exactly one `const armPlanCPUs = %q`", mod, want)
		}
		if n := strings.Count(s, "os.Setenv(\""+knobEnv+"\", armPlanCPUs)"); n != 1 || strings.Count(s, "Setenv") != 1 || !strings.Contains(s, "func init() {") {
			t.Errorf("%s/arm.go: want exactly one os.Setenv(%q, armPlanCPUs) inside init()", mod, knobEnv)
		}
		if strings.Contains(s, "os.Getenv") || strings.Contains(s, "os.Unsetenv") || strings.Contains(s, "LookupEnv") {
			t.Errorf("%s/arm.go reads or unsets the environment", mod)
		}
	}
}

// pppin8_celeris is the v3 module (celeris a193acb4), kept as the byte-for-byte reference of the module sources; rulend and vnv differ from it ONLY in the module line and the celeris pin of go.mod
// and in the celeris lines of go.sum, plus arm.go (vnv: the knob constant and its init; rulend: only a comment, it has no knob). Everything else is byte for byte its. And vnv differs from rulend
// in exactly the same way: the two arms' sources are one program, the celeris commit is what differs (by the knob code).
func TestPinplan7VariantsDifferFromPin8OnlyInThePin(t *testing.T) {
	ref := "pppin8_celeris"
	pins := map[string]string{"ppvnv7_celeris": pinVnv, "pprulend_celeris": pinRule}
	for other, pin := range pins {
		for _, f := range []string{"chain_handlers.go", "debugvars.go", "driver_handlers.go", "streaming_handlers.go", "server.go"} {
			a, _ := os.ReadFile(filepath.Join(ref, f))
			b, _ := os.ReadFile(filepath.Join(other, f))
			if !bytes.Equal(a, b) {
				t.Errorf("%s/%s differs from %s/%s", other, f, ref, f)
			}
		}
		a, _ := os.ReadFile(filepath.Join(ref, "go.mod"))
		b, _ := os.ReadFile(filepath.Join(other, "go.mod"))
		if got := strings.ReplaceAll(strings.ReplaceAll(string(a), ref, other), pinPin, pin); got != string(b) {
			t.Errorf("%s/go.mod differs from %s/go.mod by more than the module line and the celeris pin", other, ref)
		}
		ga, _ := os.ReadFile(filepath.Join(ref, "go.sum"))
		gb, _ := os.ReadFile(filepath.Join(other, "go.sum"))
		rest := func(s string) string { // go.sum without its two celeris lines
			var out []string
			for _, l := range strings.Split(s, "\n") {
				if !strings.HasPrefix(l, "github.com/goceleris/celeris ") {
					out = append(out, l)
				}
			}
			return strings.Join(out, "\n")
		}
		if rest(string(ga)) != rest(string(gb)) {
			t.Errorf("%s/go.sum differs from %s/go.sum outside the celeris lines", other, ref)
		}
		if !strings.Contains(string(gb), "github.com/goceleris/celeris "+pin+" h1:") || !strings.Contains(string(gb), "github.com/goceleris/celeris "+pin+"/go.mod h1:") || strings.Contains(string(gb), pinPin) {
			t.Errorf("%s/go.sum must hold exactly the h1 lines of celeris %s", other, pin)
		}
	}
	// vnv's pin is the knob commit on top of 7af74c6 (celeris branch ab/vnv-on-7af74c6): its short sha is the pseudo-version's suffix
	if !strings.HasSuffix(pinVnv, "-45a90edfe27f") {
		t.Errorf("the vnv pin %s is not celeris 45a90edfe27f (7af74c6 + the knob)", pinVnv)
	}
	// the two arm modules are the same program: every file but arm.go, go.mod and go.sum is byte for byte equal
	ents, _ := os.ReadDir("pprulend_celeris")
	for _, e := range ents {
		switch e.Name() {
		case "arm.go", "go.mod", "go.sum":
			continue
		}
		a, _ := os.ReadFile(filepath.Join("pprulend_celeris", e.Name()))
		b, _ := os.ReadFile(filepath.Join("ppvnv7_celeris", e.Name()))
		if !bytes.Equal(a, b) {
			t.Errorf("ppvnv7_celeris/%s differs from pprulend_celeris/%s", e.Name(), e.Name())
		}
	}
	// the rulend pin is the head of celeris PR #1085 (7af74c6142486bd049ed13db1e674d5b1bcb9c25, branch ab/rulend-20261020): its short sha is the pseudo-version's suffix
	if !strings.HasSuffix(pinRule, "-7af74c614248") {
		t.Errorf("the rulend pin %s is not celeris 7af74c614248 (#1085's head)", pinRule)
	}
	gm, _ := os.ReadFile(filepath.Join(ref, "go.mod"))
	if !strings.Contains(string(gm), "github.com/goceleris/celeris "+pinPin+"\n") {
		t.Errorf("%s/go.mod does not require celeris %s", ref, pinPin)
	}
	gs, _ := os.ReadFile(filepath.Join(ref, "go.sum"))
	if !strings.Contains(string(gs), "github.com/goceleris/celeris "+pinPin+" h1:") {
		t.Errorf("%s/go.sum has no h1 line for celeris %s", ref, pinPin)
	}
	// rulend: arm.go is the p8 one up to its comment: one `const armWorkers = 0`, no knob, no environment access, nothing else
	rb, _ := os.ReadFile(filepath.Join("pprulend_celeris", "arm.go"))
	var code []string
	for _, l := range strings.Split(string(rb), "\n") {
		if s := strings.TrimSpace(l); s != "" && !strings.HasPrefix(s, "//") {
			code = append(code, s)
		}
	}
	if strings.Join(code, "|") != "package main|const armWorkers = 0" {
		t.Errorf("pprulend_celeris/arm.go holds code other than `package main` and `const armWorkers = 0`: %v", code)
	}
}

func expandCPUs(t *testing.T, list string) map[int]bool {
	t.Helper()
	out := map[int]bool{}
	for _, tok := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(tok, "-")
		a, err := strconv.Atoi(lo)
		if err != nil {
			t.Fatalf("bad CPU list %q", list)
		}
		b := a
		if isRange {
			if b, err = strconv.Atoi(hi); err != nil {
				t.Fatalf("bad CPU list %q", list)
			}
		}
		for c := a; c <= b; c++ {
			out[c] = true
		}
	}
	return out
}

// V-NV (the one knob arm) keeps six loops on CPUs 0,1,7,9,10,11: loops on 0, 1, 7, 9, 10 and 11, none on 6 and 8 (the two busiest NIC-vector CPUs of the standing
// state) and none on a little CPU. This is the plan RULE-D has to reproduce by itself; the expected set is also written in the analyzer and the validator.
func TestPinplan7VnvCPUSet(t *testing.T) {
	vnv := expandCPUs(t, cpusOf["vnv"])
	if len(vnv) != 6 {
		t.Fatalf("loop count of vnv: %d, want 6", len(vnv))
	}
	for _, c := range []int{0, 1, 7, 9, 10, 11} {
		if !vnv[c] {
			t.Errorf("CPU %d must carry a loop in vnv", c)
		}
	}
	for _, c := range []int{2, 3, 4, 5, 6, 8} {
		if vnv[c] {
			t.Errorf("CPU %d carries a loop in vnv", c)
		}
	}
}
