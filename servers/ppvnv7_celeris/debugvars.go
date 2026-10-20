package main

// The bench SUT's /debug/vars (celeris#585).
//
// The bench observer (cmd/observer) scrapes -metrics-url once a second for
// every celeris-* column, and until this file the bench server answered
// that URL with nothing: it registered no /debug/vars route, so every
// celeris column's goroutine / heap series were zeros treated as absent and
// no engine counter ever reached a bench artifact. That made the SEND_ZC
// A/B celeris#585 asks for unanswerable: whether the zero-copy arm carried
// any bytes at all (engine ZCSendsSubmitted, InlineBytes vs RingBytes) is
// only visible from inside the SUT.
//
// Served on a SIDE listener, never through the engine under test:
//
//   - no route is added to the benchmarked router, so the radix tree and
//     every scenario's lookup are byte-identical to before;
//   - the observer's 1 Hz request is not an extra connection on the engine
//     being measured (it would count in the engine's accept / request
//     totals and, on io_uring, occupy a worker);
//   - a stalled engine still answers here, which is the property the
//     validation refapps lacked (celeris#588 review 1).
//
// Off unless PROBATORIUM_DEBUG_ADDR is set (the bench cell sets it to a
// loopback port; run_bench_cell.yml). Nothing here stops the world:
// runtime/metrics replaces runtime.ReadMemStats, whose stop-the-world at
// 1 Hz would otherwise sit inside every saturation window.
//
// The engine block is published as the whole engine.EngineMetrics struct
// under "celeris.engine_metrics" (Go field names), not as a hand list of
// keys, so this file compiles against every celeris version the bench
// may pin -- including a baseline arm on v1.5.8, whose struct simply lacks
// the SEND_ZC fields (the observer then records them as absent, not 0).

import (
	"encoding/json"
	"log"
	"net"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"time"

	"github.com/goceleris/celeris"
)

// debugAddrEnv names the side listener's bind address. Empty = disabled.
const debugAddrEnv = "PROBATORIUM_DEBUG_ADDR"

// startDebugVars starts the side listener when PROBATORIUM_DEBUG_ADDR is
// set and returns its bound address ("" when disabled) and a stop func. A
// bind failure disables it with a log line; the bench itself must never
// fail because its sidecar port was taken.
func startDebugVars(srv *celeris.Server) (addr string, stop func()) {
	bind := os.Getenv(debugAddrEnv)
	if bind == "" {
		return "", func() {}
	}
	ln, err := net.Listen("tcp", bind)
	if err != nil {
		log.Printf("celeris: %s=%s: /debug/vars side listener disabled: %v", debugAddrEnv, bind, err)
		return "", func() {}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("GET /debug/vars", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(debugVarsDocument(srv))
	})
	hs := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() { _ = hs.Serve(ln) }()
	log.Printf("celeris: /debug/vars side listener on %s", ln.Addr())
	return ln.Addr().String(), func() { _ = hs.Close() }
}

// debugVarsDocument is the expvar-shaped document the observer reads. The
// keys it already understood keep their meaning (goroutines,
// memstats.HeapInuse); "pid" lets the observer refuse a document served by
// a process other than the one it samples; the engine block is new.
func debugVarsDocument(srv *celeris.Server) map[string]any {
	heap, gcs := heapInuseAndGCs()
	doc := map[string]any{
		"pid":        os.Getpid(),
		"goroutines": runtime.NumGoroutine(),
		"memstats": map[string]any{
			"HeapInuse": heap,
			"NumGC":     gcs,
		},
	}
	if info := srv.EngineInfo(); info != nil {
		doc["celeris.engine"] = info.Type.String()
		doc["celeris.engine_metrics"] = info.Metrics
	}
	return doc
}

// heapInuseAndGCs reads MemStats.HeapInuse (= heap objects + heap unused,
// the runtime/metrics decomposition of in-use spans) and the completed GC
// cycle count without stopping the world.
func heapInuseAndGCs() (heapInuse, gcCycles uint64) {
	s := []metrics.Sample{
		{Name: "/memory/classes/heap/objects:bytes"},
		{Name: "/memory/classes/heap/unused:bytes"},
		{Name: "/gc/cycles/total:gc-cycles"},
	}
	metrics.Read(s)
	u := func(i int) uint64 {
		if s[i].Value.Kind() == metrics.KindUint64 {
			return s[i].Value.Uint64()
		}
		return 0
	}
	return u(0) + u(1), u(2)
}
