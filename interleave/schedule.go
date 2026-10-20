// Package interleave produces the cell execution schedule the probatorium
// orchestrator walks. Ordering is outermost = run index, middle =
// scenario, innermost = server. This ensures every pass through the
// matrix visits each (scenario, server) pair once before the next run
// starts, so thermal / system-noise effects are spread equally across
// all cells.
package interleave

import (
	"github.com/goceleris/probatorium/scenarios"
	"github.com/goceleris/probatorium/servers"
)

// Cell is a single unit of bench work: run RunIdx of (Scenario, Server).
type Cell struct {
	RunIdx   int
	Scenario scenarios.Scenario
	Server   servers.Server
}

// Schedule returns the full sequence of cells to execute. See the
// package doc for the ordering guarantee.
//
// Pairs for which Scenario.Applicable(Server.Features()) is false are
// omitted — they cannot run. Every (scenario, server) pair that IS
// applicable appears exactly runs times, and the ordering of pairs
// within each run is identical so noise is shared equally across runs.
func Schedule(runs int, ss []scenarios.Scenario, sv []servers.Server) []Cell {
	if runs <= 0 || len(ss) == 0 || len(sv) == 0 {
		return nil
	}

	// Pre-compute the applicable (scenario, server) pairs once; the same
	// ordered pair list is emitted for every run.
	type pair struct {
		scenario scenarios.Scenario
		server   servers.Server
	}
	pairs := make([]pair, 0, len(ss)*len(sv))
	for _, s := range ss {
		for _, srv := range sv {
			if !s.Applicable(srv.Features()) {
				continue
			}
			pairs = append(pairs, pair{s, srv})
		}
	}

	out := make([]Cell, 0, runs*len(pairs))
	for r := 0; r < runs; r++ {
		for _, p := range pairs {
			out = append(out, Cell{RunIdx: r, Scenario: p.scenario, Server: p.server})
		}
	}
	return out
}

// ReverseWithinRuns returns cells with the order inside every run reversed and
// the runs themselves kept in order: run 0's cells last to first, then run 1's.
// A BENCH_TARGET=both run walks its arm64 half this way so the heavy cells of
// the two arches fall in different halves of a column (probatorium#442). The
// input is not modified.
func ReverseWithinRuns(cells []Cell) []Cell {
	out := make([]Cell, 0, len(cells))
	for lo := 0; lo < len(cells); {
		hi := lo
		for hi < len(cells) && cells[hi].RunIdx == cells[lo].RunIdx {
			hi++
		}
		for i := hi - 1; i >= lo; i-- {
			out = append(out, cells[i])
		}
		lo = hi
	}
	return out
}
