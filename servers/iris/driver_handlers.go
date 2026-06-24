package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jackc/pgx/v5/pgxpool"
	irisv12 "github.com/kataras/iris/v12"
	"github.com/redis/go-redis/v9"
)

// sessionKey is the fixed key POST /session round-trips against, so the
// workload is a load+merge+save of one seeded blob — identical to the
// other adapters.
const sessionKey = "pmsess:bench"

// driverClients holds the lazily-constructed driver handles the four
// driver scenarios round-trip against. A nil field means the service is
// unconfigured (its env var was empty or the client failed to open), so
// the handler degrades to 503 — loadgen then counts the cell as an error
// deterministically rather than measuring a phantom 0-byte success.
//
// The iris adapter deliberately uses community-standard clients — pgx
// for Postgres, go-redis for Redis, gomemcache for memcached — so a chain
// cell reflects iris + idiomatic driver cost, never celeris in-tree
// drivers leaking into a competitor column.
type driverClients struct {
	pg    *pgxpool.Pool
	redis *redis.Client
	mc    *memcache.Client
}

// Service-endpoint env vars. Mirrors the perfmatrix services.FromEnv
// contract (PERFMATRIX_*) under the probatorium namespace. Unset vars
// yield a nil client and a 503 on the corresponding route.
const (
	envPGDSN     = "PROBATORIUM_PG_DSN"
	envRedisAddr = "PROBATORIUM_REDIS_ADDR"
	envMCAddr    = "PROBATORIUM_MEMCACHED_ADDR"
)

// userRow mirrors the seeded users table row (id, name, email, score).
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session; Seq is the
// hit counter loaded from the fixed-key blob and bumped on every request.
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

	// v1.5.4 driver-depth routes (writes / transaction / range / pipeline /
	// multiget) using the same idiomatic pgx/go-redis/gomemcache ops as the
	// other adapters. The /cache-pipeline and /mc-multiget paths are flat
	// (not /cache/pipeline) so they don't collide with the /cache/{key} and
	// /mc/{key} param routes above.

	// Postgres write: POST /db/insert — INSERT the request body into bench_writes.
	app.Post("/db/insert", func(c irisv12.Context) {
		if dc.pg == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(c.Request().Body)
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		if _, err := dc.pg.Exec(ctx,
			"INSERT INTO bench_writes(payload) VALUES($1)", string(body),
		); err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/json")
		_ = c.JSON(sessionResponse{OK: true})
	})

	// Postgres transaction: POST /db/tx/user/{id} — BEGIN; UPDATE score+1; COMMIT.
	app.Post("/db/tx/user/{id:int}", func(c irisv12.Context) {
		if dc.pg == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		id, err := c.Params().GetInt("id")
		if err != nil {
			c.StopWithStatus(http.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		tx, err := dc.pg.Begin(ctx)
		if err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		if _, err := tx.Exec(ctx, "UPDATE users SET score=score+1 WHERE id=$1", id); err != nil {
			_ = tx.Rollback(ctx)
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/json")
		_ = c.JSON(sessionResponse{OK: true, Seq: id})
	})

	// Postgres range read: GET /db/users?limit=N — SELECT N rows -> JSON array.
	app.Get("/db/users", func(c irisv12.Context) {
		if dc.pg == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		limit := c.URLParamIntDefault("limit", 50)
		if limit <= 0 || limit > 1000 {
			limit = 50
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		rows, err := dc.pg.Query(ctx,
			"SELECT id, name, email, score FROM users WHERE id BETWEEN 1 AND $1 ORDER BY id", limit)
		if err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		defer rows.Close()
		out := make([]userRow, 0, limit)
		for rows.Next() {
			var r userRow
			if err := rows.Scan(&r.ID, &r.Name, &r.Email, &r.Score); err != nil {
				c.StopWithStatus(http.StatusServiceUnavailable)
				return
			}
			out = append(out, r)
		}
		if rows.Err() != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/json")
		_ = c.JSON(out)
	})

	// Redis write: POST /cache — SET demo-write = request body, no expiry.
	app.Post("/cache", func(c irisv12.Context) {
		if dc.redis == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(c.Request().Body)
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		if err := dc.redis.Set(ctx, "demo-write", body, 0).Err(); err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/json")
		_ = c.JSON(sessionResponse{OK: true})
	})

	// Memcached write: POST /mc — SET demo-write = request body, no expiry.
	app.Post("/mc", func(c irisv12.Context) {
		if dc.mc == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		body, _ := io.ReadAll(c.Request().Body)
		if err := dc.mc.Set(&memcache.Item{Key: "demo-write", Value: body}); err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/json")
		_ = c.JSON(sessionResponse{OK: true})
	})

	// Redis pipeline: GET /cache-pipeline?n=N — pipeline N GETs of demo-key.
	app.Get("/cache-pipeline", func(c irisv12.Context) {
		if dc.redis == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		n := c.URLParamIntDefault("n", 10)
		if n <= 0 || n > 100 {
			n = 10
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()
		pipe := dc.redis.Pipeline()
		cmds := make([]*redis.StringCmd, n)
		for i := 0; i < n; i++ {
			cmds[i] = pipe.Get(ctx, "demo-key")
		}
		if _, err := pipe.Exec(ctx); err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		total := 0
		for _, cmd := range cmds {
			v, err := cmd.Bytes()
			if err != nil {
				c.StopWithStatus(http.StatusServiceUnavailable)
				return
			}
			total += len(v)
		}
		c.ContentType("application/json")
		_ = c.JSON(sessionResponse{OK: true, Seq: total})
	})

	// Memcached multiget: GET /mc-multiget?keys=N — GetMulti of N session keys.
	app.Get("/mc-multiget", func(c irisv12.Context) {
		if dc.mc == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		n := c.URLParamIntDefault("keys", 10)
		if n <= 0 || n > 100 {
			n = 10
		}
		keys := make([]string, n)
		for i := 0; i < n; i++ {
			keys[i] = "user:" + strconv.Itoa(i+1) + ":session"
		}
		items, err := dc.mc.GetMulti(keys)
		if err != nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		c.ContentType("application/json")
		_ = c.JSON(sessionResponse{OK: true, Seq: len(items)})
	})

	// Session: POST /session — fixed-key round-trip over go-redis directly
	// (no iris sessions library), so the workload matches every adapter:
	// GET the fixed-key blob (redis.Nil on the unseeded key is ignored),
	// merge the JSON request body, bump the seq hit counter, then SET the
	// blob back with a 10-minute TTL. Exactly two round-trips (GET then SET).
	app.Post("/session", func(c irisv12.Context) {
		if dc.redis == nil {
			c.StopWithStatus(http.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 5*time.Second)
		defer cancel()

		blob := map[string]any{}
		if raw, err := dc.redis.Get(ctx, sessionKey).Bytes(); err == nil {
			_ = json.Unmarshal(raw, &blob)
		}

		if body, _ := io.ReadAll(c.Request().Body); len(body) > 0 {
			var payload map[string]any
			if err := json.Unmarshal(body, &payload); err == nil {
				for k, v := range payload {
					blob[k] = v
				}
			}
		}

		seq := 0
		if n, ok := blob["seq"].(float64); ok { // JSON numbers decode to float64
			seq = int(n)
		}
		seq++
		blob["seq"] = seq

		if raw, err := json.Marshal(blob); err == nil {
			if err := dc.redis.Set(ctx, sessionKey, raw, 10*time.Minute).Err(); err != nil {
				c.StopWithStatus(http.StatusServiceUnavailable)
				return
			}
		}
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
}
