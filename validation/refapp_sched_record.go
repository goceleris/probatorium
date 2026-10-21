package validation

import (
	"context"

	"github.com/goceleris/probatorium/report"
)

// collectRefappSched reads the refapp's CPU view for the cell document.
// STUB: reads nothing yet.
func collectRefappSched(ctx context.Context, getenv func(string) string, driverMode, procRoot string, pid int, outDir, debugAddr string) *report.RefappSched {
	return nil
}

// gomaxprocsFromSchedText is the GOMAXPROCS in the REFAPP_SCHED line of a
// refapp_sched.txt body, 0 when absent. STUB.
func gomaxprocsFromSchedText(text string) int { return 0 }
