package properties

import "sort"

// All returns every registered Spec sorted by ID. The result is a
// freshly allocated slice, safe to mutate.
//
// The ordering is significant for human-readability: validator-replay's
// dry-run prints predicates in registration order to confirm the build
// includes the expected set, and the orchestrator persists predicate
// evaluation logs keyed by ID so incident triage can deterministically
// compare two runs.
func All() []Spec {
	specs := []Spec{
		ICONN1,
		ICONN2,
		IRFC1,
		IRFC2,
		IMEM1,
		IMEM2,
		IPANIC,
		IRACE,
		ICHECKPTR,
		IMWRateLimit,
		IMWSession,
		IMWJWT,
		IDRV,
		IENGIOURing,
		IENGAdaptive,
		// tier-1-walker predicates — driven by the orchestrator's
		// TallyCallback (validation/runner.go), NOT this snapshot
		// loop. Their Predicate is a no-op (always returns true);
		// the real signal arrives via the orchestrator's violations
		// channel. Registered here so dry-run output lists them
		// alongside the snapshot-driven set.
		IADVAccepted,
		IH2CCrashed,
		IWSAccepted,
		IWSHang,
	}
	out := make([]Spec, len(specs))
	copy(out, specs)
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}

// ByTier returns every Spec whose Tier equals tier. Empty tier returns
// everything (identical to [All]).
func ByTier(tier string) []Spec {
	if tier == "" {
		return All()
	}
	out := make([]Spec, 0)
	for _, s := range All() {
		if s.Tier == tier {
			out = append(out, s)
		}
	}
	return out
}

// ByID returns the Spec with the given ID and ok=true, or a zero Spec
// and ok=false if no such predicate exists. Callers should treat a
// missing ID as a programming error.
func ByID(id string) (Spec, bool) {
	for _, s := range All() {
		if s.ID == id {
			return s, true
		}
	}
	return Spec{}, false
}
