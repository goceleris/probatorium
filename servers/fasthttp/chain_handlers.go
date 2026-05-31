// Middleware-chain handlers for the fasthttp adapter: the four Phase-2
// stacks (api / auth / security / fullstack) mounted under
// /chain/<stack>/{json,upload}. fasthttp has no middleware framework, so
// each stack is a hand-rolled chain of RequestHandler decorators. The
// decorators are semantically equivalent to the celeris adapter's
// middleware packages and share the wire-parity constants in
// servers/common, so a chain throughput delta reflects middleware cost,
// not a credential / cookie-name / header-name mismatch.
//
// Stack composition (outer → inner), mirroring the celeris stacks:
//
//	api        requestid → logger → recovery → cors
//	auth       api + basicauth
//	security   auth + csrf(skip) + secure
//	fullstack  security + ratelimit → timeout → bodylimit
package main

import (
	"crypto/subtle"
	"io"
	"net/http"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/valyala/fasthttp"
	"golang.org/x/time/rate"

	"github.com/goceleris/probatorium/servers/common"
)

// chainBodyLimit is the fullstack stack's request-body ceiling (10 MiB),
// matching the celeris bodylimit.Config{Limit: "10MB"}.
const chainBodyLimit = 10 << 20

// jsonSmall is the contract /json body served by every chain's GET
// terminal, so the chain scenarios are byte-comparable with the /json
// baseline.
var jsonSmall = []byte(`{"message":"Hello, World!"}`)

// mountChainHandlers registers the four stacks onto s. The stack names
// and path prefixes come from servers/common so the adapter and the
// loadgen side cannot drift.
func mountChainHandlers(s *Server) {
	if s == nil {
		return
	}

	jsonTerminal := func(rc *fasthttp.RequestCtx) {
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(jsonSmall)
	}
	uploadTerminal := func(rc *fasthttp.RequestCtx) {
		_ = rc.PostBody()
		rc.SetContentType("text/plain")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.WriteString("OK")
	}

	wrappers := map[string]func(fasthttp.RequestHandler) fasthttp.RequestHandler{
		"api":       chainAPI,
		"auth":      chainAuth,
		"security":  chainSecurity,
		"fullstack": chainFullstack,
	}
	for _, stack := range common.ChainStacks {
		wrap := wrappers[stack]
		if wrap == nil {
			continue
		}
		prefix := common.ChainStackPrefix(stack)
		s.MountNative(http.MethodGet, prefix+"json", wrap(jsonTerminal))
		s.MountNative(http.MethodPost, prefix+"upload", wrap(uploadTerminal))
	}
}

func chainAPI(h fasthttp.RequestHandler) fasthttp.RequestHandler {
	return mwRequestID(mwLoggerDiscard(mwRecovery(mwCORS(h))))
}
func chainAuth(h fasthttp.RequestHandler) fasthttp.RequestHandler {
	return chainAPI(mwBasicAuth(h))
}
func chainSecurity(h fasthttp.RequestHandler) fasthttp.RequestHandler {
	return chainAuth(mwCSRFSkip(mwSecure(h)))
}
func chainFullstack(h fasthttp.RequestHandler) fasthttp.RequestHandler {
	return chainSecurity(mwRateLimit(mwTimeout(mwBodyLimit(h, chainBodyLimit))))
}

// mwRequestID echoes X-Request-Id or generates a new UUID, matching
// celeris's requestid middleware.
func mwRequestID(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		id := string(rc.Request.Header.Peek("X-Request-Id"))
		if id == "" {
			id = uuid.NewString()
		}
		rc.Response.Header.Set("X-Request-Id", id)
		next(rc)
	}
}

// mwLoggerDiscard formats a one-line access log into io.Discard so the
// bench sees the formatter cost without any stderr / disk IO — the
// fasthttp analogue of celeris's logger-to-discard.
func mwLoggerDiscard(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		_, _ = io.WriteString(io.Discard, string(rc.Method())+" "+string(rc.Path())+"\n")
		next(rc)
	}
}

