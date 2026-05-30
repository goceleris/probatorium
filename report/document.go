package report

import (
	"sort"
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

		sr.SaturationModeRPS[c.ScenarioName] = c.RPSMedian

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
