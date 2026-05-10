// Package fault implements the validation tier's fault-injection
// primitives. Each fault is a reversible action driven by a known-good
// shell command exec'd against either the local host or the
// load-generator host (msa2-client for tc-netem variants).
//
// Reversibility is the load-bearing property: every Apply has a
// matching Undo. The scheduler always runs Undo before the host moves
// to the next scheduled fault, and on shutdown.
//
// Faults talk to the OS via /usr/sbin/* binaries, not Go syscalls.
// Two reasons: (1) the validator runs unprivileged on the dev mac
// and as the bench-runner user on linux, and the polkit/sudoers rules
// in ansible/roles/clusternode already grant the relevant binaries
// passwordless sudo; (2) using the same binaries that operators reach
// for during incident triage keeps the fault catalogue ground-truth
// for diagnostic muscle memory.
package fault

import (
	"context"
	"fmt"
	"os/exec"
	"strings"
	"time"
)

// Fault is a reversible perturbation of the target environment. Apply
// returns nil on success; Undo is best-effort (failure is logged but
// does not propagate — the scheduler must continue to the next fault).
//
// String returns a short human-readable identifier, used by the
// scheduler in dry-run output and incident logs.
type Fault interface {
	Apply(ctx context.Context) error
	Undo(ctx context.Context) error
	String() string
}

// Host describes the machine a fault talks to. The validator
// orchestrator is local; faults that perturb the load generator land
// on msa2-client; faults that perturb celeris itself land on
// msa2-server. The empty string means "local host — exec directly,
// no ssh wrapper".
type Host string

const (
	// HostLocal is the validator's own host. Used for tests and dev
	// runs; production runs invariably target a remote.
	HostLocal Host = ""
	// HostServer is the celeris-under-test host (msa2-server).
	HostServer Host = "msa2-server"
	// HostClient is the load-generator host (msa2-client). tc-netem
	// faults live here so they perturb the path *into* celeris from
	// the loadgen's perspective.
	HostClient Host = "msa2-client"
)

// run executes argv on host, optionally with sudo. Returns combined
// stdout+stderr on failure. Local exec uses [exec.CommandContext]
// directly; remote exec wraps via ssh.
//
// The function is centralized so a future migration to a faster
// transport (mosh, agent multiplexing) does not need to touch every
// fault file.
func run(ctx context.Context, host Host, sudo bool, argv ...string) ([]byte, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("fault.run: empty argv")
	}
	var full []string
	if sudo {
		full = append([]string{"sudo", "-n"}, argv...)
	} else {
		full = argv
	}
	if host == HostLocal {
		cmd := exec.CommandContext(ctx, full[0], full[1:]...)
		return cmd.CombinedOutput()
	}
	// Remote: ssh <host> -- <quoted argv>. Joining with spaces is
	// safe because every fault constructs argv from known-safe
	// pieces; we never let user-supplied strings into the command.
	cmd := exec.CommandContext(ctx, "ssh",
		"-o", "BatchMode=yes",
		"-o", "ConnectTimeout=5",
		string(host),
		"--",
		strings.Join(full, " "),
	)
	return cmd.CombinedOutput()
}

// formatErr wraps an exec failure with the command line and combined
// output. Used uniformly so failures are triage-friendly.
func formatErr(action string, argv []string, out []byte, err error) error {
	return fmt.Errorf("fault.%s: %s: %w; output=%q",
		action, strings.Join(argv, " "), err, strings.TrimSpace(string(out)))
}

// ApplyTimeout is the per-fault Apply timeout. Faults that legitimately
// need longer (rare — Apply should be near-instant) wrap their own
// ctx; this constant is the catalogue default.
const ApplyTimeout = 10 * time.Second

// UndoTimeout is the per-fault Undo timeout. Slightly more generous than
// Apply since Undo is the path we MUST reach even when the target is
// degraded.
const UndoTimeout = 20 * time.Second
