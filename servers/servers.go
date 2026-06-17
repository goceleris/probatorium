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
//   - WS            — can host the streaming WebSocket scenarios (ws-echo,
//     ws-large-echo, ws-hub-broadcast-N) under /ws.
//   - SSE           — can host the streaming Server-Sent-Events scenarios
//     (sse-fanout-N) under /events.
//   - TLS           — reachable over HTTPS for the tls-* scenarios. In
//     Phase 2 every adapter is benched behind a shared TLS terminator (see
//     scenarios/tls.go), so this flag tracks reachability through that
//     terminator rather than native in-process TLS — which celeris lacks
//     entirely.
//   - AsyncHandlers — celeris-specific. The async handler dispatcher is
//     enabled. Affects only celeris cell-columns.
type FeatureSet struct {
	HTTP1         bool
	HTTP2C        bool
	Auto          bool
	H2CUpgrade    bool
	Drivers       bool
	Middleware    bool
	WS            bool
	SSE           bool
	TLS           bool
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

	// BinName, when set, is the staged-binary slug this column runs from,
	// overriding the column's own Name. It lets several columns share ONE
	// native build that switches mode on -engine — the native analogue of
	// the Go adapters where gin-h1 and gin-h2 both run servers/gin's binary.
	// An h2c column ("<framework>-h2", engine h2c-noupg) sets BinName to the
	// framework slug so it reuses the h1 column's competitors/<framework>
	// binary instead of demanding a non-existent competitors/<framework>-h2.
	// Empty means "run from competitors/<Name>" (the default, one build per
	// column).
	BinName string
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

	// Capabilities is the declared Phase-2 capability manifest for this
	// adapter — the single source of truth the scheduler trusts instead of
	// guessing driver / middleware / streaming support from the Category
	// or Engine name. featureSetFor projects these flags into the
	// [FeatureSet] so a scenario is only ever scheduled against an adapter
	// that actually declares the matching class; a declared-but-unserved
	// route then surfaces as a hard error at run time (the runner's cell
	// guard) rather than a silent 0-RPS / all-404 cell. Wire-protocol
	// facets (HTTP1 / HTTP2C / …) stay engine-derived and are NOT part of
	// this manifest.
	Capabilities Capabilities
}

