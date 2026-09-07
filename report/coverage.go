package report

import (
	"fmt"
	"sort"
	"strings"
)

// Streaming-endpoint coverage (probatorium#300).
//
// The WS-torture and SSE-kill slices only mean something on a refapp
// that routes /ws and /events. Seven of the eight do not, so the
// run-wide totals the v1.5.11 soak published — 285,587 WS upgrades
// against 1,999,114 endpoint-absent, 55,059 SSE streams against
// 1,499,314 — described the route table, not the engine, and the
// ws_*/sse_* zeros in 42 of 48 cells were vacuous.
//
// The validator now probes each endpoint once per cell and skips the
// slice where the route is absent (validation/route_probe.go), leaving
// the verdict in the cell's tier-1 sub-tallies. This view rolls those
// per-cell verdicts up per refapp so a reader sees WHICH refapps carry
// the coverage instead of one aggregate that averages them together.

// EndpointCoverage is one streaming endpoint's coverage for one refapp.
//
// ProbedCells and PresentCells count CELLS; the rest are event totals
// summed over them. ProbedCells < Cells means some cells never asked
// (the slice is dormant below streamingWalkerMinConcurrency), which is
// unknown coverage — not an absent route.
type EndpointCoverage struct {
	ProbedCells   int
	PresentCells  int
	Sent          int64
	Reached       int64 // ws_upgraded / sse_established
	AbsentReplies int64 // 404s seen mid-run despite a present route
}

// RouteAbsent reports a refapp that was asked and has no such route in
// any cell. Distinct from "never asked": an unprobed refapp returns
// false, because nothing was learned about it.
func (e EndpointCoverage) RouteAbsent() bool { return e.ProbedCells > 0 && e.PresentCells == 0 }

// StreamCoverage is one refapp's streaming coverage across its cells.
type StreamCoverage struct {
	Refapp string
	Cells  int
	WS     EndpointCoverage
	SSE    EndpointCoverage
}

// StreamingCoverage rolls per-cell streaming verdicts up per refapp,
// sorted by refapp so the table is stable across runs.
func StreamingCoverage(cells []ValidationCellResult) []StreamCoverage {
	byRefapp := map[string]*StreamCoverage{}
	for _, c := range cells {
		cov, ok := byRefapp[c.Refapp]
		if !ok {
			cov = &StreamCoverage{Refapp: c.Refapp}
			byRefapp[c.Refapp] = cov
		}
		cov.Cells++
		if c.Tier1 == nil {
			continue
		}
		accumulateEndpoint(&cov.WS, c.Tier1.WSTorture, "ws")
		accumulateEndpoint(&cov.SSE, c.Tier1.SSEKill, "sse")
	}
	out := make([]StreamCoverage, 0, len(byRefapp))
	for _, cov := range byRefapp {
		out = append(out, *cov)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Refapp < out[j].Refapp })
	return out
}

// accumulateEndpoint folds one cell's sub-tally into the running
// per-refapp coverage. prefix is "ws" or "sse"; the reached counter is
// the slice's own name for "the endpoint answered us".
func accumulateEndpoint(e *EndpointCoverage, m map[string]int64, prefix string) {
	if m[prefix+"_route_probed"] > 0 {
		e.ProbedCells++
	}
	if m[prefix+"_route_present"] > 0 {
		e.PresentCells++
	}
	e.Sent += m[prefix+"_sent"]
	if prefix == "ws" {
		e.Reached += m["ws_upgraded"]
	} else {
		e.Reached += m["sse_established"]
	}
	e.AbsentReplies += m[prefix+"_endpoint_absent"]
}

// FormatStreamingCoverage renders the coverage rollup as a fixed-width
// table for the run log. One line per refapp per endpoint that was
// asked about, so "no route" is as visible as a reach percentage.
func FormatStreamingCoverage(cov []StreamCoverage) string {
	var b strings.Builder
	b.WriteString("streaming endpoint coverage (per refapp)\n")
	fmt.Fprintf(&b, "  %-26s %-5s %-6s %-10s %-10s %s\n",
		"refapp", "cells", "endpt", "routed", "sent", "reached")
	for _, c := range cov {
		writeCoverageRow(&b, c.Refapp, c.Cells, "/ws", c.WS)
		writeCoverageRow(&b, c.Refapp, c.Cells, "/events", c.SSE)
	}
	return b.String()
}

func writeCoverageRow(b *strings.Builder, refapp string, cells int, endpoint string, e EndpointCoverage) {
	routed := fmt.Sprintf("%d/%d", e.PresentCells, cells)
	switch {
	case e.ProbedCells == 0:
		routed = "unprobed"
	case e.RouteAbsent():
		routed = "no route"
	}
	reached := "-"
	if e.Sent > 0 {
		reached = fmt.Sprintf("%d (%.1f%%)", e.Reached, 100*float64(e.Reached)/float64(e.Sent))
	}
	fmt.Fprintf(&b, "  %-26s %-5d %-6s %-10s %-10d %s\n",
		refapp, cells, endpoint, routed, e.Sent, reached)
}
