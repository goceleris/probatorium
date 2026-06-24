// Package scenarios enumerates every benchable workload (cell-row in the
// matrix). Each Scenario knows how to configure loadgen and reports which
// FeatureSet it requires so the scheduler can skip incompatible (server,
// scenario) pairs.
package scenarios

import (
	"sort"
	"sync"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/servers"
)

// Category groups scenarios for report sections.
const (
	CategoryStatic      = "static"
	CategoryConcurrency = "concurrency"
	CategoryChain       = "chain"
	CategoryDriver      = "driver"
	CategoryWS          = "ws"
	CategorySSE         = "sse"
	CategoryTLS         = "tls"
)

// DefaultErrorBudget is the loadgen error-ratio ceiling — errors /
// (errors + requests) — a completed cell may reach before the runner
// flags it "suspect" (data kept, integrity questionable; schema v5.4).
// 5% tolerates warmup blips and the odd dropped keep-alive without
// letting an error storm publish as a clean number.
const DefaultErrorBudget = 0.05

// ErrorBudgeter is an optional [Scenario] facet: a scenario whose
// workload legitimately produces loadgen-side errors above
// [DefaultErrorBudget] (e.g. connection churn, where refused dials are
// part of what is being measured) implements it to declare its own
// ceiling. Resolved via [ErrorBudgetFor].
type ErrorBudgeter interface {
	// ErrorBudget returns the error-ratio ceiling in (0,1].
	ErrorBudget() float64
}

// ErrorBudgetFor returns s's declared error budget, falling back to
// [DefaultErrorBudget] for scenarios that do not implement
// [ErrorBudgeter] (or declare a non-positive budget).
func ErrorBudgetFor(s Scenario) float64 {
	if eb, ok := s.(ErrorBudgeter); ok {
		if b := eb.ErrorBudget(); b > 0 {
			return b
		}
	}
	return DefaultErrorBudget
}

// Scenario is one benchable workload — it knows how to configure loadgen
// and how to interpret the result.
type Scenario interface {
	// Name is the canonical cell-row identifier. Examples: "get-json",
	// "post-4k", "get-json-1c", "driver-pg-read", "ws-hub-broadcast-128".
	Name() string

	// Category groups scenarios for report sections: "static",
	// "concurrency", "chain", "driver".
	Category() string

	// Workload returns the loadgen.Config for this scenario. Duration and
	// Warmup are filled in by the orchestrator from -duration / -warmup
	// flags.
	Workload(target string) loadgen.Config

	// Applicable returns true if this scenario is runnable against the
	// given FeatureSet. The orchestrator skips mismatched
	// (server, scenario) pairs.
	Applicable(servers.FeatureSet) bool
}

var (
	registryMu sync.RWMutex
	registry   = make(map[string]Scenario)
)

// Register adds s to the scenarios registry. Duplicate names panic.
// Safe for concurrent use; normally called from package init().
func Register(s Scenario) {
	if s == nil {
		panic("probatorium/scenarios: Register called with nil Scenario")
	}
	name := s.Name()
	if name == "" {
		panic("probatorium/scenarios: Register called with empty Name")
	}
	registryMu.Lock()
	defer registryMu.Unlock()
	if _, exists := registry[name]; exists {
		panic("probatorium/scenarios: duplicate Scenario name " + name)
	}
	registry[name] = s
}

// Registry returns every registered Scenario sorted by Name. The slice is
// a freshly allocated copy, safe to mutate.
func Registry() []Scenario {
	registryMu.RLock()
	defer registryMu.RUnlock()
	out := make([]Scenario, 0, len(registry))
	for _, s := range registry {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// Reset clears the registry. Intended for tests only.
func Reset() {
	registryMu.Lock()
	defer registryMu.Unlock()
	registry = make(map[string]Scenario)
}
