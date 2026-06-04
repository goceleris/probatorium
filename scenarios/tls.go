package scenarios

import (
	"crypto/tls"
	"strings"

	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/servers"
)

// TLS scenarios (#160): HTTPS variants of the json read and the concurrency
// sweeps, dialled with ALPN (h2, http/1.1) over a self-signed bench cert.
//
// CELERIS-CLEARTEXT ASYMMETRY (deliberate, documented per the issue):
// celeris serves cleartext only — it has NO in-tree TLS (no ListenTLS, no
// Config.TLSConfig, no crypto/tls in the core). To keep the comparison fair,
// EVERY adapter — celeris and competitors alike — is benched behind a single
// shared TLS terminator that terminates TLS + ALPN and forwards cleartext to
// the adapter's loopback port. The TLS rows therefore measure
// "framework + shared terminator" uniformly; the handshake / ALPN cost is
// attributed to the terminator, not the adapter. Native-TLS adapters
// (gin/echo/... via http.Server ALPN) are NOT measured in their native-TLS
// mode in Phase 2 — that is the fairness trade-off forced by celeris's
// cleartext-only model.
//
// The runner rewrites the cell target to the terminator's https base (via its
// -tls-terminator flag) and leaves the adapter on its cleartext loopback port.
// Until the terminator infra lands, no adapter declares fs.TLS, so these
// scenarios register but are never scheduled — keeping the cell-row taxonomy
// stable without perturbing the live matrix.

// tlsALPN is the ALPN protocol list offered on the TLS handshake: HTTP/2 first,
// HTTP/1.1 fallback. The shared terminator advertises the same list so the
// negotiated protocol matches what the benched workload expects.
var tlsALPN = []string{"h2", "http/1.1"}

// TLSScenario benches a workload over HTTPS. The scheme is forced to https and
// loadgen is told to skip cert verification (self-signed bench cert) and to
// offer the ALPN protocol list.
type TLSScenario struct {
	name string

	// Path is the request path ("/json", "/").
	Path string

	// Connections is the TCP connection count loadgen should dial.
	Connections int
}

// NewTLSScenario constructs a [TLSScenario].
func NewTLSScenario(name, path string, connections int) *TLSScenario {
	return &TLSScenario{name: name, Path: path, Connections: connections}
}

// Name implements [Scenario].
func (s *TLSScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *TLSScenario) Category() string { return CategoryTLS }

// Workload returns the loadgen.Config for this TLS scenario. The target's
// scheme is coerced to https (the runner normally hands the terminator's https
// base already, but coercing here makes the scenario correct standalone), cert
// verification is disabled for the self-signed bench cert, and a TLS config
// carrying the ALPN protocol list is supplied so the negotiated protocol is
// explicit rather than library-default.
func (s *TLSScenario) Workload(target string) loadgen.Config {
	conns := s.Connections
	if conns <= 0 {
		conns = 128
	}
	return loadgen.Config{
		URL:                forceHTTPS(target) + s.Path,
		Method:             "GET",
		Connections:        conns,
		InsecureSkipVerify: true,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true, //nolint:gosec // self-signed bench cert
			NextProtos:         tlsALPN,
		},
	}
}

// forceHTTPS rewrites an http:// target to https://; a bare host or an already
// https target is returned with an https:// scheme.
func forceHTTPS(target string) string {
	switch {
	case strings.HasPrefix(target, "https://"):
		return target
	case strings.HasPrefix(target, "http://"):
		return "https://" + strings.TrimPrefix(target, "http://")
	default:
		return "https://" + target
	}
}

// Applicable requires the server to declare TLS reachability. In Phase 2 that
// means reachable through the shared terminator; the flag is set by the runner
// only when a terminator is configured, so TLS cells stay out of the matrix
// until the infra is in place.
func (s *TLSScenario) Applicable(fs servers.FeatureSet) bool {
	return fs.TLS
}

// Compile-time assertion that TLSScenario satisfies Scenario.
var _ Scenario = (*TLSScenario)(nil)

// TLSScenarioNames is the canonical ordered list of TLS cell-rows.
var TLSScenarioNames = []string{
	"tls-get-json",
	"tls-concurrency-128c",
	"tls-concurrency-1024c",
}

func init() {
	Register(NewTLSScenario("tls-get-json", "/json", 128))
	Register(NewTLSScenario("tls-concurrency-128c", "/", 128))
	Register(NewTLSScenario("tls-concurrency-1024c", "/", 1024))
}
