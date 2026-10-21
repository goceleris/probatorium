package report

import (
	"fmt"
	"sort"
	"strings"
)

// WSEchoSlowLines renders, for ValidateGate's output, every cell that booked a
// SLOW_CLASS ws_echo fire: the ones the race tier tolerates
// (ws_echo_timeout_slow) and the ones its guard refused (ws_echo_slow_over_n,
// ws_echo_slow_no_clean_fire; the latter fail as ws_echo_timeout). It records
// the latency the race tier does not fail on (probatorium#481, celeris#871).
// It reads cells and writes nothing; [Gate] judges the counters on its own.
//
// tolerated is whether this gate run is the race tier. Then it always returns
// a header, so a run that recorded none says so. Otherwise it returns nothing
// unless a cell carries ws_echo_timeout_slow, and says that the gate fails on
// it.
//
// A cell's row counts the SLOW fires that were diagnosed (the capture budget is
// N+1 per cell; a later timeout is counted in ws_echo_timeout alone and is not a
// SLOW fire here). A cell with no refused fire
// prints exactly the row it always did; a cell with one says how many were
// tolerated and how many were refused, and is repeated in a second block of
// its own for the refused ones, which fail the gate.
func WSEchoSlowLines(cells []ValidationCellResult, tolerated bool) []string {
	var rows, refusedRows []string
	var anyOver, anyNoClean bool
	for _, c := range cells {
		if c.Tier1 == nil {
			continue
		}
		slow := c.Tier1.WSEcho[WSEchoSlowKey]
		var over, noClean int64
		if tolerated {
			over, noClean = c.Tier1.WSEcho[WSEchoSlowOverNKey], c.Tier1.WSEcho[WSEchoSlowNoCleanKey]
		}
		if slow+over+noClean == 0 {
			continue
		}
		who := fmt.Sprintf("  %-24s %-8s %-12s", c.Refapp, c.Engine, c.Arch)
		fires, ok := c.Tier1.WSEcho["ws_echo_fires"], c.Tier1.WSEcho["ws_echo_ok"]
		if over+noClean == 0 {
			rows = append(rows, fmt.Sprintf("%s %d SLOW_CLASS ws_echo timeout(s) in %d fire(s) (%d echoes ok)", who, slow, fires, ok))
			continue
		}
		anyOver, anyNoClean = anyOver || over > 0, anyNoClean || noClean > 0
		var failing []string
		if over > 0 {
			failing = append(failing, fmt.Sprintf("%d over N=%d (%s)", over, WSEchoSlowTolerance, WSEchoSlowOverNKey))
		}
		if noClean > 0 {
			failing = append(failing, fmt.Sprintf("%d with no clean fire since the first (%s)", noClean, WSEchoSlowNoCleanKey))
		}
		detail := strings.Join(failing, ", ")
		rows = append(rows, fmt.Sprintf("%s %d diagnosed SLOW_CLASS ws_echo fire(s) in %d fire(s) (%d echoes ok): %d tolerated (%s), %d FAILING: %s",
			who, slow+over+noClean, fires, ok, slow, WSEchoSlowKey, over+noClean, detail))
		refusedRows = append(refusedRows, fmt.Sprintf("%s %s; counted in ws_echo_timeout=%d",
			who, detail, c.Tier1.WSEcho["ws_echo_timeout"]))
	}
	sort.Strings(rows)
	sort.Strings(refusedRows)
	if len(rows) == 0 {
		if !tolerated {
			return nil
		}
		return []string{"ws_echo SLOW_CLASS timeouts (ws_echo_timeout_slow, race tier): none recorded in any cell"}
	}
	var head string
	if tolerated && len(refusedRows) > 0 {
		head = fmt.Sprintf("ws_echo SLOW_CLASS fires (race tier): %d cell(s) recorded latency. Up to N=%d per cell are tolerated (ws_echo_timeout_slow, NOT failing: the server read and wrote in the window); a cell that went past that or lost the persistence rule is FAILING and is listed again below. Only diagnosed fires are counted here (the capture budget is N+1 per cell); a later timeout is counted in ws_echo_timeout alone:", len(rows), WSEchoSlowTolerance)
	} else if tolerated {
		head = fmt.Sprintf("ws_echo SLOW_CLASS timeouts (ws_echo_timeout_slow, race tier): %d cell(s) recorded latency, NOT failing up to N=%d per cell (the server read and wrote in the window; a WEDGE_CLASS or INDETERMINATE timeout is ws_echo_timeout and fails). A rise in these counts is the signal to watch:", len(rows), WSEchoSlowTolerance)
	} else {
		head = fmt.Sprintf("ws_echo_timeout_slow in %d cell(s) of a tier that does not tolerate it (the gate fails on these):", len(rows))
	}
	out := append([]string{head}, rows...)
	if len(refusedRows) > 0 {
		var reasons []string
		if anyOver {
			reasons = append(reasons, fmt.Sprintf("%s: slow-class latency over the tolerance of N=%d per cell, not a wedge", WSEchoSlowOverNKey, WSEchoSlowTolerance))
		}
		if anyNoClean {
			reasons = append(reasons, fmt.Sprintf("%s: every fire after the first SLOW one timed out, none drained cleanly", WSEchoSlowNoCleanKey))
		}
		out = append(out, fmt.Sprintf("ws_echo SLOW_CLASS fires the race tier's guard refused (%s) -- %d cell(s), FAILING as ws_echo_timeout; the server read and wrote in the window:",
			strings.Join(reasons, "; "), len(refusedRows)))
		out = append(out, refusedRows...)
	}
	return out
}
