//go:build mage

package main

import (
	"reflect"
	"testing"

	"github.com/goceleris/probatorium/budget"
	"github.com/goceleris/probatorium/servers"
)

// TestCellsGlobServersDerivesFromHeadlineCells pins that the WEEKLY
// (headline) profile covers the FULL column set. The weekly profile no
// longer curates a subset — it runs every registered server x scenario via
// the "*/*" cells glob — so deriving competitor_set from that glob must
// yield EVERY registered adapter, never a narrowed subset and never an
// empty set. Guards two ways the weekly run could silently lose coverage:
// the v3.5 no-op-column regression (a glob that drops servers), and any
// future re-curation that quietly shrinks the weekly grid back to a
// "headline" subset — which the user explicitly does not want.
func TestCellsGlobServersDerivesFromHeadlineCells(t *testing.T) {
	// The weekly profile now runs the FULL grid (Globs "*/*"), so its cells
	// glob must derive the COMPLETE server set — every registered adapter,
	// the same coverage as the full profile. The old behaviour (a curated
	// ~14-server headline subset) is gone: "weekly should include all of
	// them, not a headline." This test guards that the weekly never silently
	// narrows the column set back to a subset.
	cells := budget.CellsGlob(budget.HeadlineWeekly())
	got, err := cellsGlobServers(cells)
	if err != nil {
		t.Fatalf("cellsGlobServers(%q): %v", cells, err)
	}

	want := servers.Names() // every registered adapter, sorted
	if !reflect.DeepEqual(got, want) {
		t.Errorf("cellsGlobServers(HeadlineWeekly) = %v, want the FULL registry %v "+
			"(the weekly grid must cover every server, never a curated subset)",
			got, want)
	}

	// The weekly and full profiles now derive the same column set — they
	// differ only by per-cell window. Pin that equivalence.
	fullCells := budget.CellsGlob(budget.Full())
	fullGot, err := cellsGlobServers(fullCells)
	if err != nil {
		t.Fatalf("cellsGlobServers(full %q): %v", fullCells, err)
	}
	if !reflect.DeepEqual(got, fullGot) {
		t.Errorf("weekly server set %v != full server set %v; they must match now", got, fullGot)
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
//     to "*", which keeps the celeris servers via the implicit
//     include from the empty include. Documented behaviour: empty
//     include with all excludes = "use the registry, but respect the
//     excludes against it" — same as the runner.
func TestCellsGlobServersRespectsExcludes(t *testing.T) {
	// Case 1: include "get-*/celeris-*" + exclude "get-simple/celeris-std-h1".
	// Every celeris engine column matches at least one (get-json etc.), so
	// celeris-std-h1 stays (its get-json/celeris-std-h1 pair still
	// matches the include, and no exclude covers the whole server). The
	// want set is the FULL celeris-* column family (the v1.5.4 redesign
	// expanded it from 4 to the 9 engine modes below).
	got, err := cellsGlobServers("get-*/celeris-*,!get-simple/celeris-std-h1")
	if err != nil {
		t.Fatalf("cellsGlobServers: %v", err)
	}
	want := []string{
		"celeris-adaptive-auto+upg-async",
		"celeris-adaptive-h1-async",
		"celeris-epoll-auto+upg-async",
		"celeris-epoll-h1-async",
		"celeris-epoll-h1-sync",
		"celeris-iouring-auto+upg-async",
		"celeris-iouring-h1-async",
		"celeris-iouring-h1-sync",
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
		"celeris-adaptive-auto+upg-async",
		"celeris-adaptive-h1-async",
		"celeris-epoll-auto+upg-async",
		"celeris-epoll-h1-async",
		"celeris-epoll-h1-sync",
		"celeris-iouring-auto+upg-async",
		"celeris-iouring-h1-async",
		"celeris-iouring-h1-sync",
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

// TestResolveBenchColumnsNativeH2cSharesBuild pins the native h2c column
// plumbing: an "<framework>-h2" native column reuses its h1 sibling's staged
// binary (Bin.BinName → competitors/<framework>) and passes the SUT-facing
// -engine value with the feature-set "-noupg" suffix stripped. It also pins
// that the existing h1 native columns now carry an explicit -engine h1 (so
// the run_bench_cell port-ownership guard can tell two columns sharing one
// binary apart).
func TestResolveBenchColumnsNativeH2cSharesBuild(t *testing.T) {
	cols, err := resolveBenchColumns("axum,axum-h2,aspnet,aspnet-h2,fastapi,fastapi-h2,hono,hono-h2")
	if err != nil {
		t.Fatalf("resolveBenchColumns: %v", err)
	}
	by := map[string]benchColumn{}
	for _, c := range cols {
		by[c.Slug] = c
	}
	cases := []struct {
		slug, wantBin, wantEngine string
	}{
		{"axum", "axum", "h1"},
		{"axum-h2", "axum", "h2c"}, // shares competitors/axum, -engine h2c
		{"aspnet", "aspnet", "h1"},
		{"aspnet-h2", "aspnet", "h2c"},
		{"fastapi", "fastapi", "h1"},
		{"fastapi-h2", "fastapi", "h2c"}, // shares the python launcher
		{"hono", "hono", ""},             // bun h1 column has no Engine → no flag
		{"hono-h2", "hono", "h2c"},       // shares the bun launcher
	}
	for _, c := range cases {
		got, ok := by[c.slug]
		if !ok {
			t.Errorf("column %q missing from resolveBenchColumns output", c.slug)
			continue
		}
		if got.Bin != c.wantBin {
			t.Errorf("%s: Bin = %q, want %q", c.slug, got.Bin, c.wantBin)
		}
		if got.Engine != c.wantEngine {
			t.Errorf("%s: Engine = %q, want %q", c.slug, got.Engine, c.wantEngine)
		}
	}
}
