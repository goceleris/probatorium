package report

import (
	"sort"
	"strings"
	"time"
)

// CompileOptionsFor returns the build-time knobs that produced an
// adapter's binary, keyed by source language. Both results.json emitters
// call this so the per-server compile_options agree byte-for-byte across
// the in-process and cluster paths.
//
// The Go entry mirrors the canonical cross-compile path
// (crossCompileGoBinary in mage_helpers.go): CGO_ENABLED=0, the
// arch-tuning knob, -trimpath, and the -s -w ldflags. Native adapters
// (rust / python / bun) are built by the ansible roles; their flags are
// recorded here so the metadata is non-empty and grep-friendly.
func CompileOptionsFor(language, arch string) []string {
	switch language {
	case "go":
		opts := []string{"CGO_ENABLED=0"}
		switch arch {
		case "amd64":
			opts = append(opts, "GOAMD64=v3")
		case "arm64":
			opts = append(opts, "GOARM64=v8.0")
		}
		opts = append(opts, "trimpath", "ldflags=-s -w")
		return opts
	case "rust":
		return []string{"profile=release-fat", "RUSTFLAGS=-C target-cpu=native"}
	case "python":
		return []string{"runtime=uvicorn", "loop=uvloop", "http=httptools"}
	case "bun":
		return []string{"runtime=bun", "entry=Bun.serve"}
	default:
		return []string{}
	}
}

// ServerMeta is the per-adapter metadata BuildDocument folds into each
// ServerResult. It is a plain value type — callers (the in-process
// runner and the cluster mage path) project it from their own adapter
// registry so report/ stays a leaf node and never imports servers/ or
// scenarios/.
type ServerMeta struct {
	Category         string
	Language         string
	LanguageVersion  string
	Framework        string
	FrameworkVersion string
	Engine           string
	CompileOptions   []string
}

// BuildInput carries everything BuildDocument needs to assemble a
// canonical [Document]. Agg is the per-cell aggregate map from
// [Aggregate]; Servers maps a server name to its [ServerMeta]; the rest
// are the ambient run facts the schema requires.
type BuildInput struct {
	HostArchPair    string
	Environment     Environment
	BenchmarkConfig BenchmarkConfig
	Servers         map[string]ServerMeta
	Agg             map[string]CellAggregate
	Validation      *ValidationResults
	Soak            *SoakSummary
}

