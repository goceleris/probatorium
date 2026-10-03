package scenarios

// PERF CHECKPOINT SCENARIO: throwaway branch perf/checkpoint-<A>-vs-9f4d89b, not for merge
// (evidence/perf-checkpoint-20261003/PREREG.md section 2). Queue row 62 asks for #805's cost on H1
// responses of 8 KiB or more (io_uring now copies every such body into its write buffer). The
// registry cut its large-payload rows as wire-bound (static.go), so no registered scenario sends
// one; the bench servers still serve GET /json-64k (servers/celeris/server.go). This registers that
// row for this one dispatch, with get-json's shape (GET, 128 keep-alive connections), and the
// dispatch glob runs it on the celeris columns only.
func init() {
	Register(&StaticScenario{
		name:        "get-json-64k",
		Method:      "GET",
		Path:        "/json-64k",
		Connections: 128,
	})
}
