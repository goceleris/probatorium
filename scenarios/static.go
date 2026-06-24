package scenarios

import (
	"math/rand/v2"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/servers"
)

// StaticScenario is the common shape of the 8 static cells: GET /,
// /json, /json-1k, /json-64k, POST 4k, POST 64k, POST 1m, and the churn
// variant that forces Connection: close.
//
// The zero value is not useful; construct instances via the init() block
// below (or via [NewStaticScenario] in tests) so the fields that drive
// loadgen are set explicitly and the payload byte slices are deterministic.
type StaticScenario struct {
	name string

	// Method is the HTTP method ("GET" or "POST").
	Method string

	// Path is the request path ("/", "/json", "/upload", ...).
	Path string

	// Body is the pre-generated request payload. nil for GET.
	Body []byte

	// Connections is the TCP connection count loadgen should dial.
	Connections int

	// DisableKeepAlive, when true, drives loadgen into Connection: close
	// mode (one request per conn). Used by the churn scenario.
	DisableKeepAlive bool

	// HTTP2, when true, drives loadgen in HTTP/2-cleartext prior-knowledge
	// mode — each worker opens an H2 connection and sends the client preface
	// directly (no HTTP/1.1 Upgrade handshake). Used to exercise H2C-capable
	// server cells, including h2c-noupg which refuses plain H1 by design.
	HTTP2 bool

	// ErrBudget, when > 0, overrides [DefaultErrorBudget] as this
	// scenario's loadgen error-ratio ceiling (see [ErrorBudgeter]). Only
	// churn-close sets it today.
	ErrBudget float64
}

// NewStaticScenario constructs a [StaticScenario] with the given
// canonical name. Kept for test ergonomics and backward compatibility
// with the scaffold signature; callers that want real workload fields
// should use a struct literal.
func NewStaticScenario(name string) *StaticScenario {
	return &StaticScenario{name: name}
}

// Name implements [Scenario].
func (s *StaticScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *StaticScenario) Category() string { return CategoryStatic }

// ErrorBudget implements [ErrorBudgeter]: the explicit ErrBudget when
// set, else [DefaultErrorBudget].
func (s *StaticScenario) ErrorBudget() float64 {
	if s.ErrBudget > 0 {
		return s.ErrBudget
	}
	return DefaultErrorBudget
}

// Workload returns the loadgen.Config for this scenario. The
// orchestrator overwrites Duration and Warmup after calling Workload so
// CLI flags (-duration / -warmup) win; leaving them at zero here is
// intentional.
func (s *StaticScenario) Workload(target string) loadgen.Config {
	conns := s.Connections
	if conns <= 0 {
		conns = 128
	}
	method := s.Method
	if method == "" {
		method = "GET"
	}
	cfg := loadgen.Config{
		URL:              target + s.Path,
		Method:           method,
		Body:             s.Body,
		Connections:      conns,
		DisableKeepAlive: s.DisableKeepAlive,
	}
	if s.DisableKeepAlive {
		// Connection-close (churn) scenarios open `Workers × PoolSize`
		// TCP connections in loadgen.New (h1client.go: connsPerWorker =
		// PoolSize when !keepAlive). The default PoolSize=16 ×
		// Workers=64 = 1024 simultaneous dials overwhelms
		// single-listener SUTs (Zig std.http / Rust axum 0.7 with one
		// accept loop, etc.) and manifests as the 145th-or-so dial
		// hitting the 10s default dial timeout. Cap PoolSize=1 for
		// churn so loadgen opens 1 conn per worker (64 dials), which
		// every adapter on the bench handles cleanly. (v3.8 smoke
		// test caught this on zig_zap / churn-close — single-listener
		// SUT, accept queue filled before the bench started its
		// measurement window.)
		cfg.PoolSize = 1
	}
	if s.HTTP2 {
		cfg.HTTP2 = true
		// loadgen's HTTP/2 side has its own connection count knob —
		// reuse s.Connections so an H2 variant of an H1 scenario stays
		// comparable (same number of TCP connections, each carrying
		// multiplexed streams). Defaults for MaxStreams apply.
		cfg.HTTP2Options.Connections = conns
	}
	return cfg
}

// Applicable gates on the wire protocol the scenario actually drives.
// H1 variants (HTTP2=false) apply to any server that accepts plain
// HTTP/1.1; H2 variants (HTTP2=true) apply to any HTTP/2-cleartext-
// capable server, including h2c-noupg (refuses H1 but accepts the H2
// preface directly). Without this split, h2c-noupg cells silently
// record 0 RPS on H1 scenarios and H1-only servers would pair with H2
// scenarios they can't serve.
func (s *StaticScenario) Applicable(fs servers.FeatureSet) bool {
	if s.HTTP2 {
		return fs.HTTP2C
	}
	return fs.HTTP1
}

// Compile-time assertion that StaticScenario satisfies Scenario.
var _ Scenario = (*StaticScenario)(nil)

