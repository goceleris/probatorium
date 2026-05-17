package markov

import (
	"path/filepath"
	"strings"
	"testing"
)

// TestAllRefappYAMLs_HaveRequestPerNonTerminalState asserts that every
// non-terminal state in every shipped Markov yaml declares a
// `request: METHOD path` directive. The Tier 1 walker is data-driven
// off Matrix.Requests — a non-terminal state without a request entry
// means the chain walks through it silently, generating no HTTP
// traffic.
//
// Discovered the hard way on probatorium#125: six of eight refapps
// had ZERO Tier 1 traffic because markovStateToPath was hardcoded
// for the auth_session_ratelimit endpoint set, and the other six
// shipped yamls referenced states the switch didn't know.
//
// Terminal states (empty Transitions slice — `done: {}` /
// `logout: {}`) are allowed to be silent; they model end-of-walk.
func TestAllRefappYAMLs_HaveRequestPerNonTerminalState(t *testing.T) {
	yamls, err := filepath.Glob("*.yaml")
	if err != nil {
		t.Fatalf("glob: %v", err)
	}
	if len(yamls) < 8 {
		t.Fatalf("expected at least 8 refapp yamls under validation/markov/, found %d", len(yamls))
	}
	for _, y := range yamls {
		y := y
		slug := strings.TrimSuffix(filepath.Base(y), ".yaml")
		t.Run(slug, func(t *testing.T) {
			m, err := LoadMatrixFile(y)
			if err != nil {
				t.Fatalf("load %s: %v", y, err)
			}
			var nonTerminal, missing int
			for _, state := range m.States {
				edges := m.Transitions[state]
				if len(edges) == 0 {
					// Terminal state — silent is allowed.
					continue
				}
				nonTerminal++
				req, ok := m.Requests[state]
				if !ok {
					t.Errorf("state %q has %d outgoing edges but no `request:` directive — walker will skip silently", state, len(edges))
					missing++
					continue
				}
				if req.Method == "" || req.Path == "" {
					t.Errorf("state %q has empty Method/Path: %+v", state, req)
				}
				if !strings.HasPrefix(req.Path, "/") {
					t.Errorf("state %q path %q must start with /", state, req.Path)
				}
			}
			if nonTerminal == 0 {
				t.Errorf("matrix has no non-terminal states — every state is silent")
			}
			t.Logf("%s: %d non-terminal states covered, %d missing", slug, nonTerminal-missing, missing)
		})
	}
}
