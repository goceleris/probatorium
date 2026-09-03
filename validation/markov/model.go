// Package markov implements a deterministic, seeded discrete-time
// Markov chain used by the validation tier to drive realistic
// session-shaped HTTP traffic.
//
// The chain loads a transition matrix from a small YAML dialect (only
// strings, integers, floats — no nested lists, no flow style), seeds a
// [math/rand/v2.PCG] source from a [uint64], and steps state to state
// by sampling against the per-state outgoing-weight distribution.
//
// Determinism is the load-bearing property: the same (Matrix, seed,
// step-count) tuple always produces the same trajectory across runs,
// architectures, and Go versions. This is what makes validator-replay
// work — a failure's seed plus the matrix is enough to reproduce the
// exact request stream that tripped the bug.
//
// The package has zero non-stdlib dependencies; the YAML reader is a
// purpose-built parser for the transition-matrix dialect rather than a
// general-purpose YAML library.
package markov

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"math/rand/v2"
	"os"
	"sort"
	"strconv"
	"strings"
)

// Matrix is a finite-state transition matrix. States are referenced by
// name; outgoing edges are unnormalized weights — the chain normalizes
// each row at load time. A state with no outgoing edges is a terminal
// state (Next returns "", false).
type Matrix struct {
	// Start is the canonical entry state. Required.
	Start string
	// States lists every named state in deterministic order (sorted).
	States []string
	// Transitions maps "from-state" → ordered list of weighted outgoing
	// edges. Edges within a row are sorted by destination name so
	// sampling is deterministic across map iteration orders.
	Transitions map[string][]Edge
	// Requests maps a state name to the HTTP request the Tier 1 walker
	// fires when it enters that state. States without a Requests entry
	// are silent — the chain still walks through them, but no HTTP
	// request is sent. Silent states model "logical" transitions that
	// don't correspond to a refapp endpoint (terminal "done" states,
	// pure session bookkeeping, etc.).
	Requests map[string]Request
	// Login, if set, names the HTTP request the Tier 1 walker should
	// fire BEFORE the Markov chain begins, and again whenever an
	// in-walk request returns 401. Parsed from a top-level
	// `login: METHOD path` line in the YAML. Walker uses
	// deterministic-per-seed username + password JSON body for the
	// POST/PUT case. Empty/zero Login means the refapp has no auth
	// (kitchen_sink, observability, static_swagger_proxy, driver_*)
	// — walker skips login entirely.
	Login Request
}

// Request describes the HTTP call associated with one Markov state.
// Parsed from the `request: METHOD path` line under each state in
// the YAML.
type Request struct {
	// Method is the HTTP verb (GET, POST, PUT, DELETE, PATCH). Empty
	// when the state has no `request:` directive.
	Method string
	// Path is the URL path beginning with `/`. Empty when the state
	// has no `request:` directive.
	Path string
	// Expect5xx marks a state whose request is DESIGNED to return a 5xx
	// (e.g. observability's /api/error induced-panic route, which exists
	// to exercise recovery + error metrics). Parsed from an `expect: 5xx`
	// line under the state. The Tier 1 walker tallies such responses as
	// requests_5xx_expected instead of requests_5xx, so the unexpected
	// 5xx count can be gated to zero without hiding intentional ones.
	Expect5xx bool
}

// Edge is one weighted outgoing transition.
type Edge struct {
	// To is the destination state name.
	To string
	// Weight is the unnormalized transition weight. Must be >= 0; zero
	// means the edge is dead (effectively unreachable from this row's
	// cumulative distribution).
	Weight float64
}

// Chain is a stepping iterator over a Matrix. Each Chain owns a private
// PCG source so concurrent chains over the same matrix do not share
// random state.
type Chain struct {
	matrix *Matrix
	rng    *rand.Rand
	// current is the chain's current state. Mutated by Step.
	current string
	// stepsTaken is the cumulative step count. Useful for replay
	// validation: a recorded run's step count must match a reproduction's.
	stepsTaken int64
}

// New seeds a Chain over m using the 128-bit PCG initial state derived
// from seed (lo) + 0xdeadbeefcafebabe (hi). The matrix is shared by
// reference; multiple chains over the same matrix is the common case.
func New(m *Matrix, seed uint64) *Chain {
	pcg := rand.NewPCG(seed, seed^0xdeadbeefcafebabe)
	return &Chain{
		matrix:  m,
		rng:     rand.New(pcg),
		current: m.Start,
	}
}

// Current returns the chain's current state.
func (c *Chain) Current() string { return c.current }

// Steps returns the cumulative step count since New.
func (c *Chain) Steps() int64 { return c.stepsTaken }

// Reset rewinds the chain to the matrix Start state and zeroes the
// step counter. The RNG state is preserved — call New again with a
// fresh seed if rewinding the RNG is also required.
func (c *Chain) Reset() {
	c.current = c.matrix.Start
	c.stepsTaken = 0
}

