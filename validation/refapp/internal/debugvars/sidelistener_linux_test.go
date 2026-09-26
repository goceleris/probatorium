package debugvars

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/goceleris/celeris"
)

// TestDebugListenerAnswersWhileEveryLoopIsParked is the celeris#588 defect
// the side listener exists for, on the engine class it bites: on epoll a
// handler runs on its loop, so once a held path has parked every loop the
// engine-routed /debug/pprof cannot answer until the hold ends -- the dump a
// dossier needs is missing or post-stall. The side listener, outside the
// engine, still returns a dump naming the holder and the parked requests.
// The engine leg is the premise: if the engine did answer, this host did not
// reproduce the wedge and the test says so instead of passing vacuously.
func TestDebugListenerAnswersWhileEveryLoopIsParked(t *testing.T) {
	var banner syncBuf
	prev := debugBannerOut
	debugBannerOut = &banner
	t.Cleanup(func() { debugBannerOut = prev })
	t.Setenv(DebugAddrEnv, "127.0.0.1:0")

	const loops = 2
	dv, base, _ := startHeld(t, celeris.Epoll, loops, "/slow:3s@100ms")
	time.Sleep(250 * time.Millisecond) // inside the hold (100 ms .. 3100 ms)

	// Enough held requests that every loop accepts one and parks on it.
	var wg sync.WaitGroup
	for range 8 * loops {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if resp, err := (&http.Client{Timeout: 6 * time.Second}).Get(base + "/slow"); err == nil {
				_ = resp.Body.Close()
			}
		}()
	}
	t.Cleanup(wg.Wait)
	time.Sleep(300 * time.Millisecond)

	if _, d, err := getDump(base, 800*time.Millisecond); err == nil {
		t.Fatalf("premise: the engine answered /debug/pprof in %s with every loop held -- the wedge did not reproduce here", d)
	}
	dump, d, err := getDump("http://"+dv.DebugAddr(), 2*time.Second)
	if err != nil {
		t.Fatalf("side listener with every loop parked: %v", err)
	}
	if n := strings.Count(dump, "debugvars.(*FaultHold).wait"); n < loops || !strings.Contains(dump, "debugvars.(*FaultHold).run") {
		t.Fatalf("side-listener dump (%s, %d bytes) names %d parked request(s) and holder=%v; want >= %d and the holder",
			d, len(dump), n, strings.Contains(dump, "debugvars.(*FaultHold).run"), loops)
	}
}
