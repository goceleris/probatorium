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

// THROWAWAY (ab/irqplace-v2, ab/irqplace-v2-90s). Pins the layout of the IRQPLACE v2 re-run (PLAN-irqplace-v2.md): the exact (scenario, column)
// pairs each dispatch glob selects, one cell per column, two arms (m = d8b2a0f, p2 = 7d81cc1d) whose modules differ in the celeris pin only,
// the vector CPU per block, the paired and arm-balanced schedule, and that every column's registry pin equals the celeris require of its module.
const (
	fastGlob   = "get-simple-256c/irqplace-*-io256,get-simple-128c/irqplace-*-ad128,get-simple-512c/irqplace-*-ep512,post-4k/irqplace-*-post4k"
	ninetyGlob = "get-simple-512c/irqplace-*-io512"

	wantFast = 64 // 2 vector blocks x 4 reps x 2 arms x 4 cells
	wantNine = 12 // 2 vector blocks x 3 reps x 2 arms x io512

	pinM  = "v1.5.12-0.20261017204940-d8b2a0f78ed5"
	pinP2 = "v1.5.12-0.20261018131023-7d81cc1dca47"
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

func TestIRQPlaceGlobsSelectOneCellPerColumn(t *testing.T) {
	fast := matched(t, fastGlob)
	if len(fast) != wantFast {
		t.Fatalf("dispatch 1 glob selects %d columns, want %d", len(fast), wantFast)
	}
	for c, s := range fast {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1 (the stall captures assume one window per column)", c, len(s), s)
		}
	}
	ninety := matched(t, ninetyGlob)
	if len(ninety) != wantNine {
		t.Fatalf("dispatch 2 glob selects %d columns, want %d", len(ninety), wantNine)
	}
	for c, s := range ninety {
		if len(s) != 1 {
			t.Errorf("column %s matches %d scenarios %v, want exactly 1", c, len(s), s)
		}
		if _, dup := fast[c]; dup {
			t.Errorf("column %s is in both dispatches", c)
		}
	}
}

type col struct {
	name, arm, cell string
	seq, blk, rep   int
}

func irqCols(t *testing.T) (fast, nine []col) {
	t.Helper()
	re := regexp.MustCompile(`^irqplace-(\d{3})-b([12])r(\d)-(m|p2)-(io256|ad128|ep512|post4k|io512)$`)
	for name := range servers.Registry {
		if !strings.HasPrefix(name, "irqplace-") {
			continue
		}
		m := re.FindStringSubmatch(name)
		if m == nil {
			t.Errorf("column %s does not match the v2 naming irqplace-<seq>-b<block>r<rep>-<m|p2>-<cell>", name)
			continue
		}
		seq, _ := strconv.Atoi(m[1])
		blk, _ := strconv.Atoi(m[2])
		rep, _ := strconv.Atoi(m[3])
		c := col{name, m[4], m[5], seq, blk, rep}
		if c.cell == "io512" {
			nine = append(nine, c)
		} else {
			fast = append(fast, c)
		}
	}
	sort.Slice(fast, func(i, j int) bool { return fast[i].name < fast[j].name })
	sort.Slice(nine, func(i, j int) bool { return nine[i].name < nine[j].name })
	return fast, nine
}

func modDir(name string) string {
	return filepath.Join("..", servers.Registry[name].Bin.(servers.GoBinary).ModuleDir)
}

// checkSchedule verifies one dispatch: seq numbering, the vector per block, reps x arms x cells, the pairing (the columns of rep k of both
// vectors are contiguous), and that each arm leads in a balanced number of reps in each vector block.
func checkSchedule(t *testing.T, cols []col, firstSeq, reps int, cells []string) {
	t.Helper()
	perRep := 2 * len(cells)
	if len(cols) != 2*reps*perRep {
		t.Fatalf("%d columns, want %d", len(cols), 2*reps*perRep)
	}
	for i, c := range cols { // sorted name order is the execution order: seq firstSeq.. without gaps
		if c.seq != firstSeq+i {
			t.Fatalf("sorted column %d is %s, want seq %03d", i, c.name, firstSeq+i)
		}
	}
	lead := map[[2]string]int{} // (block, arm) -> reps in which the arm leads
	firstBlk := map[string]int{}
	for k := 0; k < reps; k++ { // pair k: 2 rep-blocks of perRep columns, both vectors, rep k+1
		pair := cols[k*2*perRep : (k+1)*2*perRep]
		blkFirst := pair[0].blk
		firstBlk[strconv.Itoa(blkFirst)]++
		for b := 0; b < 2; b++ {
			rb := pair[b*perRep : (b+1)*perRep]
			if (b == 0) != (rb[0].blk == blkFirst) {
				t.Errorf("pair %d: the second rep-block must be the other vector (%s)", k+1, rb[0].name)
			}
			lead[[2]string{strconv.Itoa(rb[0].blk), rb[0].arm}]++
			for a := 0; a < 2; a++ { // arm slot: all cells of one arm, in the fixed cell order
				slot := rb[a*len(cells) : (a+1)*len(cells)]
				for ci, c := range slot {
					if c.blk != rb[0].blk || c.rep != k+1 || c.arm != slot[0].arm || c.cell != cells[ci] {
						t.Errorf("%s: out of place in pair %d (block %d, rep %d, arm %s, cell order %v)", c.name, k+1, rb[0].blk, k+1, slot[0].arm, cells)
					}
				}
				if a == 1 && slot[0].arm == rb[0].arm {
					t.Errorf("pair %d block %d: the two arm slots are both %s", k+1, rb[0].blk, slot[0].arm)
				}
			}
		}
	}
	for _, blk := range []string{"1", "2"} {
		for _, arm := range []string{"m", "p2"} {
			n := lead[[2]string{blk, arm}]
			if reps%2 == 0 && n != reps/2 || reps%2 == 1 && n != reps/2 && n != reps/2+1 {
				t.Errorf("block %s: arm %s leads in %d of %d reps (the order must be balanced)", blk, arm, n, reps)
			}
		}
		n := firstBlk[blk]
		if reps%2 == 0 && n != reps/2 || reps%2 == 1 && n != reps/2 && n != reps/2+1 {
			t.Errorf("block %s goes first in %d of %d pairs (must be balanced)", blk, n, reps)
		}
	}
}

