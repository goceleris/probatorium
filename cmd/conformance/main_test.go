package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/goceleris/probatorium/servers/common"
)

// stubMux serves the canonical contract endpoints with byte-identical
// bodies, so a conformance probe against it must report zero failures.
// The mux is the minimum surface area: anything more would test the
// stub instead of the probe.
func stubMux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, ep := range common.Endpoints {
		ep := ep
		switch ep.Path {
		case "/users/:id":
			mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
				common.WritePath(w, r.PathValue("id"))
			})
		case "/upload":
			mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				common.WriteBody(w)
			})
		default:
			pattern := ep.Method + " " + ep.Path
			body := ep.ResponseBody
			ct := ep.ResponseContentType
			mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(body)
			})
		}
	}
	return mux
}

func TestProbeContract_PassesAgainstStub(t *testing.T) {
	srv := httptest.NewServer(stubMux())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	failures := ProbeContract(ctx, srv.URL, 2*time.Second)
	if len(failures) != 0 {
		t.Fatalf("expected zero failures against stub, got %d: %+v", len(failures), failures)
	}
}

// brokenMux is the stub mux but with /json swapped out for a wrong-body
// handler. Confirms the comparator catches drift instead of silently
// passing every request. ServeMux refuses duplicate registrations, so
// this is built from scratch rather than mutating stubMux.
func brokenMux() *http.ServeMux {
	mux := http.NewServeMux()
	for _, ep := range common.Endpoints {
		ep := ep
		switch ep.Path {
		case "/users/:id":
			mux.HandleFunc("GET /users/{id}", func(w http.ResponseWriter, r *http.Request) {
				common.WritePath(w, r.PathValue("id"))
			})
		case "/upload":
			mux.HandleFunc("POST /upload", func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				common.WriteBody(w)
			})
		case "/json":
			mux.HandleFunc("GET /json", func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write([]byte(`{"message":"not the right body"}`))
			})
		default:
			pattern := ep.Method + " " + ep.Path
			body := ep.ResponseBody
			ct := ep.ResponseContentType
			mux.HandleFunc(pattern, func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", ct)
				w.Header().Set("Content-Length", strconv.Itoa(len(body)))
				w.WriteHeader(http.StatusOK)
				_, _ = w.Write(body)
			})
		}
	}
	return mux
}

func TestProbeContract_FlagsBadStub(t *testing.T) {
	srv := httptest.NewServer(brokenMux())
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	failures := ProbeContract(ctx, srv.URL, 2*time.Second)
	if len(failures) == 0 {
		t.Fatalf("expected at least one failure for broken stub")
	}
	found := false
	for _, f := range failures {
		if f.Path == "/json" {
			found = true
			if !strings.Contains(f.Reason, "body mismatch") {
				t.Errorf("expected body mismatch reason, got %q", f.Reason)
			}
		}
	}
	if !found {
		t.Errorf("expected /json failure, got: %+v", failures)
	}
}

// TestProbeContract_HandlesMissingEndpoint covers the case where an
// adapter has not registered a route — the server returns 404, and the
// probe must report that as a failure rather than logging a 200.
func TestProbeContract_HandlesMissingEndpoint(t *testing.T) {
	mux := http.NewServeMux()
	// Only register /; the rest 404.
	mux.HandleFunc("GET /", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("Hello, World!"))
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	failures := ProbeContract(ctx, srv.URL, 2*time.Second)
	if len(failures) == 0 {
		t.Fatalf("expected failures from sparse stub, got zero")
	}
}
