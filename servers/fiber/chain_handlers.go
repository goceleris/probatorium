package main

import (
	"crypto/subtle"
	"io"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/goceleris/probatorium/servers/common"
)

// mountChainHandlers mounts the 4 middleware stacks under
// /chain/<stack>/{json,upload}. fiber v2 ships a full native middleware
// suite (fiber/v2/middleware/{requestid,logger,recover,basicauth,csrf,
// limiter}), but every probatorium server hand-rolls each step so the
// observable per-decorator overhead shape is identical across adapters —
// the bench measures uniform middleware cost across frameworks, not each
// ecosystem's idiomatic wrapper. The stack ordering and semantics mirror
// the celeris and net/http reference packages.
func mountChainHandlers(app *fiber.App) {
	jsonTerminal := func(c *fiber.Ctx) error {
		c.Set("Content-Type", "application/json")
		return c.Send([]byte(`{"message":"Hello, World!"}`))
	}
	uploadTerminal := func(c *fiber.Ctx) error {
		_ = c.Body()
		c.Set("Content-Type", "text/plain; charset=utf-8")
		return c.SendString("OK")
	}

	for _, stack := range common.ChainStacks {
		prefix := common.ChainStackPrefix(stack) // "/chain/<stack>/"
		grp := app.Group(prefix)
		for _, mw := range chainStack(stack) {
			grp.Use(mw)
		}
		grp.Get("json", jsonTerminal)
		grp.Post("upload", uploadTerminal)
	}
}

// chainStack returns the ordered middleware list for one stack. Each
// larger stack is a strict superset of the previous, matching the
// scenarios.ChainProfiles layering.
func chainStack(stack string) []fiber.Handler {
	api := []fiber.Handler{
		fiberRequestID,
		fiberLoggerDiscard,
		fiberRecovery,
		fiberCORS,
	}
	switch stack {
	case "api":
		return api
	case "auth":
		return append(api, fiberBasicAuth(common.BasicAuthUser, common.BasicAuthPass))
	case "security":
		auth := append(api, fiberBasicAuth(common.BasicAuthUser, common.BasicAuthPass))
		return append(auth, fiberCSRFSkip, fiberSecure)
	case "fullstack":
		auth := append(api, fiberBasicAuth(common.BasicAuthUser, common.BasicAuthPass))
		security := append(auth, fiberCSRFSkip, fiberSecure)
		return append(security, fiberRateLimit, fiberTimeoutDummy, fiberBodyLimit(10<<20))
	default:
		return api
	}
}

// fiberRequestID assigns X-Request-Id from the incoming header or a new
// UUID and mirrors it onto the response.
func fiberRequestID(c *fiber.Ctx) error {
	id := c.Get("X-Request-Id")
	if id == "" {
		id = uuid.NewString()
	}
	c.Set("X-Request-Id", id)
	return c.Next()
}

// fiberLoggerDiscard writes a one-liner to io.Discard so the logger's
// formatting cost shows up in the bench without polluting stderr.
func fiberLoggerDiscard(c *fiber.Ctx) error {
	_, _ = io.WriteString(io.Discard, c.Method()+" "+c.Path()+"\n")
	return c.Next()
}

// fiberRecovery defers recover() on the downstream handler.
func fiberRecovery(c *fiber.Ctx) error {
	defer func() {
		if rec := recover(); rec != nil {
			c.Status(http.StatusInternalServerError)
			_ = c.SendString("internal error")
		}
	}()
	return c.Next()
}

// fiberCORS sets allow-all CORS headers and short-circuits preflight.
func fiberCORS(c *fiber.Ctx) error {
	c.Set("Access-Control-Allow-Origin", "*")
	c.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
	c.Set("Access-Control-Allow-Headers", "*")
	if c.Method() == http.MethodOptions && c.Get("Access-Control-Request-Method") != "" {
		return c.SendStatus(http.StatusNoContent)
	}
	return c.Next()
}

// fiberBasicAuth enforces user:pass on the Authorization header,
// validating against the shared bench credential with a constant-time
// compare.
func fiberBasicAuth(user, pass string) fiber.Handler {
	expect := []byte(common.BasicAuthHeader) // "Basic base64(bench:bench)"
	_ = user
	_ = pass
	return func(c *fiber.Ctx) error {
		got := []byte(c.Get("Authorization"))
		if len(got) != len(expect) || subtle.ConstantTimeCompare(got, expect) != 1 {
			c.Set("WWW-Authenticate", `Basic realm="`+common.BasicAuthRealm+`"`)
			c.Status(http.StatusUnauthorized)
			return c.SendString("unauthorized")
		}
		return c.Next()
	}
}

// fiberCSRFSkip emits a CSRF cookie on every response but skips
// validation: loadgen cannot mint a valid token, so the security stack
// measures the cookie-set cost without rejecting every benched request.
func fiberCSRFSkip(c *fiber.Ctx) error {
	c.Cookie(&fiber.Cookie{Name: common.CSRFCookieName, Value: "skip-token-bench", Path: "/", HTTPOnly: true})
	return c.Next()
}

// fiberSecure emits the OWASP security-header set (mirrors the
// unrolled/secure default set used by the other adapters).
func fiberSecure(c *fiber.Ctx) error {
	c.Set("X-Content-Type-Options", "nosniff")
	c.Set("X-Frame-Options", "SAMEORIGIN")
	c.Set("Referrer-Policy", "strict-origin-when-cross-origin")
	c.Set("X-XSS-Protection", "0")
	return c.Next()
}

// chainLimiter is sized far above the bench's offered load so the rate
// limiter's Allow() branch is exercised on every request without ever
// shedding traffic — the cost is the token-bucket check, not the drop.
var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func fiberRateLimit(c *fiber.Ctx) error {
	if !chainLimiter.Allow() {
		c.Status(http.StatusTooManyRequests)
		return c.SendString("rate limited")
	}
	return c.Next()
}

// fiberTimeoutDummy approximates a timeout middleware. fiber v2's Ctx
// does not surface a context.CancelFunc per request without rebinding
// UserContext, so the observable cost here is a method call + branch,
// matching the competitor implementations.
func fiberTimeoutDummy(c *fiber.Ctx) error {
	_ = 30 * time.Second
	return c.Next()
}

// fiberBodyLimit rejects bodies over limit bytes.
func fiberBodyLimit(limit int) fiber.Handler {
	return func(c *fiber.Ctx) error {
		if len(c.Body()) > limit {
			c.Status(http.StatusRequestEntityTooLarge)
			return c.SendString("body too large")
		}
		return c.Next()
	}
}
