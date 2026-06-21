package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/servers"
)

// ConcurrencyProfile enumerates the per-target concurrency profiles the
// matrix sweeps: 1, 128, 256, 512, and 1024 connections. The 256/512
// mid-high points make the engine crossover visible — celeris's io_uring
// engine ties through ~256c and pulls ahead by 512c, peaking at 1024c;
// without them the sweep jumps 128c→1024c and hides the inflection.
const (
	ProfileSingle = "single-conn"
	ProfileMid    = "128-conn"
	ProfileMidHi  = "256-conn"
	ProfileHi512  = "512-conn"
	ProfileHigh   = "1024-conn"
)

// ConcurrencyScenario parameterises a static workload with one of the
// four concurrency profiles listed above.
type ConcurrencyScenario struct {
	name    string
	profile string

	// Method is the HTTP method ("GET" for every registered profile).
	Method string

	// Path is the request path ("/" or "/json").
	Path string

	// Connections is the TCP connection count passed to loadgen.
	Connections int
}

// NewConcurrencyScenario constructs a [ConcurrencyScenario]. Kept for
// test ergonomics and backward compatibility with the scaffold signature.
func NewConcurrencyScenario(name, profile string) *ConcurrencyScenario {
	return &ConcurrencyScenario{name: name, profile: profile}
}

// Name implements [Scenario].
func (s *ConcurrencyScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *ConcurrencyScenario) Category() string { return CategoryConcurrency }

// Profile returns the concurrency profile identifier (one of [ProfileSingle],
// [ProfileMid], [ProfileMidHi], [ProfileHi512], [ProfileHigh]).
func (s *ConcurrencyScenario) Profile() string { return s.profile }

// Workload returns the loadgen.Config for this scenario. The orchestrator
// overwrites Duration and Warmup after calling Workload so CLI flags
// (-duration / -warmup) win; leaving them at zero here is intentional.
func (s *ConcurrencyScenario) Workload(target string) loadgen.Config {
	conns := s.Connections
	if conns <= 0 {
		conns = 1
	}
	method := s.Method
	if method == "" {
		method = "GET"
	}
	cfg := loadgen.Config{
		URL:         target + s.Path,
		Method:      method,
		Connections: conns,
	}
	return cfg
}

// Applicable: every concurrency profile drives plain H1 on the wire, so a
// column is in scope iff it speaks HTTP/1.1. H2C-prior-knowledge-only
// servers (h2c-noupg) are excluded — they would silently record 0 RPS.
func (s *ConcurrencyScenario) Applicable(fs servers.FeatureSet) bool {
	return fs.HTTP1
}

// Compile-time assertion that ConcurrencyScenario satisfies Scenario.
var _ Scenario = (*ConcurrencyScenario)(nil)

// ConcurrencyProfiles is the canonical ordered list of profiles.
var ConcurrencyProfiles = []string{
	ProfileSingle,
	ProfileMid,
	ProfileMidHi,
	ProfileHi512,
	ProfileHigh,
}

func init() {
	Register(&ConcurrencyScenario{
		name:        "get-json-1c",
		profile:     ProfileSingle,
		Method:      "GET",
		Path:        "/json",
		Connections: 1,
	})
	Register(&ConcurrencyScenario{
		name:        "get-simple-128c",
		profile:     ProfileMid,
		Method:      "GET",
		Path:        "/",
		Connections: 128,
	})
	Register(&ConcurrencyScenario{
		name:        "get-simple-256c",
		profile:     ProfileMidHi,
		Method:      "GET",
		Path:        "/",
		Connections: 256,
	})
	Register(&ConcurrencyScenario{
		name:        "get-simple-512c",
		profile:     ProfileHi512,
		Method:      "GET",
		Path:        "/",
		Connections: 512,
	})
	Register(&ConcurrencyScenario{
		name:        "get-simple-1024c",
		profile:     ProfileHigh,
		Method:      "GET",
		Path:        "/",
		Connections: 1024,
	})
}
