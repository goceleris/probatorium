// chain_handlers.go mounts the four Phase-2 middleware stacks (api, auth,
// security, fullstack) under /chain/<stack>/{json,upload}, ported from the
// celeris reference (test/perfmatrix/servers/celeris/chain_handlers.go)
// and expressed with celeris's own in-tree middleware/ packages — so the
// bench numbers reflect the celeris hot path end-to-end, the way a
// perf-aware celeris user would actually deploy.
//
// Each stack is a strict superset of the one before it, so scenarios
// differ only by middleware depth — the same 4-step ladder the reference
// uses:
//
//	api       = requestid -> logger(discard) -> recovery -> cors
//	auth      = api       + basicauth(bench:bench)
//	security  = auth      + csrf(skip-validate) -> secure
//	fullstack = security  + ratelimit -> timeout -> bodylimit(10MB)
//
// Credentials, cookie names, and the WWW-Authenticate realm come from
// servers/common so no value can drift between the adapter and the loadgen
// side. The csrf layer is configured to skip validation (the stateless
// loadgen carries no token) while still running, so its per-request cost
// is measured without rejecting authenticated requests — the documented
// choice the reference makes too. The ratelimit RPS/burst are set far
// above any bench load so the limiter's hot-path cost is measured without
// ever denying a request; its eviction goroutine's lifetime is tied to the
// server lifetime context so repeat cells don't leak goroutines.
package main

import (
	"context"
	"io"
	"log/slog"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/basicauth"
	"github.com/goceleris/celeris/middleware/bodylimit"
	"github.com/goceleris/celeris/middleware/cors"
	"github.com/goceleris/celeris/middleware/csrf"
	"github.com/goceleris/celeris/middleware/logger"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/timeout"

	"github.com/goceleris/probatorium/servers/common"
)

// discardSlog drops every record via celeris's zero-allocation FastHandler:
// the bench reflects logger formatter/dispatch cost without disk/stderr IO.
var discardSlog = slog.New(logger.NewFastHandler(io.Discard, nil))

// mountChainHandlers mounts every stack in common.ChainStacks under its
// canonical prefix. The terminal handlers are shared across stacks so the
// only measured difference is middleware cost. lifetime bounds the
// ratelimit eviction goroutine.
func mountChainHandlers(srv *celeris.Server, lifetime context.Context) {
	stacks := map[string][]celeris.HandlerFunc{
		"api":       chainAPI(),
		"auth":      chainAuth(),
		"security":  chainSecurity(),
		"fullstack": chainFullstack(lifetime),
	}

	for _, name := range common.ChainStacks {
		mws, ok := stacks[name]
		if !ok {
			continue
		}
		// common.ChainStackPrefix yields e.g. "/chain/api/"; the group
		// prefix drops the trailing slash and the routes re-add it.
		grp := srv.Group("/chain/"+name, mws...)
		grp.GET("/json", chainJSONTerminal)
		grp.POST("/upload", chainUploadTerminal)
	}
}

// chainJSONTerminal is the GET terminal: the canonical /json body.
func chainJSONTerminal(c *celeris.Context) error {
	return c.Blob(200, "application/json", []byte(`{"message":"Hello, World!"}`))
}

// chainUploadTerminal is the POST terminal: drain the (already
// body-limited, in the fullstack stack) request body and reply "OK".
func chainUploadTerminal(c *celeris.Context) error {
	_ = c.Body()
	return c.Blob(200, "text/plain; charset=utf-8", []byte("OK"))
}

// --- stack assemblies (each a superset of the previous) ---

func chainAPI() []celeris.HandlerFunc {
	return []celeris.HandlerFunc{
		requestid.New(),
		logger.New(logger.Config{Output: discardSlog}),
		recovery.New(),
		cors.New(),
	}
}

func chainAuth() []celeris.HandlerFunc {
	return append(chainAPI(), basicauth.New(basicauth.Config{
		Users: map[string]string{common.BasicAuthUser: common.BasicAuthPass},
		Realm: common.BasicAuthRealm,
	}))
}

func chainSecurity() []celeris.HandlerFunc {
	return append(chainAuth(),
		csrf.New(csrf.Config{
			CookieName: common.CSRFCookieName,
			// Skip validation: the stateless loadgen carries no token, so
			// we measure the middleware's cost without rejecting requests.
			Skip: func(*celeris.Context) bool { return true },
		}),
		secure.New(),
	)
}

func chainFullstack(lifetime context.Context) []celeris.HandlerFunc {
	return append(chainSecurity(),
		ratelimit.New(ratelimit.Config{
			RPS:            1_000_000,
			Burst:          1_000_000,
			CleanupContext: lifetime,
		}),
		timeout.New(timeout.Config{Timeout: 30 * time.Second}),
		bodylimit.New(bodylimit.Config{Limit: "10MB"}),
	)
}
