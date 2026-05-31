package main

import (
	"context"
	"crypto/subtle"
	"encoding/base64"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"golang.org/x/time/rate"

	"github.com/goceleris/probatorium/servers/common"
)

// Cookie names are sourced from the shared contract so the wire bytes
// match every other adapter. sessionCookieName is used by the driver
// session route; csrfCookieName by the security/fullstack chains.
const (
	sessionCookieName = common.SessionCookieName
	csrfCookieName    = common.CSRFCookieName
)

// mountChainHandlers wires the four middleware-stack route groups onto r.
// Unlike the net/http decorator adapters (stdhttp / chi), the gin variant
// uses gin's native middleware (gin.HandlerFunc + c.Next/c.Abort) so the
// stacks are gin-idiomatic. The composition order is identical to the
// celeris reference, so the cross-framework comparison tracks middleware
// cost, not ordering differences.
func mountChainHandlers(r *gin.Engine) {
	stacks := []struct {
		stack string
		mw    []gin.HandlerFunc
	}{
		{"api", chainAPI()},
		{"auth", chainAuth()},
		{"security", chainSecurity()},
		{"fullstack", chainFullstack()},
	}
	for _, s := range stacks {
		prefix := common.ChainStackPrefix(s.stack)
		// strings.TrimRight drops the trailing slash so RouterGroup paths
		// join cleanly: "/chain/api" + "/json" -> "/chain/api/json".
		g := r.Group(strings.TrimRight(prefix, "/"), s.mw...)
		g.GET("/json", chainJSONTerminal)
		g.POST("/upload", chainUploadTerminal)
	}
}

func chainJSONTerminal(c *gin.Context) {
	c.Data(http.StatusOK, "application/json", []byte(`{"message":"Hello, World!"}`))
}

func chainUploadTerminal(c *gin.Context) {
	_, _ = io.Copy(io.Discard, c.Request.Body)
	c.Data(http.StatusOK, "text/plain; charset=utf-8", []byte("OK"))
}

// Stack composition mirrors the celeris reference exactly: each tier
// layers additional middleware on top of the previous one, in the same
// order, so the only measured difference between stacks is the added
// middleware cost.
func chainAPI() []gin.HandlerFunc {
	return []gin.HandlerFunc{mwRequestID(), mwLoggerDiscard(), mwRecovery(), mwCORS()}
}
func chainAuth() []gin.HandlerFunc {
	return append(chainAPI(), mwBasicAuth(common.BasicAuthUser, common.BasicAuthPass))
}
func chainSecurity() []gin.HandlerFunc {
	return append(chainAuth(), mwCSRFSkip(), mwSecure())
}
func chainFullstack() []gin.HandlerFunc {
	return append(chainSecurity(), mwRateLimit(), mwTimeoutDummy(), mwBodyLimit(10<<20))
}

func mwRequestID() gin.HandlerFunc {
	return func(c *gin.Context) {
		id := c.GetHeader("X-Request-Id")
		if id == "" {
			id = uuid.NewString()
		}
		c.Header("X-Request-Id", id)
		c.Next()
	}
}

func mwLoggerDiscard() gin.HandlerFunc {
	return func(c *gin.Context) {
		_, _ = io.WriteString(io.Discard, c.Request.Method+" "+c.Request.URL.Path+"\n")
		c.Next()
	}
}

func mwRecovery() gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if rec := recover(); rec != nil {
				c.AbortWithStatus(http.StatusInternalServerError)
			}
		}()
		c.Next()
	}
}

func mwCORS() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("Access-Control-Allow-Origin", "*")
		h.Set("Access-Control-Allow-Methods", "GET,POST,PUT,PATCH,DELETE,OPTIONS")
		h.Set("Access-Control-Allow-Headers", "*")
		if c.Request.Method == http.MethodOptions && c.GetHeader("Access-Control-Request-Method") != "" {
			c.AbortWithStatus(http.StatusNoContent)
			return
		}
		c.Next()
	}
}

const basicAuthHeaderPrefix = "Basic "

func mwBasicAuth(user, pass string) gin.HandlerFunc {
	expect := []byte(base64.StdEncoding.EncodeToString([]byte(user + ":" + pass)))
	realm := `Basic realm="` + common.BasicAuthRealm + `"`
	return func(c *gin.Context) {
		auth := c.GetHeader("Authorization")
		if !strings.HasPrefix(auth, basicAuthHeaderPrefix) {
			c.Header("WWW-Authenticate", realm)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		got := []byte(auth[len(basicAuthHeaderPrefix):])
		if subtle.ConstantTimeCompare(got, expect) != 1 {
			c.Header("WWW-Authenticate", realm)
			c.AbortWithStatus(http.StatusUnauthorized)
			return
		}
		c.Next()
	}
}

func mwCSRFSkip() gin.HandlerFunc {
	return func(c *gin.Context) {
		http.SetCookie(c.Writer, &http.Cookie{Name: csrfCookieName, Value: "skip-token-bench", Path: "/", HttpOnly: true})
		c.Next()
	}
}

func mwSecure() gin.HandlerFunc {
	return func(c *gin.Context) {
		h := c.Writer.Header()
		h.Set("X-Content-Type-Options", "nosniff")
		h.Set("X-Frame-Options", "SAMEORIGIN")
		h.Set("Referrer-Policy", "strict-origin-when-cross-origin")
		h.Set("X-XSS-Protection", "0")
		c.Next()
	}
}

var chainLimiter = rate.NewLimiter(rate.Limit(1_000_000), 1_000_000)

func mwRateLimit() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !chainLimiter.Allow() {
			c.AbortWithStatus(http.StatusTooManyRequests)
			return
		}
		c.Next()
	}
}

func mwTimeoutDummy() gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), 30*time.Second)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

func mwBodyLimit(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		c.Next()
	}
}
