// chain_handlers.go mounts the four Phase-2 middleware stacks (api, auth,
// security, fullstack) under /chain/<stack>/{json,upload}, ported from the
// celeris reference (test/perfmatrix/servers/celeris/chain_handlers.go) and
// expressed with idiomatic net/http middleware decorators on a ServeMux.
//
// Each stack is a strict superset of the one before it, so scenarios
// differ only by middleware depth — the same 4-step ladder the celeris
// stacks use:
//
//	api       = requestID -> logger(discard) -> recovery -> cors
//	auth      = api       + basicAuth(bench:bench)
//	security  = auth      + csrf(cookie-emit) -> secure-headers
//	fullstack = security  + rateLimit -> timeout -> bodyLimit(10 MiB)
//
// net/http has no first-class middleware, so each piece is a plain
// func(http.Handler) http.Handler decorator. The middleware bodies are
// identical to the chi adapter's so the wire behaviour is byte-for-byte
// the same across adapters; only the composition mechanism differs (a
// fold over the slice here versus chi's r.Use). Credentials, cookie
// names, and the WWW-Authenticate realm come from servers/common so no
// value can drift between the adapter and the loadgen side.
package main

import (
	"context"
	"crypto/subtle"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/goceleris/probatorium/servers/common"
)

const (
	basicAuthHeaderPrefix = "Basic "
	sessionCookieName     = common.SessionCookieName
	csrfCookieName        = common.CSRFCookieName
)

// chain folds the middleware slice around h so the first listed runs
// outermost (request enters mws[0] first, leaves mws[0] last) — matching
// chi's r.Use ordering exactly.
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}
	return h
}

// mountChainHandlers mounts every stack in common.ChainStacks under its
// canonical prefix. The terminal handlers are shared across stacks so the
// only measured difference is middleware cost.
func mountChainHandlers(mux *http.ServeMux) {
	stacks := map[string][]func(http.Handler) http.Handler{
		"api":       chainAPI(),
		"auth":      chainAuth(),
		"security":  chainSecurity(),
		"fullstack": chainFullstack(),
	}

	jsonTerminal := http.HandlerFunc(chainJSONTerminal)
	uploadTerminal := http.HandlerFunc(chainUploadTerminal)

	for _, name := range common.ChainStacks {
		mws, ok := stacks[name]
		if !ok {
			continue
		}
		prefix := common.ChainStackPrefix(name) // e.g. "/chain/api/"
		mux.Handle("GET "+prefix+"json", chain(jsonTerminal, mws...))
		mux.Handle("POST "+prefix+"upload", chain(uploadTerminal, mws...))
	}
}

// chainJSONTerminal is the GET terminal: the canonical /json body.
func chainJSONTerminal(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(`{"message":"Hello, World!"}`))
}

// chainUploadTerminal is the POST terminal: drain the (already
// body-limited, in the fullstack stack) request body and reply "OK".
func chainUploadTerminal(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("OK"))
}

// --- stack assemblies (each a superset of the previous) ---

func chainAPI() []func(http.Handler) http.Handler {
	return []func(http.Handler) http.Handler{
		mwRequestID, mwLoggerDiscard, mwRecovery, mwCORS,
	}
}

func chainAuth() []func(http.Handler) http.Handler {
	return append(chainAPI(), mwBasicAuth)
}

func chainSecurity() []func(http.Handler) http.Handler {
	return append(chainAuth(), mwCSRF, mwSecure)
}

func chainFullstack() []func(http.Handler) http.Handler {
	return append(chainSecurity(), mwRateLimit, mwTimeout, mwBodyLimit)
}

// --- middleware (hand-rolled net/http decorators) ---

func mwRequestID(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id := r.Header.Get("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		w.Header().Set("X-Request-Id", id)
		next.ServeHTTP(w, r)
	})
}

func mwLoggerDiscard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.WriteString(io.Discard, r.Method+" "+r.URL.Path+"\n")
		next.ServeHTTP(w, r)
	})
}

func mwRecovery(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				http.Error(w, "internal error", http.StatusInternalServerError)
			}
		}()
		next.ServeHTTP(w, r)
	})
}

func mwCORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "*")
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// basicAuthExpect is the precomputed Basic credential bytes (bench:bench)
// from the shared contract, compared in constant time.
var basicAuthExpect = []byte(strings.TrimPrefix(common.BasicAuthHeader, basicAuthHeaderPrefix))

func mwBasicAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		if !strings.HasPrefix(auth, basicAuthHeaderPrefix) ||
			subtle.ConstantTimeCompare([]byte(auth[len(basicAuthHeaderPrefix):]), basicAuthExpect) != 1 {
			w.Header().Set("WWW-Authenticate", `Basic realm="`+common.BasicAuthRealm+`"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// mwCSRF emits a CSRF cookie on the hot path without validating a token —
// the same documented choice as the reference: token validation needs a
// stateful lifecycle a stateless loadgen cannot fake, so we measure the
// generation cost only.
func mwCSRF(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     csrfCookieName,
			Value:    "skip-token-bench",
			Path:     "/",
			HttpOnly: true,
		})
		next.ServeHTTP(w, r)
	})
}

func mwSecure(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h := w.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-XSS-Protection", "0")
		next.ServeHTTP(w, r)
	})
}

// chainLimiter is a single shared token bucket sized far above any bench
// load so the limiter's hot-path cost is measured without ever denying a
// request (matches the reference's 1e6 rate/burst).
var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func mwRateLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !chainLimiter.Allow() {
			http.Error(w, "rate limited", http.StatusTooManyRequests)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func mwTimeout(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func mwBodyLimit(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		r.Body = http.MaxBytesReader(w, r.Body, 10<<20)
		next.ServeHTTP(w, r)
	})
}
