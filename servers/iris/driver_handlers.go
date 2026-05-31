package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jackc/pgx/v5/pgxpool"
	irisv12 "github.com/kataras/iris/v12"
	"github.com/kataras/iris/v12/sessions"
	"github.com/redis/go-redis/v9"

	"github.com/goceleris/probatorium/servers/common"
)

// driverClients holds the lazily-constructed driver handles the four
// driver scenarios round-trip against. A nil field means the service is
// unconfigured (its env var was empty or the client failed to open), so
// the handler degrades to 503 — loadgen then counts the cell as an error
// deterministically rather than measuring a phantom 0-byte success.
//
// The iris adapter deliberately uses community-standard clients — pgx
// for Postgres, go-redis for Redis, gomemcache for memcached, and iris's
// own sessions package — so a chain cell reflects iris + idiomatic driver
// cost, never celeris in-tree drivers leaking into a competitor column.
type driverClients struct {
	pg       *pgxpool.Pool
	redis    *redis.Client
	mc       *memcache.Client
	sessions *sessions.Sessions
}

// Service-endpoint env vars. Mirrors the perfmatrix services.FromEnv
// contract (PERFMATRIX_*) under the probatorium namespace. Unset vars
// yield a nil client and a 503 on the corresponding route.
const (
	envPGDSN     = "PROBATORIUM_PG_DSN"
	envRedisAddr = "PROBATORIUM_REDIS_ADDR"
	envMCAddr    = "PROBATORIUM_MC_ADDR"
)

// userRow mirrors the seeded users table row (id, name, email, score).
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session; Seq is the
// per-cookie hit counter the session middleware increments each request.
type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

// newDriverClients opens every configured driver client. Open failures
// (bad DSN, unreachable host) leave the field nil so the route returns
// 503 instead of panicking at registration time. Pool size 16 per driver
// keeps the bench server-bound, not client-bound.
func newDriverClients() *driverClients {
	dc := &driverClients{}

	if dsn := os.Getenv(envPGDSN); dsn != "" {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err == nil {
			cfg.MaxConns = 16
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if pool, perr := pgxpool.NewWithConfig(ctx, cfg); perr == nil {
				dc.pg = pool
			}
			cancel()
		}
	}

	if addr := os.Getenv(envRedisAddr); addr != "" {
		dc.redis = redis.NewClient(&redis.Options{Addr: addr, PoolSize: 16})
	}

	if addr := os.Getenv(envMCAddr); addr != "" {
		mc := memcache.New(addr)
		mc.MaxIdleConns = 16
		dc.mc = mc
	}

	// The session store rides on Redis when it is configured; otherwise
	// POST /session degrades to 503 like the other unconfigured routes.
	if dc.redis != nil {
		dc.sessions = sessions.New(sessions.Config{
			Cookie:  common.SessionCookieName,
			Expires: 10 * time.Minute,
		})
	}

	return dc
}

// mountDriverHandlers wires the four driver-backed routes onto app. Path
// templates from common.DriverEndpoints are translated to iris syntax
// once here (:id -> {id:int}, :key -> {key}).
func mountDriverHandlers(app *irisv12.Application, dc *driverClients) {
	// Postgres: GET /db/user/{id}
	app.Get("/db/user/{id:int}", func(c irisv12.Context) {
		if dc.pg == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		id, _ := c.Params().GetInt("id")
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		row := userRow{}
		err := dc.pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score)
		if err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/json")
		_ = c.JSON(row)
	})

	// Redis: GET /cache/{key}
	app.Get("/cache/{key}", func(c irisv12.Context) {
		if dc.redis == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		val, err := dc.redis.Get(ctx, c.Params().Get("key")).Bytes()
		if err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/octet-stream")
		_, _ = c.Write(val)
	})

	// Memcached: GET /mc/{key}
	app.Get("/mc/{key}", func(c irisv12.Context) {
		if dc.mc == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		it, err := dc.mc.Get(c.Params().Get("key"))
		if err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/octet-stream")
		_, _ = c.Write(it.Value)
	})

	// Session: POST /session — iris's own session middleware, started
	// inline so the hit counter only fires on this route.
	app.Post("/session", func(c irisv12.Context) {
		if dc.sessions == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		sess := dc.sessions.Start(c)
		body, _ := io.ReadAll(c.Request().Body)
		if len(body) > 0 {
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err == nil {
				for k, v := range payload {
					sess.Set(k, v)
				}
			}
		}
		seq := sess.Increment("seq", 1)
		c.ContentType("application/json")
		_ = c.JSON(sessionResponse{OK: true, Seq: seq})
	})
}

// closeDriverClients releases every open driver handle. Called on
// shutdown so repeated start/stop cycles in tests don't leak pool
// connections.
func closeDriverClients(dc *driverClients) {
	if dc == nil {
		return
	}
	if dc.pg != nil {
		dc.pg.Close()
		dc.pg = nil
	}
	if dc.redis != nil {
		_ = dc.redis.Close()
		dc.redis = nil
	}
	dc.mc = nil
	dc.sessions = nil
}
