package report

import (
	"fmt"
	"sort"
)

// WSEchoSlowLines renders, for ValidateGate's output, every cell that booked a
// SLOW_CLASS ws_echo timeout (ws_echo_timeout_slow): the latency the race tier
// records instead of failing on (probatorium#481, celeris#871). It reads cells
// and writes nothing; [Gate] judges the same counter on its own.
//
// tolerated is whether this gate run is the race tier. Then it always returns
// a header, so a run that recorded none says so. Otherwise it returns nothing
// unless a cell carries the counter, and says that the gate fails on it.
func WSEchoSlowLines(cells []ValidationCellResult, tolerated bool) []string {
	var rows []string
	for _, c := range cells {
		if c.Tier1 == nil || c.Tier1.WSEcho[WSEchoSlowKey] == 0 {
			continue
		}
		rows = append(rows, fmt.Sprintf("  %-24s %-8s %-12s %d SLOW_CLASS ws_echo timeout(s) in %d fire(s) (%d echoes ok)",
			c.Refapp, c.Engine, c.Arch, c.Tier1.WSEcho[WSEchoSlowKey], c.Tier1.WSEcho["ws_echo_fires"], c.Tier1.WSEcho["ws_echo_ok"]))
	}
	sort.Strings(rows)
	if len(rows) == 0 {
		if !tolerated {
			return nil
		}
		return []string{"ws_echo SLOW_CLASS timeouts (ws_echo_timeout_slow, race tier): none recorded in any cell"}
	}
	var head string
	if tolerated {
		head = fmt.Sprintf("ws_echo SLOW_CLASS timeouts (ws_echo_timeout_slow, race tier): %d cell(s) recorded latency, NOT failing up to N=%d per cell (the server read and wrote in the window; a WEDGE_CLASS or INDETERMINATE timeout is ws_echo_timeout and fails). A rise in these counts is the signal to watch:", len(rows), WSEchoSlowTolerance)
	} else {
		head = fmt.Sprintf("ws_echo_timeout_slow in %d cell(s) of a tier that does not tolerate it (the gate fails on these):", len(rows))
	}
	return append([]string{head}, rows...)
}
