package validation

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/goceleris/probatorium/report"
)

// The refapp's CPU placement in the cell document (schema 5.23,
// probatorium#481).
//
// Until now it existed only in refapp_sched.txt, which a cell writes only
// when an arm or the stall capture is on, so the routine race run that raised
// the question (39560945814) could not say what the refapp ran under. This is
// the cheapest correct place: the validator reads the refapp's own /proc at
// ready, once, off the property loop's path. It touches nothing in the refapp
// build and starts no listener.
//
// What /proc cannot give is GOMAXPROCS. It is recorded when the refapp's side
// listener answers /debug/sched, which only a stall-capture run starts; in
// every other run it is 0 with a note saying so. Recording it always would
// need the refapp to publish it (validation/refapp/internal/debugvars), which
// is a change to the refapp build and is left undone on purpose.

const gomaxprocsNoSideListener = "not recorded: GOMAXPROCS is in no /proc file, and the refapp's side listener (/debug/sched) is started only in a stall-capture run"

// collectRefappSched reads the refapp's CPU view. getenv decides whether the
// arm / stall-capture readback file is also written (as before); the record
// is returned in every run. The result is never nil for a refapp that
// became ready: when nothing could be read, Unavailable says why.
func collectRefappSched(ctx context.Context, getenv func(string) string, driverMode, procRoot string, pid int, outDir, debugAddr string) *report.RefappSched {
	rec := &report.RefappSched{At: time.Now().UTC().Format(time.RFC3339Nano)}
	switch {
	case driverMode == "ssh":
		rec.Unavailable = "ssh driver: the refapp runs on another host and this host's /proc is not its /proc"
	case pid <= 0:
		rec.Unavailable = "no refapp pid"
	default:
		ts, err := readThreads(procRoot, pid)
		if err != nil {
			rec.Unavailable = fmt.Sprintf("unreadable: %v", err)
			break
		}
		rec.Threads = len(ts)
		rec.LeaderCPUsAllowedList = "absent"
		rec.ThreadMasks = map[string]int{}
		for _, t := range ts {
			rec.ThreadMasks[t.cpus]++
			if t.tid == pid {
				rec.LeaderCPUsAllowedList = t.cpus
			}
		}
	}
	rec.GOMAXPROCSNote = gomaxprocsNoSideListener
	switch {
	case driverMode == "ssh":
		// The local /proc and the local readback say nothing about a refapp
		// that runs on another host; a record that is Unavailable must not
		// also carry a GOMAXPROCS read from them.
		rec.GOMAXPROCSNote = "not recorded: ssh driver, the refapp runs on another host"
	case pid > 0 && armsActive(getenv):
		// The arm / stall-capture readback, as before; its /debug/sched
		// answer is where GOMAXPROCS comes from.
		text, err := writeRefappReadbackText(ctx, outDir, getenv, procRoot, pid, debugAddr)
		if err != nil {
			fmt.Fprintf(os.Stderr, "validation: refapp readback: %v\n", err)
			rec.GOMAXPROCSNote = fmt.Sprintf("not recorded: the readback could not be written: %v", err)
		} else if g := gomaxprocsFromSchedText(text); g > 0 {
			rec.GOMAXPROCS, rec.GOMAXPROCSNote = g, ""
		} else {
			rec.GOMAXPROCSNote = "not recorded: the refapp's side listener (/debug/sched) did not answer"
		}
	}
	return rec
}

// gomaxprocsFromSchedText is the GOMAXPROCS in the REFAPP_SCHED line of a
// refapp_sched.txt body, 0 when absent or unreadable.
func gomaxprocsFromSchedText(text string) int {
	for _, line := range strings.Split(text, "\n") {
		body, ok := strings.CutPrefix(line, "REFAPP_SCHED ")
		if !ok {
			continue
		}
		var doc struct {
			GOMAXPROCS int `json:"gomaxprocs"`
		}
		if json.Unmarshal([]byte(body), &doc) != nil {
			return 0
		}
		return max(doc.GOMAXPROCS, 0)
	}
	return 0
}

// cloneRefappSched copies a record so a cell document does not alias the
// tally snapshot's map.
func cloneRefappSched(r *report.RefappSched) *report.RefappSched {
	if r == nil {
		return nil
	}
	c := *r
	if r.ThreadMasks != nil {
		c.ThreadMasks = make(map[string]int, len(r.ThreadMasks))
		for k, v := range r.ThreadMasks {
			c.ThreadMasks[k] = v
		}
	}
	return &c
}
