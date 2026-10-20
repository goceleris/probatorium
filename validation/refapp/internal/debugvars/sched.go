package debugvars

import (
	"encoding/json"
	"net/http"
	"runtime"
	"strings"
)

// SchedPath is the side listener's scheduling document (probatorium#481).
const SchedPath = "/debug/sched"

// SchedDocument is what the validator's ws_echo timeout diagnosis asks the
// refapp for: the Go runtime's own CPU settings, which /proc cannot show
// (GOMAXPROCS is not in any /proc file), and the engine counters that say
// whether the adaptive engine had switched and what its transplant had left
// behind (workers, adaptive_switches, standby_active_conns,
// transplant_residual_*). Everything under "celeris." of /debug/vars except
// memstats, whose ReadMemStats stops the world.
func (v *Vars) SchedDocument() map[string]any {
	engine := map[string]any{}
	for k, val := range v.document() {
		if strings.HasPrefix(k, "celeris.") {
			engine[strings.TrimPrefix(k, "celeris.")] = val
		}
	}
	return map[string]any{
		"gomaxprocs": runtime.GOMAXPROCS(0),
		"numcpu":     runtime.NumCPU(),
		"goroutines": runtime.NumGoroutine(),
		"engine":     engine,
	}
}

// serveSched is the side listener's /debug/sched handler: one JSON line.
// Loopback peers only, like the rest of the listener.
func (v *Vars) serveSched(w http.ResponseWriter, _ *http.Request) {
	b, err := json.Marshal(v.SchedDocument())
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write(append(b, '\n'))
}
