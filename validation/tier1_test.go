package validation

import (
	"context"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/markov"
	"github.com/goceleris/probatorium/validation/remote"
)

// minimalMatrix builds the smallest valid Matrix the Markov walker
// can step through. Two states, one edge each way, so the walker
// alternates deterministically. Each state declares a request
// directive so the data-driven walker actually fires HTTP traffic
// (post-#125, walkers are silent on states without a request entry).
// A top-level `login:` directive is included so cookie-flow tests
// exercise walkerLogin via the same data-driven path real refapps
// use (post-route-fix, walkerLogin is skipped entirely when login
// is absent).
func minimalMatrix(t *testing.T) *markov.Matrix {
	t.Helper()
	const yaml = `login: POST /login
start: home
states:
  home:
    request: GET /
    list_users: 1.0
  list_users:
    request: GET /api/users
    home: 1.0
`
	m, err := markov.LoadMatrix(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	return m
}

// TestAuthSessionRatelimit_StateRequestCoverage verifies the
// auth_session_ratelimit.yaml Markov matrix declares a `request: ...`
// directive for every non-terminal state. The walker is data-driven
// off this map; a state missing a request entry is silently skipped
// — exactly the regression we shipped on probatorium#125 where six
// of eight refapps had effectively no Tier 1 traffic.
func TestAuthSessionRatelimit_StateRequestCoverage(t *testing.T) {
	m, err := markov.LoadMatrixFile("../validation/markov/auth_session_ratelimit.yaml")
	if err != nil {
		// Fall back to relative-from-package path when run via
		// `go test ./...` from the repo root.
		m, err = markov.LoadMatrixFile("markov/auth_session_ratelimit.yaml")
		if err != nil {
			t.Fatalf("load auth_session_ratelimit.yaml: %v", err)
		}
	}
	// Yaml updated post-nightly-25977080887 analysis: the old paths
	// (/, /api/login, /api/logout, /api/users/u1) did not match the
	// refapp's actual routes. The current yaml uses the real routes
	// from validation/refapp/auth_session_ratelimit/main.go and adds
	// a top-level `login:` directive.
	wantLogin := markov.Request{Method: "POST", Path: "/login"}
	if m.Login != wantLogin {
		t.Errorf("login: got %+v, want %+v", m.Login, wantLogin)
	}
	want := map[string]struct{ Method, Path string }{
		"me":          {"GET", "/me"},
		"list_users":  {"GET", "/api/users"},
		"user_detail": {"GET", "/api/users/walker-u1"},
		"create_user": {"POST", "/api/users"},
		"update_user": {"PUT", "/api/users/walker-u1"},
		"user_delete": {"DELETE", "/api/users/walker-u1"},
		"user_posts":  {"GET", "/api/users/walker-u1/posts"},
		"logout":      {"POST", "/logout"},
	}
	for state, w := range want {
		got, ok := m.Requests[state]
		if !ok {
			t.Errorf("state %q: missing request directive", state)
			continue
		}
		if got.Method != w.Method || got.Path != w.Path {
			t.Errorf("state %q: got %s %s, want %s %s", state, got.Method, got.Path, w.Method, w.Path)
		}
	}
}

func TestDoMarkovRequest_Counters(t *testing.T) {
	// Spin up a server that returns each status family in turn.
	var hit int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		switch atomic.AddInt32(&hit, 1) {
		case 1:
			w.WriteHeader(200)
		case 2:
			w.WriteHeader(404)
		case 3:
			w.WriteHeader(500)
		default:
			w.WriteHeader(200)
		}
	}))
	defer srv.Close()

	var tally tier1Tally
	hc := &http.Client{Timeout: time.Second}
	for i := 0; i < 3; i++ {
		doMarkovRequest(context.Background(), hc, "GET", srv.URL, false, &tally)
	}
	s := tally.snapshot()
	if s.RequestsSent != 3 {
		t.Errorf("RequestsSent: got %d, want 3", s.RequestsSent)
	}
	if s.Requests2xx != 1 {
		t.Errorf("Requests2xx: got %d, want 1", s.Requests2xx)
	}
	if s.Requests4xx != 1 {
		t.Errorf("Requests4xx: got %d, want 1", s.Requests4xx)
	}
	if s.Requests5xx != 1 {
		t.Errorf("Requests5xx: got %d, want 1", s.Requests5xx)
	}
}

