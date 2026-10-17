package checker

import (
	"bufio"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// fixtureHeap reads the heap_inuse series of msr1's observability/std cell of
// soak 38441116109 (properties/testdata): no leak, one I-MEM-1 trip before the
// scatter floor.
func fixtureHeap(t *testing.T) []int64 {
	t.Helper()
	f, err := os.Open("../properties/testdata/imem1_obs_std_arm64_soak38441116109.csv")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []int64
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		v, err := strconv.ParseInt(line, 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, v)
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// replayHeap feeds a heap_inuse series through the real Evaluator the way the
// property loop does: the first 151 s are idle window 1 (the prelude), after
// which sustained load starts and the slope warm-up runs from there.
func replayHeap(heap []int64, specs []properties.Spec) Tally {
	ev := NewEvaluator(specs)
	base := int64(1_700_000_000)
	for i, h := range heap {
		snap := properties.Snapshot{TS: base + int64(i), HeapInuseBytes: h, GoroutineCount: 100}
		if i < 151 {
			snap.IdleWindow = 1
		}
		ev.Observe(snap, time.Unix(snap.TS, 0))
	}
	return ev.Tally()
}

// A series on which the old rule trips and the scatter floor does not: the
// gated verdict is the floor's (no violation), and the informational twin
// records the old rule's trip as absolved.
func TestInformational_oldTripAbsolvedByScatterFloor(t *testing.T) {
	tally := replayHeap(fixtureHeap(t), []properties.Spec{properties.IMEM1})
	if got := tally.PerPredicate["I-MEM-1"]; got != 0 {
		t.Fatalf("gated I-MEM-1 violations = %d, want 0", got)
	}
	if tally.Violations != 0 || len(tally.ViolationIDs) != 0 || tally.Failed() != 0 {
		t.Fatalf("the gated tally must be clean: violations=%d ids=%v failed=%d", tally.Violations, tally.ViolationIDs, tally.Failed())
	}
	sh, ok := tally.Informational["I-MEM-1"]
	if !ok {
		t.Fatal("no informational record of the old rule's trip")
	}
	// the soak recorded 77 persisted violations, first at t=1498 s
	if sh.Violations != 77 || sh.AbsolvedEvaluations != 77 || !sh.Absolved {
		t.Fatalf("informational = %+v, want 77 violations, all 77 absolved", sh)
	}
	if sh.Label != properties.IMEM1ShadowLabel || !strings.Contains(sh.FirstMessage, "I-MEM-1 violated") {
		t.Fatalf("label/message wrong: %+v", sh)
	}
}

// A real leak is a trip both rules raise: the gated verdict fails, and the
// informational record is not flagged as absolved.
func TestInformational_bothTrip(t *testing.T) {
	heap := make([]int64, 2701)
	for i := range heap {
		heap[i] = 20<<20 + int64(max(0, i-451))*4096 // 4 KB/s from the end of warm-up
	}
	tally := replayHeap(heap, []properties.Spec{properties.IMEM1})
	if tally.PerPredicate["I-MEM-1"] == 0 || tally.Failed() != 1 {
		t.Fatalf("a 4 KB/s leak must fail the gated rule: per=%v failed=%d", tally.PerPredicate, tally.Failed())
	}
	sh := tally.Informational["I-MEM-1"]
	if sh.Violations < tally.PerPredicate["I-MEM-1"] || sh.Absolved {
		t.Fatalf("informational = %+v with %d gated violations: a trip both rules raise is not absolved", sh, tally.PerPredicate["I-MEM-1"])
	}
}

// The informational twin never changes anything the gate reads: the whole
// tally, minus the informational map, is the same with and without it.
func TestInformational_doesNotChangeTheGatedTally(t *testing.T) {
	without := properties.IMEM1
	without.Shadow = nil
	for name, heap := range map[string][]int64{
		"fixture": fixtureHeap(t),
		"leak": func() []int64 {
			h := make([]int64, 2701)
			for i := range h {
				h[i] = 20<<20 + int64(max(0, i-451))*4096
			}
			return h
		}(),
	} {
		a := replayHeap(heap, []properties.Spec{properties.IMEM1})
		b := replayHeap(heap, []properties.Spec{without})
		a.Informational = nil
		ja, _ := json.Marshal(a)
		jb, _ := json.Marshal(b)
		if string(ja) != string(jb) {
			t.Errorf("%s: the gated tally differs with the informational twin:\n with: %s\n without: %s", name, ja, jb)
		}
	}
}

// BenchmarkIMEM1Observe measures the evaluator over a 45 minute cell with and
// without the informational twin.
func BenchmarkIMEM1Observe(b *testing.B) {
	heap := make([]int64, 2701)
	for i := range heap {
		heap[i] = 20<<20 + int64(i%97)*3000
	}
	run := func(b *testing.B, spec properties.Spec) {
		for b.Loop() {
			replayHeap(heap, []properties.Spec{spec})
		}
	}
	b.Run("with-twin", func(b *testing.B) { run(b, properties.IMEM1) })
	without := properties.IMEM1
	without.Shadow = nil
	b.Run("without-twin", func(b *testing.B) { run(b, without) })
}