// BuildDocument folds a per-cell aggregate map into the canonical v5.1
// [Document]. Both results.json emitters (cmd/runner and the cluster
// mage Bench path) route through here so the on-disk shape is owned in
// exactly one place.
//
// Each Benchmarks entry covers every scenario for one server. Per-server
// metadata is read from in.Servers rather than from the on-disk cells so
// the output is grounded against the caller's adapter table. Benchmarks
// is sorted by Name for byte-stable output across reruns.
func BuildDocument(in BuildInput) *Document {
	byAdapter := map[string]*ServerResult{}
	for _, c := range in.Agg {
		sr := byAdapter[c.ServerName]
		if sr == nil {
			sr = &ServerResult{
				Name:                    c.ServerName,
				SaturationModeRPS:       map[string]float64{},
				RatedModeP99AtTargetRPS: map[string]time.Duration{},
				LatencyAtSLO:            map[string]map[int]int{},
				HdrHistogramB64:         map[string]string{},
				LoadgenCPUP95:           map[string]float64{},
				SentVsHandledDeltaPct:   map[string]float64{},
			}
			if m, ok := in.Servers[c.ServerName]; ok {
				sr.Category = m.Category
				sr.Language = m.Language
				sr.LanguageVersion = m.LanguageVersion
				sr.Framework = m.Framework
				sr.FrameworkVersion = m.FrameworkVersion
				sr.Engine = m.Engine
				sr.CompileOptions = m.CompileOptions
			}
			byAdapter[c.ServerName] = sr
		}

		// Per-run outcome evidence (schema v5.4): emitted whenever at least
		// one run came back non-OK so a clean rerun can never erase a prior
		// crash; all-OK cells add no bytes.
		recordRunStatuses(sr, c)

		// A non-OK cell is route/protocol not-implemented (not_applicable),
		// an infra failure (dnf), or integrity-questionable data (suspect).
		// Record it in CellStatuses (schema v5.3). Cells with no data skip
		// every headline map so they are never ranked as a 0-RPS also-ran;
		// suspect cells fall through and keep their numbers next to the
		// flag (schema v5.4). An empty-string Status is treated as OK for
		// back-compat with callers that pre-date the classification (they
		// only ever hand OK cells anyway).
		if c.Status != "" && c.Status != CellOK {
			if sr.CellStatuses == nil {
				sr.CellStatuses = map[string]string{}
			}
			sr.CellStatuses[c.ScenarioName] = string(c.Status)
			if !c.Status.HasData() {
				continue
			}
		}

		sr.SaturationModeRPS[c.ScenarioName] = c.RPSMedian

		// Validity telemetry. Always surface when a run reported them
		// (LoadgenCPUP95 > 0 / SentVsHandledDeltaPct > 0) so a release
		// reader sees the loadgen-bottleneck and drop-rate signals
		// alongside the saturation number. Zero values are deliberately
		// omitted so a loadgen build that did not sample self-CPU does
		// not surface a misleading "0% loadgen CPU" headline.
		if c.LoadgenCPUP95 > 0 {
			sr.LoadgenCPUP95[c.ScenarioName] = c.LoadgenCPUP95
		}
		if c.SentVsHandledDeltaPct > 0 {
			sr.SentVsHandledDeltaPct[c.ScenarioName] = c.SentVsHandledDeltaPct
		}
		// Connect-class error split (additive within v5.4). Zero values
		// are omitted: a pre-ConnectErrors loadgen build must not surface
		// a misleading "0 connect errors" claim next to a real error
		// count.
		if c.ConnectErrors > 0 {
			if sr.ConnectErrors == nil {
				sr.ConnectErrors = map[string]uint64{}
			}
			sr.ConnectErrors[c.ScenarioName] = c.ConnectErrors
		}

		// LatencyAtSLO + RatedModeP99AtTargetRPS come from the real rated
		// (closed-loop, coordinated-omission-corrected) sweep when it ran
		// (probatorium#156). LatencyAtSLO[ms] is the max sustained target RPS
		// whose median P99 stayed under ms — a throughput-at-SLO number (bigger
		// is better), never a raw latency, which is what keeps the BenchSince
		// regression gate's sign correct. When rated mode was off, both leaves
		// stay empty so the gate sees no signal for this cell rather than a
		// synthetic one slid off the single saturation pass.
		if len(c.LatencyAtSLO) > 0 {
			slo := make(map[int]int, len(c.LatencyAtSLO))
			for ms, rps := range c.LatencyAtSLO {
				slo[ms] = rps
			}
			sr.LatencyAtSLO[c.ScenarioName] = slo
		}
		if p99, ok := ratedHeadlineP99(c.RatedP99ByTarget); ok {
			sr.RatedModeP99AtTargetRPS[c.ScenarioName] = p99
		}

		if c.MergedHistogramB64 != "" {
			sr.HdrHistogramB64[c.ScenarioName] = c.MergedHistogramB64
		}

		// Server-side resource aggregate (#154). Surfaced for every
		// data-bearing cell that captured an observer sidecar so the report
		// can rank adapters by CPU/RSS efficiency — the differentiator for
		// the NIC-bound large-payload cells where raw RPS converges at the
		// fabric ceiling. Nil for runs with no observer (in-process loopback)
		// so the field stays omitted there. The map is lazily allocated so a
		// resource-free run emits no empty "resources":{} object.
		if c.Resources != nil {
			if sr.Resources == nil {
				sr.Resources = map[string]*ResourceStats{}
			}
			sr.Resources[c.ScenarioName] = c.Resources
		}

		// Network-bound annotation (schema v5.5): a large-payload cell whose
		// achieved egress bandwidth sat at the fabric ceiling while the
		// loadgen still had CPU headroom is NIC-limited, not server-limited.
		// Its saturation RPS converges across every fast adapter and must not
		// be read as a ranking — the CPU efficiency in Resources is the real
		// signal. Detection fires only when the fabric's line rate is known
		// (the LAN; the Tailscale overlay reports 0 and flags nothing). The
		// v1.5.4 grid carries no by-design wire-bound row anymore (post-1m and
		// the 8k/16k/64k payload rows were removed), so this is purely runtime.
		if isNetworkBound(c.BytesMedian, c.LoadgenCPUP95, in.Environment.FabricLineRateBitsPerSec) {
			if sr.NetworkBound == nil {
				sr.NetworkBound = map[string]bool{}
			}
			sr.NetworkBound[c.ScenarioName] = true
		}
	}

	out := &Document{
		SchemaVersion:   SchemaVersion,
		HostArchPair:    in.HostArchPair,
		Environment:     in.Environment,
		BenchmarkConfig: in.BenchmarkConfig,
		Validation:      in.Validation,
		Soak:            in.Soak,
	}
	names := make([]string, 0, len(byAdapter))
	for k := range byAdapter {
		names = append(names, k)
	}
	sort.Strings(names)
	for _, n := range names {
		out.Benchmarks = append(out.Benchmarks, *byAdapter[n])
	}
	return out
}

