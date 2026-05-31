package main

import (
	"context"
	"crypto/subtle"
	"net/http"
	"time"

	irisv12 "github.com/kataras/iris/v12"
	"github.com/kataras/iris/v12/middleware/cors"
	"github.com/kataras/iris/v12/middleware/logger"
	"github.com/kataras/iris/v12/middleware/rate"
	"github.com/kataras/iris/v12/middleware/recover"
	"github.com/kataras/iris/v12/middleware/requestid"

	"github.com/goceleris/probatorium/servers/common"
)

// mountChainHandlers mounts the four middleware-stack scenarios under
// /chain/<stack>/{json,upload}. Each stack is an iris Party whose Use
// chain composes iris's idiomatic middleware (requestid, logger, recover,
// cors, rate) plus small iris.Handler middlewares for the cross-cutting
// concerns iris v12 does not ship a package for (basic-auth with realm
// parity, a CSRF cookie, secure headers, request timeout, body limit).
//
// The stacks are cumulative and mirror celeris's chain composition so a
// per-stack throughput delta reflects middleware cost, not a behavioural
// mismatch:
//
//	api       -> requestid + logger + recover + cors
//	auth      -> api + basicauth
//	security  -> auth + csrf + secure
//	fullstack -> security + ratelimit + timeout + bodylimit
func mountChainHandlers(app *irisv12.Application) {
	jsonTerminal := func(c irisv12.Context) {
		c.ContentType("application/json")
		_, _ = c.Write([]byte(`{"message":"Hello, World!"}`))
	}
	uploadTerminal := func(c irisv12.Context) {
		_, _ = c.GetBody()
		c.ContentType("text/plain")
		_, _ = c.WriteString("OK")
	}

	for _, stack := range common.ChainStacks {
		prefix := common.ChainStackPrefix(stack) // "/chain/<stack>/"
		party := app.Party(prefix)
		party.Use(chainMiddleware(stack)...)
		party.Get("json", jsonTerminal)
		party.Post("upload", uploadTerminal)
	}
}

// chainMiddleware returns the ordered middleware slice for a stack. The
// slices are cumulative (auth ⊃ api, security ⊃ auth, fullstack ⊃
// security) so the four cells form a strict superset ladder.
func chainMiddleware(stack string) []irisv12.Handler {
	api := []irisv12.Handler{
		requestid.New(),
		logger.New(loggerConfig()),
		recover.New(),
		corsHandler(),
		mwCORSMethods(),
	}
	auth := append(api, mwBasicAuth())
	security := append(append([]irisv12.Handler{}, auth...), mwCSRFCookie(), mwSecureHeaders())
	fullstack := append(append([]irisv12.Handler{}, security...),
		rate.Limit(1_000_000, 1_000_000), mwTimeout(30*time.Second), mwBodyLimit(10<<20))

	switch stack {
	case "api":
		return api
	case "auth":
		return auth
	case "security":
		return security
	case "fullstack":
		return fullstack
	default:
		return api
	}
}

// loggerConfig keeps iris's logger middleware on the hot path (so its
// cost is measured) while routing output to the framework logger, which
// the server sets to "warn" — no per-request stdout write under load.
func loggerConfig() logger.Config {
	return logger.Config{
		Status: true,
		IP:     false,
		Method: true,
		Path:   true,
		Query:  false,
	}
}

// corsHandler builds the iris CORS middleware with the same permissive
// origin/header policy the other adapters advertise. Iris's CORS package
// has no AllowMethods builder (it manages the preflight method list
// internally), so the canonical Access-Control-Allow-Methods header is
// emitted by the companion mwCORSMethods middleware for wire parity.
func corsHandler() irisv12.Handler {
	return cors.New().
		AllowOrigin("*").
		AllowHeaders("*").
		Handler()
}

// mwCORSMethods advertises the canonical CORS method set so the header is
// byte-identical to the other adapters' CORS output, and short-circuits a
// preflight OPTIONS the same way the reference net/http CORS decorator does.
func mwCORSMethods() irisv12.Handler {
	return func(c irisv12.Context) {
		c.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		r := c.Request()
		if r.Method == http.MethodOptions && r.Header.Get("Access-Control-Request-Method") != "" {
			c.StatusCode(http.StatusNoContent)
			c.StopExecution()
			return
		}
		c.Next()
	}
}

// mwBasicAuth validates HTTP Basic credentials against the shared
// bench:bench header with a constant-time compare and emits the
// perfmatrix realm on failure. Expressed as an iris.Handler rather than
// basicauth.Default so the WWW-Authenticate realm matches the wire-parity
// constant in common.
func mwBasicAuth() irisv12.Handler {
	expect := []byte(common.BasicAuthHeader)
	return func(c irisv12.Context) {
		got := c.GetHeader("Authorization")
		if subtle.ConstantTimeCompare([]byte(got), expect) != 1 {
			c.Header("WWW-Authenticate", `Basic realm="`+common.BasicAuthRealm+`"`)
			c.StopWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

// mwCSRFCookie sets the security stack's CSRF cookie. The benched
// requests are read-only / idempotent, so this mirrors celeris's
// csrf-cookie issuance without rejecting the request (the loadgen client
// echoes no token).
func mwCSRFCookie() irisv12.Handler {
	return func(c irisv12.Context) {
		c.SetCookie(&http.Cookie{
			Name:     common.CSRFCookieName,
			Value:    "skip-token-bench",
			Path:     "/",
			HttpOnly: true,
		})
		c.Next()
	}
}

// mwSecureHeaders applies the canonical security-header set.
func mwSecureHeaders() irisv12.Handler {
	return func(c irisv12.Context) {
		c.Header("X-Content-Type-Options", "nosniff")
		c.Header("X-Frame-Options", "SAMEORIGIN")
		c.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		c.Header("X-XSS-Protection", "0")
		c.Next()
	}
}

// mwTimeout bounds the handler chain with a per-request context deadline,
// mirroring celeris's timeout middleware and the perfmatrix reference's
// dummy 30s timeout. Iris runs the chain on the request goroutine, so this
// installs the deadline on the request context rather than racing a
// watchdog goroutine — the benched handlers complete well under it, so the
// cost measured is the context plumbing, identical to the other stacks.
func mwTimeout(d time.Duration) irisv12.Handler {
	return func(c irisv12.Context) {
		ctx, cancel := context.WithTimeout(c.Request().Context(), d)
		defer cancel()
		c.ResetRequest(c.Request().WithContext(ctx))
		c.Next()
	}
}

// mwBodyLimit caps the request body at maxBytes for the fullstack stack,
// matching celeris's body-limit middleware. Iris ships LimitRequestBody on
// the Application; here it is applied per-stack as a handler so only the
// fullstack chain pays the cost.
func mwBodyLimit(maxBytes int64) irisv12.Handler {
	return func(c irisv12.Context) {
		c.SetMaxRequestBodySize(maxBytes)
		c.Next()
	}
}
