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
