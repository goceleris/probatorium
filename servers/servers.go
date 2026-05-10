// Package servers is the registry of every adapter (server-under-test) the
// probatorium orchestrator can build, deploy, and benchmark.
//
// Each adapter is a (Name, Category, Language, Framework, Engine) tuple
// plus a [BuildSpec] describing how to produce the runnable binary.
// Wave 2c registers only [GoBinary] adapters (Go stdlib + 8 competitor
// frameworks + 4 celeris engine modes). Native (Rust / Bun / Python /
// JVM) adapters land in wave 4 via [NativeBinary].
//
// The package also owns [FeatureSet], a small set of capability flags
// every scenario interrogates via Scenario.Applicable so mismatched
// (server, scenario) pairs are skipped before the orchestrator dials
// loadgen — preventing silent 0-RPS cells when, for example, an H2-only
// fixture is paired with an H1-only server.
package servers

import (
	"sort"
	"sync"
)

// FeatureSet advertises which scenario categories an adapter can host on
// the wire. The scheduler skips (server, scenario) pairs whose Scenario.
// Applicable returns false against the server's FeatureSet so silent
// 0-RPS cells never show up in the report.
//
// Capability semantics:
//
//   - HTTP1         — accepts plain HTTP/1.1 on the listener.
//   - HTTP2C        — accepts HTTP/2 cleartext via prior knowledge.
//     Includes h2c-noupg, which refuses H1 entirely.
//   - Auto          — adaptive engine that auto-switches between HTTP/1.1
//     and HTTP/2 depending on the wire protocol observed.
//   - H2CUpgrade    — accepts the HTTP/1.1 → H2C Upgrade handshake.
//   - Drivers       — can host the driver-* scenarios (PG / Redis /
//     memcached round-trips behind /db, /cache, /mc, /session).
//   - Middleware    — can host the chain-* scenarios (the 4 stacks
//     mounted under /chain/<chain>/).
//   - AsyncHandlers — celeris-specific. The async handler dispatcher is
//     enabled. Affects only celeris cell-columns.
type FeatureSet struct {
	HTTP1         bool
	HTTP2C        bool
	Auto          bool
	H2CUpgrade    bool
	Drivers       bool
	Middleware    bool
	AsyncHandlers bool
}

// BuildSpec is the build recipe for one adapter. Every concrete value
// answers Kind() with a stable string the orchestrator switches on
// ("go" or "native") so the adapter table stays homogeneous.
type BuildSpec interface {
	Kind() string
}

// GoBinary is the BuildSpec for a Go-module-based adapter. Each
// servers/<framework>/ subdirectory carries its own go.mod with a local
// `replace github.com/goceleris/probatorium => ../..` so the build
// graph never depends on the parent module's transitive deps.
//
// ModuleDir is the path to the Go module relative to the probatorium
// repo root (e.g. "servers/gin"). BuildTags are appended to `go build
// -tags` verbatim. Env entries (KEY=VALUE) are layered on top of the
// orchestrator's environment so cross-compile knobs (GOOS, GOARCH,
// GOAMD64, GOARM64) can be set per adapter.
type GoBinary struct {
	ModuleDir string
	BuildTags []string
	Env       []string
}

// Kind reports the BuildSpec discriminant ("go").
func (GoBinary) Kind() string { return "go" }

// NativeBinary is the BuildSpec for a non-Go adapter (Rust / Bun /
// Python / JVM). Wave 2c does not register any NativeBinary entries —
// they land in wave 4 once cross-compile and ansible staging support
// the toolchains. Kept here so the registry shape is stable across
// waves.
type NativeBinary struct {
	Lang       string
	BuildSteps []string
	RunCmd     string
}

// Kind reports the BuildSpec discriminant ("native").
func (NativeBinary) Kind() string { return "native" }