// mwRecovery converts a panic in a downstream handler into a 500.
func mwRecovery(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		defer func() {
			if rec := recover(); rec != nil {
				rc.SetStatusCode(fasthttp.StatusInternalServerError)
				_, _ = rc.WriteString("internal error")
			}
		}()
		next(rc)
	}
}

// mwCORS sets allow-all CORS headers and short-circuits a preflight.
func mwCORS(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		h := &rc.Response.Header
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		h.Set("Access-Control-Allow-Headers", "*")
		if string(rc.Method()) == http.MethodOptions &&
			len(rc.Request.Header.Peek("Access-Control-Request-Method")) > 0 {
			rc.SetStatusCode(fasthttp.StatusNoContent)
			return
		}
		next(rc)
	}
}

// mwBasicAuth enforces the shared bench:bench credential via a
// constant-time compare against common.BasicAuthHeader. The realm matches
// common.BasicAuthRealm so the 401 challenge is byte-identical across
// adapters.
func mwBasicAuth(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	expect := []byte(common.BasicAuthHeader)
	challenge := `Basic realm="` + common.BasicAuthRealm + `"`
	return func(rc *fasthttp.RequestCtx) {
		auth := rc.Request.Header.Peek("Authorization")
		if subtle.ConstantTimeCompare(auth, expect) != 1 {
			rc.Response.Header.Set("WWW-Authenticate", challenge)
			rc.SetStatusCode(fasthttp.StatusUnauthorized)
			_, _ = rc.WriteString("unauthorized")
			return
		}
		next(rc)
	}
}

// mwCSRFSkip issues the double-submit CSRF cookie without validating it.
// The loadgen does not carry CSRF tokens, so the middleware runs (to
// measure its cost) but does not reject authenticated requests —
// mirroring the celeris csrf.Config{Skip: always-true}.
func mwCSRFSkip(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		ck := fasthttp.AcquireCookie()
		ck.SetKey(common.CSRFCookieName)
		ck.SetValue("skip-token-bench")
		ck.SetPath("/")
		ck.SetHTTPOnly(true)
		rc.Response.Header.SetCookie(ck)
		fasthttp.ReleaseCookie(ck)
		next(rc)
	}
}

// mwSecure emits the OWASP security headers celeris's secure middleware
// sets by default.
func mwSecure(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		h := &rc.Response.Header
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-XSS-Protection", "0")
		next(rc)
	}
}

// chainLimiter is sized so no bench rig exceeds it: the goal is to measure
// the rate-limiter's hot-path cost, not to drop requests. Matches the
// celeris ratelimit.Config{RPS: 1e6, Burst: 1e6}.
var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func mwRateLimit(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		if !chainLimiter.Allow() {
			rc.SetStatusCode(fasthttp.StatusTooManyRequests)
			_, _ = rc.WriteString("rate limited")
			return
		}
		next(rc)
	}
}

// timeoutCounter keeps the no-op timeout decorator observable to the
// compiler; it is incremented but never read.
var timeoutCounter atomic.Int64

// mwTimeout approximates celeris's timeout middleware. fasthttp enforces
// request deadlines on its own transport, so a per-request context
// deadline has little effect here; the decorator runs the same branch +
// deferred cleanup so the observable per-request cost is comparable.
func mwTimeout(next fasthttp.RequestHandler) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		defer timeoutCounter.Add(1)
		next(rc)
	}
}

// mwBodyLimit rejects requests whose body exceeds maxBytes. fasthttp has
// already read the body into rc.Request.Body(), so this is a length check
// matching celeris's bodylimit middleware.
func mwBodyLimit(next fasthttp.RequestHandler, maxBytes int) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		if len(rc.Request.Body()) > maxBytes {
			rc.SetStatusCode(fasthttp.StatusRequestEntityTooLarge)
			_, _ = rc.WriteString("body too large")
			return
		}
		next(rc)
	}
}
