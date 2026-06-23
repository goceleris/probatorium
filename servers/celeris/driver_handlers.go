// driver_handlers.go mounts the four Phase-2 driver-backed routes onto the
// celeris adapter, ported from the celeris reference
// (test/perfmatrix/servers/celeris/driver_handlers.go) and expressed with
// celeris's own in-tree driver packages — the idiomatic choice for a
// celeris user, and the one the reference adapter blesses:
//
//   - GET  /db/user/:id  — driver/postgres pool, SELECT by id, JSON row.
//   - GET  /cache/:key   — driver/redis, GET raw bytes.
//   - GET  /mc/:key      — driver/memcached, GET raw bytes.
//   - POST /session      — driver/redis GET+SET round-trip on the fixed key
//     pmsess:bench: load the seeded blob, merge the JSON body, save with a
//     10-minute TTL, JSON {ok,seq} reply. Exactly two round-trips.
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
)

// sessionKey is the fixed key every adapter's POST /session round-trips
// against, so the workload is a load+merge+save of one seeded blob — no
// per-cookie key fan-out — and identical across frameworks.
const sessionKey = "pmsess:bench"

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
// hit counter loaded from the fixed-key blob and bumped on every request.
// Shape matches the reference.
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

// driverClients are the lazily-opened, process-lifetime backend handles.
type driverClients struct {
	pg    *postgres.Pool
	redis *redis.Client
	mc    *memcached.Client
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

	srv.GET("/db/user/:id", c.dbUserHandler).Async()
	srv.GET("/cache/:key", c.cacheHandler).Async()
	srv.GET("/mc/:key", c.mcHandler).Async()

	// v1.5.4 driver-depth routes (writes / transaction / range / pipeline /
	// multiget). Same WithEngine().Async() discipline as the reads above.
	srv.POST("/db/insert", c.dbInsertHandler).Async()
	srv.POST("/db/tx/user/:id", c.dbTxHandler).Async()
	srv.GET("/db/users", c.dbUsersRangeHandler).Async()
	srv.POST("/cache", c.cacheSetHandler).Async()
	srv.GET("/cache-pipeline", c.cachePipelineHandler).Async()
	srv.GET("/mc-multiget", c.mcMultiGetHandler).Async()

	srv.POST("/session", c.sessionHandler).Async()

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

// dbInsertHandler serves POST /db/insert: INSERT the request body into the
// unlogged bench_writes table (driver-pg-write; "bench_writes" matches
// services.FixtureWritesTable). nil pool / exec error -> 503.
func (c *driverClients) dbInsertHandler(ctx *celeris.Context) error {
	if c.pg == nil {
		return ctx.AbortWithStatus(503)
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	if _, err := c.pg.ExecContext(qctx,
		"INSERT INTO bench_writes(payload) VALUES($1)", string(ctx.Body()),
	); err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.JSON(200, sessionResponse{OK: true})
}

// dbTxHandler serves POST /db/tx/user/:id: BEGIN; UPDATE score+1; COMMIT
// (driver-pg-update-tx) — an explicit transaction round-trip on the hot row.
func (c *driverClients) dbTxHandler(ctx *celeris.Context) error {
	if c.pg == nil {
		return ctx.AbortWithStatus(503)
	}
	id, err := strconv.Atoi(ctx.Param("id"))
	if err != nil {
		return ctx.AbortWithStatus(400)
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	tx, err := c.pg.BeginTx(qctx, nil)
	if err != nil {
		return ctx.AbortWithStatus(503)
	}
	if _, err := tx.ExecContext(qctx, "UPDATE users SET score=score+1 WHERE id=$1", id); err != nil {
		_ = tx.Rollback()
		return ctx.AbortWithStatus(503)
	}
	if err := tx.Commit(); err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.JSON(200, sessionResponse{OK: true, Seq: id})
}

// dbUsersRangeHandler serves GET /db/users?limit=N: SELECT the first N rows
// and JSON-encode the array (driver-pg-read-range) — result-set marshalling
// rather than a single-row read.
func (c *driverClients) dbUsersRangeHandler(ctx *celeris.Context) error {
	if c.pg == nil {
		return ctx.AbortWithStatus(503)
	}
	limit, err := strconv.Atoi(ctx.Query("limit"))
	if err != nil || limit <= 0 || limit > 1000 {
		limit = 50
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	rows, err := c.pg.QueryContext(qctx,
		"SELECT id, name, email, score FROM users WHERE id BETWEEN 1 AND $1 ORDER BY id", limit,
	)
	if err != nil {
		return ctx.AbortWithStatus(503)
	}
	defer func() { _ = rows.Close() }()
	out := make([]userRow, 0, limit)
	for rows.Next() {
		var r userRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Email, &r.Score); err != nil {
			return ctx.AbortWithStatus(503)
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.JSON(200, out)
}

// cacheSetHandler serves POST /cache: SET demo-write = request body
// (driver-redis-set; "demo-write" matches services.FixtureRedisWriteKey).
func (c *driverClients) cacheSetHandler(ctx *celeris.Context) error {
	if c.redis == nil {
		return ctx.AbortWithStatus(503)
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	if err := c.redis.SetBytes(qctx, "demo-write", ctx.Body(), 0); err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.JSON(200, sessionResponse{OK: true})
}

// cachePipelineHandler serves GET /cache-pipeline?n=N: pipeline N GETs of
// demo-key in one round-trip (driver-redis-pipeline) — the native-driver
// batching differentiator. "demo-key" matches services.FixtureDemoKey.
func (c *driverClients) cachePipelineHandler(ctx *celeris.Context) error {
	if c.redis == nil {
		return ctx.AbortWithStatus(503)
	}
	n, err := strconv.Atoi(ctx.Query("n"))
	if err != nil || n <= 0 || n > 100 {
		n = 10
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	p := c.redis.Pipeline()
	defer p.Release()
	cmds := make([]*redis.StringCmd, n)
	for i := 0; i < n; i++ {
		cmds[i] = p.Get("demo-key")
	}
	if err := p.Exec(qctx); err != nil {
		return ctx.AbortWithStatus(503)
	}
	total := 0
	for _, cmd := range cmds {
		v, err := cmd.Result()
		if err != nil {
			return ctx.AbortWithStatus(503)
		}
		total += len(v)
	}
	return ctx.JSON(200, sessionResponse{OK: true, Seq: total})
}

// mcMultiGetHandler serves GET /mc-multiget?keys=N: GetMulti of N seeded
// user:<id>:session keys in one batch (driver-mc-multiget).
func (c *driverClients) mcMultiGetHandler(ctx *celeris.Context) error {
	if c.mc == nil {
		return ctx.AbortWithStatus(503)
	}
	n, err := strconv.Atoi(ctx.Query("keys"))
	if err != nil || n <= 0 || n > 100 {
		n = 10
	}
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = "user:" + strconv.Itoa(i+1) + ":session"
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()
	vals, err := c.mc.GetMulti(qctx, keys...)
	if err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.JSON(200, sessionResponse{OK: true, Seq: len(vals)})
}

// sessionHandler serves POST /session via the native redis driver: GET the
// fixed-key blob (ErrNil when unseeded is ignored), merge the JSON request
// body, bump the seq hit counter, then SET the blob back with a 10-minute
// TTL. Exactly two round-trips (GET then SET) — identical work to every
// other adapter, differing only in this being celeris's in-tree driver. No
// Redis -> 503.
func (c *driverClients) sessionHandler(ctx *celeris.Context) error {
	if c.redis == nil {
		return ctx.AbortWithStatus(503)
	}
	qctx, cancel := context.WithTimeout(ctx.Context(), 5*time.Second)
	defer cancel()

	blob := map[string]any{}
	if raw, err := c.redis.GetBytes(qctx, sessionKey); err == nil {
		_ = json.Unmarshal(raw, &blob)
	}

	// Merge the JSON request body if present (the scenario POSTs a ~256B
	// payload). Parse failures are non-fatal.
	if body := ctx.Body(); len(body) > 0 {
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
		if err := c.redis.SetBytes(qctx, sessionKey, raw, 10*time.Minute); err != nil {
			return ctx.AbortWithStatus(503)
		}
	}
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
