// driver_handlers.go mounts the four Phase-2 driver-backed routes onto the
// celeris adapter, ported from the celeris reference
// (test/perfmatrix/servers/celeris/driver_handlers.go) and expressed with
// celeris's own in-tree driver packages — the idiomatic choice for a
// celeris user, and the one the reference adapter blesses:
//
//   - GET  /db/user/:id  — driver/postgres pool, SELECT by id, JSON row.
//   - GET  /cache/:key   — driver/redis, GET raw bytes.
//   - GET  /mc/:key      — driver/memcached, GET raw bytes.
//   - POST /session      — middleware/session over a redisstore backend:
//     load/merge/save a hit counter keyed by the pmsid cookie, JSON
//     {ok,seq} reply.
//
// Each celeris driver is opened WithEngine(srv) so that — when the server
// runs with AsyncHandlers — the driver auto-selects its direct net.Conn
// path and the handler goroutine parks on Go netpoll instead of blocking
// an I/O worker (see Config.AsyncHandlers doc). Pool size 16 per driver
// keeps the bench server-bound, matching the reference's
// WithMaxOpen(16)/WithPoolSize(16).
//
// Backends are addressed via the same PROBATORIUM_* environment variables
// the chi / gin / echo adapters document, because the probatorium binary
// takes only -bind/-engine flags; the orchestrator's service-provisioning
// step exports these before exec. An unset/unreachable backend makes the
// matching handler answer 503 (via AbortWithStatus) so loadgen counts
// errors deterministically — byte-for-byte the same contract the reference
// enforces with its nil-handle 503 guard.
//
// Clients are opened once at mount and reused for the process lifetime
// (the bench runs one adapter process per cell); driverClients.close lets
// the signal handler release them on shutdown.
package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/memcached"
	"github.com/goceleris/celeris/driver/postgres"
	"github.com/goceleris/celeris/driver/redis"
	"github.com/goceleris/celeris/middleware/session"
	"github.com/goceleris/celeris/middleware/session/redisstore"

	"github.com/goceleris/probatorium/servers/common"
)

// userRow mirrors the seeded users table row (id, name, email, score),
// matching the reference's userRow so the JSON body is identical across
// adapters.
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session. seq is the
// session's hit counter, incremented on every request carrying the same
// pmsid cookie. Shape matches the reference.
type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

// driverConfig holds the backend addresses read from the environment.
// Empty fields disable the matching route (handler returns 503).
type driverConfig struct {
	pgDSN     string
	redisAddr string
	mcAddr    string
}

