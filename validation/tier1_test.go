package validation

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/goceleris/probatorium/validation/markov"
	"github.com/goceleris/probatorium/validation/remote"
)

// minimalMatrix builds the smallest valid Matrix the Markov walker
// can step through. Two states, one edge each way, so the walker
// alternates deterministically.
func minimalMatrix(t *testing.T) *markov.Matrix {
	t.Helper()
	const yaml = `start: home
states:
  home:
    list_users: 1.0
  list_users:
    home: 1.0
`
	m, err := markov.LoadMatrix(strings.NewReader(yaml))
	if err != nil {
		t.Fatalf("load matrix: %v", err)
	}
	return m
}

func TestMarkovStateToPath_KnownStates(t *testing.T) {
	cases := map[string]string{
		"home":       "/",
		"login":      "/api/login",
		"list_users": "/api/users",
		"user_detail": "/api/users/u1",
		"logout":     "/api/logout",
		// Silent states (POST flows reserved for Tier 2 fuzzer).
		"create_user": "",
		"update_user": "",
		// Unknown state — defensive default.
		"never-seen": "",
	}
	for state, want := range cases {
		if got := markovStateToPath(state); got != want {
			t.Errorf("markovStateToPath(%q): got %q, want %q", state, got, want)
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
		doMarkovRequest(context.Background(), hc, srv.URL, &tally)
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
	doMarkovRequest(context.Background(), hc, "http://127.0.0.1:1/never-listens", &tally)
	s := tally.snapshot()
	if s.RequestsError != 1 {
		t.Errorf("RequestsError: got %d, want 1", s.RequestsError)
	}
	if s.RequestsSent != 1 {
		t.Errorf("RequestsSent: got %d, want 1", s.RequestsSent)
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
