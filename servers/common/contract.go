// Package common is the shared contract every probatorium server adapter
// implements: the canonical endpoint set, deterministic JSON payload
// generators, and helpers for writing standard responses on top of
// net/http.
//
// The adapter authors do NOT redefine routes — they iterate
// [Endpoints] (or call the per-endpoint helpers in payload.go and
// common.go) so every framework serves byte-identical responses for the
// same request. Loadgen-side fixtures rely on that equivalence: a
// regression on /json-64k between Adapter A and Adapter B means a real
// throughput delta, never a content-size artefact.
package common

// Endpoint describes one of the canonical request/response shapes.
// Every adapter MUST handle every Endpoint in [Endpoints]; missing
// routes show up as 4xx in loadgen and the cell records 0 valid RPS.
//
// ResponseBody semantics:
//
//   - Non-empty bytes are written verbatim with status 200 and the
//     specified Content-Type.
//   - An empty slice signals a dynamic body — currently used only for
//     /upload, where the handler reads-and-discards the request body
//     and replies with a small "OK". Adapters check for nil/len==0 and
//     dispatch to [WriteBody] (or its framework equivalent).
//
// Path templates use the colon-prefix convention (":id") because every
// supported router accepts that syntax verbatim or after a trivial
// rewrite (chi uses {id}, iris uses {id:string}). Adapters translate
// the template once at registration time; loadgen always sends literal
// paths like /users/42.
type Endpoint struct {
	Method              string
	Path                string
	ResponseContentType string
	ResponseBody        []byte
}

// Endpoints is the canonical contract every adapter implements. Order
// is stable so adapters can range over it for registration without
// per-route boilerplate.
//
// The `Hello, World!` text on / and the /json static body
// `{"message":"Hello, World!"}` are the loadgen baselines, sized for
// the smallest meaningful response (no router/codec overhead in the
// numerator). /json-1k and /json-64k carry the deterministic paginated
// payloads from payload.go so framework-side encoding work is
// constant-time across runs. /users/:id and /upload exercise the
// router (path param) and the body parser respectively.
var Endpoints = []Endpoint{
	{
		Method:              "GET",
		Path:                "/",
		ResponseContentType: "text/plain",
		ResponseBody:        []byte("Hello, World!"),
	},
	{
		Method:              "GET",
		Path:                "/json",
		ResponseContentType: "application/json",
		ResponseBody:        []byte(`{"message":"Hello, World!"}`),
	},
	{
		Method:              "GET",
		Path:                "/json-1k",
		ResponseContentType: "application/json",
		// ResponseBody is filled in init() from the deterministic
		// generator so the slice header is byte-identical to what the
		// per-framework helpers serve.
	},
	{
		Method:              "GET",
		Path:                "/json-64k",
		ResponseContentType: "application/json",
		// ResponseBody filled in init().
	},
	{
		Method:              "GET",
		Path:                "/users/:id",
		ResponseContentType: "text/plain",
		// Empty ResponseBody — dynamic, handler echoes the path param.
	},
	{
		Method:              "POST",
		Path:                "/upload",
		ResponseContentType: "text/plain",
		ResponseBody:        []byte("OK"),
	},
}

// init wires the deterministic 1k / 64k payloads into the contract
// slice so external consumers (test fixtures, conformance probes) get
// the exact bytes a live server would return.
//
// generateJSONPayload is invoked here directly rather than via the
// JSON1KPayload / JSON64KPayload accessors so this init does NOT
// depend on payload.go's init having already populated the
// package-level json1KPayload / json64KPayload buffers. Go runs init
// funcs across files in alphabetical filename order; contract.go
// sorts before payload.go, so the accessor path would observe empty
// slices on every fresh process. Computing the bytes here is
// idempotent — payload.go's init then overwrites the package buffers
// with the same values and the accessors return them thereafter.
func init() {
	for i := range Endpoints {
		switch Endpoints[i].Path {
		case "/json-1k":
			Endpoints[i].ResponseBody = generateJSONPayload(1024)
		case "/json-64k":
			Endpoints[i].ResponseBody = generateJSONPayload(65536)
		}
	}
}