// Capabilities is an adapter's declared Phase-2 capability manifest. Each
// flag asserts the adapter mounts the routes for that scenario class:
//
//   - Static     — the 6 canonical [common.Endpoints]. Every adapter sets
//     this; it is the baseline contract.
//   - Drivers    — the driver-* routes (/db, /cache, /mc, /session).
//   - Middleware — the chain-* routes (/chain/<stack>/{json,upload}).
//   - WS         — the streaming WebSocket routes under /ws.
//   - SSE        — the streaming Server-Sent-Events routes under /events.
//   - TLS        — reachable for the tls-* scenarios (in Phase 2, via the
//     shared terminator). Left false until the terminator infra lands so
//     no TLS cell is scheduled prematurely.
//
// The manifest is intentionally declarative: it is the claim. The runtime
// cell guard is the proof — the two must agree or the bench fails loudly.
type Capabilities struct {
	Static     bool
	Drivers    bool
	Middleware bool
	WS         bool
	SSE        bool
	TLS        bool
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
		Name:         "stdhttp-h1",
		Category:     "go-net-http",
		Language:     "go",
		Framework:    "stdhttp",
		Engine:       "h1",
		Bin:          GoBinary{ModuleDir: "servers/stdhttp"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"stdhttp-h2": {
		Name:         "stdhttp-h2",
		Category:     "go-net-http",
		Language:     "go",
		Framework:    "stdhttp",
		Engine:       "h2c-noupg",
		Bin:          GoBinary{ModuleDir: "servers/stdhttp"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"stdhttp-hybrid": {
		Name:         "stdhttp-hybrid",
		Category:     "go-net-http",
		Language:     "go",
		Framework:    "stdhttp",
		Engine:       "hybrid",
		Bin:          GoBinary{ModuleDir: "servers/stdhttp"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},

	// gorilla_ws — the WS/SSE reference adapter. A net/http server whose
	// streaming surface is hand-rolled on gorilla/websocket (RWMutex
	// connection-set broadcast) plus a flusher-based SSE broker — the naive
	// baseline the celeris middleware/websocket Hub and middleware/sse Broker
	// are designed to replace. It serves the static contract too so it is a
	// fully valid adapter, but its purpose is the WS/SSE comparison: this is
	// the column the matrix pairs against celeris's streaming cells. H1-only
	// (the WS upgrade + SSE long-poll both ride HTTP/1.1); no driver /
	// middleware-chain support. TLS is reachable via the shared terminator
	// like every other adapter.
	"gorilla_ws": {
		Name: "gorilla_ws", Category: "go-net-http", Language: "go", Framework: "gorilla", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/gorilla_ws"},
		Capabilities: Capabilities{Static: true, WS: true, SSE: true, TLS: true},
	},

	// gin / echo / chi / iris — net/http-based routers. Each carries an
	// h1 and an h2c (h2c.NewHandler-wrapped) variant.
	"gin-h1": {
		Name: "gin-h1", Category: "go-net-http", Language: "go", Framework: "gin", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/gin"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"gin-h2": {
		Name: "gin-h2", Category: "go-net-http", Language: "go", Framework: "gin", Engine: "h2c",
		Bin:          GoBinary{ModuleDir: "servers/gin"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"echo-h1": {
		Name: "echo-h1", Category: "go-net-http", Language: "go", Framework: "echo", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/echo"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"echo-h2": {
		Name: "echo-h2", Category: "go-net-http", Language: "go", Framework: "echo", Engine: "h2c",
		Bin:          GoBinary{ModuleDir: "servers/echo"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"chi-h1": {
		Name: "chi-h1", Category: "go-net-http", Language: "go", Framework: "chi", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/chi"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"chi-h2": {
		Name: "chi-h2", Category: "go-net-http", Language: "go", Framework: "chi", Engine: "h2c",
		Bin:          GoBinary{ModuleDir: "servers/chi"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"iris-h1": {
		Name: "iris-h1", Category: "go-net-http", Language: "go", Framework: "iris", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/iris"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"iris-h2": {
		Name: "iris-h2", Category: "go-net-http", Language: "go", Framework: "iris", Engine: "h2c",
		Bin:          GoBinary{ModuleDir: "servers/iris"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},

	// hertz — netpoll-backed. H1 + native H2 via hertz-contrib.
	"hertz-h1": {
		Name: "hertz-h1", Category: "go-netpoll", Language: "go", Framework: "hertz", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/hertz"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"hertz-h2": {
		Name: "hertz-h2", Category: "go-netpoll", Language: "go", Framework: "hertz", Engine: "h2c",
		Bin:          GoBinary{ModuleDir: "servers/hertz"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},

	// gnet — the canonical Go event-loop server (panjf2000/gnet), the closest
	// architectural peer to celeris's epoll engine: a fixed pool of event loops
	// over epoll/kqueue parsing HTTP off a zero-copy inbound ring buffer. No
	// HTTP codec ships with gnet, so the adapter carries a minimal HTTP/1.1
	// framer and serves only the six static contract endpoints — hence the
	// Static-only capability manifest (no Drivers / Middleware / WS / SSE /
	// TLS). H1-only.
	"gnet-h1": {
		Name: "gnet-h1", Category: "go-gnet", Language: "go", Framework: "gnet", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/gnet"},
		Capabilities: Capabilities{Static: true},
	},

	// fasthttp + fiber — H1-only. fiber wraps fasthttp.
	"fasthttp-h1": {
		Name: "fasthttp-h1", Category: "go-fasthttp", Language: "go", Framework: "fasthttp", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/fasthttp"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},
	"fiber-h1": {
		Name: "fiber-h1", Category: "go-fasthttp", Language: "go", Framework: "fiber", Engine: "h1",
		Bin:          GoBinary{ModuleDir: "servers/fiber"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true},
	},

	// rust adapters (wave 4a) — three frameworks built natively on the
	// bench host by ansible/roles/rust + ansible/tasks/build_native_
	// competitor.yml. The build pushes a tarball of servers/<name>/ to
	// the cluster, runs `cargo build --profile release-fat` with
	// RUSTFLAGS="-C target-cpu=native", and symlinks the produced
	// release-fat binary into ${bench_root}/competitors/<name>. The
	// runner invokes that symlink with `-bind <addr>` and waits for
	// `ready addr=<addr>` on stdout. SIGTERM triggers graceful shutdown
	// inside servers/start.go's 5-second grace window. Each now carries an
	// h1 column (below) and a prior-knowledge h2c column (-h2): hyper, ntex,
	// and axum-on-hyper all serve cleartext h2c, selected via -engine h2c.
	"axum": {
		Name: "axum", Category: "rust-tower", Language: "rust", Framework: "axum", Engine: "h1",
		Bin: NativeBinary{
			Lang: "rust",
			BuildSteps: []string{
				"source $RUSTUP_HOME/env",
				"cd $SRC && cargo build --profile release-fat",
			},
			RunCmd: "{bin} -bind {bind}",
		},
		Capabilities: Capabilities{Static: true},
	},
	// axum-h2 — the same servers/axum binary in prior-knowledge h2c mode
	// (-engine h2c serves HTTP/2 cleartext only, refusing H1 like
	// stdhttp-h2). hyper, which axum sits on, speaks h2c natively. Shares
	// the competitors/axum build via Bin.BinName so the deploy builds axum
	// once and this column reuses it. Engine "h2c-noupg" → featureSetFor
	// gives HTTP2C=true / HTTP1=false, so only the H2 scenarios schedule
	// here (the H1 grid stays on the axum column).
	"axum-h2": {
		Name: "axum-h2", Category: "rust-tower", Language: "rust", Framework: "axum", Engine: "h2c-noupg",
		Bin: NativeBinary{
			Lang: "rust",
			BuildSteps: []string{
				"source $RUSTUP_HOME/env",
				"cd $SRC && cargo build --profile release-fat",
			},
			RunCmd:  "{bin} -bind {bind}",
			BinName: "axum",
		},
		Capabilities: Capabilities{Static: true},
	},
	"ntex": {
		Name: "ntex", Category: "rust-ntex", Language: "rust", Framework: "ntex", Engine: "h1",
		Bin: NativeBinary{
			Lang: "rust",
			BuildSteps: []string{
				"source $RUSTUP_HOME/env",
				"cd $SRC && cargo build --profile release-fat",
			},
			RunCmd: "{bin} -bind {bind}",
		},
		Capabilities: Capabilities{Static: true},
	},
	// ntex-h2 — prior-knowledge h2c via ntex's native HTTP/2 codec. Shares
	// the competitors/ntex build (see axum-h2).
	"ntex-h2": {
		Name: "ntex-h2", Category: "rust-ntex", Language: "rust", Framework: "ntex", Engine: "h2c-noupg",
		Bin: NativeBinary{
			Lang: "rust",
			BuildSteps: []string{
				"source $RUSTUP_HOME/env",
				"cd $SRC && cargo build --profile release-fat",
			},
			RunCmd:  "{bin} -bind {bind}",
			BinName: "ntex",
		},
		Capabilities: Capabilities{Static: true},
	},

	// hyper — the raw Rust baseline axum / ntex are all built
	// on or measured against. No router crate, no tower stack: the adapter
	// drives hyper's H1 server directly with a hand-rolled (method, path)
	// match, so this column is the floor the framework columns add their
	// abstraction cost on top of. Same wave-4a build/run/lifecycle contract
	// as the other three Rust adapters (release-fat + target-cpu=native,
	// `-bind`, `ready addr=` on stdout, SIGTERM graceful drain). H1-only.
	"hyper": {
		Name: "hyper", Category: "rust-hyper", Language: "rust", Framework: "hyper", Engine: "h1",
		Bin: NativeBinary{
			Lang: "rust",
			BuildSteps: []string{
				"source $RUSTUP_HOME/env",
				"cd $SRC && cargo build --profile release-fat",
			},
			RunCmd: "{bin} -bind {bind}",
		},
		Capabilities: Capabilities{Static: true},
	},
	// hyper-h2 — raw hyper's http2 server builder serves prior-knowledge
	// h2c directly. Shares the competitors/hyper build (see axum-h2).
	"hyper-h2": {
		Name: "hyper-h2", Category: "rust-hyper", Language: "rust", Framework: "hyper", Engine: "h2c-noupg",
		Bin: NativeBinary{
			Lang: "rust",
			BuildSteps: []string{
				"source $RUSTUP_HOME/env",
				"cd $SRC && cargo build --profile release-fat",
			},
			RunCmd:  "{bin} -bind {bind}",
			BinName: "hyper",
		},
		Capabilities: Capabilities{Static: true},
	},

	// drogon — the top C++ contender. Built natively on the bench host via
	// CMake against libdrogon (drogon + trantor + jsoncpp + OpenSSL), the
	// same staging shape as the Rust competitors: a tarball of
	// servers/drogon/ lands at $SRC, configures + compiles in-tree, and the
	// produced build/drogon-adapter binary is symlinked into
	// ${bench_root}/competitors/drogon. The runner invokes it with
	// `-bind <addr>` and waits for `ready addr=<addr>` on stdout.
	// CMAKE_PREFIX_PATH points at the brew Drogon CMake package so the
	// Drogon::Drogon imported target resolves every transitive dep.
	// Capabilities are all-false: drogon is benched on the static +
	// concurrency scenarios only, so driver / middleware / streaming cells
	// are never scheduled against it.
	"drogon": {
		Name: "drogon", Category: "cpp-drogon", Language: "cpp", Framework: "drogon", Engine: "h1",
		Bin: NativeBinary{
			Lang: "cpp",
			BuildSteps: []string{
				"cd $SRC && cmake -S . -B build -DCMAKE_PREFIX_PATH=/opt/homebrew -DCMAKE_BUILD_TYPE=Release",
				"cd $SRC && cmake --build build -j",
			},
			RunCmd: "{bin} -bind {bind}",
		},
		Capabilities: Capabilities{},
	},
	// drogon has NO h2c column: drogon 1.9.x exposes no server-side HTTP/2
	// at all (HttpTypes.h Version is kHttp10/kHttp11 only; addListener has no
	// protocol/h2c flag). Cleartext h2c prior-knowledge is impossible, so the
	// adapter's -engine h2c fails fast and we deliberately register NO
	// drogon-h2 column rather than ship a guaranteed-DNF cell. drogon stays
	// H1-only; its N/A on the h2 scenarios is correct, not a gap.

	// fastapi — python adapter, native (NO docker). The launcher script
	// at {bench}/competitors/fastapi/server is rendered by the python
	// ansible role's build_competitor.yml; it activates the per-adapter
	// venv and execs uvicorn with uvloop+httptools and one worker per
	// CPU. Blessed FastAPI fast-path: async def handlers +
	// ORJSONResponse default class. See servers/fastapi/pyproject.toml
	// for the always-latest dep set (no upper pins).
	"fastapi": {
		Name: "fastapi", Category: "python-fastapi", Language: "python", Framework: "fastapi", Engine: "h1",
		Bin: NativeBinary{
			Lang:   "python",
			RunCmd: "{bench}/competitors/{name}/server -bind {bind}",
		},
		Capabilities: Capabilities{Static: true},
	},
	// fastapi-h2 — prior-knowledge h2c via hypercorn (the h1 column stays on
	// uvicorn, the tuned fast path). uvicorn has no HTTP/2; hypercorn serves
	// cleartext h2c prior-knowledge with no TLS — the H11 reader swaps to the
	// H2 protocol on seeing the "PRI * HTTP/2.0" preface. The launcher
	// dispatches -engine h2c → hypercorn. Shares the competitors/fastapi
	// launcher via Bin.BinName (see axum-h2).
	"fastapi-h2": {
		Name: "fastapi-h2", Category: "python-fastapi", Language: "python", Framework: "fastapi", Engine: "h2c-noupg",
		Bin: NativeBinary{
			Lang:    "python",
			RunCmd:  "{bench}/competitors/{name}/server -bind {bind}",
			BinName: "fastapi",
		},
		Capabilities: Capabilities{Static: true},
	},

	// aspnet — ASP.NET Core (Kestrel, minimal APIs) on .NET 10, built
	// natively on the bench host like the Rust adapters. `dotnet publish
	// -c Release` emits a framework-dependent app under {src}/publish/
	// whose native apphost binary is named `aspnet` (AssemblyName); the
	// build symlinks that apphost into {bench}/competitors/aspnet, and the
	// runner invokes it with `-bind <addr>`, waiting for `ready addr=<addr>`
	// on stdout. Kestrel is tuned for throughput in Program.cs (server GC +
	// concurrent, tiered PGO, ReadyToRun, no logging providers, no dev
	// middleware, AddServerHeader=false). The JSON payloads come from the
	// same deterministic generator as every other adapter so /json-1k and
	// /json-64k are byte-identical across languages. SIGTERM triggers
	// Kestrel's graceful shutdown inside the runner's grace window. Carries
	// both an h1 column and an aspnet-h2 prior-knowledge h2c column (Kestrel
	// HttpProtocols.Http2, selected via -engine h2c).
	"aspnet": {
		Name: "aspnet", Category: "dotnet-aspnetcore", Language: "csharp", Framework: "aspnet", Engine: "h1",
		Bin: NativeBinary{
			Lang: "dotnet",
			BuildSteps: []string{
				"cd $SRC && dotnet publish -c Release -o publish",
			},
			RunCmd: "{bin} -bind {bind}",
		},
		Capabilities: Capabilities{Static: true},
	},
	// aspnet-h2 — Kestrel serves prior-knowledge h2c when the endpoint's
	// HttpProtocols is Http2; the -engine h2c mode selects it. Shares the
	// competitors/aspnet build (see axum-h2).
	"aspnet-h2": {
		Name: "aspnet-h2", Category: "dotnet-aspnetcore", Language: "csharp", Framework: "aspnet", Engine: "h2c-noupg",
		Bin: NativeBinary{
			Lang: "dotnet",
			BuildSteps: []string{
				"cd $SRC && dotnet publish -c Release -o publish",
			},
			RunCmd:  "{bin} -bind {bind}",
			BinName: "aspnet",
		},
		Capabilities: Capabilities{Static: true},
	},

	// zig_zap — REMOVED from the registry on 2026-06-11. The Zig 0.16
	// std.http.Server's accept loop is single-listener / single-thread
	// (Zig 0.16's IpAddress.listen has no SO_REUSEPORT primitive — the
	// old `reuse_address: true` workaround doesn't apply to
	// ephemeral-port sharing in the v3.8 build of std). At the bench's
	// default Workers=64, the second cell (get-json, keep-alive with
	// 64 simultaneous dials) deadlocked loadgen.New's h1client dial
	// burst and the runner was killed by the bench's SIGKILL timeout
	// (8m56s). The cell came back as a misclassified not_applicable
	// ("zero-request cell: errors=0 duration=318µs"). Manual test:
	// zig_zap handles 8 / 16 / 32 concurrent conns cleanly, but 64
	// simultaneous dials overflow the kernel accept queue
	// (LISTEN 48 active, 128 in backlog; the 65th-and-up dials block
	// on accept). With the v3.7-era stdlib going multi-listener is
	// non-trivial and we don't want to pin a stale Zig nightly. The
	// right call at this point in the v3.8 cycle is to retire the
	// column; the Zig adapter was a single-listener entrant in a
	// bench where every other adapter is either multi-thread + SO_REUSEPORT
	// (Rust / Go / .NET) or has a fast accept loop (celeris epoll /
	// iouring). The `servers/zig_zap/` source is left in tree for
	// reference and to be re-introduced when Zig 0.16+ lands a proper
	// reuse-port primitive; the registry entry is intentionally
	// absent so the bench never schedules a column against it.

	// celeris — 4 engine modes selected at runtime via -engine. The
	// binary is the same; entries differ only in Engine + Name.
	"celeris-iouring-h1-async": {
		Name: "celeris-iouring-h1-async", Category: "celeris", Language: "go", Framework: "celeris",
		Engine:       "iouring-h1-async",
		Bin:          GoBinary{ModuleDir: "servers/celeris"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true, WS: true, SSE: true, TLS: true},
	},
	"celeris-iouring-auto+upg-async": {
		Name: "celeris-iouring-auto+upg-async", Category: "celeris", Language: "go", Framework: "celeris",
		Engine:       "iouring-auto+upg-async",
		Bin:          GoBinary{ModuleDir: "servers/celeris"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true, WS: true, SSE: true, TLS: true},
	},
	"celeris-epoll-h1-sync": {
		Name: "celeris-epoll-h1-sync", Category: "celeris", Language: "go", Framework: "celeris",
		Engine:       "epoll-h1-sync",
		Bin:          GoBinary{ModuleDir: "servers/celeris"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true, WS: true, SSE: true, TLS: true},
	},
	"celeris-std-h1": {
		Name: "celeris-std-h1", Category: "celeris", Language: "go", Framework: "celeris",
		Engine:       "std-h1",
		Bin:          GoBinary{ModuleDir: "servers/celeris"},
		Capabilities: Capabilities{Static: true, Drivers: true, Middleware: true, WS: true, SSE: true, TLS: true},
	},

	// hono / elysia — wave 4b. TypeScript adapters running natively
	// under Bun (no docker, no node). The "binary" the runner exec's
	// is a small POSIX-sh launcher (servers/<framework>/server, written
	// by ansible/roles/bun/tasks/build_competitor.yml) that does
	// `exec bun run dist/server "$@"`. RunCmd's {name} token expands to
	// the adapter slug, which startNativeAdapter then routes through
	// resolveAdapterBinary so PROBATORIUM_BENCH_ROOT override and the
	// local-dev fallback apply uniformly across language families.
	//
	// Both Bun adapters call Bun.serve directly with the framework's
	// .fetch handler — Hono via app.fetch (the documented fast path,
	// skipping @hono/node-server) and Elysia via app.fetch (skipping
	// Elysia's .listen wrapper) for the H1 fast path. Bun.serve has no
	// cleartext-H2C server option, but `node:http2`'s createServer (which
	// Bun implements) does serve cleartext h2c prior-knowledge, so the
	// h2 columns below bridge an http2 server to the same app.fetch handler.
	"hono": {
		Name: "hono", Category: "bun-ts", Language: "bun", Framework: "hono", Engine: "",
		Bin: NativeBinary{
			Lang:   "bun",
			RunCmd: "{name} -bind {bind}",
		},
	},
	// hono-h2 — prior-knowledge h2c via node:http2.createServer bridged to
	// Hono's app.fetch; -engine h2c selects it. Shares the competitors/hono
	// launcher via Bin.BinName (the launcher forwards "$@", so -engine h2c
	// reaches the same dist/server entry).
	"hono-h2": {
		Name: "hono-h2", Category: "bun-ts", Language: "bun", Framework: "hono", Engine: "h2c-noupg",
		Bin: NativeBinary{
			Lang:    "bun",
			RunCmd:  "{name} -bind {bind}",
			BinName: "hono",
		},
	},
	"elysia": {
		Name: "elysia", Category: "bun-ts", Language: "bun", Framework: "elysia", Engine: "",
		Bin: NativeBinary{
			Lang:   "bun",
			RunCmd: "{name} -bind {bind}",
		},
	},
	// elysia-h2 — prior-knowledge h2c via node:http2 bridged to Elysia's
	// app.fetch (see hono-h2). Shares the competitors/elysia launcher.
	"elysia-h2": {
		Name: "elysia-h2", Category: "bun-ts", Language: "bun", Framework: "elysia", Engine: "h2c-noupg",
		Bin: NativeBinary{
			Lang:    "bun",
			RunCmd:  "{name} -bind {bind}",
			BinName: "elysia",
		},
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