func TestDoMarkovRequest_NetworkErrorIncrementsError(t *testing.T) {
	var tally tier1Tally
	hc := &http.Client{Timeout: 100 * time.Millisecond}
	doMarkovRequest(context.Background(), hc, "GET", "http://127.0.0.1:1/never-listens", false, &tally)
	s := tally.snapshot()
	if s.RequestsError != 1 {
		t.Errorf("RequestsError: got %d, want 1", s.RequestsError)
	}
	if s.RequestsSent != 1 {
		t.Errorf("RequestsSent: got %d, want 1", s.RequestsSent)
	}
}

// TestWalkerLogin_SetsCookie verifies the per-walker login path
// drops a session cookie into the client's jar. Without this, every
// subsequent authed-endpoint GET 401s — exactly the gap that turned
// the 3-day soak's requests_2xx counter into a 9B-record zero.
func TestWalkerLogin_SetsCookie(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/login" && r.Method == "POST" {
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "test-session", Path: "/"})
			w.WriteHeader(200)
			_, _ = w.Write([]byte(`{"sid":"test-session"}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer srv.Close()
	jar, _ := cookiejar.New(nil)
	hc := &http.Client{Timeout: time.Second, Jar: jar}
	login := markov.Request{Method: "POST", Path: "/login"}
	if err := walkerLogin(context.Background(), hc, srv.URL, login, "alice", "pw"); err != nil {
		t.Fatalf("walkerLogin: %v", err)
	}
	u, _ := url.Parse(srv.URL)
	cookies := jar.Cookies(u)
	if len(cookies) == 0 {
		t.Fatal("jar got no cookie after login")
	}
	if cookies[0].Name != "sid" || cookies[0].Value != "test-session" {
		t.Errorf("cookie mismatch: %+v", cookies[0])
	}
}

// TestRunMarkovWalker_LoginThenCookieFlow verifies the full walker
// loop: POSTs /login first, then carries the cookie through state
// transitions and gets 2xx on subsequent GETs. This was the
// 3-day-soak gap.
func TestRunMarkovWalker_LoginThenCookieFlow(t *testing.T) {
	var (
		mu          sync.Mutex
		loginPosts  int
		authedReqs  int
		got2xxAfter bool
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.URL.Path == "/login" && r.Method == "POST" {
			loginPosts++
			http.SetCookie(w, &http.Cookie{Name: "sid", Value: "abc", Path: "/"})
			w.WriteHeader(200)
			return
		}
		// Other paths: 401 without cookie, 200 with.
		c, err := r.Cookie("sid")
		if err != nil || c.Value == "" {
			w.WriteHeader(401)
			return
		}
		authedReqs++
		got2xxAfter = true
		w.WriteHeader(200)
	}))
	defer srv.Close()

	var tally tier1Tally
	parent := &http.Client{Timeout: time.Second}
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	runMarkovWalker(ctx, parent, srv.URL, minimalMatrix(t), 0xa11ce, &tally)

	mu.Lock()
	defer mu.Unlock()
	if loginPosts < 1 {
		t.Errorf("walker didn't POST /login (loginPosts=%d)", loginPosts)
	}
	if !got2xxAfter {
		t.Errorf("no authed 2xx — cookie not flowing")
	}
	if authedReqs < 1 {
		t.Errorf("expected several authed requests after login, got %d", authedReqs)
	}
}

// TestWaitForReady_NoGoroutineLeak repeatedly calls waitForReady
// against a refapp that prints `ready addr=` then continues spamming
// log lines indefinitely. Pre-fix, the scanner goroutine inside
// waitForReady would block on a full lineCh and leak forever — Tier 3
// calls this once per seed, so a 72h soak with ~20K seeds was
// retaining ~1.4 GB of orphan-goroutine state. Post-fix the goroutine
// selects on readyCtx for every send, so deferred cancel reaps it.
//
// The test asserts goroutine count stays bounded after many calls.
func TestWaitForReady_NoGoroutineLeak(t *testing.T) {
	// Drop GOMAXPROCS-dependent flakiness by sampling AFTER GC.
	settle := func() {
		for i := 0; i < 3; i++ {
			runtime.GC()
			time.Sleep(20 * time.Millisecond)
		}
	}
	settle()
	base := runtime.NumGoroutine()

	d := remote.NewLocal("/bin/sh")
	const iterations = 30
	for i := 0; i < iterations; i++ {
		// Script prints `ready addr=foo` then loops printing more lines
		// fast enough that the scanner goroutine's lineCh buffer fills
		// — recreating the pre-fix deadlock scenario.
		args := []string{
			"-c",
			`echo "ready addr=127.0.0.1:0"; for i in $(seq 1 200); do echo "log line $i"; done; sleep 1`,
		}
		proc, err := d.Start(context.Background(), args)
		if err != nil {
			t.Fatalf("start: %v", err)
		}
		addr, err := waitForReady(context.Background(), proc, time.Second)
		if err != nil {
			t.Fatalf("waitForReady iter %d: %v", i, err)
		}
		if addr != "127.0.0.1:0" {
			t.Fatalf("waitForReady iter %d: addr=%q, want 127.0.0.1:0", i, addr)
		}
		// SIGTERM the refapp to free its pipe goroutines.
		_ = proc.Signal(0xf)
	}
	// Allow background reaping.
	settle()

	final := runtime.NumGoroutine()
	growth := final - base
	// Some growth is normal (Go scheduler workers, test framework
	// goroutines). Pre-fix this test would show ~`iterations` worth of
	// orphans (~30 goroutines linearly accumulated). Cap at 10 — a
	// generous bound that still detects a per-iteration linear leak.
	if growth > 10 {
		t.Errorf("goroutine count grew by %d over %d iterations (base=%d final=%d) — likely leak",
			growth, iterations, base, final)
	}
}

func TestDriveTier1_NilDriverRejected(t *testing.T) {
	_, err := driveTier1(context.Background(), tier1Config{Matrix: minimalMatrix(t)})
	if err == nil {
		t.Fatal("expected error for nil Driver")
	}
}

func TestDriveTier1_NilMatrixRejected(t *testing.T) {
	_, err := driveTier1(context.Background(), tier1Config{Driver: remote.NewLocal("/usr/bin/true")})
	if err == nil {
		t.Fatal("expected error for nil Matrix")
	}
}

// fakeReadyDriver implements remote.Driver by spawning a small shell
// script that prints `ready addr=...` then serves an HTTP server.
// Used by the end-to-end Tier 1 test below.
func TestDriveTier1_EndToEnd(t *testing.T) {
	// Stand up a real HTTP server that returns 200 for every path
	// the Markov matrix hits. The refapp-under-test isn't part of
	// this test — we just want to exercise the walker plumbing.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	// A driver that prints `ready addr=<srv.URL>` then sleeps so the
	// walker can fire requests. The actual sleep target doesn't matter
	// — the walker hits httptest.Server (a separate process); the
	// driver's job is only to satisfy waitForReady.
	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    2,
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.RequestsSent < 1 {
		t.Errorf("RequestsSent: got %d, want >= 1", s.RequestsSent)
	}
	// Server returns 200 unconditionally — non-error responses
	// should be 2xx only. Some RequestsError is allowed because the
	// parent context expires mid-flight; any request that was
	// already dispatched when ctx cancelled counts as an error.
	if s.Requests4xx+s.Requests5xx > 0 {
		t.Errorf("non-error non-2xx leaked: %+v", s)
	}
	// errors / sent ratio should be tiny (<= 1%) — the ratio is the
	// number of requests in-flight at cancel divided by the total.
	if s.RequestsSent > 100 && s.RequestsError*100 > s.RequestsSent {
		t.Errorf("error rate too high: %d errors / %d sent (>1%%)", s.RequestsError, s.RequestsSent)
	}
	t.Logf("end-to-end tally: %s", s)
}

// TestDriveTier1_TallyCallbackFires verifies the periodic callback
// installed by the orchestrator runs at the configured interval and
// receives a snapshot containing the current counter values. This is
// the wiring that makes mid-run incident emission possible: the
// callback fires every few seconds, the orchestrator wraps it with
// "emit Incident on first non-zero HIGH counter", and `handleIncident`
// + auto-bisect kick in immediately rather than at end-of-run.
func TestDriveTier1_TallyCallbackFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	var (
		mu        sync.Mutex
		callCount int
		lastSnap  tier1TallySnapshot
	)
	cfg := tier1Config{
		Driver: remote.NewLocal("/bin/sh"),
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    1,
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
		TallyCallback: func(snap tier1TallySnapshot) {
			mu.Lock()
			defer mu.Unlock()
			callCount++
			lastSnap = snap
		},
		TallyCallbackInterval: 100 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 600*time.Millisecond)
	defer cancel()
	if _, err := driveTier1(ctx, cfg); err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if callCount < 2 {
		t.Errorf("TallyCallback called %d times, want >= 2 over 600ms with 100ms interval", callCount)
	}
	if lastSnap.RequestsSent == 0 {
		t.Errorf("last snapshot has zero RequestsSent — callback didn't see live state")
	}
}

// TestDriveTier1_SnapshotPathWritesPeriodically verifies the new
// SnapshotPath knob persists a fresh tier1_tally.json on every
// callback tick, so long-running soaks have mid-run visibility
// without waiting for the orchestrator's end-of-run flush.
func TestDriveTier1_SnapshotPathWritesPeriodically(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	dir := t.TempDir()
	snapPath := dir + "/tier1_tally.json"
	cfg := tier1Config{
		Driver: remote.NewLocal("/bin/sh"),
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:               srv.URL,
		Matrix:                minimalMatrix(t),
		Seed:                  42,
		Concurrency:           1,
		ReadyTimeout:          2 * time.Second,
		RequestTimeout:        time.Second,
		SnapshotPath:          snapPath,
		TallyCallbackInterval: 100 * time.Millisecond,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	if _, err := driveTier1(ctx, cfg); err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	data, err := os.ReadFile(snapPath)
	if err != nil {
		t.Fatalf("snapshot path not written: %v", err)
	}
	if len(data) == 0 {
		t.Fatal("snapshot file empty")
	}
	if !strings.Contains(string(data), `"requests_sent"`) {
		t.Errorf("snapshot missing canonical field; got:\n%s", data)
	}
}

// TestDriveTier1_AdversarialSliceFires verifies driveTier1's
// adversarial walker fans alongside the Markov walker — the Tier 1
// fan-out at concurrency >= 5 reserves one walker for adversarial
// traffic, which targets the same hostPort with malformed bytes.
//
// Asserts that adversarial Sent > 0 after the run; the well-rejected
// vs accepted balance is a separate predicate concern handled by the
// orchestrator (not under test here).
func TestDriveTier1_AdversarialSliceFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    5, // ≥ 5 so one walker is adversarial
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.Adversarial.Sent < 1 {
		t.Errorf("adversarial walker didn't fire — Sent=%d", s.Adversarial.Sent)
	}
	t.Logf("end-to-end adversarial: %+v", s.Adversarial)
}

// TestDriveTier1_H2CChurnSliceFires verifies the h2c churn walker
// fans alongside Markov + adversarial. Tier 1 reserves a walker for
// h2c churn at concurrency >= 10. Asserts h2c.Sent > 0 after the run.
func TestDriveTier1_H2CChurnSliceFires(t *testing.T) {
	// Server that 101's every request — keeps the upgrade path warm so
	// the walker observes Upgraded outcomes.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// httptest can't hijack into raw bytes from a 101 cleanly; reply
		// with 200 instead. The walker only needs a real listener — the
		// classification (upgraded vs declined) isn't what this test
		// asserts.
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    10, // ≥ 10 so one walker is h2c churn
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.H2CChurn.Sent < 1 {
		t.Errorf("h2c churn walker didn't fire — Sent=%d", s.H2CChurn.Sent)
	}
	t.Logf("end-to-end h2c churn: %+v", s.H2CChurn)
}

// TestDriveTier1_WSTortureSliceFires verifies the WS torture walker
// fans alongside Markov + adversarial + h2c churn. The slice
// activates at concurrency >= 20; asserts ws.Sent > 0 after the run.
//
// The httptest server doesn't speak WS — handshake will 400 — so
// outcomes will all classify as HandshakeFail. Sent > 0 is the only
// signal the walker is actually firing.
func TestDriveTier1_WSTortureSliceFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    20, // ≥ 20 so one walker is WS torture
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 800*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.WSTorture.Sent < 1 {
		t.Errorf("ws torture walker didn't fire — Sent=%d", s.WSTorture.Sent)
	}
	t.Logf("end-to-end ws torture: %+v", s.WSTorture)
}

// TestDriveTier1_SSEKillSliceFires verifies the SSE long-poll
// kill-mid-stream walker fans alongside the other slices. Slice
// activates at concurrency >= 20; asserts sse.Sent > 0 after the run.
//
// The httptest server doesn't emit text/event-stream so the walker
// will classify outcomes as HandshakeFail. Sent > 0 confirms the
// walker fired.
func TestDriveTier1_SSEKillSliceFires(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    20, // ≥ 20 so one walker is SSE kill
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.SSEKill.Sent < 1 {
		t.Errorf("sse kill walker didn't fire — Sent=%d", s.SSEKill.Sent)
	}
	t.Logf("end-to-end sse kill: %+v", s.SSEKill)
}

// TestDriveTier1_SSEDormantBelowThreshold confirms the SSE slice
// stays dormant on smoke-sized runs (concurrency <
// streamingWalkerMinConcurrency) so single-walker iteration stays cheap.
func TestDriveTier1_SSEDormantBelowThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    streamingWalkerMinConcurrency - 1, // smoke: below threshold
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.SSEKill.Sent != 0 {
		t.Errorf("sse kill fired below threshold — Sent=%d, want 0", s.SSEKill.Sent)
	}
}

// TestDriveTier1_WSDormantBelowThreshold confirms the WS slice stays
// dormant on smoke-sized runs (concurrency <
// streamingWalkerMinConcurrency).
func TestDriveTier1_WSDormantBelowThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    streamingWalkerMinConcurrency - 1, // smoke: below threshold
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.WSTorture.Sent != 0 {
		t.Errorf("ws torture fired below threshold — Sent=%d, want 0", s.WSTorture.Sent)
	}
}

// TestDriveTier1_StreamingActiveAtDefaultConcurrency locks in the GAP-A fix:
// at the matrix's per-cell / default concurrency of 10, BOTH the WS and SSE
// slices must fire. This is the regression guard that keeps the engine's
// inline streaming-Detach path exercised on every non-smoke validation run —
// the coverage hole that let celeris#309 reach a release.
func TestDriveTier1_StreamingActiveAtDefaultConcurrency(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    10, // the matrix per-cell default
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 1500*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.WSTorture.Sent < 1 {
		t.Errorf("ws torture dormant at default concurrency 10 — Sent=%d, want >=1", s.WSTorture.Sent)
	}
	if s.SSEKill.Sent < 1 {
		t.Errorf("sse kill dormant at default concurrency 10 — Sent=%d, want >=1", s.SSEKill.Sent)
	}
}

// TestDriveTier1_LivenessDetectsCrash is the GAP-B regression guard: a refapp
// that prints `ready addr=` and then dies with a Go runtime `fatal error:`
// (exactly the shape of celeris#309) must be reported as Crashed, with the
// signature scraped and a non-zero exit observed. Before the liveness oracle,
// this death was invisible — the walkers saw only connection-refused.
func TestDriveTier1_LivenessDetectsCrash(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	// Bind-and-die: announce ready, serve briefly, then emit a fatal-error
	// banner + stack and exit 2 — the canonical Go unrecoverable-failure shape.
	script := `echo "ready addr=` + srv.URL + `"
sleep 0.3
echo "fatal error: sync: unlock of unlocked mutex" >&2
echo "" >&2
echo "goroutine 42 [running]:" >&2
echo "runtime.throw(...)" >&2
exit 2`
	cfg := tier1Config{
		Driver:         d,
		RefappArgs:     []string{"-c", script},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    10,
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	// Generous ctx so the crash (not the deadline) is what ends the run.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if !s.Liveness.Crashed {
		t.Fatalf("liveness oracle missed the crash — Liveness=%+v", s.Liveness)
	}
	if !strings.Contains(s.Liveness.Signature, "unlock of unlocked mutex") {
		t.Errorf("crash signature not scraped — got %q", s.Liveness.Signature)
	}
	if !s.Liveness.Exited || s.Liveness.ExitCode != 2 {
		t.Errorf("expected observed exit code 2, got Exited=%v ExitCode=%d", s.Liveness.Exited, s.Liveness.ExitCode)
	}
	t.Logf("liveness: %+v", s.Liveness)
}

// TestDriveTier1_H2CDormantBelowThreshold confirms the h2c slice
// stays dormant when concurrency < 10 — small smoke runs shouldn't
// pay the h2c churn budget.
func TestDriveTier1_H2CDormantBelowThreshold(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(200)
	}))
	defer srv.Close()

	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver: d,
		RefappArgs: []string{
			"-c",
			`echo "ready addr=` + srv.URL + `"; sleep 10`,
		},
		BaseURL:        srv.URL,
		Matrix:         minimalMatrix(t),
		Seed:           42,
		Concurrency:    5, // below threshold
		ReadyTimeout:   2 * time.Second,
		RequestTimeout: time.Second,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 400*time.Millisecond)
	defer cancel()
	s, err := driveTier1(ctx, cfg)
	if err != nil {
		t.Fatalf("driveTier1: %v", err)
	}
	if s.H2CChurn.Sent != 0 {
		t.Errorf("h2c churn fired below threshold — Sent=%d, want 0", s.H2CChurn.Sent)
	}
}

func TestDriveTier1_RefappNeverReadyTimesOut(t *testing.T) {
	// Driver prints nothing — waitForReady should time out.
	d := remote.NewLocal("/bin/sh")
	cfg := tier1Config{
		Driver:       d,
		RefappArgs:   []string{"-c", "sleep 30"},
		BaseURL:      "http://127.0.0.1:1",
		Matrix:       minimalMatrix(t),
		ReadyTimeout: 200 * time.Millisecond,
	}
	_, err := driveTier1(context.Background(), cfg)
	if err == nil {
		t.Fatal("expected ready timeout error")
	}
	if !strings.Contains(err.Error(), "not ready") {
		t.Errorf("expected 'not ready' in error, got %q", err)
	}
}
