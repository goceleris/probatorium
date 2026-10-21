package validation

import (
	"fmt"
	"strings"
	"sync"
)

// The race tier judges liveness and records latency (probatorium#481,
// celeris#871).
//
// The adaptive cell of the race tier is deliberately small (2 workers, a
// floor of 110 walkers, cmd/validator/matrix.go) and runs under -race
// instrumentation. On amd64 its 64 KiB ws_echo fire timed out in every run,
// and every timeout the two-instant capture classified was SLOW_CLASS: the
// server application read and wrote in the window, the echoes were only later
// than the fire's deadline. That is a capacity and latency effect, not a
// liveness failure, and a gate that fails on it fails every run for a reason
// that says nothing about a defect.
//
// So, in the race tier only (wsEchoDiag.SlowTolerance = N > 0, set by the
// validator's -race-tier), a timed-out fire whose capture says SLOW_CLASS is
// booked in ws_echo_timeout_slow, a counter the gate does not fail on, and its
// record-only dossier is I-WS-ECHO-SLOW. Everything else keeps failing exactly
// as before, in the race tier too:
//
//   - WEDGE_CLASS and INDETERMINATE (including a fire with no capture at all)
//     are ws_echo_timeout;
//   - the (N+1)th SLOW_CLASS timeout in one cell is ws_echo_timeout (guard
//     "over_n");
//   - a SLOW_CLASS timeout after the first, with no fire in the cell having
//     drained cleanly since the first one, is ws_echo_timeout (guard
//     "no_clean_fire_since_first_slow"): every fire after the first timed out,
//     which is a server delivering nothing, not one that is late now and then;
//   - every other ws_echo class, and every other violation.
//
// The tolerance is explicit: with SlowTolerance 0 (nightly, soak, weekend,
// checkptr, PR tier) a SLOW_CLASS timeout is ws_echo_timeout.

// Dispositions, the machine-readable values of WS_ECHO_DISPOSITION (the
// second line of ws-echo-diag.txt; the first stays WS_ECHO_VERDICT).
const (
	dispositionTolerated = "TOLERATED_SLOW"
	dispositionFail      = "FAIL"
)

// wsEchoSlowGuard is the per-cell state of the two guard rules.
type wsEchoSlowGuard struct {
	mu   sync.Mutex
	slow int // SLOW_CLASS timeouts admitted so far, plus the one being decided
	// cleanAtFirst is tally.cleanFires when the first SLOW_CLASS timeout was
	// admitted.
	cleanAtFirst int64
}

// admit decides one SLOW_CLASS timeout: n is the tolerance, cleanNow the
// number of fires that drained cleanly so far in the cell. It returns whether
// the timeout is tolerated, the reason when it is not, and the cell's SLOW
// count including this timeout.
func (g *wsEchoSlowGuard) admit(n int, cleanNow int64) (ok bool, reason string, count int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	count = g.slow + 1
	switch {
	case count > n:
		return false, "over_n", count
	case count == 1:
		g.cleanAtFirst = cleanNow
	case cleanNow == g.cleanAtFirst:
		return false, "no_clean_fire_since_first_slow", count
	}
	g.slow = count
	return true, "", count
}

// book decides what a diagnosed timeout is, writes the decision into the
// record (a second line, after the unchanged WS_ECHO_VERDICT line) and returns
// whether it is a tolerated SLOW_CLASS finding. cleanNow is the cell's count of
// cleanly drained fires.
func (d *wsEchoDiag) book(rep *wsEchoDiagReport, cleanNow int64) bool {
	var line string
	switch {
	case d.SlowTolerance <= 0:
		line = fmt.Sprintf("WS_ECHO_DISPOSITION=%s reason=tolerance_off n=0 tier=not_race", dispositionFail)
	case rep.Verdict.Class == verdictWedge:
		line = fmt.Sprintf("WS_ECHO_DISPOSITION=%s reason=verdict_wedge n=%d tier=race", dispositionFail, d.SlowTolerance)
	case rep.Verdict.Class != verdictSlow:
		line = fmt.Sprintf("WS_ECHO_DISPOSITION=%s reason=verdict_indeterminate n=%d tier=race", dispositionFail, d.SlowTolerance)
	default:
		ok, reason, count := d.guard.admit(d.SlowTolerance, cleanNow)
		if ok {
			rep.Tolerated = true
			line = fmt.Sprintf("WS_ECHO_DISPOSITION=%s ws_echo_timeout_slow=%d n=%d finding=I-WS-ECHO-SLOW tier=race", dispositionTolerated, count, d.SlowTolerance)
		} else {
			line = fmt.Sprintf("WS_ECHO_DISPOSITION=%s reason=%s slow_count=%d n=%d tier=race", dispositionFail, reason, count, d.SlowTolerance)
		}
	}
	rep.Disposition = line
	if i := strings.IndexByte(rep.Text, '\n'); i >= 0 {
		rep.Text = rep.Text[:i+1] + line + "\n" + rep.Text[i+1:]
	} else {
		rep.Text += "\n" + line + "\n"
	}
	return rep.Tolerated
}