// ============================================================================
// Phase-2 declared routes (driver + chain).
//
// These are NOT part of [Endpoints]: the conformance harness byte-compares
// every entry in Endpoints against a fixed ResponseBody, and these routes are
// dynamic (DB-backed, middleware-gated). They live here so the contract stays
// the single source of truth for paths and content-types, but they carry no
// ResponseBody — adapters and probes shape-check them instead of byte-matching.
//
// Availability is capability-gated: an adapter only mounts these when its
// [servers.Capabilities] declares Drivers / Middleware. A declared-but-unserved
// route is a hard error at run time (the runner's per-cell guard), never a
// silent 0-RPS cell.
// ============================================================================

// DriverEndpoints are the four driver-backed routes (PG / Redis / memcached /
// session). Path templates use the colon-prefix convention; adapters translate
// to their router's syntax ({id} / {key}) once at registration. The benched
// requests pin id=42 and a fixed demo key (see scenarios/driver.go).
var DriverEndpoints = []Endpoint{
	{Method: "GET", Path: "/db/user/:id", ResponseContentType: "application/json"},
	{Method: "GET", Path: "/cache/:key", ResponseContentType: "application/octet-stream"},
	{Method: "GET", Path: "/mc/:key", ResponseContentType: "application/octet-stream"},
	{Method: "POST", Path: "/session", ResponseContentType: "application/json"},
}

// ChainStacks is the ordered list of middleware stacks, each mounted under
// /chain/<stack>/. It mirrors scenarios.ChainProfiles so the loadgen side and
// the adapter side cannot drift. For each stack an adapter serves a GET
// .../json and a POST .../upload route.
var ChainStacks = []string{"api", "auth", "security", "fullstack"}

// ChainStackPrefix returns the HTTP path prefix for a chain stack, e.g.
// "api" -> "/chain/api/". Both the adapter chain_handlers.go and the
// scenarios package mount/dial through this single source of truth.
func ChainStackPrefix(stack string) string { return "/chain/" + stack + "/" }

// ChainEndpoints expands ChainStacks into the concrete declared routes
// (GET <prefix>json + POST <prefix>upload per stack). Like DriverEndpoints,
// these are capability-gated and carry no fixed ResponseBody.
var ChainEndpoints = func() []Endpoint {
	eps := make([]Endpoint, 0, len(ChainStacks)*2)
	for _, stack := range ChainStacks {
		prefix := ChainStackPrefix(stack)
		eps = append(eps,
			Endpoint{Method: "GET", Path: prefix + "json", ResponseContentType: "application/json"},
			Endpoint{Method: "POST", Path: prefix + "upload", ResponseContentType: "text/plain"},
		)
	}
	return eps
}()

// Shared wire-parity constants for the chain (middleware) scenarios. Every
// adapter and the loadgen side reference these so the auth / session / csrf
// behaviour is byte-identical across frameworks and languages — a chain
// throughput delta then reflects middleware cost, never a credential or
// cookie-name mismatch.
const (
	// BasicAuthUser / BasicAuthPass are the shared credential the auth,
	// security, and fullstack stacks expect.
	BasicAuthUser = "bench"
	BasicAuthPass = "bench"
	// BasicAuthHeader is the RFC 7617 Authorization value for bench:bench
	// (base64("bench:bench")). Adapters validate against it; loadgen sends it.
	BasicAuthHeader = "Basic YmVuY2g6YmVuY2g="
	// BasicAuthRealm is the WWW-Authenticate realm, kept for wire parity.
	BasicAuthRealm = "perfmatrix"

	// SessionCookieName / CSRFCookieName are the cookie names the session and
	// security stacks emit.
	SessionCookieName = "pmsid"
	CSRFCookieName    = "_csrf"
)
