// Command auth_jwt_csrf is the JWT + CSRF + keyauth validation refapp.
// Complements kitchen_sink (which covers session-cookie auth via
// basicauth) and auth_session_ratelimit (which covers session middleware
// directly) — together they exercise every auth-related celeris
// middleware in the Tier 1 walker traffic.
//
// Coverage per probatorium#103:
//   - jwt       (Authorization: Bearer <token> required on /api/*)
//   - csrf      (synchronizer-token pattern on state-mutating verbs)
//   - keyauth   (X-API-Key required on /key/*)
//   - secure    (always-on)
//   - recovery  (always-on)
//   - requestid (per-req tagging)
//
// Walker behavior:
//   - GET /jwt-public: 200 (no auth — exercises the chain happy path)
//   - GET /api/*: 401 without bearer, 200 with (exercises jwt reject)
//   - POST /csrf-protected: 403 without token, 200 with
//   - GET /key/*: 401 without X-API-Key, 200 with
//
// On startup the refapp prints the canonical ready line:
//
//	ready addr=<bind-addr>
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/csrf"
	"github.com/goceleris/celeris/middleware/jwt"
	"github.com/goceleris/celeris/middleware/keyauth"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/probatorium/validation/refapp/internal/debugvars"
)

// jwtSecret is the symmetric HMAC secret. Hardcoded — this is a
// validation refapp, not production.
var jwtSecret = []byte("walker-validation-secret-not-for-production")

// jwtTokenTTL is how long a token minted at /jwt-token stays valid.
//
// Short on purpose. The walker fires continuously and carries the cookie in
// its jar, so a few seconds of validity means every walker crosses the expiry
// boundary many times a minute: the middleware's ACCEPT path runs (it used to
// be dead — nothing ever presented a valid token, so every /api request 401'd
// and I-MW-JWT's whole reason for existing went unexercised) and so does the
// reject-after-expiry path the predicate actually judges.
const jwtTokenTTL = 5 * time.Second

// jwtExpiryLeeway is how far past exp an admission is tolerated before it
// counts as a late admit. Non-zero because RFC 7519 §4.1.4 allows a verifier
// some clock leeway and because the refapp re-reads exp a moment after the
// middleware did; one second is far below jwtTokenTTL, so a middleware that
// ignores exp is still caught on essentially every request.
const jwtExpiryLeeway = time.Second

// jwtCookieName is where /jwt-token puts the minted token. A cookie and not
// an Authorization header because the Tier 1 walker carries a cookie jar and
// sends no custom headers — this is what makes the token reach /api/* at all.
const jwtCookieName = "jwt"