// loadDriverConfig reads the backend wiring from the environment, with the
// same names and localhost defaults the chi/gin adapters use so a
// developer can smoke-test against a local stack without exporting
// anything.
func loadDriverConfig() driverConfig {
	return driverConfig{
		pgDSN:     envOr("PROBATORIUM_PG_DSN", "postgres://bench:bench@127.0.0.1:5432/bench?sslmode=disable"),
		redisAddr: envOr("PROBATORIUM_REDIS_ADDR", "127.0.0.1:6379"),
		mcAddr:    envOr("PROBATORIUM_MEMCACHED_ADDR", "127.0.0.1:11211"),
	}
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// driverClients are the lazily-opened, process-lifetime backend handles
// plus the per-route session middleware.
type driverClients struct {
	pg        *postgres.Pool
	redis     *redis.Client
	mc        *memcached.Client
	sessionMW celeris.HandlerFunc
}

// mountDriverHandlers opens the celeris drivers and attaches the four
// driver routes onto srv. A failed open leaves the handle nil and the
// route answers 503 (deterministic error), never a panic. The returned
// *driverClients lets the caller close the handles on shutdown.
func mountDriverHandlers(srv *celeris.Server) *driverClients {
	cfg := loadDriverConfig()
	c := &driverClients{}

	// Postgres pool, capped at 16 conns, engine-integrated. A
	// parse/connect failure yields a nil pool -> 503.
	if cfg.pgDSN != "" {
		if pool, err := postgres.Open(cfg.pgDSN,
			postgres.WithMaxOpen(16),
			postgres.WithEngine(srv),
		); err == nil {
			c.pg = pool
		}
	}

	// Redis client, pool size 16, engine-integrated.
	if cfg.redisAddr != "" {
		if rdb, err := redis.NewClient(cfg.redisAddr,
			redis.WithPoolSize(16),
			redis.WithEngine(srv),
		); err == nil {
			c.redis = rdb
		}
	}

	// Memcached client, max 16 open conns, engine-integrated.
	if cfg.mcAddr != "" {
		if mc, err := memcached.NewClient(cfg.mcAddr,
			memcached.WithMaxOpen(16),
			memcached.WithEngine(srv),
		); err == nil {
			c.mc = mc
		}
	}

	// Session middleware over a redisstore backend, available only when
	// Redis opened. The cookie name and idle timeout match the reference.
	if c.redis != nil {
		store := redisstore.New(c.redis)
		c.sessionMW = session.New(session.Config{
			Store:       store,
			CookieName:  common.SessionCookieName,
			IdleTimeout: 10 * time.Minute,
		})
	}

	srv.GET("/db/user/:id", c.dbUserHandler).Async()
	srv.GET("/cache/:key", c.cacheHandler).Async()
	srv.GET("/mc/:key", c.mcHandler).Async()

	// The session middleware is mounted as a per-route layer (not globally)
	// so its load/save round-trip only fires on /session requests. When
	// Redis is unavailable the route degrades to a deterministic 503.
	if c.sessionMW != nil {
		srv.POST("/session", sessionTerminal).Use(c.sessionMW).Async()
	} else {
		srv.POST("/session", func(ctx *celeris.Context) error {
			return ctx.AbortWithStatus(503)
		}).Async()
	}

	return c
}

// dbUserHandler serves GET /db/user/:id: SELECT id,name,email,score and
// JSON-encode the row. nil pool / query error -> 503; bad id -> 400.
func (c *driverClients) dbUserHandler(ctx *celeris.Context) error {
	if c.pg == nil {
		return ctx.AbortWithStatus(503)
	}
	id, err := strconv.Atoi(ctx.Param("id"))
	if err != nil {
		return ctx.AbortWithStatus(400)
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	var row userRow
	qerr := c.pg.QueryRow(qctx,
		"SELECT id, name, email, score FROM users WHERE id=$1", id,
	).Scan(&row.ID, &row.Name, &row.Email, &row.Score)
	if qerr != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.JSON(200, row)
}

// cacheHandler serves GET /cache/:key: Redis GET, raw bytes back.
func (c *driverClients) cacheHandler(ctx *celeris.Context) error {
	if c.redis == nil {
		return ctx.AbortWithStatus(503)
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	val, err := c.redis.GetBytes(qctx, ctx.Param("key"))
	if err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.Blob(200, "application/octet-stream", val)
}

// mcHandler serves GET /mc/:key: memcached GET, raw bytes back.
func (c *driverClients) mcHandler(ctx *celeris.Context) error {
	if c.mc == nil {
		return ctx.AbortWithStatus(503)
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	val, err := c.mc.GetBytes(qctx, ctx.Param("key"))
	if err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.Blob(200, "application/octet-stream", val)
}

// sessionTerminal is the inner handler the session middleware wraps (via
// Route.Use): the middleware loads the session, calls c.Next() into this
// terminal, then saves on the way out. A loadgen client that reuses the
// pmsid cookie observes a monotonically increasing seq, proving the store
// round-trip. This terminal merges the request body if it is JSON, bumps
// seq, and replies {ok,seq}.
func sessionTerminal(ctx *celeris.Context) error {
	sess := session.FromContext(ctx)
	if sess == nil {
		return ctx.AbortWithStatus(503)
	}
	if body := ctx.Body(); len(body) > 0 {
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err == nil {
			for k, v := range payload {
				sess.Set(k, v)
			}
		}
	}
	seq := sess.GetInt("seq") + 1
	sess.Set("seq", seq)
	return ctx.JSON(200, sessionResponse{OK: true, Seq: seq})
}

// close releases any opened driver handles. Safe to call with nil fields.
func (c *driverClients) close() {
	if c == nil {
		return
	}
	if c.pg != nil {
		_ = c.pg.Close()
	}
	if c.redis != nil {
		_ = c.redis.Close()
	}
	if c.mc != nil {
		_ = c.mc.Close()
	}
}
