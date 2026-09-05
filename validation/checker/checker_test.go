package checker

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/properties"
)

func TestPoll_ParsesDebugVarsShape(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{
			"goroutines": 42,
			"celeris.accepted_conn_total": 1000,
			"celeris.closed_conn_total": 990,
			"celeris.active_conns": 10,
			"celeris.panic_count": 1,
			"celeris.adaptive_switches": 3,
			"memstats": {"HeapInuse": 123456, "HeapAlloc": 100000, "NumGC": 7}
		}`))
	}))
	defer srv.Close()
	now := time.Unix(1_700_000_000, 0)
	snap, err := Poll(context.Background(), srv.Client(), srv.URL+"/debug/vars", now)
	if err != nil {
		t.Fatalf("Poll: %v", err)
	}
	want := properties.Snapshot{TS: now.Unix(), GoroutineCount: 42, AcceptedConnTotal: 1000, ClosedConnTotal: 990,
		ActiveConns: 10, PanicCount: 1, AdaptiveSwitches: 3, HeapInuseBytes: 123456, HeapAllocBytes: 100000}
	if snap != want {
		t.Fatalf("snapshot mismatch:\n got %+v\nwant %+v", snap, want)
	}
}

// A dead endpoint must be reported, not silently read as a healthy
// all-zero process (which is exactly how an 18 GB heap passed every
// predicate in the 2026-09-04 soak).
func TestPoll_ErrorsAreSurfaced(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(404) }))
	defer srv.Close()
	if _, err := Poll(context.Background(), srv.Client(), srv.URL+"/debug/vars", now); err == nil {
		t.Fatal("404 must be an error")
	}
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write([]byte("not json")) }))
	defer bad.Close()
	if _, err := Poll(context.Background(), bad.Client(), bad.URL, now); err == nil {
		t.Fatal("unparseable body must be an error")
	}
	hc := &http.Client{Timeout: 200 * time.Millisecond}
	snap, err := Poll(context.Background(), hc, "http://127.0.0.1:1/debug/vars", now)
	if err == nil {
		t.Fatal("connection refused must be an error")
	}
	if snap.TS != now.Unix() {
		t.Fatalf("zero snapshot must still be stamped: %+v", snap)
	}
}

func TestParseVmRSS(t *testing.T) {
	status := "Name:\trefapp\nVmPeak:\t  300000 kB\nVmRSS:\t  204800 kB\nThreads:\t12\n"
	rss, err := ParseVmRSS(strings.NewReader(status))
	if err != nil {
		t.Fatal(err)
	}
	if rss != 204800*1024 {
		t.Fatalf("rss=%d want %d", rss, 204800*1024)
	}
	if _, err := ParseVmRSS(strings.NewReader("Name:\tx\n")); err == nil {
		t.Fatal("missing VmRSS must error")
	}
	if ReadRSS(0) != 0 {
		t.Fatal("pid 0 must read as 0")
	}
	if ReadRSS(1<<30) != 0 {
		t.Fatal("nonexistent pid must read as 0")
	}
}

func TestSelectPredicates(t *testing.T) {
	core := SelectPredicates("core")
	if len(core) == 0 {
		t.Fatal("expected core specs")
	}
	for _, s := range core {
		if s.Tier != "core" {
			t.Errorf("non-core %s tier=%s", s.ID, s.Tier)
		}
	}
	both := SelectPredicates("core, middleware,")
	saw := map[string]bool{}
	for _, s := range both {
		saw[s.Tier] = true
	}
	if !saw["core"] || !saw["middleware"] {
		t.Fatalf("expected both tiers, saw %v", saw)
	}
	for _, s := range SelectPredicates("") {
		if s.Tier == "tier-1-walker" {
			t.Fatalf("%s: walker specs must never enter the snapshot loop", s.ID)
		}
	}
	if len(SelectPredicates("tier-1-walker")) != 0 {
		t.Fatal("walker tier alone must select nothing")
	}
}

func TestEvaluator_BaselineViolationsAndTally(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	start := time.Unix(1_700_000_000, 0)
	// First sample sets the baseline; PanicCount=0 passes everything.
	v := e.Observe(properties.Snapshot{TS: start.Unix(), GoroutineCount: 30, HeapInuseBytes: 1 << 20}, start)
	if len(v) != 0 {
		t.Fatalf("clean first sample produced violations: %+v", v)
	}
	if c := e.Context(); !c.RunStartedAt.Equal(start) || c.BaselineGoroutines != 30 {
		t.Fatalf("context not initialised from the first sample: %+v", c)
	}
	// A panic shows up: I-PANIC fails on this and every following sample;
	// only the first is flagged First.
	for i := 1; i <= 3; i++ {
		now := start.Add(time.Duration(i) * time.Second)
		v = e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 30, HeapInuseBytes: 1 << 20, PanicCount: 1}, now)
		if len(v) != 1 || v[0].ID != "I-PANIC" {
			t.Fatalf("sample %d: want exactly one I-PANIC violation, got %+v", i, v)
		}
		if v[0].First != (i == 1) {
			t.Fatalf("sample %d: First=%v", i, v[0].First)
		}
		if v[0].Snapshot.PanicCount != 1 {
			t.Fatalf("violation must carry the offending snapshot: %+v", v[0].Snapshot)
		}
	}
	tl := e.Tally()
	if tl.Samples != 4 || tl.Violations != 3 || tl.Failed() != 1 || tl.ViolationIDs[0] != "I-PANIC" {
		t.Fatalf("tally: %+v", tl)
	}
	if tl.Evaluations != 4*int64(len(SelectPredicates("core"))) {
		t.Fatalf("evaluations=%d", tl.Evaluations)
	}
	if !strings.Contains(tl.FailureSummaries["I-PANIC"], "I-PANIC") {
		t.Fatalf("failure summary missing: %+v", tl.FailureSummaries)
	}
	if tl.PerPredicate["I-PANIC"] != 3 || tl.PerPredicate["I-MEM-3"] != 0 {
		t.Fatalf("per-predicate counts: %+v", tl.PerPredicate)
	}
	// Uninstrumented core predicates are listed and not counted as passed;
	// I-MEM-4 joins them because no sample carried an RSS reading.
	if len(tl.NotInstrumented) == 0 {
		t.Fatal("core tier has uninstrumented predicates; none listed")
	}
	if !slices.Contains(tl.NotInstrumented, "I-MEM-4") {
		t.Fatalf("I-MEM-4 without RSS samples must be listed as not instrumented: %v", tl.NotInstrumented)
	}
	if slices.Contains(tl.NotInstrumented, "I-MEM-3") || slices.Contains(tl.NotInstrumented, "I-PANIC") {
		t.Fatalf("instrumented predicates listed as not instrumented: %v", tl.NotInstrumented)
	}
	instrumented := len(tl.Predicates) - len(tl.NotInstrumented)
	if tl.Passed() != instrumented-1 {
		t.Fatalf("passed=%d want %d (instrumented %d minus the failed one)", tl.Passed(), instrumented-1, instrumented)
	}
	if tl.BaselineGoroutines != 30 || tl.LastGoroutines != 30 || tl.FirstHeapInuse != 1<<20 {
		t.Fatalf("resource points: %+v", tl)
	}
	e.RecordPollError()
	if e.Tally().PollErrors != 1 {
		t.Fatal("poll error not counted")
	}
}

func TestEvaluator_NoSamplesPassesNothing(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	tl := e.Tally()
	if tl.Passed() != 0 || tl.Failed() != 0 || tl.Evaluations != 0 {
		t.Fatalf("an evaluator that never observed a sample must report 0/0: %+v", tl)
	}
}

func TestEvaluator_HistoryIsCapped(t *testing.T) {
	e := NewEvaluator(nil)
	start := time.Unix(1_700_000_000, 0)
	for i := 0; i < HistoryCap+100; i++ {
		now := start.Add(time.Duration(i) * time.Second)
		e.Observe(properties.Snapshot{TS: now.Unix()}, now)
	}
	if n := len(e.Context().History); n != HistoryCap {
		t.Fatalf("history len=%d want %d", n, HistoryCap)
	}
}

// End-to-end through the predicates: a 3 goroutine/s leak fed through
// Observe at 1 Hz trips I-MEM-3 at t=15min and not before.
func TestEvaluator_GoroutineLeakTripsIMEM3(t *testing.T) {
	e := NewEvaluator(SelectPredicates("core"))
	start := time.Unix(1_700_000_000, 0)
	firstAt := -1
	for i := 0; i <= 16*60; i++ {
		now := start.Add(time.Duration(i) * time.Second)
		v := e.Observe(properties.Snapshot{TS: now.Unix(), GoroutineCount: 100 + 3*int64(i), HeapInuseBytes: 50 << 20}, now)
		for _, x := range v {
			if x.ID == "I-MEM-3" && firstAt < 0 {
				firstAt = i
			}
		}
	}
	if firstAt < 0 {
		t.Fatal("I-MEM-3 never fired on a 3/s goroutine leak")
	}
	if firstAt < 15*60 || firstAt > 15*60+2 {
		t.Fatalf("I-MEM-3 first fired at t=%ds, want ~900s", firstAt)
	}
	if tl := e.Tally(); tl.Failed() != 1 || tl.ViolationIDs[0] != "I-MEM-3" {
		t.Fatalf("tally: %+v", tl)
	}
}

// fakeValidationSocket binds a unix socket and serves body on GET
// /snapshot. Uses /tmp directly (not t.TempDir): macOS caps unix socket
// paths at 104 chars.
func fakeValidationSocket(t *testing.T, body []byte) string {
	t.Helper()
	f, err := os.CreateTemp("/tmp", "probtest-vsock-*.sock")
	if err != nil {
		t.Fatalf("create temp: %v", err)
	}
	sock := f.Name()
	_ = f.Close()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/snapshot", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	})
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: time.Second}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() {
		_ = srv.Shutdown(context.Background())
		_ = os.Remove(sock)
	})
	return sock
}

func TestPollValidationSocket_PopulatesSnapshot(t *testing.T) {
	body, _ := json.Marshal(map[string]int64{
		"panic_count": 0, "ratelimit_token_violations": 3, "session_owner_mismatches": 1,
		"jwt_late_admits": 0, "iouring_sqe_corruptions": 2,
	})
	hc := NewSocketClient(fakeValidationSocket(t, body), 500*time.Millisecond)
	var snap properties.Snapshot
	PollValidationSocket(context.Background(), hc, &snap)
	if snap.RatelimitTokenViolations != 3 || snap.SessionOwnerMismatches != 1 || snap.IouringSQECorruptions != 2 || snap.JWTLateAdmits != 0 {
		t.Fatalf("socket counters not copied: %+v", snap)
	}
}

func TestPollValidationSocket_PanicCountWinsOverDebugVars(t *testing.T) {
	body, _ := json.Marshal(map[string]int64{"panic_count": 5})
	hc := NewSocketClient(fakeValidationSocket(t, body), 500*time.Millisecond)
	snap := properties.Snapshot{PanicCount: 2}
	PollValidationSocket(context.Background(), hc, &snap)
	if snap.PanicCount != 5 {
		t.Fatalf("PanicCount=%d want 5", snap.PanicCount)
	}
}

func TestPollValidationSocket_MissingSocketIsNonFatal(t *testing.T) {
	hc := NewSocketClient("/tmp/this-socket-does-not-exist-probatorium-test", 200*time.Millisecond)
	snap := properties.Snapshot{RatelimitTokenViolations: 99}
	PollValidationSocket(context.Background(), hc, &snap)
	if snap.RatelimitTokenViolations != 99 {
		t.Fatalf("missing socket clobbered snapshot: %d", snap.RatelimitTokenViolations)
	}
	off := NewSocketClient("", 200*time.Millisecond)
	PollValidationSocket(context.Background(), off, &snap)
	if snap.RatelimitTokenViolations != 99 {
		t.Fatal("disabled socket clobbered snapshot")
	}
}
