package debugvars

import (
	"fmt"
	"os"
	"strings"

	"github.com/goceleris/celeris"
)

// WorkloadHintEnv sets celeris.Config.WorkloadHint on every refapp the
// validator launches (probatorium#481 arm B): "high" starts the adaptive
// engine on io_uring instead of epoll-then-switch, "low" keeps it on epoll.
// It only matters to the adaptive engine; the other engines ignore the hint.
// Empty (every routine run) leaves the config alone. The validator's copy of
// the spelling is validation/refapparms.WorkloadHintEnv.
const WorkloadHintEnv = "PROBATORIUM_REFAPP_WORKLOAD_HINT"

// workloadHint reads WorkloadHintEnv through getenv. ok is false when it is
// unset; an unknown value is an error, because a typo that silently ran the
// default would waste a cluster dispatch.
func workloadHint(getenv func(string) string) (h celeris.WorkloadHint, ok bool, err error) {
	switch v := strings.TrimSpace(getenv(WorkloadHintEnv)); v {
	case "":
		return celeris.WorkloadUnspecified, false, nil
	case "high":
		return celeris.WorkloadHighConcurrency, true, nil
	case "low":
		return celeris.WorkloadLowConcurrency, true, nil
	default:
		return celeris.WorkloadUnspecified, false, fmt.Errorf("%s=%q: want high, low or empty", WorkloadHintEnv, v)
	}
}

// applyWorkloadHint sets cfg.WorkloadHint from the environment unless the
// refapp set one itself. A bad value stops the refapp at startup with the
// reason on stderr, which the validator reports as a cell that never got
// ready.
func applyWorkloadHint(cfg *celeris.Config, getenv func(string) string) {
	h, ok, err := workloadHint(getenv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "debugvars: %v\n", err)
		os.Exit(2)
	}
	if ok && cfg.WorkloadHint == celeris.WorkloadUnspecified {
		cfg.WorkloadHint = h
	}
}