const (
	// networkBoundBandwidthFraction is the share of the fabric line rate a
	// cell's achieved egress bandwidth must reach to be called network-bound.
	// 0.80 leaves headroom below the theoretical ceiling: the 2x10G LACP
	// fabric tops out near 18.8/20 Gbps in practice (per-flow hashing keeps a
	// single TCP flow on one 10G member), and only the large-payload cells
	// (64k/1m, ~18.8 Gbps) ever approach it — every small-response cell stays
	// orders of magnitude below, so there are no false positives.
	networkBoundBandwidthFraction = 0.80

	// networkBoundLoadgenCPUCeiling guards against mislabelling a
	// loadgen-bottlenecked cell as NIC-bound. LoadgenCPUP95 is a fraction of
	// one core; a value at/above this means the load generator itself was the
	// limit, so the cell's ceiling is a client artefact, not the fabric.
	// (Realistically a NIC-bound cell shows LOW loadgen CPU — the client is
	// blocked on the wire, not burning cycles.)
	networkBoundLoadgenCPUCeiling = 8.0
)

// isFanoutBound reports whether a scenario's throughput is paced by the
// server's fixed publish tick rather than by CPU. The hub-broadcast and
// SSE-fanout cells push to N subscribers on a 1 ms cadence, so their RPS
// ceiling is ~1000*N regardless of server headroom — a fan-out rate, not a
// throughput the field can be ranked by. Their real signal is delivery
// latency (the tail-latency section), so they are kept out of the headline
// ranking just like the wire-bound cells. The echo modes (ws-echo /
// ws-large-echo) are client-driven round-trips and stay ranked.
func isFanoutBound(scenarioName string) bool {
	switch scenarioName {
	case "ws-hub-broadcast-128", "ws-hub-broadcast-1024",
		"sse-fanout-128", "sse-fanout-1024":
		return true
	}
	return false
}

// isLatencyProbeByDesign reports whether a scenario is a single-connection
// latency probe whose saturation "RPS" is a latency reciprocal (1/RTT)
// rather than a throughput. At one connection requests serialize, so the
// number rewards low per-request latency, not throughput, and must never
// head a raw-RPS ranking — its real signal is the tail-latency section. The
// "-1c" suffix is the single-conn marker (scenarios.ProfileSingle);
// get-json-1c is the only such scenario today.
func isLatencyProbeByDesign(scenarioName string) bool {
	return strings.HasSuffix(scenarioName, "-1c")
}

// isNetworkBound reports whether a cell's achieved egress bandwidth sat at
// the fabric line rate (NIC-limited) rather than the server's CPU limit.
// bytesPerSec is the median across-runs throughput; loadgenCPUP95 is the
// loadgen self-CPU fraction; lineRateBits is the fabric ceiling in bits/sec
// (0 when unknown → never flagged).
func isNetworkBound(bytesPerSec, loadgenCPUP95 float64, lineRateBits int64) bool {
	if lineRateBits <= 0 || bytesPerSec <= 0 {
		return false
	}
	if loadgenCPUP95 >= networkBoundLoadgenCPUCeiling {
		return false
	}
	achievedBits := bytesPerSec * 8
	return achievedBits >= networkBoundBandwidthFraction*float64(lineRateBits)
}

// recordRunStatuses copies a cell's per-run outcome sequence into
// ServerResult.CellRunStatuses (schema v5.4) when at least one run was
// non-OK. All-OK cells are skipped so the field stays absent for the
// common case and v5.3-era output is byte-identical.
func recordRunStatuses(sr *ServerResult, c CellAggregate) {
	anyNonOK := false
	for _, st := range c.RunStatuses {
		if st != CellOK {
			anyNonOK = true
			break
		}
	}
	if !anyNonOK {
		return
	}
	if sr.CellRunStatuses == nil {
		sr.CellRunStatuses = map[string][]string{}
	}
	runs := make([]string, len(c.RunStatuses))
	for i, st := range c.RunStatuses {
		runs[i] = string(st)
	}
	sr.CellRunStatuses[c.ScenarioName] = runs
}

// ratedHeadlineP99 picks the canonical rated-pass P99 to surface in
// RatedModeP99AtTargetRPS: the P99 at the highest target RPS the sweep drove
// (the load closest to saturation, where tail behaviour matters most).
// Returns ok=false when the cell carries no rated data.
func ratedHeadlineP99(byTarget map[int]time.Duration) (time.Duration, bool) {
	best := -1
	for t := range byTarget {
		if t > best {
			best = t
		}
	}
	if best < 0 {
		return 0, false
	}
	return byTarget[best], true
}
