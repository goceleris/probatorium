package report

import (
	"bytes"
	"strings"
	"testing"

	"github.com/goceleris/loadgen"
)

// bwCell builds a one-run data-bearing CellResult whose throughput is
// bytesPerSec and whose loadgen self-CPU is loadgenCPUPct (a percent, as
// loadgen emits it). Optionally carries a server resource sample.
func bwCell(scenario, server string, bytesPerSec, loadgenCPUPct, rps float64, serverCPU *float64) CellResult {
	cr := CellResult{
		ScenarioName: scenario,
		ServerName:   server,
		Samples: []loadgen.Result{{
			RequestsPerSec: rps,
			ThroughputBPS:  bytesPerSec,
			CPUPctP95:      loadgenCPUPct,
			Requests:       1_000_000,
		}},
	}
	if serverCPU != nil {
		cr.Resources = []*ResourceStats{rstat(*serverCPU, 100, 12)}
	}
	return cr
}

// TestNetworkBoundFlaggedAtLineRate pins the network-bound detection: a
// large-payload cell at ~19 Gbps over the 20 Gbps fabric (with the loadgen
// not CPU-bound) is flagged; an identical-shape small cell well below the
// ceiling is not; and a run with no known line rate flags nothing.
func TestNetworkBoundFlaggedAtLineRate(t *testing.T) {
	t.Parallel()
	const lineRate = int64(20_000_000_000) // 20 Gbps
	cpu := 42.0

	// ~19.2 Gbps = 2.4e9 bytes/sec — above 0.80 * 20 Gbps.
	hot := bwCell("get-json-64k", "celeris-iouring-h1-async", 2.4e9, 30, 35000, &cpu)
	// ~0.29 Gbps — a small-response cell, nowhere near the ceiling.
	cold := bwCell("get-json", "celeris-iouring-h1-async", 3.6e7, 30, 350000, &cpu)

	agg := Aggregate([]CellResult{hot, cold})
	doc := BuildDocument(BuildInput{
		HostArchPair: "linux/amd64",
		Environment:  Environment{Fabric: "3-host LACP 20G", FabricLineRateBitsPerSec: lineRate},
		Servers:      map[string]ServerMeta{"celeris-iouring-h1-async": {Language: "go"}},
		Agg:          agg,
	})
	if len(doc.Benchmarks) != 1 {
		t.Fatalf("benchmarks=%d want 1", len(doc.Benchmarks))
	}
	nb := doc.Benchmarks[0].NetworkBound
	if !nb["get-json-64k"] {
		t.Error("get-json-64k should be flagged network-bound at 19.2 Gbps")
	}
	if nb["get-json"] {
		t.Error("get-json (0.29 Gbps) must NOT be flagged network-bound")
	}

	// No line rate → nothing flagged, even for the hot cell.
	docNoRate := BuildDocument(BuildInput{
		HostArchPair: "linux/amd64",
		Environment:  Environment{Fabric: "3-host Tailscale overlay"},
		Servers:      map[string]ServerMeta{"celeris-iouring-h1-async": {Language: "go"}},
		Agg:          Aggregate([]CellResult{hot}),
	})
	if len(docNoRate.Benchmarks[0].NetworkBound) != 0 {
		t.Errorf("no line rate must flag nothing, got %v", docNoRate.Benchmarks[0].NetworkBound)
	}
}

// TestNetworkBoundLoadgenSaturatedNotFlagged guards the loadgen-bottleneck
// exclusion: a cell at the bandwidth ceiling but with the loadgen itself
// pegged is a client artefact, not a fabric ceiling, so it is not flagged.
func TestNetworkBoundLoadgenSaturatedNotFlagged(t *testing.T) {
	t.Parallel()
	cpu := 42.0
	// 19.2 Gbps but loadgen self-CPU p95 = 900% (9 cores) → above the ceiling.
	hot := bwCell("post-64k", "axum", 2.4e9, 900, 35000, &cpu)
	agg := Aggregate([]CellResult{hot})
	doc := BuildDocument(BuildInput{
		Environment: Environment{FabricLineRateBitsPerSec: 20_000_000_000},
		Servers:     map[string]ServerMeta{"axum": {Language: "rust"}},
		Agg:         agg,
	})
	if doc.Benchmarks[0].NetworkBound["post-64k"] {
		t.Error("loadgen-saturated cell must not be flagged network-bound")
	}
}

