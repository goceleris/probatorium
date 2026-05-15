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
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/csrf"
	"github.com/goceleris/celeris/middleware/jwt"
	"github.com/goceleris/celeris/middleware/keyauth"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
)

// jwtSecret is the symmetric HMAC secret. Hardcoded — this is a
// validation refapp, not production. Walker doesn't currently mint
// JWTs so every JWT-gated request returns 401, exercising the reject
// path.
var jwtSecret = []byte("walker-validation-secret-not-for-production")

// apiKeys is the keyauth allowlist. Same intent as jwtSecret.
var apiKeys = []string{"walker-api-key-1", "walker-api-key-2"}

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	engineFlag := flag.String("engine", "auto", "engine: iouring | epoll | std | adaptive | auto")
	flag.Parse()

	engineType := resolveEngine(*engineFlag)

	srv := celeris.New(celeris.Config{
		Addr:            *bind,
		Engine:          engineType,
		Protocol:        celeris.HTTP1,
		AsyncHandlers:   true,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	})

	// Always-on middlewares.
	srv.Use(recovery.New())
	srv.Use(requestid.New())
	srv.Use(secure.New())

	// /public — no auth, exercises chain happy path.
	srv.GET("/public", func(c *celeris.Context) error {
		return c.JSON(200, map[string]any{"public": true})
	})

	// /api/* — gated by jwt. Walker is unauthed → 401.
	apiGroup := srv.Group("/api",
		jwt.New(jwt.Config{
			SigningKey: jwtSecret,
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

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("auth_jwt_csrf: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	fmt.Printf("ready addr=%s\n", *bind)
	if err := srv.Start(); err != nil {
		log.Fatalf("auth_jwt_csrf: start: %v", err)
	}
}
