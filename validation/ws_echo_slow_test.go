package validation

import (
	"context"
	"fmt"
	"math/rand/v2"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// The race tier judges liveness and records latency (probatorium#481,
// celeris#871). A ws_echo timeout whose two-instant capture says SLOW_CLASS is
// booked as ws_echo_timeout_slow, a non-failing finding, when the run is the
// race tier (wsEchoDiag.SlowTolerance = N > 0). A WEDGE_CLASS or
// INDETERMINATE timeout, a SLOW_CLASS one in any other tier, the (N+1)th SLOW
// one in a cell, and a second SLOW one with no clean fire since the first all
// stay ws_echo_timeout, which fails the gate exactly as before.

// slowSS is a fake ss whose server socket's write counter keeps moving: every
// pair of instants sees the application write, so the verdict is SLOW_CLASS.
func slowSS() func(context.Context, int, int) (string, error) {
	ss, _ := fakeSS(func(call int) fakeSrv {
		q := flatSrv
		q.Acked += int64(call) * 100000
		return q
	})
	return ss
}

// lateEcho is a server whose third echo comes after the fire's hold: the
// fire times out while the server is only late.
func lateEcho(t *testing.T) *fakeWSServer {
	t.Helper()
	return echoServer(t, func(n int, _ bool, _ byte, p []byte) ([]byte, bool) {
		if n == 3 {
			time.Sleep(550 * time.Millisecond)
		}
		return p, false
	})
}

// plainEcho echoes everything at once: a clean fire.
func plainEcho(t *testing.T) *fakeWSServer {
	t.Helper()
	return echoServer(t, func(_ int, _ bool, _ byte, p []byte) ([]byte, bool) { return p, false })
}

func fireOn(srv *fakeWSServer, tally *wsEchoTally) {
	fireWSLargeEcho(context.Background(), srv.HostPort(), "/ws", rand.New(rand.NewPCG(1, 2)), tally)
}

func slowRig(t *testing.T, tolerance int) (*wsEchoTally, *wsEchoDiag, *diagRecorder) {
	t.Helper()
	shortEchoFire(t, 400*time.Millisecond)
	shortDiag(t, 600*time.Millisecond, 100*time.Millisecond)
	tally := &wsEchoTally{}
	rec := &diagRecorder{}
	d := &wsEchoDiag{SS: slowSS(), SlowTolerance: tolerance}
	d.Deliver = rec.deliver(tally)
	tally.diag.Store(d)
	return tally, d, rec
}

func secondLine(text string) string {
	lines := strings.SplitN(text, "\n", 3)
	if len(lines) < 2 {
		return ""
	}
	return lines[1]
}

// Race tier, SLOW_CLASS: not a timeout, a recorded finding. The data path of
// PR #525 is intact: the file's first line is still the verdict line, and the
// disposition is the second.
func TestSlowTimeoutIsRecordedNotFailedInTheRaceTier(t *testing.T) {
	tally, _, rec := slowRig(t, 3)
	fireOn(lateEcho(t), tally)
	s := tally.snapshot()

	if len(rec.reps) != 1 || rec.reps[0].Verdict.Class != verdictSlow {
		t.Fatalf("reports=%d, want one SLOW_CLASS: %+v", len(rec.reps), rec.reps)
	}
	if s.Timeout != 0 {
		t.Errorf("ws_echo_timeout=%d, want 0: a SLOW_CLASS timeout in the race tier must not fail the gate", s.Timeout)
	}
	if s.TimeoutSlow != 1 {
		t.Errorf("ws_echo_timeout_slow=%d, want 1", s.TimeoutSlow)
	}
	rep := rec.reps[0]
	if !rep.Tolerated {
		t.Errorf("report not marked tolerated:\n%s", rep.Text)
	}
	first, _, _ := strings.Cut(rep.Text, "\n")
	if !strings.HasPrefix(first, "WS_ECHO_VERDICT=SLOW_CLASS ") {
		t.Errorf("first line %q: the PR #525 data path (WS_ECHO_VERDICT first) is broken", first)
	}
	if got := secondLine(rep.Text); !strings.HasPrefix(got, "WS_ECHO_DISPOSITION=TOLERATED_SLOW ") ||
		!strings.Contains(got, "ws_echo_timeout_slow=1") || !strings.Contains(got, "n=3") {
		t.Errorf("second line %q, want WS_ECHO_DISPOSITION=TOLERATED_SLOW ... ws_echo_timeout_slow=1 ... n=3", got)
	}
	if rec.timeoutAtDeliver[0] != 0 {
		t.Errorf("a timeout was counted (%d) before the report was delivered", rec.timeoutAtDeliver[0])
	}
}

// Any other tier (no tolerance): the same SLOW_CLASS timeout is the failure it
// always was.
func TestSlowTimeoutStillFailsOutsideTheRaceTier(t *testing.T) {
	tally, _, rec := slowRig(t, 0)
	fireOn(lateEcho(t), tally)
	s := tally.snapshot()
	if s.Timeout != 1 || s.TimeoutSlow != 0 {
		t.Fatalf("timeout=%d timeout_slow=%d, want 1/0: nightly, soak and the rest keep failing on a ws_echo timeout", s.Timeout, s.TimeoutSlow)
	}
	if len(rec.reps) != 1 || rec.reps[0].Tolerated {
		t.Fatalf("reports %+v, want one, not tolerated", rec.reps)
	}
	if got := secondLine(rec.reps[0].Text); !strings.HasPrefix(got, "WS_ECHO_DISPOSITION=FAIL ") || !strings.Contains(got, "reason=tolerance_off") {
		t.Errorf("second line %q, want WS_ECHO_DISPOSITION=FAIL reason=tolerance_off", got)
	}
}

// WEDGE_CLASS and INDETERMINATE stay failures in the race tier.
func TestWedgeAndIndeterminateStillFailInTheRaceTier(t *testing.T) {
	flat, _ := flatSS()
	cases := map[string]struct {
		ss        func(context.Context, int, int) (string, error)
		want, why string
	}{
		"wedge": {flat, verdictWedge, "reason=verdict_wedge"},
		"indeterminate": {func(context.Context, int, int) (string, error) { return "", fmt.Errorf("exec: ss: not found") },
			verdictIndeterminate, "reason=verdict_indeterminate"},
	}
	for name, c := range cases {
		t.Run(name, func(t *testing.T) {
			tally, d, rec := slowRig(t, 3)
			d.SS = c.ss
			fireOn(echoServer(t, swallowAfterTwo()), tally)
			s := tally.snapshot()
			if len(rec.reps) != 1 || rec.reps[0].Verdict.Class != c.want {
				t.Fatalf("reports %+v, want one %s", rec.reps, c.want)
			}
			if s.Timeout != 1 || s.TimeoutSlow != 0 {
				t.Errorf("timeout=%d timeout_slow=%d, want 1/0: %s must fail the gate in the race tier", s.Timeout, s.TimeoutSlow, c.want)
			}
			if rec.reps[0].Tolerated {
				t.Errorf("a %s report was marked tolerated", c.want)
			}
			if got := secondLine(rec.reps[0].Text); !strings.HasPrefix(got, "WS_ECHO_DISPOSITION=FAIL ") || !strings.Contains(got, c.why) {
				t.Errorf("disposition %q, want FAIL %s", got, c.why)
			}
		})
	}
}

// No capture at all (the walker has no diagnosis installed) is INDETERMINATE
// by construction: the timeout is counted as ever.
func TestNoCaptureTimeoutStillFailsEvenWithTolerance(t *testing.T) {
	shortEchoFire(t, 400*time.Millisecond)
	tally := &wsEchoTally{}
	fireOn(echoServer(t, swallowAfterTwo()), tally)
	if s := tally.snapshot(); s.Timeout != 1 || s.TimeoutSlow != 0 {
		t.Fatalf("timeout=%d timeout_slow=%d, want 1/0", s.Timeout, s.TimeoutSlow)
	}
}

// The guard, count: N = 3 SLOW timeouts in a cell are tolerated, the 4th
// fails. A clean fire runs between every pair so the other rule stays out of
// it. The 4th is DIAGNOSED (the capture budget is N+1), so the report says
// why it failed.
func TestSlowGuardFailsTheFourthSlowTimeoutInACell(t *testing.T) {
	tally, _, rec := slowRig(t, 3)
	late, plain := lateEcho(t), plainEcho(t)
	for i := 1; i <= 4; i++ {
		fireOn(late, tally)
		fireOn(plain, tally)
		s := tally.snapshot()
		wantSlow, wantFail := int64(min(i, 3)), int64(max(i-3, 0))
		if s.TimeoutSlow != wantSlow || s.Timeout != wantFail {
			t.Fatalf("after %d slow timeouts: timeout_slow=%d timeout=%d, want %d/%d", i, s.TimeoutSlow, s.Timeout, wantSlow, wantFail)
		}
	}
	if len(rec.reps) != 4 {
		t.Fatalf("%d reports, want 4: the 4th must be diagnosed so its dossier says why it failed", len(rec.reps))
	}
	if got := secondLine(rec.reps[3].Text); !strings.HasPrefix(got, "WS_ECHO_DISPOSITION=FAIL ") || !strings.Contains(got, "reason=over_n") ||
		!strings.Contains(got, "slow_count=4") || !strings.Contains(got, "n=3") {
		t.Errorf("4th disposition %q, want FAIL reason=over_n slow_count=4 n=3", got)
	}
	if rec.reps[3].Tolerated {
		t.Error("4th report marked tolerated")
	}
}

// The guard, persistence: a second SLOW timeout with no clean fire since the
// first means every fire after the first timed out slow. That is a server
// that delivers nothing, not one that is slow now and then.
func TestSlowGuardFailsWhenNoFireSucceedsAfterTheFirstSlowOne(t *testing.T) {
	tally, _, rec := slowRig(t, 3)
	late := lateEcho(t)
	fireOn(late, tally)
	fireOn(late, tally)
	s := tally.snapshot()
	if s.TimeoutSlow != 1 || s.Timeout != 1 {
		t.Fatalf("timeout_slow=%d timeout=%d, want 1/1: the second slow timeout with no clean fire between must fail", s.TimeoutSlow, s.Timeout)
	}
	if len(rec.reps) != 2 {
		t.Fatalf("%d reports, want 2", len(rec.reps))
	}
	if got := secondLine(rec.reps[1].Text); !strings.Contains(got, "reason=no_clean_fire_since_first_slow") {
		t.Errorf("2nd disposition %q, want reason=no_clean_fire_since_first_slow", got)
	}
}

// With one clean fire in between, two SLOW timeouts are tolerated.
func TestSlowGuardAllowsSlowTimeoutsInterleavedWithCleanFires(t *testing.T) {
	tally, _, _ := slowRig(t, 3)
	late, plain := lateEcho(t), plainEcho(t)
	fireOn(late, tally)
	fireOn(plain, tally)
	fireOn(late, tally)
	s := tally.snapshot()
	if s.TimeoutSlow != 2 || s.Timeout != 0 {
		t.Fatalf("timeout_slow=%d timeout=%d, want 2/0", s.TimeoutSlow, s.Timeout)
	}
	if got := tally.cleanFires.Load(); got != 1 {
		t.Errorf("clean fires = %d, want 1", got)
	}
}

// The guard is the tolerance, not the capture budget: with N = 3 the budget
// is N+1 captures. (Mutant: a budget of 3 leaves the 4th timeout undiagnosed,
// which still fails but loses the reason; TestSlowGuardFails... above reads
// the reason.)
func TestSlowToleranceRaisesTheCaptureBudgetToNPlusOne(t *testing.T) {
	d := &wsEchoDiag{SlowTolerance: 3}
	for i := 0; i < 4; i++ {
		if !d.claim() {
			t.Fatalf("claim %d refused with tolerance 3", i+1)
		}
	}
	if d.claim() {
		t.Error("claim 5 granted with tolerance 3: the budget is N+1")
	}
	plain := &wsEchoDiag{}
	for i := 0; i < wsEchoDiagPerCell; i++ {
		plain.claim()
	}
	if plain.claim() {
		t.Errorf("without tolerance the budget stays %d", wsEchoDiagPerCell)
	}
}

// Counted and exported: the summary helper carries the new counter, and the
// results export carries it as a key.
func TestSlowTimeoutIsSummarisedAndExported(t *testing.T) {
	line := summariseWSEcho(wsEchoSnapshot{Fires: 9, Timeout: 0, TimeoutSlow: 2})
	if !strings.Contains(line, "ws_echo_timeout=0") || !strings.Contains(line, "ws_echo_timeout_slow=2") {
		t.Errorf("summary line %q lacks ws_echo_timeout_slow=2", line)
	}
	var snap tier1TallySnapshot
	snap.WSEcho.TimeoutSlow = 2
	if got, ok := snap.Tier1Summary().WSEcho["ws_echo_timeout_slow"]; !ok || got != 2 {
		t.Errorf("exported ws_echo_timeout_slow = %d, present=%v, want 2", got, ok)
	}
}

// The properties registry knows the new finding, and it is never a violation.
func TestIWSEchoSlowIsARecordOnlyNote(t *testing.T) {
	if properties.IWSEchoSlow.ID != "I-WS-ECHO-SLOW" {
		t.Fatalf("id %q", properties.IWSEchoSlow.ID)
	}
	if !strings.Contains(properties.IWSEchoSlow.Description, "SLOW_CLASS") || !strings.Contains(properties.IWSEchoSlow.Description, "not a violation") {
		t.Errorf("description %q must say it is a SLOW_CLASS note and not a violation", properties.IWSEchoSlow.Description)
	}
}

// The incident: tolerated => record-only I-WS-ECHO-SLOW; anything else keeps
// I-WS-ECHO-STALL. The dossier carries the whole record either way.
func TestDeliverNamesTheDossierByDisposition(t *testing.T) {
	for _, c := range []struct {
		tolerated bool
		want      string
	}{{true, "I-WS-ECHO-SLOW"}, {false, "I-WS-ECHO-STALL"}} {
		ch := make(chan Incident, 1)
		h := wsEchoDiagHook(func(k string) string { return map[string]string{stallCaptureEnv: "1"}[k] }, 3, ch, func() int { return 42 }, nil, "/proc")
		text := "WS_ECHO_VERDICT=SLOW_CLASS reason=x\nWS_ECHO_DISPOSITION=y\n### t0\n"
		h.Deliver(context.Background(), wsEchoDiagReport{Verdict: wsEchoVerdict{Class: verdictSlow, Line: "WS_ECHO_VERDICT=SLOW_CLASS reason=x"}, Text: text, Tolerated: c.tolerated})
		inc := <-ch
		if inc.PredicateID != c.want || !inc.RecordOnly || !inc.SkipCore {
			t.Errorf("tolerated=%v: incident %+v, want a record-only no-core %s", c.tolerated, inc, c.want)
		}
		if inc.Attachments["ws-echo-diag.txt"] != text {
			t.Errorf("tolerated=%v: attachment lost the record", c.tolerated)
		}
	}
}

// The capture is armed by the tolerance too: the race tier is not dispatched
// with stall_capture by default, and without a capture every timeout would be
// INDETERMINATE and stay red. Off with neither.
func TestSlowToleranceArmsTheWSEchoCaptureWithoutStallCapture(t *testing.T) {
	ch := make(chan Incident, 1)
	none := func(string) string { return "" }
	if h := wsEchoDiagHook(none, 0, ch, func() int { return 1 }, nil, "/proc"); h != nil {
		t.Fatal("no stall capture and no tolerance: the diagnosis is armed")
	}
	h := wsEchoDiagHook(none, 3, ch, func() int { return 1 }, nil, "/proc")
	if h == nil {
		t.Fatal("tolerance 3 without stall capture: the diagnosis is not armed, every race timeout would be INDETERMINATE")
	}
	if h.SlowTolerance != 3 {
		t.Errorf("SlowTolerance=%d, want 3", h.SlowTolerance)
	}
	if h2 := wsEchoDiagHook(func(k string) string { return map[string]string{stallCaptureEnv: "1"}[k] }, 0, ch, func() int { return 1 }, nil, "/proc"); h2 == nil || h2.SlowTolerance != 0 {
		t.Errorf("stall capture alone must arm the diagnosis with no tolerance, got %+v", h2)
	}
}

// In-run: the tally tick's hard I-WS-ECHO fires on the failing counters only.
// A cell whose only ws_echo finding is a tolerated SLOW timeout keeps running.
func TestTickDoesNotAbortOnATolerantSlowTimeout(t *testing.T) {
	t.Setenv(refappFaultEnv, "")
	o := &Orchestrator{}
	violations := make(chan Incident, 4)
	cb := o.tallyIncidentCallback(context.Background(), violations, func() int { return 4242 })

	var snap tier1TallySnapshot
	snap.WSEcho.TimeoutSlow = 2
	cb(snap)
	if ids := tallyIDs(violations); len(ids) != 0 {
		t.Fatalf("a tolerated SLOW timeout produced incidents %v, want none", ids)
	}
	snap.WSEcho.Timeout = 1
	cb(snap)
	if ids := tallyIDs(violations); len(ids) != 1 || ids[0] != properties.IWSEcho.ID {
		t.Fatalf("a failing timeout beside slow ones produced %v, want [%s]", ids, properties.IWSEcho.ID)
	}
}
