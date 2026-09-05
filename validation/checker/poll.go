// Package checker is the snapshot-driven property evaluator shared by
// the in-process Tier 1 property loop (validation/propertyloop.go) and
// the standalone cmd/validator-checker binary.
//
// It owns three things:
//
//   - Poll / PollValidationSocket: fetch one [properties.Snapshot] from
//     a refapp's /debug/vars document (and, when present, from the
//     validation-build unix socket).
//   - ReadRSS: the /proc/<pid>/status VmRSS sample that feeds I-MEM-4.
//   - Evaluator: the rolling History, the [properties.Context]
//     bookkeeping (RunStartedAt, BaselineGoroutines) and the
//     per-predicate pass/violation tally.
//
// Keeping this in one package means there is exactly one parser and
// one evaluation loop, so the binary and the orchestrator cannot drift.
package checker

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

// DebugVarsKeys documents the /debug/vars JSON keys Poll reads. The
// refapps publish exactly this shape (validation/refapp/internal/
// debugvars); anything else parses as zero.
//
//	{
//	  "goroutines":                  int,     // runtime.NumGoroutine
//	  "celeris.accepted_conn_total": int,     // Config.OnConnect count
//	  "celeris.closed_conn_total":   int,     // Config.OnDisconnect count
//	  "celeris.active_conns":        int,     // EngineMetrics.ActiveConnections
//	  "celeris.panic_count":         int,     // recovered panics
//	  "celeris.adaptive_switches":   int,     // EngineMetrics.AdaptiveSwitches
//	  "memstats": { "HeapInuse": int, "HeapAlloc": int, ... }  // runtime.MemStats
//	}
const DebugVarsKeys = "goroutines, celeris.accepted_conn_total, celeris.closed_conn_total, celeris.active_conns, celeris.panic_count, celeris.adaptive_switches, memstats.HeapInuse, memstats.HeapAlloc"

// Poll fetches url and projects the /debug/vars document into a
// [properties.Snapshot] stamped with t. Missing keys default to zero.
// A transport, HTTP-status, read or parse failure returns the stamped
// zero snapshot AND a non-nil error so callers can count dead polls
// instead of mistaking them for a healthy all-zero process.
func Poll(ctx context.Context, hc *http.Client, url string, t time.Time) (properties.Snapshot, error) {
	snap := properties.Snapshot{TS: t.Unix()}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return snap, err
	}
	resp, err := hc.Do(req)
	if err != nil {
		return snap, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		_, _ = io.Copy(io.Discard, resp.Body)
		return snap, fmt.Errorf("%s: status %d", url, resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return snap, err
	}
	if err := ParseDebugVars(body, &snap); err != nil {
		return snap, err
	}
	return snap, nil
}

// ParseDebugVars decodes one /debug/vars document into snap (TS and any
// field the document does not carry are left untouched).
func ParseDebugVars(body []byte, snap *properties.Snapshot) error {
	var doc map[string]any
	if err := json.Unmarshal(body, &doc); err != nil {
		return fmt.Errorf("parse /debug/vars: %w", err)
	}
	snap.GoroutineCount = readInt64(doc, "goroutines")
	snap.AcceptedConnTotal = readInt64(doc, "celeris.accepted_conn_total")
	snap.ClosedConnTotal = readInt64(doc, "celeris.closed_conn_total")
	snap.ActiveConns = readInt64(doc, "celeris.active_conns")
	snap.PanicCount = readInt64(doc, "celeris.panic_count")
	snap.AdaptiveSwitches = readInt64(doc, "celeris.adaptive_switches")
	if ms, ok := doc["memstats"].(map[string]any); ok {
		snap.HeapInuseBytes = readInt64(ms, "HeapInuse")
		snap.HeapAllocBytes = readInt64(ms, "HeapAlloc")
	}
	return nil
}

func readInt64(m map[string]any, key string) int64 {
	switch v := m[key].(type) {
	case float64:
		return int64(v)
	case int64:
		return v
	case int:
		return int64(v)
	case json.Number:
		n, _ := v.Int64()
		return n
	default:
		return 0
	}
}

// NewSocketClient returns an http.Client that dials the unix-domain
// socket at path for every request (net/http does not speak unix://
// URLs natively). An empty path yields a client whose dials fail, so
// PollValidationSocket becomes a no-op.
func NewSocketClient(path string, timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext: func(_ context.Context, _, _ string) (net.Conn, error) {
				if path == "" {
					return nil, errors.New("validation socket disabled")
				}
				return net.Dial("unix", path)
			},
		},
	}
}

// PollValidationSocket fetches the in-process assertion counters from
// celeris's validation-build unix-domain endpoint and copies them onto
// snap. Schema matches `celeris/validation.Counters` (v1.4.3+):
//
//	{
//	  "panic_count":                  uint64,
//	  "ratelimit_token_violations":   uint64,
//	  "session_owner_mismatches":     uint64,
//	  "jwt_late_admits":              uint64,
//	  "iouring_sqe_corruptions":      uint64
//	}
//
// A connection failure (production build, socket missing, ECONNREFUSED)
// is non-fatal and leaves snap untouched. PanicCount takes the larger of
// the socket value and whatever /debug/vars already put on snap.
func PollValidationSocket(ctx context.Context, hc *http.Client, snap *properties.Snapshot) {
	// The URL host is ignored because DialContext routes to the unix
	// socket regardless. The path "/snapshot" matches the canonical
	// route in celeris/validation/endpoint.go.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://unix/snapshot", nil)
	if err != nil {
		return
	}
	resp, err := hc.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return
	}
	var c struct {
		PanicCount               int64 `json:"panic_count"`
		RatelimitTokenViolations int64 `json:"ratelimit_token_violations"`
		SessionOwnerMismatches   int64 `json:"session_owner_mismatches"`
		JWTLateAdmits            int64 `json:"jwt_late_admits"`
		IouringSQECorruptions    int64 `json:"iouring_sqe_corruptions"`
	}
	if err := json.Unmarshal(body, &c); err != nil {
		return
	}
	if c.PanicCount > snap.PanicCount {
		snap.PanicCount = c.PanicCount
	}
	snap.RatelimitTokenViolations = c.RatelimitTokenViolations
	snap.SessionOwnerMismatches = c.SessionOwnerMismatches
	snap.JWTLateAdmits = c.JWTLateAdmits
	snap.IouringSQECorruptions = c.IouringSQECorruptions
}

// SelectPredicates resolves a comma-separated tier filter into the
// snapshot-driven specs to evaluate. Empty means every registered
// predicate. The tier-1-walker specs are always excluded: their
// Predicate is a no-op (the orchestrator's TallyCallback emits them),
// so counting them here would inflate properties_passed.
func SelectPredicates(tier string) []properties.Spec {
	var pool []properties.Spec
	if tier == "" {
		pool = properties.All()
	} else {
		seen := map[string]bool{}
		for _, t := range strings.Split(tier, ",") {
			t = strings.TrimSpace(t)
			if t == "" {
				continue
			}
			for _, p := range properties.ByTier(t) {
				if !seen[p.ID] {
					pool = append(pool, p)
					seen[p.ID] = true
				}
			}
		}
	}
	out := make([]properties.Spec, 0, len(pool))
	for _, p := range pool {
		if p.Tier == "tier-1-walker" {
			continue
		}
		out = append(out, p)
	}
	return out
}
