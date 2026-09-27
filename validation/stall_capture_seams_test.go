package validation

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// The seams probatorium#412's review round added to runner.go / liveness.go:
// the stall-capture gate, the at-most-once incident offer and the in-stall
// dossier budget. See stall_capture_gate_test.go for the behaviour at the
// orchestrator's boundary.

func TestStallCaptureEnabledOnlyForAFaultOrTheKnob(t *testing.T) {
	for _, tc := range []struct {
		fault, knob string
		want        bool
	}{
		{"", "", false},
		{"  ", "", false},
		{"", "0", false},
		{"", "yes", false},
		{"/ws:8s@30s,/:40s@60s", "", true},
		{"", "1", true},
		{"/ws:8s@30s", "1", true},
	} {
		env := map[string]string{refappFaultEnv: tc.fault, stallCaptureEnv: tc.knob}
		if got := stallCaptureEnabled(func(k string) string { return env[k] }); got != tc.want {
			t.Errorf("fault=%q knob=%q: %v, want %v", tc.fault, tc.knob, got, tc.want)
		}
	}
}

// refappFaultEnv is the refapp module's debugvars.FaultEnv, which this
// package cannot import: the gate and the fault must read the same name.
func TestRefappFaultEnvMatchesDebugvars(t *testing.T) {
	src, err := os.ReadFile(filepath.Join("refapp", "internal", "debugvars", "faults.go"))
	if err != nil {
		t.Fatal(err)
	}
	if want := `const FaultEnv = "` + refappFaultEnv + `"`; !strings.Contains(string(src), want) {
		t.Errorf("debugvars/faults.go lacks %s", want)
	}
}

// offerOnce is the property tier's at-most-once incident filter. A send the
// busy loop could not take (the channel holds a synchronous dossier's
// incident) must leave the counter unmarked, so the next tally tick offers
// it again (CodeRabbit on probatorium#412, runner.go:1045); a send that
// happened marks it, and it is never offered twice.
func TestOfferOnceRetriesADroppedIncident(t *testing.T) {
	ch := make(chan Incident, 1)
	ch <- Incident{PredicateID: "I-WS-STALL"} // the loop is busy with a dossier
	alerted := map[string]bool{}
	inc := Incident{PredicateID: "I-WS-ACCEPTED", Message: "server accepted bad WebSocket frame"}
	if offerOnce(alerted, ch, inc) {
		t.Fatal("offered into a full channel: reported sent")
	}
	<-ch // the loop finished its dossier
	if !offerOnce(alerted, ch, inc) {
		t.Fatal("the counter's first offer was dropped by a busy loop, and the next tick's offer was refused: its incident is lost for the rest of the run")
	}
	if got := <-ch; got.PredicateID != "I-WS-ACCEPTED" {
		t.Fatalf("sent %q", got.PredicateID)
	}
	if offerOnce(alerted, ch, inc) || len(ch) != 0 {
		t.Fatal("a counter that already produced its incident was offered again")
	}
}

// The in-stall dossier budget counts dossiers TAKEN, not attempts: an
// attempt a busy loop dropped gives its slot (and its cooldown) back. The
// bound holds across stall episodes, and concurrent triggers of one burst
// take one dossier.
func TestStallDossierHookBudgetCountsTakenDossiers(t *testing.T) {
	clock := time.Date(2026, 9, 27, 4, 0, 0, 0, time.UTC)
	prev := stallHookNow
	stallHookNow = func() time.Time { return clock }
	t.Cleanup(func() { stallHookNow = prev })
	next := func() { clock = clock.Add(stallDossierCooldown + time.Second) } // a later stall episode

	ch := make(chan Incident, 1)
	hook := newStallDossierHook(ch, func() int { return 4242 }, 1)
	ch <- Incident{PredicateID: "I-WS-HANDSHAKE"} // busy
	hook("ws")                                    // dropped
	<-ch
	hook("ws") // same instant: the dropped attempt gave its cooldown back too
	select {
	case inc := <-ch:
		if inc.PredicateID != "I-WS-STALL" || !inc.RecordOnly || !inc.SkipCore || inc.RefappPID != 4242 {
			t.Fatalf("stall dossier incident %+v", inc)
		}
	default:
		t.Fatal("a dropped attempt spent the kind's only dossier: the next stall took none")
	}
	next()
	hook("ws")
	if len(ch) != 0 {
		t.Fatal("the bound of 1 ws dossier was exceeded")
	}
	hook("h2c") // the other kind has its own budget and cooldown
	if inc := <-ch; inc.PredicateID != "I-H2C-STALL" {
		t.Fatalf("h2c stall dossier %+v", inc)
	}

	// Episodes: never more than perKind dossiers in all.
	big := make(chan Incident, 64)
	hook = newStallDossierHook(big, func() int { return 1 }, 4)
	for range 8 {
		next()
		hook("ws")
	}
	if n := len(big); n != 4 {
		t.Fatalf("8 stall episodes took %d ws dossiers, want exactly 4", n)
	}

	// Bursts of concurrent triggers, released together: one dossier each.
	// The cooldown window is claimed by compare-and-swap; a check-then-store
	// lets two triggers of one burst both through.
	for b := range 200 {
		burst := make(chan Incident, 64)
		hook := newStallDossierHook(burst, func() int { return 1 }, 4)
		next()
		start := make(chan struct{})
		var wg sync.WaitGroup
		for range 64 {
			wg.Add(1)
			go func() { defer wg.Done(); <-start; hook("ws") }()
		}
		close(start)
		wg.Wait()
		if n := len(burst); n != 1 {
			t.Fatalf("burst %d: 64 concurrent triggers of one stall took %d ws dossiers, want 1", b, n)
		}
	}
}

// The property tier arms Tier 1's in-stall trigger only in a stall-capture
// run: in a routine run the hook is nil, so no walker read carries a stall
// timer and no in-stall dossier can reach the serial incident loop.
func TestWalkerStallHookIsNilOutsideAStallCaptureRun(t *testing.T) {
	ch := make(chan Incident, 1)
	pid := func() int { return 7 }
	routine := map[string]string{}
	if h := walkerStallHook(func(k string) string { return routine[k] }, ch, pid); h != nil {
		t.Fatal("routine run: the in-stall hook is armed")
	}
	for name, env := range map[string]map[string]string{
		"fault run": {refappFaultEnv: "/ws:8s@30s"},
		"knob":      {stallCaptureEnv: "1"},
	} {
		h := walkerStallHook(func(k string) string { return env[k] }, ch, pid)
		if h == nil {
			t.Fatalf("%s: the in-stall hook is not armed", name)
		}
		h("ws")
		if inc := <-ch; inc.PredicateID != "I-WS-STALL" || inc.RefappPID != 7 {
			t.Fatalf("%s: hook sent %+v", name, inc)
		}
	}
}
