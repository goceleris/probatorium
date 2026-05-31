package main

import (
	"context"
	"crypto/subtle"
	"io"
	"net/http"
	"time"

	echov4 "github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/goceleris/probatorium/servers/common"
)

// chainBodyLimit is the upload ceiling the fullstack stack enforces, matching
// the reference celeris/stdhttp stacks (10 MiB).
const chainBodyLimit = "10M"

// rateLimitBurst is sized so the limiter never throttles the bench: the chain
// scenarios are a middleware-overhead comparison, not a 429 test. A huge
// bucket keeps the limiter's per-request bookkeeping on the hot path (the
// cost we want to measure) while admitting every request.
const rateLimitBurst = 1_000_000

// registerChainHandlers mounts the four middleware stacks under
// /chain/<stack>/{json,upload}. Each stack is built from Echo's own
// middleware where the framework ships it natively; the one piece Echo's
// core module does not provide for a token-less hot path (CSRF) is reduced
// to a cookie-emit stand-in, exactly as the reference adapters do — loadgen
// cannot supply real CSRF tokens, so a token-validating middleware would
// reject every request and the cell would record only 403s.
//
// Stack composition (cumulative, mirrors scenarios.ChainProfiles):
//
//   - api:       requestid + logger + recovery + cors
//   - auth:      api + basicauth
//   - security:  auth + csrf + secure
//   - fullstack: security + ratelimit + timeout + bodylimit
func registerChainHandlers(e *echov4.Echo) {
	stacks := map[string][]echov4.MiddlewareFunc{
		"api":       chainAPI(),
		"auth":      chainAuth(),
		"security":  chainSecurity(),
		"fullstack": chainFullstack(),
	}

	for _, stack := range common.ChainStacks {
		mws := stacks[stack]
		g := e.Group(common.ChainStackPrefix(stack), mws...)
		g.GET("json", chainJSON)
		g.POST("upload", chainUpload)
	}
}

// chainJSON is the terminal handler for GET /chain/<stack>/json: the canonical
// small /json body.
func chainJSON(c echov4.Context) error {
	return c.Blob(http.StatusOK, "application/json", []byte(`{"message":"Hello, World!"}`))
}

// chainUpload is the terminal handler for POST /chain/<stack>/upload: drain
// the body (so the body path is part of the measured cost) and ack with "OK".
func chainUpload(c echov4.Context) error {
	_, _ = io.Copy(io.Discard, c.Request().Body)
	return c.String(http.StatusOK, "OK")
}

func chainAPI() []echov4.MiddlewareFunc {
	return []echov4.MiddlewareFunc{
		middleware.RequestID(),
		// Discard the access log so logging formatting cost is on the hot
		// path without I/O contention skewing the comparison.
		middleware.LoggerWithConfig(middleware.LoggerConfig{Output: io.Discard}),
		middleware.Recover(),
		middleware.CORSWithConfig(middleware.CORSConfig{
			AllowOrigins: []string{"*"},
			AllowMethods: []string{
				http.MethodGet, http.MethodPost, http.MethodPut,
				http.MethodPatch, http.MethodDelete, http.MethodOptions,
			},
		}),
	}
}

func chainAuth() []echov4.MiddlewareFunc {
	return append(chainAPI(), basicAuthMW())
}

func chainSecurity() []echov4.MiddlewareFunc {
	return append(chainAuth(), csrfEmitMW(), middleware.Secure())
}

func chainFullstack() []echov4.MiddlewareFunc {
	return append(chainSecurity(),
		middleware.RateLimiter(middleware.NewRateLimiterMemoryStore(rateLimitBurst)),
		timeoutMW(30*time.Second),
		middleware.BodyLimit(chainBodyLimit),
	)
}

// basicAuthMW validates the shared bench:bench credential in constant time.
// The expected values come from the common contract so the wire credential
// cannot drift from what loadgen sends.
func basicAuthMW() echov4.MiddlewareFunc {
	return middleware.BasicAuthWithConfig(middleware.BasicAuthConfig{
		Realm: common.BasicAuthRealm,
		Validator: func(user, pass string, _ echov4.Context) (bool, error) {
			ok := subtle.ConstantTimeCompare([]byte(user), []byte(common.BasicAuthUser)) == 1
			ok = subtle.ConstantTimeCompare([]byte(pass), []byte(common.BasicAuthPass)) == 1 && ok
			return ok, nil
		},
	})
}

// csrfEmitMW is the CSRF hot-path stand-in: it emits the _csrf cookie on the
// happy path without demanding a token loadgen cannot produce. Echo's real
// middleware.CSRF requires a token round-trip; reducing it to a cookie emit
// keeps the per-request cookie-write cost while staying loadgen-driveable —
// the same compromise every reference adapter makes for this stack.
func csrfEmitMW() echov4.MiddlewareFunc {
	return func(next echov4.HandlerFunc) echov4.HandlerFunc {
		return func(c echov4.Context) error {
			c.SetCookie(&http.Cookie{
				Name:     common.CSRFCookieName,
				Value:    "skip-token-bench",
				Path:     "/",
				HttpOnly: true,
			})
			return next(c)
		}
	}
}

// timeoutMW caps request handling at d. Echo's middleware.Timeout spins a
// watchdog goroutine per request; this lighter variant just bounds the
// request context, matching the reference stacks' dummy-timeout intent
// (bench requests complete well within the window).
func timeoutMW(d time.Duration) echov4.MiddlewareFunc {
	return func(next echov4.HandlerFunc) echov4.HandlerFunc {
		return func(c echov4.Context) error {
			ctx, cancel := context.WithTimeout(c.Request().Context(), d)
			defer cancel()
			c.SetRequest(c.Request().WithContext(ctx))
			return next(c)
		}
	}
}