// TestNetworkBoundMarkdownSection asserts the markdown renders the
// efficiency table for flagged cells and ranks by Gbps/CPU%.
func TestNetworkBoundMarkdownSection(t *testing.T) {
	t.Parallel()
	const lineRate = int64(20_000_000_000)
	lowCPU := 30.0  // efficient: more Gbps per CPU%
	highCPU := 90.0 // less efficient

	eff := bwCell("get-json-64k", "celeris-iouring-h1-async", 2.4e9, 25, 35000, &lowCPU)
	ineff := bwCell("get-json-64k", "fastapi", 2.3e9, 25, 33000, &highCPU)

	agg := Aggregate([]CellResult{eff, ineff})
	doc := BuildDocument(BuildInput{
		Environment: Environment{FabricLineRateBitsPerSec: lineRate},
		Servers: map[string]ServerMeta{
			"celeris-iouring-h1-async": {Language: "go"},
			"fastapi":                  {Language: "python"},
		},
		Agg: agg,
	})

	var buf bytes.Buffer
	if err := WriteMarkdown(&buf, doc, agg, Meta{GitRef: "t"}); err != nil {
		t.Fatalf("WriteMarkdown: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "Network-bound cells") {
		t.Fatal("markdown missing network-bound section")
	}
	// The efficient (low-CPU) adapter must be ranked above the inefficient one.
	effIdx := strings.Index(out, "celeris-iouring-h1-async |")
	ineffIdx := strings.Index(out, "fastapi |")
	if effIdx < 0 || ineffIdx < 0 || effIdx > ineffIdx {
		t.Errorf("expected celeris ranked above fastapi in NIC-bound table (eff=%d ineff=%d)", effIdx, ineffIdx)
	}
}

// TestHeadlineExcludesNonCPUBoundScenarios pins the headline ranking gate:
// a scenario whose saturation RPS is not server-CPU-bound — wire-bound by
// design (post-1m), fan-out (ws-hub/sse-fanout), or a single-conn latency
// probe (get-json-1c) — must NOT head a bolded Latency-at-SLO table, while a
// genuine CPU-bound row (get-json) still does. This is the consult of the
// NetworkBound flag that the headline ranking previously ignored.
func TestHeadlineExcludesNonCPUBoundScenarios(t *testing.T) {
	t.Parallel()
	slo := map[int]int{10: 1000, 50: 2000, 100: 3000, 500: 4000, 1000: 5000}
	doc := &Document{
		SchemaVersion: SchemaVersion,
		Benchmarks: []ServerResult{{
			Name: "celeris-iouring-h1-async",
			LatencyAtSLO: map[string]map[int]int{
				"get-json":             slo, // CPU-bound → ranked
				"post-1m":              slo, // wire-bound by design → excluded
				"get-json-1c":          slo, // single-conn latency probe → excluded
				"ws-hub-broadcast-128": slo, // fan-out → excluded
			},
			NetworkBound: map[string]bool{"post-1m": true},
		}},
	}
	var buf bytes.Buffer
	if err := writeLatencyAtSLOSection(&buf, doc); err != nil {
		t.Fatalf("writeLatencyAtSLOSection: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "### get-json\n") {
		t.Error("get-json (CPU-bound) must head a headline ranking table")
	}
	for _, sc := range []string{"### post-1m", "### get-json-1c", "### ws-hub-broadcast-128"} {
		if strings.Contains(out, sc) {
			t.Errorf("%q must NOT head a headline ranking table", sc)
		}
	}
	if !strings.Contains(out, "Not ranked here") {
		t.Error("excluded scenarios should be disclosed in a note")
	}
}
