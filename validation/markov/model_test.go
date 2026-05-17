package markov

import (
	"strings"
	"testing"
)

const tinyYAML = `
start: a
states:
  a:
    b: 1.0
  b:
    a: 0.5
    c: 0.5
  c: {}
`

func TestLoadMatrix_Roundtrip(t *testing.T) {
	m, err := LoadMatrix(strings.NewReader(tinyYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if m.Start != "a" {
		t.Fatalf("start=%q", m.Start)
	}
	if got := len(m.States); got != 3 {
		t.Fatalf("states=%d", got)
	}
	// Verify sort.
	if m.States[0] != "a" || m.States[1] != "b" || m.States[2] != "c" {
		t.Fatalf("states unsorted: %+v", m.States)
	}
	if got := len(m.Transitions["c"]); got != 0 {
		t.Fatalf("c should be terminal, got %d edges", got)
	}
}

func TestLoadMatrix_BadIndent(t *testing.T) {
	bad := "start: a\nstates:\n a:\n    b: 1.0\n"
	_, err := LoadMatrix(strings.NewReader(bad))
	if err == nil {
		t.Fatal("expected indent error")
	}
}

func TestLoadMatrix_DanglingRef(t *testing.T) {
	bad := "start: a\nstates:\n  a:\n    nope: 1.0\n"
	_, err := LoadMatrix(strings.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "destination state not declared") {
		t.Fatalf("expected dangling ref error, got %v", err)
	}
}

func TestLoadMatrix_NegativeWeight(t *testing.T) {
	bad := "start: a\nstates:\n  a:\n    a: -1.0\n"
	_, err := LoadMatrix(strings.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "negative weight") {
		t.Fatalf("expected negative-weight error, got %v", err)
	}
}

func TestLoadMatrix_MissingStart(t *testing.T) {
	bad := "states:\n  a: {}\n"
	_, err := LoadMatrix(strings.NewReader(bad))
	if err == nil || !strings.Contains(err.Error(), "missing required 'start:'") {
		t.Fatalf("expected missing-start error, got %v", err)
	}
}

func TestChain_DeterministicAcrossInstances(t *testing.T) {
	m, err := LoadMatrix(strings.NewReader(tinyYAML))
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	collect := func(seed uint64) []string {
		c := New(m, seed)
		var seq []string
		c.Walk(100, func(s string) { seq = append(seq, s) })
		return seq
	}
	a := collect(0x42)
	b := collect(0x42)
	if len(a) != len(b) {
		t.Fatalf("differing walk lengths: %d vs %d", len(a), len(b))
	}
	for i := range a {
		if a[i] != b[i] {
			t.Fatalf("step %d diverged: %q vs %q", i, a[i], b[i])
		}
	}
}

func TestChain_DifferentSeedsDiverge(t *testing.T) {
	// Need a matrix with non-degenerate fan-out from the start state
	// for seed divergence to be observable in a short prefix.
	y := "start: a\nstates:\n  a:\n    b: 1.0\n    c: 1.0\n  b:\n    a: 1.0\n  c:\n    a: 1.0\n"
	m, _ := LoadMatrix(strings.NewReader(y))
	a := New(m, 0x1)
	b := New(m, 0x42)
	var diverged bool
	for i := 0; i < 50; i++ {
		sa, _ := a.Next()
		sb, _ := b.Next()
		if sa != sb {
			diverged = true
			break
		}
	}
	if !diverged {
		t.Fatal("two distinct seeds produced identical 50-step prefixes; unlikely")
	}
}

func TestChain_TerminalState(t *testing.T) {
	// "a -> c" goes to terminal. After Walk, last state should be c.
	m, _ := LoadMatrix(strings.NewReader(tinyYAML))
	c := New(m, 99)
	c.Walk(200, nil)
	// Reset and verify it parks back at start.
	c.Reset()
	if c.Current() != m.Start {
		t.Fatalf("Reset failed: current=%q", c.Current())
	}
	if c.Steps() != 0 {
		t.Fatalf("Reset failed to zero steps: %d", c.Steps())
	}
}

func TestChain_SelfLoop(t *testing.T) {
	// Matrix with only a self-loop; walking should stay parked at a.
	y := "start: a\nstates:\n  a:\n    a: 1.0\n"
	m, err := LoadMatrix(strings.NewReader(y))
	if err != nil {
		t.Fatal(err)
	}
	c := New(m, 0xfeed)
	for i := 0; i < 20; i++ {
		s, ok := c.Next()
		if !ok || s != "a" {
			t.Fatalf("self-loop broke at step %d: ok=%v s=%q", i, ok, s)
		}
	}
}

func TestChain_WeightedSampling(t *testing.T) {
	// 90/10 split between two outgoing edges: with 10k samples the
	// observed split must be within 5 percentage points of the prior.
	y := "start: a\nstates:\n  a:\n    b: 0.9\n    c: 0.1\n  b:\n    a: 1.0\n  c:\n    a: 1.0\n"
	m, _ := LoadMatrix(strings.NewReader(y))
	const n = 10_000
	c := New(m, 0xc0ffee)
	bCount, cCount := 0, 0
	for c.Steps() < n {
		s, ok := c.Next()
		if !ok {
			t.Fatal("chain stalled")
		}
		switch s {
		case "b":
			bCount++
		case "c":
			cCount++
		}
	}
	frac := float64(bCount) / float64(bCount+cCount)
	if frac < 0.85 || frac > 0.95 {
		t.Fatalf("weighted sampling skewed: b/(b+c)=%.3f, expected ≈0.9", frac)
	}
}

func TestLoadMatrixFile_AuthSessionRatelimit(t *testing.T) {
	m, err := LoadMatrixFile("auth_session_ratelimit.yaml")
	if err != nil {
		t.Fatalf("load yaml file: %v", err)
	}
	// Post-#126 + route-fix rewrite — chain now starts at `me` since
	// the refapp has no `/` route and the walker bootstraps the
	// session via the top-level `login:` directive before stepping.
	if m.Start != "me" {
		t.Fatalf("expected start=me, got %q", m.Start)
	}
	// Top-level login directive must point at the real refapp endpoint.
	if m.Login.Method != "POST" || m.Login.Path != "/login" {
		t.Fatalf("expected login=POST /login, got %s %s", m.Login.Method, m.Login.Path)
	}
	wantStates := []string{"me", "list_users", "user_detail", "create_user", "update_user", "user_delete", "user_posts", "logout"}
	for _, s := range wantStates {
		if _, ok := m.Transitions[s]; !ok {
			t.Errorf("missing state %q", s)
		}
	}
	// logout is still terminal (no outgoing edges). The walker's
	// outer loop sees Next() return ("", false) and calls
	// chain.Reset() — the next iteration starts back at `me` and
	// the walker's 401-retry path re-logs in via the `login:`
	// directive. Soak workloads stay unbounded.
	if len(m.Transitions["logout"]) != 0 {
		t.Fatalf("logout should be terminal; has %d edges", len(m.Transitions["logout"]))
	}
	// Walk a long trajectory and assert we visit at least 5 distinct
	// non-terminal states — sanity that the matrix is connected.
	c := New(m, 0x1)
	seen := map[string]bool{}
	for i := 0; i < 5000 && c.Steps() < 5000; i++ {
		s, ok := c.Next()
		if !ok {
			c.Reset()
			continue
		}
		seen[s] = true
	}
	if len(seen) < 5 {
		t.Fatalf("expected to visit ≥5 distinct states, saw %d: %+v", len(seen), seen)
	}
}