func TestIRQPlaceLayout(t *testing.T) {
	fast, nine := irqCols(t)
	checkSchedule(t, fast, 1, 4, []string{"io256", "ad128", "ep512", "post4k"})
	checkSchedule(t, nine, 901, 3, []string{"io512"})
	for _, c := range append(append([]col{}, fast...), nine...) {
		a := servers.Registry[c.name]
		wantVec := map[int]string{1: "4", 2: "1"}[c.blk]
		if got := a.SUTEnv["AB_IRQ_VECTOR_CPU"]; got != wantVec || len(a.SUTEnv) != 1 {
			t.Errorf("%s: SUTEnv %v, want only AB_IRQ_VECTOR_CPU=%s", c.name, a.SUTEnv, wantVec)
		}
		wantMod, wantPin := map[string]string{"m": "irqm_celeris", "p2": "irqp2_celeris"}[c.arm], map[string]string{"m": pinM, "p2": pinP2}[c.arm]
		if filepath.Base(modDir(c.name)) != wantMod || a.FrameworkVersion != wantPin || a.Framework != "celeris-irq-"+c.arm {
			t.Errorf("%s: module %s framework %s version %s, want %s celeris-irq-%s %s", c.name, modDir(c.name), a.Framework, a.FrameworkVersion, wantMod, c.arm, wantPin)
		}
		gomod, err := os.ReadFile(filepath.Join(modDir(c.name), "go.mod"))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(gomod), "github.com/goceleris/celeris "+a.FrameworkVersion+"\n") {
			t.Errorf("%s: registry pin %s is not the celeris require of %s", c.name, a.FrameworkVersion, modDir(c.name))
		}
		src, _ := os.ReadFile(filepath.Join(modDir(c.name), "server.go"))
		if strings.Contains(string(src), "Workers:") {
			t.Errorf("%s: the module sets Workers; both arms run the default worker count (the rule must plan it, not the harness)", c.name)
		}
	}
}

// The two arms must differ in the celeris pin and nothing else: P2 - M is then the rule (git diff d8b2a0f 7d81cc1d has no dependency change).
func TestIRQPlaceArmModulesDifferOnlyInThePin(t *testing.T) {
	m, p2 := "irqm_celeris", "irqp2_celeris"
	ents, err := os.ReadDir(m)
	if err != nil {
		t.Fatal(err)
	}
	other, err := os.ReadDir(p2)
	if err != nil {
		t.Fatal(err)
	}
	if len(ents) != len(other) {
		t.Fatalf("irqm_celeris has %d files, irqp2_celeris %d", len(ents), len(other))
	}
	for _, e := range ents {
		a, _ := os.ReadFile(filepath.Join(m, e.Name()))
		b, err := os.ReadFile(filepath.Join(p2, e.Name()))
		if err != nil {
			t.Fatal(err)
		}
		switch e.Name() {
		case "go.mod":
			a2 := strings.ReplaceAll(strings.ReplaceAll(string(a), "irqm_celeris", "irqp2_celeris"), pinM, pinP2)
			if a2 != string(b) {
				t.Errorf("go.mod differs by more than the module line and the celeris pin")
			}
		case "go.sum":
			for _, l := range strings.Split(string(b), "\n") {
				if l != "" && !strings.Contains(string(a), l) && !strings.Contains(l, pinP2) {
					t.Errorf("go.sum of irqp2_celeris carries %q, which is neither shared with irqm_celeris nor the p2 pin", l)
				}
			}
		default:
			if !bytes.Equal(a, b) {
				t.Errorf("%s differs between irqm_celeris and irqp2_celeris", e.Name())
			}
		}
	}
}