// Next picks one outgoing edge from the current state, weighted by the
// matrix's transition probabilities, advances the chain to that
// destination, and returns it. If the current state has no outgoing
// edges, Next returns ("", false) and the chain remains parked.
//
// Self-loops are supported — an edge whose To equals the current
// state simply keeps the chain in place. Zero-weight edges are silently
// skipped during sampling so a matrix can declare a structurally
// reachable edge without actually walking it.
func (c *Chain) Next() (string, bool) {
	edges, ok := c.matrix.Transitions[c.current]
	if !ok || len(edges) == 0 {
		return "", false
	}
	var total float64
	for _, e := range edges {
		if e.Weight > 0 {
			total += e.Weight
		}
	}
	if total <= 0 {
		return "", false
	}
	// Sample uniformly in [0, total) and walk the cumulative
	// distribution. The edges slice is sorted by destination name so
	// the sampling order is independent of map iteration.
	pick := c.rng.Float64() * total
	var cum float64
	for _, e := range edges {
		if e.Weight <= 0 {
			continue
		}
		cum += e.Weight
		if pick < cum {
			c.current = e.To
			c.stepsTaken++
			return c.current, true
		}
	}
	// Float64 rounding may overshoot the cumulative sum; fall back to
	// the last positive-weight edge so we never return ("", false) by
	// arithmetic accident.
	for i := len(edges) - 1; i >= 0; i-- {
		if edges[i].Weight > 0 {
			c.current = edges[i].To
			c.stepsTaken++
			return c.current, true
		}
	}
	return "", false
}

// Walk steps the chain n times, calling cb on every visited state
// (including the destination of each step, not the start state).
// Returns the number of steps actually taken — terminating early when
// the chain hits a state with no outgoing edges.
//
// Useful for one-shot trajectory generation; for stream consumption the
// caller is better off driving Next() directly.
func (c *Chain) Walk(n int, cb func(state string)) int {
	for i := 0; i < n; i++ {
		s, ok := c.Next()
		if !ok {
			return i
		}
		if cb != nil {
			cb(s)
		}
	}
	return n
}