// Adapter is one cell-column in the matrix: the (server, engine) pair
// the scheduler walks against every applicable scenario.
//
// Name is the canonical identifier — stable across runs, used as a
// filename component in results, and emitted verbatim into the v5.0
// schema. Category is the language family ("go-net-http", "go-fasthttp",
// "celeris", "rust-axum", ...). Language and Framework spell out the
// two facets independently for the markdown report. Engine is the
// celeris-specific runtime mode ("iouring-h1-async",
// "iouring-auto+upg-async", "epoll-h1-sync", "std-h1") and is empty
// for non-celeris adapters.
type Adapter struct {
	Name      string
	Category  string
	Language  string
	Framework string
	Engine    string
	Bin       BuildSpec
}

// Registry is the set of every adapter known to probatorium, keyed by
// Adapter.Name. Wave 2c populates the 8 Go competitor frameworks plus
// the 4 celeris engine modes. Wave 4 adds Rust / Bun / Python /
// other-language adapters via NativeBinary — those entries land in a
// sibling registry file rather than mutating this one.
var Registry = map[string]Adapter{
	// stdhttp baseline — 3 modes (H1, H2C-only, hybrid). Treated as
	// three distinct cell-columns because performance differs by mode.
	"stdhttp-h1": {
		Name:      "stdhttp-h1",
		Category:  "go-net-http",
		Language:  "go",
		Framework: "stdhttp",
		Engine:    "h1",
		Bin:       GoBinary{ModuleDir: "servers/stdhttp"},
	},
	"stdhttp-h2": {
		Name:      "stdhttp-h2",
		Category:  "go-net-http",
		Language:  "go",
		Framework: "stdhttp",
		Engine:    "h2c",
		Bin:       GoBinary{ModuleDir: "servers/stdhttp"},
	},
	"stdhttp-hybrid": {
		Name:      "stdhttp-hybrid",
		Category:  "go-net-http",
		Language:  "go",
		Framework: "stdhttp",
		Engine:    "hybrid",
		Bin:       GoBinary{ModuleDir: "servers/stdhttp"},
	},

	// gin / echo / chi / iris — net/http-based routers. Each carries an
	// h1 and an h2c (h2c.NewHandler-wrapped) variant.
	"gin-h1": {
		Name: "gin-h1", Category: "go-net-http", Language: "go", Framework: "gin", Engine: "h1",
		Bin: GoBinary{ModuleDir: "servers/gin"},
	},
	"gin-h2": {
		Name: "gin-h2", Category: "go-net-http", Language: "go", Framework: "gin", Engine: "h2c",
		Bin: GoBinary{ModuleDir: "servers/gin"},
	},
	"echo-h1": {
		Name: "echo-h1", Category: "go-net-http", Language: "go", Framework: "echo", Engine: "h1",
		Bin: GoBinary{ModuleDir: "servers/echo"},
	},
	"echo-h2": {
		Name: "echo-h2", Category: "go-net-http", Language: "go", Framework: "echo", Engine: "h2c",
		Bin: GoBinary{ModuleDir: "servers/echo"},
	},
	"chi-h1": {
		Name: "chi-h1", Category: "go-net-http", Language: "go", Framework: "chi", Engine: "h1",
		Bin: GoBinary{ModuleDir: "servers/chi"},
	},
	"chi-h2": {
		Name: "chi-h2", Category: "go-net-http", Language: "go", Framework: "chi", Engine: "h2c",
		Bin: GoBinary{ModuleDir: "servers/chi"},
	},
	"iris-h1": {
		Name: "iris-h1", Category: "go-net-http", Language: "go", Framework: "iris", Engine: "h1",
		Bin: GoBinary{ModuleDir: "servers/iris"},
	},
	"iris-h2": {
		Name: "iris-h2", Category: "go-net-http", Language: "go", Framework: "iris", Engine: "h2c",
		Bin: GoBinary{ModuleDir: "servers/iris"},
	},

	// hertz — netpoll-backed. H1 + native H2 via hertz-contrib.
	"hertz-h1": {
		Name: "hertz-h1", Category: "go-netpoll", Language: "go", Framework: "hertz", Engine: "h1",
		Bin: GoBinary{ModuleDir: "servers/hertz"},
	},
	"hertz-h2": {
		Name: "hertz-h2", Category: "go-netpoll", Language: "go", Framework: "hertz", Engine: "h2c",
		Bin: GoBinary{ModuleDir: "servers/hertz"},
	},

	// fasthttp + fiber — H1-only. fiber wraps fasthttp.
	"fasthttp-h1": {
		Name: "fasthttp-h1", Category: "go-fasthttp", Language: "go", Framework: "fasthttp", Engine: "h1",
		Bin: GoBinary{ModuleDir: "servers/fasthttp"},
	},
	"fiber-h1": {
		Name: "fiber-h1", Category: "go-fasthttp", Language: "go", Framework: "fiber", Engine: "h1",
		Bin: GoBinary{ModuleDir: "servers/fiber"},
	},

	// celeris — 4 engine modes selected at runtime via -engine. The
	// binary is the same; entries differ only in Engine + Name.
	"celeris-iouring-h1-async": {
		Name: "celeris-iouring-h1-async", Category: "celeris", Language: "go", Framework: "celeris",
		Engine: "iouring-h1-async",
		Bin:    GoBinary{ModuleDir: "servers/celeris"},
	},
	"celeris-iouring-auto+upg-async": {
		Name: "celeris-iouring-auto+upg-async", Category: "celeris", Language: "go", Framework: "celeris",
		Engine: "iouring-auto+upg-async",
		Bin:    GoBinary{ModuleDir: "servers/celeris"},
	},
	"celeris-epoll-h1-sync": {
		Name: "celeris-epoll-h1-sync", Category: "celeris", Language: "go", Framework: "celeris",
		Engine: "epoll-h1-sync",
		Bin:    GoBinary{ModuleDir: "servers/celeris"},
	},
	"celeris-std-h1": {
		Name: "celeris-std-h1", Category: "celeris", Language: "go", Framework: "celeris",
		Engine: "std-h1",
		Bin:    GoBinary{ModuleDir: "servers/celeris"},
	},
}

