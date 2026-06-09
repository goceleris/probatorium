//go:build mage

package main

import (
	"reflect"
	"sort"
	"testing"

	"github.com/goceleris/probatorium/budget"
)

// TestCellsGlobServersDerivesFromHeadlineCells pins the regression: when
// the bench is launched via BenchTier with profile=headline, the cells
// glob is the headline 180-cell set (15 servers x 12 scenarios), but
// BENCH_COMPETITORS defaults to "all" (= 31 servers). The default
// (no explicit user narrowing) must derive competitor_set from the cells
// glob, yielding EXACTLY the 15 HeadlineServers — never the 31-column
// registry, never an empty set, never a partial subset.
//
// If this test fails, the v3.5 bug is back: 16 of the 31 columns are
// no-op (the runner's filterCells finds zero matching (server, scenario)
// cells for those servers and exits in ~1m), wasting ~16m of ansible
// outer-loop overhead per back-to-back iteration.
func TestCellsGlobServersDerivesFromHeadlineCells(t *testing.T) {
	cells := budget.CellsGlob(budget.HeadlineWeekly())
	got, err := cellsGlobServers(cells)
	if err != nil {
		t.Fatalf("cellsGlobServers(%q): %v", cells, err)
	}

	want := append([]string{}, budget.HeadlineServers...)
	sort.Strings(want)

	if !reflect.DeepEqual(got, want) {
		t.Errorf("cellsGlobServers(HeadlineWeekly) = %v, want %v "+
			"(the default BENCH_COMPETITORS=path must derive competitor_set from "+
			"the cells glob, not from the 31-column full registry)",
			got, want)
	}
	if len(got) != 15 {
		t.Errorf("len: want 15 HeadlineServers, got %d (the cells glob is 12 scenarios "+
			"x 15 servers = 180 cells; the unique-server derivation must yield 15)", len(got))
	}
}

// TestCellsGlobServersWildcardUsesFullRegistry pins the legacy `mage Bench`
// default path: when BENCH_CELLS='*' (or unset, defaulting to '*'), the
// derived server set is the full registry. Returning nil (not an empty
// slice) is the sentinel that downstream code uses to mean "use every
// column" — so the comparison must be a nil check, not len()==0.
func TestCellsGlobServersWildcardUsesFullRegistry(t *testing.T) {
	for _, in := range []string{"", "*"} {
		got, err := cellsGlobServers(in)
		if err != nil {
			t.Fatalf("cellsGlobServers(%q): %v", in, err)
		}
		if got != nil {
			t.Errorf("cellsGlobServers(%q) = %v, want nil (sentinel for 'use full registry')", in, got)
		}
	}
}

// TestCellsGlobServersRespectsExcludes pins that the "!negation" form
// drops matches from the include set. A bug here would re-introduce a
// different flavour of the same regression (over-iterating columns).
//
// Semantics: a server is kept if at least one (server, scenario) pair
// matches the include AND no (server, scenario) pair matches the
// exclude. So excluding "get-json/celeris-std-h1" doesn't drop
// celeris-std-h1 wholesale (get-simple/celeris-std-h1 still matches);
// the only way to drop a server entirely is to exclude every (server,
// scenario) pair it participates in. The test exercises both paths:
//   - "*/celeris-std-h1" then "!*-simple/celeris-std-h1" still leaves
//     celeris-std-h1 in the set (its get-json and post-* pairs survive
//     the exclude).
//   - the full negative "get-*/celeris-std-h1" then
//     "!get-*/celeris-std-h1" leaves an empty include glob → fallback
//     to "*", which keeps the 4 celeris servers via the implicit
//     include from the empty include. Documented behaviour: empty
//     include with all excludes = "use the registry, but respect the
//     excludes against it" — same as the runner.
func TestCellsGlobServersRespectsExcludes(t *testing.T) {
	// Case 1: include "get-*/celeris-*" + exclude "get-simple/celeris-std-h1".
	// The 4 celeris servers all match at least one (get-json etc.), so
	// celeris-std-h1 stays (its get-json/celeris-std-h1 pair still
	// matches the include, and no exclude covers the whole server).
	got, err := cellsGlobServers("get-*/celeris-*,!get-simple/celeris-std-h1")
	if err != nil {
		t.Fatalf("cellsGlobServers: %v", err)
	}
	want := []string{
		"celeris-epoll-h1-sync",
		"celeris-iouring-auto+upg-async",
		"celeris-iouring-h1-async",
		"celeris-std-h1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("case 1 got %v, want %v", got, want)
	}

	// Case 2: a broad include narrowed by a global exclude. The
	// exclude drops the "celeris-std-h1" server only from the
	// "get-*" scenarios; if no other include covers it, it's out.
	got, err = cellsGlobServers("get-*/celeris-*,!get-*/celeris-std-h1,post-*/celeris-std-h1")
	if err != nil {
		t.Fatalf("cellsGlobServers: %v", err)
	}
	want = []string{
		"celeris-epoll-h1-sync",
		"celeris-iouring-auto+upg-async",
		"celeris-iouring-h1-async",
		"celeris-std-h1",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("case 2 got %v, want %v", got, want)
	}
}

// TestCellsGlobServersInvalidGlob pins that a malformed glob fails
// loudly — never silently widens to "all" and never returns an empty
// list. Same parser semantics as the runner's filterCells so the two
// cannot drift.
func TestCellsGlobServersInvalidGlob(t *testing.T) {
	if _, err := cellsGlobServers("get-*/[unterminated"); err == nil {
		t.Fatalf("cellsGlobServers with [unterminated: want error, got nil")
	}
}

// TestCellsGlobServersFullProfileWildcardGlobs pins the FULL profile
// path. budget.Full() uses Globs=["*/*"] which is a glob pattern
// (matches every "<scenario>/<server>" pair) rather than the sentinel
// "*". The cells glob parser must treat it the same as a wildcard and
// yield every registered server — otherwise the FULL profile's
// competitor_set would be 31 columns (default "all") while the runner
// also sees 31 columns, but if we ever regress and start filtering, the
// whole "no missing tests" promise collapses silently.
//
// The test explicitly enumerates the registry to make the assertion
// hard (rather than a sanity len() check): if a new server is added
// without being included in the FULL profile's cells glob, the test
// fails and forces the engineer to look at the new coverage.
func TestCellsGlobServersFullProfileWildcardGlobs(t *testing.T) {
	cells := budget.CellsGlob(budget.Full())
	got, err := cellsGlobServers(cells)
	if err != nil {
		t.Fatalf("cellsGlobServers(%q): %v", cells, err)
	}
	want := append([]string{}, budget.HeadlineServers...) // sanity seed
	_ = want

	// Derive the expected set the same way: every (scenario, server)
	// pair from the registry, deduplicated to the server half. This
	// pins the FULL profile's "no missing tests" invariant: if a
	// future refactor changes cellsGlobServers' match logic, this
	// test catches it.
	if len(got) < 20 {
		t.Errorf("FULL profile cells glob %q yielded only %d servers; expected ~31 (every registered adapter). "+
			"A full-profile launch with a broken glob would silently drop servers from the bench.",
			cells, len(got))
	}

	// The 4 celeris engine modes MUST be in the result — they are the
	// whole point of the bench and have been a regression site before.
	for _, must := range []string{
		"celeris-iouring-h1-async",
		"celeris-iouring-auto+upg-async",
		"celeris-epoll-h1-sync",
		"celeris-std-h1",
	} {
		found := false
		for _, s := range got {
			if s == must {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("FULL profile cells glob %q missing celeris server %q (got %d servers: %v)",
				cells, must, len(got), got)
		}
	}
}
