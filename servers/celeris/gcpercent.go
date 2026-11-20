package main

// The bench SUT's opt-in GC target (celeris#756, celeris#1310).
//
// celeris.Config.GCPercent is a default-off library option: zero leaves the
// Go runtime's GC target alone. The bench adapter exposes it through ONE
// environment variable so a lane can build a variant column that turns it on
// (per-column Adapter.SUTEnv) without touching any production column:
//
//	PROBATORIUM_CELERIS_GC_PERCENT unset, empty or "0"  -> Config.GCPercent = 0
//	                                                       (the library does not touch the runtime;
//	                                                        the process behaves exactly as before)
//	PROBATORIUM_CELERIS_GC_PERCENT = N (N > 0)          -> Config.GCPercent = N
//	anything else (non-integer, negative)               -> the adapter refuses to start
//
// A negative value is refused here rather than forwarded: -1 would mean "GC
// off" to debug.SetGCPercent, and a typo in a column definition must never
// turn that on silently. The Go runtime's own GOGC environment variable is
// untouched and is a different mechanism (read by the runtime at process
// start, not by celeris); an explicit celeris option wins over it.

import (
	"fmt"
	"runtime/metrics"
	"strconv"
	"strings"
)

// gcPercentEnv names the adapter's switch for celeris.Config.GCPercent.
const gcPercentEnv = "PROBATORIUM_CELERIS_GC_PERCENT"

// gcPercentFromEnv parses the adapter switch. getenv is os.Getenv in main;
// it is a parameter so the test does not touch the process environment.
func gcPercentFromEnv(getenv func(string) string) (int, error) {
	raw := strings.TrimSpace(getenv(gcPercentEnv))
	if raw == "" {
		return 0, nil
	}
	n, err := strconv.Atoi(raw)
	if err != nil {
		return 0, fmt.Errorf("%s=%q: not an integer: %w", gcPercentEnv, raw, err)
	}
	if n < 0 {
		return 0, fmt.Errorf("%s=%d: must be 0 (off) or a positive GC percent; a negative value would disable the GC", gcPercentEnv, n)
	}
	return n, nil
}

// effectiveGCPercent reads the GC target currently in force without writing
// it (debug.SetGCPercent would change it). -1 means the GC is off; 0 is
// reported when the runtime does not publish the metric.
func effectiveGCPercent() int {
	s := []metrics.Sample{{Name: "/gc/gogc:percent"}}
	metrics.Read(s)
	if s[0].Value.Kind() == metrics.KindUint64 {
		return int(int64(s[0].Value.Uint64())) // -1 (GC off) is stored as a wrapped uint64
	}
	return 0
}
