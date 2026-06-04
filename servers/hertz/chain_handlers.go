package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"strings"
	"time"

	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/goceleris/probatorium/servers/common"
)

// mountChainHandlers wires the four middleware-stack route groups onto h
// using hertz-native middleware (app.HandlerFunc + ctx.Next/ctx.Abort).
// Every probatorium Go adapter hand-rolls each step so the observable
// per-decorator overhead shape is identical across frameworks — the bench
// measures uniform middleware cost, not each ecosystem's idiomatic
// wrapper. The stack ordering and semantics mirror the gin / fiber /
// celeris reference packages exactly, so the cross-framework comparison
// tracks middleware cost, not ordering differences.
func mountChainHandlers(h *server.Hertz) {
	stacks := []struct {
		stack string
		mw    []app.HandlerFunc
	}{
		{"api", chainAPI()},
		{"auth", chainAuth()},
		{"security", chainSecurity()},
		{"fullstack", chainFullstack()},
	}
	for _, s := range stacks {
		prefix := common.ChainStackPrefix(s.stack)
		// TrimRight drops the trailing slash so RouterGroup paths join
		// cleanly: "/chain/api" + "/json" -> "/chain/api/json".
		g := h.Group(strings.TrimRight(prefix, "/"), s.mw...)
		g.GET("/json", chainJSONTerminal)
		g.POST("/upload", chainUploadTerminal)
	}
}

func chainJSONTerminal(_ context.Context, ctx *app.RequestContext) {
	ctx.Data(consts.StatusOK, "application/json", []byte(`{"message":"Hello, World!"}`))
}

func chainUploadTerminal(_ context.Context, ctx *app.RequestContext) {
	_ = ctx.Request.Body()
	ctx.Data(consts.StatusOK, "text/plain; charset=utf-8", []byte("OK"))
}

// Stack composition mirrors the celeris reference exactly: each tier
// layers additional middleware on top of the previous one, in the same
// order, so the only measured difference between stacks is the added
// middleware cost.
func chainAPI() []app.HandlerFunc {
	return []app.HandlerFunc{mwRequestID(), mwLoggerDiscard(), mwRecovery(), mwCORS()}
}
func chainAuth() []app.HandlerFunc {
	return append(chainAPI(), mwBasicAuth(common.BasicAuthUser, common.BasicAuthPass))
}
func chainSecurity() []app.HandlerFunc {
	return append(chainAuth(), mwCSRFSkip(), mwSecure())
}
func chainFullstack() []app.HandlerFunc {
	return append(chainSecurity(), mwRateLimit(), mwTimeoutDummy(), mwBodyLimit(10<<20))
}

func mwRequestID() app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		id := string(ctx.GetHeader("X-Request-Id"))
		if id == "" {
			id = uuid.NewString()
		}
		ctx.Header("X-Request-Id", id)
		ctx.Next(c)
	}
}

func mwLoggerDiscard() app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		_, _ = io.WriteString(io.Discard, string(ctx.Method())+" "+string(ctx.Path())+"\n")
		ctx.Next(c)
	}
}

func mwRecovery() app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		defer func() {
			if rec := recover(); rec != nil {
				ctx.AbortWithStatus(consts.StatusInternalServerError)
			}
		}()
		ctx.Next(c)
	}
}

func mwCORS() app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		ctx.Header("Access-Control-Allow-Origin", "*")
		ctx.Header("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		ctx.Header("Access-Control-Allow-Headers", "*")
		if string(ctx.Method()) == consts.MethodOptions && len(ctx.GetHeader("Access-Control-Request-Method")) > 0 {
			ctx.AbortWithStatus(consts.StatusNoContent)
			return
		}
		ctx.Next(c)
	}
}

const basicAuthHeaderPrefix = "Basic "

func mwBasicAuth(user, pass string) app.HandlerFunc {
	expect := []byte(base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)))
	realm := `Basic realm="` + common.BasicAuthRealm + `"`
	return func(c context.Context, ctx *app.RequestContext) {
		auth := string(ctx.GetHeader("Authorization"))
		if !strings.HasPrefix(auth, basicAuthHeaderPrefix) {
			ctx.Header("WWW-Authenticate", realm)
			ctx.AbortWithStatus(consts.StatusUnauthorized)
			return
		}
		got := []byte(auth[len(basicAuthHeaderPrefix):])
		if subtle.ConstantTimeCompare(got, expect) != 1 {
			ctx.Header("WWW-Authenticate", realm)
			ctx.AbortWithStatus(consts.StatusUnauthorized)
			return
		}
		ctx.Next(c)
	}
}

func mwCSRFSkip() app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		ctx.SetCookie(common.CSRFCookieName, "skip-token-bench", 0, "/", "", protocol.CookieSameSiteDisabled, false, true)
		ctx.Next(c)
	}
}

func mwSecure() app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		ctx.Header("X-Content-Type-Options", "nosniff")
		ctx.Header("X-Frame-Options", "SAMEORIGIN")
		ctx.Header("Referrer-Policy", "strict-origin-when-cross-origin")
		ctx.Header("X-XSS-Protection", "0")
		ctx.Next(c)
	}
}

// chainLimiter is sized far above the bench's offered load so the rate
// limiter's Allow() branch is exercised on every request without ever
// shedding traffic — the cost is the token-bucket check, not the drop.
var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func mwRateLimit() app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		if !chainLimiter.Allow() {
			ctx.AbortWithStatus(consts.StatusTooManyRequests)
			return
		}
		ctx.Next(c)
	}
}

// mwTimeoutDummy approximates a timeout middleware. The observable cost is
// a context derivation + branch, matching the gin/fiber implementations.
func mwTimeoutDummy() app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		tctx, cancel := context.WithTimeout(c, 30*time.Second)
		defer cancel()
		ctx.Next(tctx)
	}
}

// mwBodyLimit rejects bodies over limit bytes. hertz buffers the full
// request body, so len(Body()) is the realized size.
func mwBodyLimit(limit int) app.HandlerFunc {
	return func(c context.Context, ctx *app.RequestContext) {
		if len(ctx.Request.Body()) > limit {
			ctx.AbortWithStatus(consts.StatusRequestEntityTooLarge)
			return
		}
		ctx.Next(c)
	}
}
