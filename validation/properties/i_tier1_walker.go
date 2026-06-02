package properties

// Tier 1 walker-driven predicates.
//
// These four predicates aren't evaluated by the per-second
// validator-checker loop the way I-CONN / I-MEM / I-PANIC are. They
// are emitted directly by the orchestrator's TallyCallback (see
// validation/runner.go) the FIRST time the matching tier-1 sub-tally
// counter goes non-zero. Registering them here makes:
//
//   - `validator -dry-run` list them alongside the snapshot-driven
//     predicates (so the operator sees the full set of things the
//     run watches for, not just the per-second ones).
//   - Per-incident dossier dir names use a canonical ID
//     ("incidents/<ts>-I-ADV-ACCEPTED/") that grep + dashboards
//     can rely on.
//
// The Predicate field is a no-op that always returns true. The
// per-second loop walks every registered Spec, evaluates the
// Predicate, and writes the result row to sqlite — these will
// always log ok=1 there. The real signal arrives via the
// orchestrator's violations channel.
var (
	IADVAccepted = Spec{
		ID:          "I-ADV-ACCEPTED",
		Description: "server must reject malformed adversarial bytes (Tier 1 adv.wrong_accepted == 0)",
		Tier:        "tier-1-walker",
		Predicate:   alwaysOK,
	}
	IH2CCrashed = Spec{
		ID:          "I-H2C-CRASHED",
		Description: "engine must not crash on h2c upgrade churn (Tier 1 h2c.crashed == 0)",
		Tier:        "tier-1-walker",
		Predicate:   alwaysOK,
	}
	IWSAccepted = Spec{
		ID:          "I-WS-ACCEPTED",
		Description: "server must reject bad WebSocket frames (Tier 1 ws.accepted_bad_frame == 0)",
		Tier:        "tier-1-walker",
		Predicate:   alwaysOK,
	}
	IWSHang = Spec{
		ID:          "I-WS-HANG",
		Description: "WebSocket conn must not hang past close timeout (Tier 1 ws.hang_no_close == 0)",
		Tier:        "tier-1-walker",
		Predicate:   alwaysOK,
	}
	// ILiveness is the engine-agnostic crash oracle: the refapp PROCESS must
	// survive the whole run. Unlike the other walker predicates (which key on
	// a protocol-specific torture counter), this fires on ANY process death —
	// a Go fatal throw, an escaped panic, a SIGSEGV, an OOM kill — because the
	// walkers themselves are blind to it (a dead server just looks like
	// connection-refused). Emitted by the orchestrator's TallyCallback when
	// Tier 1's liveness tally reports Crashed. See validation/liveness.go.
	ILiveness = Spec{
		ID:          "I-LIVENESS",
		Description: "refapp process must stay alive for the whole run (Tier 1 liveness.crashed == false)",
		Tier:        "tier-1-walker",
		Predicate:   alwaysOK,
	}
)

// alwaysOK is the placeholder Predicate for tier-1-walker entries.
// The actual violation signal comes via the orchestrator's
// TallyCallback, not this evaluator.
func alwaysOK(_ *Snapshot, _ Context) (bool, string) { return true, "" }