// apiKeys is the keyauth allowlist. Same intent as jwtSecret.
var apiKeys = []string{"walker-api-key-1", "walker-api-key-2"}

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	engineFlag := flag.String("engine", "auto", "engine: iouring | epoll | std | adaptive | auto")
	workersFlag := flag.Int("workers", 0, "io worker count (0 = celeris default GOMAXPROCS); celeris requires >=2 if set")
	flag.Parse()

	engineType := resolveEngine(*engineFlag)

	dv := debugvars.New() // /debug/vars + /debug/pprof for the validator's property loop
	srv := dv.NewServer(celeris.Config{
		Addr:            *bind,
		Engine:          engineType,
		Workers:         *workersFlag,
		Protocol:        celeris.HTTP1,
		AsyncHandlers:   true,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	})

	// Always-on middlewares.
	// Recovery middleware logger: explicit io.Discard sink, NOT
	// slog.Default(). The stdlib default routes through Go's text
	// handler whose defaultHandler mutex serializes a blocking
	// os.Stderr.Write across every conn + worker; under iouring/epoll's
	// per-conn async-dispatch model that stderr lock is held inside
	// cs.detachMu (around ProcessH1), gating the worker thread and
	// letting concurrent slowloris header-deadlines slip past.
	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv.Use(recovery.New(recovery.Config{Logger: dv.RecoveryLogger(discardLog)}))
	srv.Use(requestid.New())
	srv.Use(secure.New())

	// /public — no auth, exercises chain happy path.
	srv.GET("/public", func(c *celeris.Context) error {
		return c.JSON(200, map[string]any{"public": true})
	})

	// /jwt-token — mints a short-lived HS256 token into the walker's cookie
	// jar. Without it nothing ever presents a VALID token and I-MW-JWT
	// judges an accept path that never runs.
	srv.GET("/jwt-token", func(c *celeris.Context) error {
		exp := time.Now().Add(jwtTokenTTL)
		tok, err := mintHS256(jwtSecret, exp)
		if err != nil {
			return c.JSON(500, map[string]string{"error": "mint: " + err.Error()})
		}
		c.SetCookie(&celeris.Cookie{
			Name:     jwtCookieName,
			Value:    tok,
			Path:     "/",
			MaxAge:   int(jwtTokenTTL.Seconds()),
			HTTPOnly: true,
		})
		return c.JSON(200, map[string]any{"expires_at": exp.UTC().Format(time.RFC3339)})
	})

	// /api/* — gated by jwt. Unauthed walkers 401; walkers that passed
	// through /jwt-token carry the cookie and are admitted until it expires.
	apiGroup := srv.Group("/api",
		jwt.New(jwt.Config{
			SigningKey: jwtSecret,
			// Header first (the conventional source), cookie second so
			// the cookie-jar walker can reach the accept path.
			TokenLookup: "header:Authorization:Bearer ,cookie:" + jwtCookieName,
			// I-MW-JWT: the middleware just decided this token is good.
			// Re-read its exp INDEPENDENTLY -- from the raw token, not
			// from whatever the middleware parsed -- and count every
			// admission that is already past it. That is the invariant
			// the predicate states ("rejects every token past its
			// expiry") and it needs no -tags=validation build.
			SuccessHandler: func(c *celeris.Context) {
				dv.JWTValidated(true)
				if exp, ok := tokenExpiry(rawToken(c)); ok && time.Now().After(exp.Add(jwtExpiryLeeway)) {
					dv.RecordJWTLateAdmit()
				}
			},
			ErrorHandler: func(_ *celeris.Context, err error) error {
				dv.JWTValidated(false)
				return err // unchanged 401; the handler only counts
			},
		}),
	)
	apiGroup.GET("/me", func(c *celeris.Context) error {
		return c.JSON(200, map[string]any{"authed": true})
	})
	apiGroup.GET("/users", func(c *celeris.Context) error {
		return c.JSON(200, map[string]any{"users": []string{}})
	})

	// /key/* — gated by keyauth via X-API-Key header.
	keyGroup := srv.Group("/key",
		keyauth.New(keyauth.Config{
			KeyLookup: "header:X-API-Key",
			Validator: func(_ *celeris.Context, key string) (bool, error) {
				for _, k := range apiKeys {
					if k == key {
						return true, nil
					}
				}
				return false, nil
			},
		}),
	)
	keyGroup.GET("/whoami", func(c *celeris.Context) error {
		return c.JSON(200, map[string]any{"key_authed": true})
	})

	// /csrf-protected — gated by csrf. Walker doesn't fetch the token
	// first; POSTs return 403. GET fetches a token in the cookie +
	// returns the same in the response body.
	csrfMW := csrf.New()
	srv.GET("/csrf-token", csrfMW, func(c *celeris.Context) error {
		// csrf middleware sets the cookie; handler returns the token
		// value (via request context) for the walker to optionally
		// echo back.
		return c.JSON(200, map[string]any{"hint": "fetch + echo via X-CSRF-Token header"})
	})
	srv.POST("/csrf-protected", csrfMW, func(c *celeris.Context) error {
		return c.JSON(200, map[string]any{"posted": true})
	})

	// The matrix's only JWT refapp, so the only cell that can judge
	// I-MW-JWT. Every other refapp leaves the jwt_* counters at zero and
	// stays silent, and the checker reports the predicate as
	// not-instrumented there rather than passing it on an input nothing
	// writes to (probatorium#297).
	dv.Declare("I-MW-JWT")

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("auth_jwt_csrf: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("auth_jwt_csrf: listen: %v", err)
	}
	fmt.Printf("ready addr=%s\n", ln.Addr().String())
	if err := srv.StartWithListener(ln); err != nil {
		log.Fatalf("auth_jwt_csrf: start: %v", err)
	}
}

// mintHS256 builds a compact HS256 JWS carrying a single `exp` claim.
//
// Hand-rolled rather than borrowed from celeris: the parser under test lives
// in middleware/jwt/internal/jwtparse, and an oracle that mints with the same
// code it is checking cannot catch a bug that is symmetric across both. Thirty
// lines of stdlib keep the two sides independent.
func mintHS256(secret []byte, exp time.Time) (string, error) {
	header, err := json.Marshal(map[string]string{"alg": "HS256", "typ": "JWT"})
	if err != nil {
		return "", err
	}
	claims, err := json.Marshal(map[string]int64{"exp": exp.Unix(), "iat": time.Now().Unix()})
	if err != nil {
		return "", err
	}
	enc := base64.RawURLEncoding
	signing := enc.EncodeToString(header) + "." + enc.EncodeToString(claims)
	mac := hmac.New(sha256.New, secret)
	_, _ = mac.Write([]byte(signing))
	return signing + "." + enc.EncodeToString(mac.Sum(nil)), nil
}

// rawToken returns the token the request presented, trying the same two
// sources as the middleware's TokenLookup and in the same order.
func rawToken(c *celeris.Context) string {
	if h := c.Header("Authorization"); strings.HasPrefix(h, "Bearer ") {
		return strings.TrimPrefix(h, "Bearer ")
	}
	if v, err := c.Cookie(jwtCookieName); err == nil {
		return v
	}
	return ""
}

// tokenExpiry decodes the `exp` claim out of a compact JWS WITHOUT verifying
// the signature: the middleware has already vouched for that, and what is
// under test here is whether it honoured the expiry it just read. ok=false
// when the token carries no usable exp, which is not a verdict either way.
func tokenExpiry(token string) (time.Time, bool) {
	parts := strings.Split(token, ".")
	if len(parts) != 3 {
		return time.Time{}, false
	}
	payload, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return time.Time{}, false
	}
	var claims struct {
		Exp int64 `json:"exp"`
	}
	if err := json.Unmarshal(payload, &claims); err != nil || claims.Exp == 0 {
		return time.Time{}, false
	}
	return time.Unix(claims.Exp, 0), true
}