// LoadMatrix parses a Matrix from r using the [matrix-yaml dialect]
// documented in package doc above. The dialect is intentionally narrow
// so the parser is short and bullet-proof:
//
//	start: state-a
//	states:
//	  state-a:
//	    state-b: 0.7
//	    state-c: 0.2
//	    state-a: 0.1   # self-loop
//	  state-b:
//	    state-a: 1.0
//	  state-c: {}      # terminal (no outgoing edges)
//
// Indentation MUST be two spaces. Comments (#) and blank lines are
// allowed anywhere. Keys are bare identifiers; values are either a
// number, the literal "{}" (empty map), or a child map.
func LoadMatrix(r io.Reader) (*Matrix, error) {
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 0, 64*1024), 1<<20)

	var (
		m           = &Matrix{Transitions: map[string][]Edge{}, Requests: map[string]Request{}}
		expect5xx   = map[string]bool{}
		currentFrom string
		lineNo      int
	)
	for scan.Scan() {
		lineNo++
		raw := scan.Text()
		line := stripComment(raw)
		if strings.TrimSpace(line) == "" {
			continue
		}
		indent := countLeadingSpaces(line)
		body := strings.TrimSpace(line)

		switch {
		case indent == 0 && strings.HasPrefix(body, "start:"):
			m.Start = strings.TrimSpace(strings.TrimPrefix(body, "start:"))
		case indent == 0 && strings.HasPrefix(body, "login:"):
			// Top-level `login: METHOD path` directive. Same shape as
			// the per-state request line — METHOD<space>path.
			spec := strings.TrimSpace(strings.TrimPrefix(body, "login:"))
			method, path, ok := strings.Cut(spec, " ")
			if !ok || method == "" || path == "" {
				return nil, fmt.Errorf("markov: line %d: login %q must be \"METHOD path\"", lineNo, spec)
			}
			if !strings.HasPrefix(path, "/") {
				return nil, fmt.Errorf("markov: line %d: login path %q must start with /", lineNo, path)
			}
			m.Login = Request{Method: strings.ToUpper(method), Path: path}
		case indent == 0 && body == "states:":
			// header; nothing to do.
		case indent == 2:
			// "state-name:" or "state-name: {}"
			name, val, err := splitKeyVal(body)
			if err != nil {
				return nil, fmt.Errorf("markov: line %d: %w", lineNo, err)
			}
			currentFrom = name
			if _, exists := m.Transitions[currentFrom]; !exists {
				m.Transitions[currentFrom] = nil
			}
			if val == "{}" {
				// terminal state: keep empty slice.
				m.Transitions[currentFrom] = []Edge{}
			} else if val != "" {
				return nil, fmt.Errorf("markov: line %d: unexpected value %q after state name", lineNo, val)
			}
		case indent == 4:
			name, val, err := splitKeyVal(body)
			if err != nil {
				return nil, fmt.Errorf("markov: line %d: %w", lineNo, err)
			}
			if currentFrom == "" {
				return nil, fmt.Errorf("markov: line %d: edge before state header", lineNo)
			}
			// Special-case `request: METHOD path` — these aren't edges,
			// they're the HTTP request the Tier 1 walker fires when it
			// enters this state. The split is on the first space:
			// "GET /api/me" → Method=GET, Path=/api/me.
			// `expect: 5xx` — the state's request is designed to fail.
			// Recorded aside and merged into Requests after parsing, since
			// the directive may precede or follow the `request:` line.
			if name == "expect" {
				if strings.TrimSpace(val) != "5xx" {
					return nil, fmt.Errorf("markov: line %d: expect %q must be \"5xx\"", lineNo, val)
				}
				expect5xx[currentFrom] = true
				break
			}
			if name == "request" {
				method, path, ok := strings.Cut(val, " ")
				if !ok || method == "" || path == "" {
					return nil, fmt.Errorf("markov: line %d: request %q must be \"METHOD path\"", lineNo, val)
				}
				if !strings.HasPrefix(path, "/") {
					return nil, fmt.Errorf("markov: line %d: request path %q must start with /", lineNo, path)
				}
				m.Requests[currentFrom] = Request{
					Method: strings.ToUpper(method),
					Path:   path,
				}
				break
			}
			w, err := strconv.ParseFloat(val, 64)
			if err != nil {
				return nil, fmt.Errorf("markov: line %d: bad weight %q: %w", lineNo, val, err)
			}
			if w < 0 {
				return nil, fmt.Errorf("markov: line %d: negative weight %v", lineNo, w)
			}
			m.Transitions[currentFrom] = append(m.Transitions[currentFrom], Edge{To: name, Weight: w})
		default:
			return nil, fmt.Errorf("markov: line %d: unexpected indent %d", lineNo, indent)
		}
	}
	if err := scan.Err(); err != nil {
		return nil, fmt.Errorf("markov: scan: %w", err)
	}

	if m.Start == "" {
		return nil, errors.New("markov: matrix missing required 'start:' field")
	}
	if _, ok := m.Transitions[m.Start]; !ok {
		return nil, fmt.Errorf("markov: start state %q has no entry under 'states:'", m.Start)
	}

	// Build the deterministic state list and sort each row.
	for name, edges := range m.Transitions {
		m.States = append(m.States, name)
		sort.Slice(edges, func(i, j int) bool { return edges[i].To < edges[j].To })
		m.Transitions[name] = edges
	}
	sort.Strings(m.States)
	for st := range expect5xx {
		req, ok := m.Requests[st]
		if !ok {
			return nil, fmt.Errorf("markov: state %q has `expect: 5xx` but no `request:` line", st)
		}
		req.Expect5xx = true
		m.Requests[st] = req
	}

	if err := m.validate(); err != nil {
		return nil, err
	}
	return m, nil
}

// LoadMatrixFile is a convenience wrapper that opens path and calls
// [LoadMatrix].
func LoadMatrixFile(path string) (*Matrix, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer func() { _ = f.Close() }()
	return LoadMatrix(f)
}

// validate ensures every edge destination is itself a declared state.
// A dangling reference (typo in the YAML) would otherwise surface as a
// silent "state-name not in Transitions" mid-walk, which is much
// harder to debug than a load-time error.
func (m *Matrix) validate() error {
	known := map[string]bool{}
	for _, s := range m.States {
		known[s] = true
	}
	for from, edges := range m.Transitions {
		for _, e := range edges {
			if !known[e.To] {
				return fmt.Errorf("markov: state %q -> %q: destination state not declared", from, e.To)
			}
		}
	}
	return nil
}

// stripComment removes the first '#' comment and everything after it.
// Comments inside quoted strings are not a concern — the dialect has
// no quoted strings.
func stripComment(line string) string {
	if i := strings.IndexByte(line, '#'); i >= 0 {
		return line[:i]
	}
	return line
}

// countLeadingSpaces returns the count of leading ASCII spaces. Tabs
// are not permitted and surface later as "unexpected indent" errors.
func countLeadingSpaces(line string) int {
	n := 0
	for _, r := range line {
		if r != ' ' {
			break
		}
		n++
	}
	return n
}

// splitKeyVal parses "key: value" or "key:" into (key, value). Value
// is the trimmed remainder, which may legitimately be the empty
// string (the form used for state-header lines under "states:").
func splitKeyVal(body string) (string, string, error) {
	i := strings.IndexByte(body, ':')
	if i < 0 {
		return "", "", fmt.Errorf("missing ':' in %q", body)
	}
	key := strings.TrimSpace(body[:i])
	val := strings.TrimSpace(body[i+1:])
	if key == "" {
		return "", "", fmt.Errorf("empty key in %q", body)
	}
	return key, val, nil
}
