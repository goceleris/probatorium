package markov

import (
	"strings"
	"testing"
)

// `expect: 5xx` marks a state whose request is designed to fail; the Tier 1
// walker tallies its 5xx separately so the unexpected count can be zero.
func TestParse_ExpectFiveXX(t *testing.T) {
	src := `
start: a
states:
  a:
    request: GET /api/error
    expect: 5xx
    b: 1.0
  b:
    request: GET /ok
    a: 1.0
`
	m, err := LoadMatrix(strings.NewReader(src))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if !m.Requests["a"].Expect5xx {
		t.Fatal("state a should have Expect5xx=true")
	}
	if m.Requests["b"].Expect5xx {
		t.Fatal("state b must not inherit Expect5xx")
	}
	// order-insensitive: expect before request
	src2 := strings.Replace(src, "    request: GET /api/error\n    expect: 5xx\n", "    expect: 5xx\n    request: GET /api/error\n", 1)
	m2, err := LoadMatrix(strings.NewReader(src2))
	if err != nil || !m2.Requests["a"].Expect5xx {
		t.Fatalf("expect-before-request: err=%v Expect5xx=%v", err, m2 != nil && m2.Requests["a"].Expect5xx)
	}
}

func TestParse_ExpectRejectsBadValueAndSilentState(t *testing.T) {
	if _, err := LoadMatrix(strings.NewReader("start: a\nstates:\n  a:\n    request: GET /x\n    expect: 4xx\n    a: 1.0\n")); err == nil || !strings.Contains(err.Error(), "expect") {
		t.Fatalf("bad expect value must be rejected, got %v", err)
	}
	if _, err := LoadMatrix(strings.NewReader("start: a\nstates:\n  a:\n    expect: 5xx\n    a: 1.0\n")); err == nil || !strings.Contains(err.Error(), "no `request:`") {
		t.Fatalf("expect on a silent state must be rejected, got %v", err)
	}
}