// StaticScenarioNames is the canonical ordered list of static cell-rows.
// Consumers that enumerate scenarios by name (report tables, CLI filters)
// use this slice rather than iterating the registry so ordering stays
// stable across runs.
var StaticScenarioNames = []string{
	"get-simple",
	"get-json",
	"get-json-1k",
	"post-4k",
	// The large-payload rows (8k/16k/64k GET+POST and the 1 MiB post-1m) were
	// cut as wire-bound: on the 20G LACP fabric every fast adapter converges at
	// the line rate, so raw RPS stopped differentiating them. Server CPU/RSS at
	// the shared ceiling — not these rows — is the large-payload signal now.
	"churn-close",
	// HTTP/2 prior-knowledge variants — exercise every HTTP2C-capable
	// server, including h2c-noupg (which refuses H1 entirely). The 64k h2
	// rows were cut in v1.5.4 (NIC-bound like their H1 twins).
	"get-json-h2",
	"post-4k-h2",
}

// Pre-generated POST payloads. They are built once at package init() so
// every run (and every registered scenario) observes byte-identical
// bodies — any regression on server-side body parsing then shows up as a
// throughput delta rather than a content-size artefact.
//
// Each payload uses its own deterministic seed so a future change to one
// size leaves the others byte-identical.
var post4KBody = makeRandomBody(4*1024, 0xA11CE_4000)

// makeRandomBody returns a byte slice of exactly n bytes filled with a
// deterministic PRNG keyed by seed. math/rand/v2's ChaCha8 is allocation-
// free after construction and produces enough entropy that the result
// does not compress — important when benching content-length parsers and
// keep-alive framing under load.
func makeRandomBody(n int, seed uint64) []byte {
	r := rand.New(rand.NewChaCha8([32]byte{
		byte(seed), byte(seed >> 8), byte(seed >> 16), byte(seed >> 24),
		byte(seed >> 32), byte(seed >> 40), byte(seed >> 48), byte(seed >> 56),
		0x9E, 0x37, 0x79, 0xB9, 0x7F, 0x4A, 0x7C, 0x15,
		0xF3, 0x9C, 0xC0, 0x60, 0x5C, 0xED, 0xC8, 0x34,
		0x10, 0x82, 0x27, 0x6B, 0xF3, 0xA2, 0x72, 0x51,
	}))
	b := make([]byte, n)
	i := 0
	for ; i+8 <= n; i += 8 {
		u := r.Uint64()
		b[i+0] = byte(u)
		b[i+1] = byte(u >> 8)
		b[i+2] = byte(u >> 16)
		b[i+3] = byte(u >> 24)
		b[i+4] = byte(u >> 32)
		b[i+5] = byte(u >> 40)
		b[i+6] = byte(u >> 48)
		b[i+7] = byte(u >> 56)
	}
	if i < n {
		u := r.Uint64()
		for j := 0; i < n; i, j = i+1, j+1 {
			b[i] = byte(u >> (8 * j))
		}
	}
	return b
}

func init() {
	Register(&StaticScenario{
		name:        "get-simple",
		Method:      "GET",
		Path:        "/",
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "get-json",
		Method:      "GET",
		Path:        "/json",
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "get-json-1k",
		Method:      "GET",
		Path:        "/json-1k",
		Connections: 128,
	})
	Register(&StaticScenario{
		name:        "post-4k",
		Method:      "POST",
		Path:        "/upload",
		Body:        post4KBody,
		Connections: 128,
	})
	Register(&StaticScenario{
		name:             "churn-close",
		Method:           "GET",
		Path:             "/",
		Connections:      32,
		DisableKeepAlive: true,
		// Churn legitimately produces refused dials — the accept-backlog
		// overflowing under connection churn is part of what the scenario
		// measures — so the default 5% budget would flag every healthy
		// run. But the v3.8 evidence (and every archived run before it)
		// shows errors at 28x–97x REQUESTS on every server: under loadgen
		// <= v1.4.7 a refused dial is retried in a hot loop with no
		// backoff, so the counter records loadgen's retry-spin rate
		// (~10^5/s), not the SUT's failure rate, and the published RPS
		// describes a SUT in permanent accept-overload. Budget 0.5 means
		// "failed dial attempts may not outnumber completed requests":
		// generous headroom for the benign churn fraction, while every
		// historical 0.96+-ratio cell — previously status=ok — is now
		// flagged suspect until the loadgen dial-backoff fix lands.
		ErrBudget: 0.5,
	})

	// HTTP/2 prior-knowledge variants. Paired with the H1 versions on the
	// same endpoints so an H2 regression shows up as a delta against its
	// H1 twin rather than an isolated number. Connection count is kept
	// lower than H1 because H2 multiplexes streams — 32 TCP conns ×
	// default 100 concurrent streams gives the same or higher effective
	// concurrency as H1's 128 conns.
	Register(&StaticScenario{
		name:        "get-json-h2",
		Method:      "GET",
		Path:        "/json",
		Connections: 32,
		HTTP2:       true,
	})
	Register(&StaticScenario{
		name:        "post-4k-h2",
		Method:      "POST",
		Path:        "/upload",
		Body:        post4KBody,
		Connections: 32,
		HTTP2:       true,
	})
	// get-json-64k-h2 + post-64k-h2 were removed in v1.5.4 — saturated
	// large-body h2 (NIC-bound like their H1 64k twins).
}
