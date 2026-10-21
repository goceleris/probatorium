package validation

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/goceleris/probatorium/validation/refapparms"
)

// The experiment arms of the race tier's adaptive/amd64 ws_echo
// investigation (probatorium#481), read from the validator's environment on
// the cluster host. Both are off unless set, and an off arm changes nothing.
// See validation/refapparms for the spelling and PLAN-race-onecpu.md (in the
// RACEGATE lane evidence) for the pre-registered use.

// refappCPUsPrefix is the command that confines a refapp to the CPU list in
// refapparms.CPUsEnv: taskset -c LIST, which execs the refapp so its PID and
// process group are its own. nil when the arm is off. A list taskset cannot be
// trusted with, or a host without taskset, is an error: a cell must not run
// quietly unconfined and be read as arm A.
func refappCPUsPrefix(getenv func(string) string, lookPath func(string) (string, error)) ([]string, error) {
	list := strings.TrimSpace(getenv(refapparms.CPUsEnv))
	if list == "" {
		return nil, nil
	}
	if _, err := refapparms.ParseCPUs(list); err != nil {
		return nil, err
	}
	taskset, err := lookPath("taskset")
	if err != nil {
		return nil, fmt.Errorf("%s=%q: taskset is not available on this host: %w", refapparms.CPUsEnv, list, err)
	}
	return []string{taskset, "-c", list}, nil
}

// armsActive reports whether either arm (or the stall capture, whose dossiers
// are what an arm is read from) is on, which is when a cell writes its
// readback file. A routine run writes nothing new.
func armsActive(getenv func(string) string) bool {
	return strings.TrimSpace(getenv(refapparms.CPUsEnv)) != "" ||
		strings.TrimSpace(getenv(refapparms.WorkloadHintEnv)) != "" ||
		stallCaptureEnabled(getenv)
}

// refappReadbackFile is the per-cell record of the arm as the refapp lived it.
const refappReadbackFile = "refapp_sched.txt"

// writeRefappReadback records, once the refapp is ready, which arm this cell
// ran under and what the running refapp reports: every thread's CPU mask and
// state, GOMAXPROCS, and the engine counters. The pre-registered analysis
// reads the arm back from this file, not from the dispatch inputs, because an
// input that did not reach the refapp would otherwise be read as a null
// result. Best effort: an unreadable leg says so in the file.
func writeRefappReadback(ctx context.Context, dir string, getenv func(string) string, procRoot string, pid int, debugAddr string) error {
	_, err := writeRefappReadbackText(ctx, dir, getenv, procRoot, pid, debugAddr)
	return err
}

// writeRefappReadbackText is writeRefappReadback that also returns the text it
// wrote, so the cell document can take the refapp's GOMAXPROCS from the same
// /debug/sched answer instead of asking the refapp twice.
func writeRefappReadbackText(ctx context.Context, dir string, getenv func(string) string, procRoot string, pid int, debugAddr string) (string, error) {
	cctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cpus := strings.TrimSpace(getenv(refapparms.CPUsEnv))
	hint := strings.TrimSpace(getenv(refapparms.WorkloadHintEnv))
	if cpus == "" {
		cpus = "none"
	}
	if hint == "" {
		hint = "none"
	}
	text := fmt.Sprintf("ARM refapp_cpus=%s workload_hint=%s at=%s\n%s",
		cpus, hint, time.Now().UTC().Format(time.RFC3339Nano), refappSchedText(cctx, procRoot, pid, debugAddr))
	return text, writePlainText(filepath.Join(dir, refappReadbackFile), text)
}

// logArm says on stderr that an arm is on, once per cell (one line in the
// validator log per cell, so a cell that ran unarmed is visible there).
func logArm(getenv func(string) string) {
	if v := strings.TrimSpace(getenv(refapparms.CPUsEnv)); v != "" {
		fmt.Fprintf(os.Stderr, "validation: arm A on: refapps confined to CPUs %s (%s)\n", v, refapparms.CPUsEnv)
	}
	if v := strings.TrimSpace(getenv(refapparms.WorkloadHintEnv)); v != "" {
		fmt.Fprintf(os.Stderr, "validation: arm B on: refapp WorkloadHint=%s (%s)\n", v, refapparms.WorkloadHintEnv)
	}
}
