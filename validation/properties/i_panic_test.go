package properties

import (
	"strings"
	"testing"
)

func panicSnaps(vals ...[2]int64) (Snapshot, Context) {
	var hist []Snapshot
	for i, v := range vals {
		hist = append(hist, Snapshot{TS: int64(1000 + i), PanicCount: v[0], ExpectedPanics: v[1]})
	}
	cur := hist[len(hist)-1]
	return cur, Context{History: hist}
}

// Designed panics (corpus `expect: panic`) are netted out: a server that
// panicked exactly as often as the workload asked passes.
func TestIPANIC_DesignedPanicsPass(t *testing.T) {
	cur, ctx := panicSnaps([2]int64{2994, 2994}, [2]int64{3010, 3010}, [2]int64{3022, 3022})
	if ok, msg := IPANIC.Predicate(&cur, ctx); !ok {
		t.Fatalf("designed panics must not fire I-PANIC: %s", msg)
	}
}

// A one-sample lead of the server over the walker (panic recorded before its
// 5xx is tallied) is transient and must not fire.
func TestIPANIC_TransientLeadPasses(t *testing.T) {
	cur, ctx := panicSnaps([2]int64{10, 10}, [2]int64{11, 11}, [2]int64{13, 12})
	if ok, msg := IPANIC.Predicate(&cur, ctx); !ok {
		t.Fatalf("a one-sample lead must not fire: %s", msg)
	}
}

// An unexpected panic persisting across three snapshots fires, naming both counts.
func TestIPANIC_PersistentUnexpectedPanicFires(t *testing.T) {
	cur, ctx := panicSnaps([2]int64{5, 5}, [2]int64{6, 5}, [2]int64{6, 5}, [2]int64{6, 5})
	ok, msg := IPANIC.Predicate(&cur, ctx)
	if ok {
		t.Fatal("a persistent unexpected panic must fire I-PANIC")
	}
	if want := "1 unexpected panic(s) (panic_count=6, expected=5)"; !strings.Contains(msg, want) {
		t.Fatalf("message %q lacks %q", msg, want)
	}
}

// With no expected-panic accounting wired (standalone checker), any persistent
// panic is unexpected — the pre-existing contract.
func TestIPANIC_NoAccountingKeepsOldContract(t *testing.T) {
	cur, ctx := panicSnaps([2]int64{1, 0}, [2]int64{1, 0}, [2]int64{1, 0})
	if ok, _ := IPANIC.Predicate(&cur, ctx); ok {
		t.Fatal("a persistent panic with no accounting must fire")
	}
}