// Names returns every registered adapter name in stable sorted order.
// The returned slice is freshly allocated and safe to mutate.
func Names() []string {
	out := make([]string, 0, len(Registry))
	for k := range Registry {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// ====================================================================
// In-process Server interface (used by perfmatrix-style harnesses).
// ====================================================================
//
// The Server interface and its sister Registry below are kept for
// backward compatibility with the perfmatrix scenario fixtures vendored
// in wave 2c. probatorium itself drives binaries (not in-process server
// types) via the [Adapter] table above; the Server interface is used by
// in-process tests and by the schedule-applicability machinery shared
// with perfmatrix.

// Server is one in-process benched server instance. Each registered
// Server corresponds to one cell-column in an in-process matrix run.
//
// Wave 2c does not register any Server values — the binaries-driven
// orchestrator uses [Adapter] instead. The interface stays so the
// perfmatrix scenarios package, which type-asserts against
// servers.Server in tests, continues to compile.
type Server interface {
	Name() string
	Kind() string
	Features() FeatureSet
}

var (
	srvMu  sync.RWMutex
	srvReg = make(map[string]Server)
)

// RegisterServer adds s to the in-process registry. Duplicate names
// panic.
func RegisterServer(s Server) {
	if s == nil {
		panic("probatorium/servers: RegisterServer called with nil Server")
	}
	name := s.Name()
	if name == "" {
		panic("probatorium/servers: RegisterServer called with empty Name")
	}
	srvMu.Lock()
	defer srvMu.Unlock()
	if _, exists := srvReg[name]; exists {
		panic("probatorium/servers: duplicate Server name " + name)
	}
	srvReg[name] = s
}

// ServerRegistry returns every registered Server sorted by Name.
func ServerRegistry() []Server {
	srvMu.RLock()
	defer srvMu.RUnlock()
	out := make([]Server, 0, len(srvReg))
	for _, s := range srvReg {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out
}

// ResetServers clears the in-process Server registry. Tests only.
func ResetServers() {
	srvMu.Lock()
	defer srvMu.Unlock()
	srvReg = make(map[string]Server)
}
