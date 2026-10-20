package debugvars

import (
	"encoding/json"
	"io"
	"net/http"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// The side listener serves the runtime's CPU settings and the engine
// counters the ws_echo timeout diagnosis needs, and not memstats.
func TestSchedEndpointOnTheSideListener(t *testing.T) {
	var banner syncBuf
	prev := debugBannerOut
	debugBannerOut = &banner
	t.Cleanup(func() { debugBannerOut = prev })
	t.Setenv(DebugAddrEnv, "127.0.0.1:0")

	dv, _, _, _ := startHeld(t, celeris.Std, 0, "")
	addr := dv.DebugAddr()
	if addr == "" {
		t.Fatal("no side listener")
	}
	resp, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + addr + SchedPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("HTTP %d: %s", resp.StatusCode, body)
	}
	var doc struct {
		Gomaxprocs int            `json:"gomaxprocs"`
		Numcpu     int            `json:"numcpu"`
		Engine     map[string]any `json:"engine"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("not JSON: %v\n%s", err, body)
	}
	if doc.Gomaxprocs != runtime.GOMAXPROCS(0) || doc.Numcpu != runtime.NumCPU() {
		t.Errorf("gomaxprocs=%d numcpu=%d, want %d/%d", doc.Gomaxprocs, doc.Numcpu, runtime.GOMAXPROCS(0), runtime.NumCPU())
	}
	// The engine counters (engine_workers, adaptive_switches,
	// engine_standby_active_conns, engine_transplant_residual_busy) come with
	// srv.EngineInfo, which only the native engines on Linux fill; the keys
	// every build has are checked here, and that the others pass through under
	// the same prefix rule is the loop in SchedDocument.
	for _, key := range []string{"accepted_conn_total", "panic_count"} {
		if _, ok := doc.Engine[key]; !ok {
			t.Errorf("engine counters lack %q: %v", key, doc.Engine)
		}
	}
	if strings.Contains(string(body), "memstats") || strings.Contains(string(body), "HeapAlloc") {
		t.Errorf("the document carries memstats (a stop-the-world read): %s", body)
	}
	// Not served on the engine: the listener is the only place.
}
