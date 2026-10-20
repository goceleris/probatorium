// Package refapparms holds the two experiment arms of the race tier's
// adaptive/amd64 ws_echo investigation (probatorium#481, celeris#871), so the
// validator, the mage target and the tests agree on one spelling and one
// validation. Both arms default to off: an unset variable changes nothing.
//
//   - Arm A (CPUsEnv): the refapp is confined to a CPU list with taskset.
//   - Arm B (WorkloadHintEnv): the adaptive engine is started on io_uring
//     through celeris.Config.WorkloadHint instead of switching to it under
//     load.
package refapparms

import (
	"fmt"
	"sort"
	"strconv"
	"strings"
)

const (
	// CPUsEnv is the CPU list (taskset -c syntax) the validator confines every
	// refapp it launches on this host to. Read by the validator.
	CPUsEnv = "PROBATORIUM_REFAPP_CPUS"
	// WorkloadHintEnv is the celeris.Config.WorkloadHint the refapps set:
	// "high" starts the adaptive engine on io_uring, "low" pins it to epoll.
	// Read by validation/refapp/internal/debugvars (its own module, so the
	// spelling is repeated there; refapparms_test.go keeps the two equal).
	WorkloadHintEnv = "PROBATORIUM_REFAPP_WORKLOAD_HINT"
)

// MinCPUs is the fewest CPUs an arm A list may name. celeris requires two
// workers at least, and a one-CPU arm would be a different experiment (the
// "one CPU" reading that the incident dossier suggested is a pinned thread,
// not a confinement; see PLAN-race-onecpu.md).
const MinCPUs = 2

// maxCPU is far above any host's CPU count and far under anything that
// could make a range expansion expensive.
const maxCPU = 4095

// ParseCPUs parses a taskset -c list ("2-5", "0,2,4", "0-3,8") into its
// ascending distinct CPUs and refuses anything else: an empty or malformed
// token, a descending range, a CPU past maxCPU, or fewer than MinCPUs distinct
// CPUs. The string it was given is what the caller hands to taskset; a list
// it accepts is one taskset accepts.
func ParseCPUs(list string) ([]int, error) {
	list = strings.TrimSpace(list)
	if list == "" {
		return nil, fmt.Errorf("%s: empty CPU list", CPUsEnv)
	}
	seen := map[int]bool{}
	for _, tok := range strings.Split(list, ",") {
		lo, hi, isRange := strings.Cut(tok, "-")
		a, err := parseCPU(lo)
		if err != nil {
			return nil, fmt.Errorf("%s=%q: %w", CPUsEnv, list, err)
		}
		b := a
		if isRange {
			if b, err = parseCPU(hi); err != nil {
				return nil, fmt.Errorf("%s=%q: %w", CPUsEnv, list, err)
			}
			if b < a {
				return nil, fmt.Errorf("%s=%q: descending range %q", CPUsEnv, list, tok)
			}
		}
		for c := a; c <= b; c++ {
			seen[c] = true
		}
	}
	out := make([]int, 0, len(seen))
	for c := range seen {
		out = append(out, c)
	}
	sort.Ints(out)
	if len(out) < MinCPUs {
		return nil, fmt.Errorf("%s=%q names %d CPU(s), want at least %d", CPUsEnv, list, len(out), MinCPUs)
	}
	return out, nil
}

func parseCPU(s string) (int, error) {
	if s == "" {
		return 0, fmt.Errorf("empty CPU number")
	}
	for _, r := range s {
		if r < '0' || r > '9' {
			return 0, fmt.Errorf("%q is not a CPU number", s)
		}
	}
	n, err := strconv.Atoi(s)
	if err != nil || n > maxCPU {
		return 0, fmt.Errorf("CPU %q is out of range (0-%d)", s, maxCPU)
	}
	return n, nil
}

// ValidateWorkloadHint accepts "" (arm off), "high" and "low".
func ValidateWorkloadHint(v string) error {
	switch strings.TrimSpace(v) {
	case "", "high", "low":
		return nil
	}
	return fmt.Errorf("%s=%q: want high, low or empty", WorkloadHintEnv, v)
}
