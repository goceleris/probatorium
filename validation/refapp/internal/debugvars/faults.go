package debugvars

// Mechanism-shaped fault injection for the celeris#588 capture control
// (probatorium#351).
//
// celeris#588 asks that the NEXT h2c_hang / ws_handshake_fail be
// root-causable from the artifact alone, and its review asks that this be
// PROVEN with a fault injected at a known instant, not waited for. The
// strongest prior for both events is celeris#493's shape: one lock, held by
// one goroutine for seconds, that every request on some path needs. That is
// exactly what a FaultHold is: at a fixed offset after the refapp's ready
// line one goroutine takes a mutex and sleeps for the hold; every request
// whose path the hold names blocks on the same mutex before routing until
// it is released. Nothing else is touched -- /healthz, /debug/vars and
// /debug/pprof keep answering (the liveness oracle and the dossier's
// goroutine dump both need that), and other paths never wait.
//
// Off unless PROBATORIUM_REFAPP_FAULT is set. Format, comma-separated:
//
//	<path>:<hold>@<at>      e.g. /ws:8s@30s,/:40s@60s
//
// holds <path> for <hold>, starting <at> after the ready line. The paths a
// walker uses: "/ws" is the WS-torture handshake (2 s budget -> a hold over
// 2 s fails it), "/" is the h2c-churn preamble (20 s budget -> a hold over
// 20 s is an h2c_hang). Each hold logs its start and its release to stderr
// with the instant, so the control's checker has the ground truth to judge
// the capture against: "[fault] hold path=/ws hold=8s start=<RFC3339Nano>"
// and "[fault] release path=/ws end=<RFC3339Nano> waiters=<n>".

import (
	"fmt"
	"io"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/goceleris/celeris"
)

// FaultEnv names the fault specification variable.
const FaultEnv = "PROBATORIUM_REFAPP_FAULT"

// maxFaultHold bounds one hold: long enough for any walker budget (h2c
// 20 s) plus a dossier taken inside it, short enough that a typo cannot
// freeze a soak cell for its whole budget.
const maxFaultHold = 5 * time.Minute

// FaultHold is one scheduled lock hold on one request path.
type FaultHold struct {
	Path string
	Hold time.Duration
	At   time.Duration

	mu      sync.Mutex
	waiters atomic.Int64 // requests that blocked on this hold
}

// ParseFaults parses a PROBATORIUM_REFAPP_FAULT value. Empty yields nil.
// Every entry must be <path>:<hold>@<at> with an absolute path, a positive
// hold of at most five minutes and a non-negative offset; a path may be
// held only once. Entries come back sorted by offset.
func ParseFaults(spec string) ([]*FaultHold, error) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		return nil, nil
	}
	var out []*FaultHold
	seen := map[string]bool{}
	for _, raw := range strings.Split(spec, ",") {
		e := strings.TrimSpace(raw)
		i := strings.LastIndex(e, ":")
		j := strings.LastIndex(e, "@")
		if i <= 0 || j < i {
			return nil, fmt.Errorf("fault %q: want <path>:<hold>@<at>", e)
		}
		path, holdS, atS := e[:i], e[i+1:j], e[j+1:]
		if !strings.HasPrefix(path, "/") {
			return nil, fmt.Errorf("fault %q: path must start with /", e)
		}
		if seen[path] {
			return nil, fmt.Errorf("fault %q: path %s is already held by an earlier entry", e, path)
		}
		hold, err := time.ParseDuration(holdS)
		if err != nil || hold <= 0 || hold > maxFaultHold {
			return nil, fmt.Errorf("fault %q: hold must be a duration in (0, %s]", e, maxFaultHold)
		}
		at, err := time.ParseDuration(atS)
		if err != nil || at < 0 {
			return nil, fmt.Errorf("fault %q: offset must be a duration >= 0", e)
		}
		seen[path] = true
		out = append(out, &FaultHold{Path: path, Hold: hold, At: at})
	}
	sort.SliceStable(out, func(a, b int) bool { return out[a].At < out[b].At })
	return out, nil
}

// InstallFaults mounts the waiting side of every hold on srv as a Pre
// handler. Mount (NewServer) must have run first so /debug/vars and
// /debug/pprof are answered ahead of it and stay reachable while a hold is
// in force.
func InstallFaults(srv *celeris.Server, holds []*FaultHold) {
	if len(holds) == 0 {
		return
	}
	byPath := make(map[string]*FaultHold, len(holds))
	for _, h := range holds {
		byPath[h.Path] = h
	}
	srv.Pre(func(c *celeris.Context) error {
		if h := byPath[c.Path()]; h != nil {
			h.wait()
		}
		return c.Next()
	})
}

// StartFaults schedules every hold relative to ready and logs to w (the
// refapp's stderr). It returns immediately.
func StartFaults(holds []*FaultHold, ready time.Time, w io.Writer) {
	for _, h := range holds {
		go h.run(ready, w)
	}
}

// wait is the request side: it blocks while the hold is in force. The name
// is what a goroutine dump taken inside the hold shows for every blocked
// request, so keep it stable (report.CheckFaultControl looks for it).
func (h *FaultHold) wait() {
	if !h.mu.TryLock() {
		h.waiters.Add(1)
		h.mu.Lock()
	}
	h.mu.Unlock() //nolint:staticcheck // SA2001: an empty critical section is the point: wait for the holder
}

// run is the holder side. Its frame, asleep inside the hold, is the other
// half of the dump's signature.
func (h *FaultHold) run(ready time.Time, w io.Writer) {
	time.Sleep(time.Until(ready.Add(h.At)))
	h.mu.Lock()
	start := time.Now().UTC()
	_, _ = fmt.Fprintf(w, "[fault] hold path=%s hold=%s start=%s\n", h.Path, h.Hold, start.Format(time.RFC3339Nano))
	time.Sleep(h.Hold)
	end := time.Now().UTC()
	h.mu.Unlock()
	_, _ = fmt.Fprintf(w, "[fault] release path=%s end=%s waiters=%d\n", h.Path, end.Format(time.RFC3339Nano), h.waiters.Load())
}
