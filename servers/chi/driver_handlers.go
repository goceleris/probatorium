// driver_handlers.go mounts the four Phase-2 driver-backed routes onto the
// chi adapter, ported from the celeris reference
// (test/perfmatrix/servers/celeris/driver_handlers.go) but expressed with
// the mainstream Go client libraries a chi user would reach for:
//
//   - GET  /db/user/{id}  — jackc/pgx/v5 pool, SELECT by id, JSON row.
//   - GET  /cache/{key}   — redis/go-redis/v9, GET raw bytes.
//   - GET  /mc/{key}      — bradfitz/gomemcache, GET raw bytes.
//   - POST /session       — go-redis GET+SET round-trip on the fixed key
//     pmsess:bench: load/merge/save a hit counter, JSON {ok,seq} reply.
//
// Backends are addressed via environment variables (see driverConfig).
// The probatorium binary takes only -bind/-engine flags, so the
// orchestrator's service-provisioning step exports these before exec; an
// unset/unreachable backend makes the corresponding handler answer 503 so
// loadgen counts errors deterministically — byte-for-byte the same
// contract the celeris reference enforces with its nil-handle 503 guard.
//
// Clients are package-level singletons opened once on first mount and
// reused for the process lifetime (the bench runs one adapter process per
// cell). Pool size 16 per driver keeps the bench server-bound, matching
// the reference's WithMaxOpen(16)/WithPoolSize(16).
package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/go-chi/chi/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

// sessionKey is the fixed key POST /session round-trips against, so the
// workload is a load+merge+save of one seeded blob — identical to the
// other adapters.
const sessionKey = "pmsess:bench"

// userRow mirrors the seeded users table row (id, name, email, score),
// matching the celeris reference's userRow so the JSON body is identical
// across adapters.
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session. seq is the
// hit counter loaded from the fixed-key blob and bumped on every request.
// Shape matches the celeris reference.
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

// loadDriverConfig reads the backend wiring from the environment. Names
// follow the orchestrator's service-provisioning convention; defaults
// point at the conventional localhost ports so a developer can smoke-test
// against a local stack without exporting anything.
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
	pg    *pgxpool.Pool
	redis *redis.Client
	mc    *memcache.Client
}

// mountDriverHandlers attaches the four driver routes onto r. Clients are
// opened eagerly here; a failed open leaves the handle nil and the route
// answers 503 (deterministic error), never a panic.
func mountDriverHandlers(r chi.Router) {
	cfg := loadDriverConfig()
	c := &driverClients{}

	// Postgres pool. ParseConfig caps the pool at 16 conns to match the
	// reference. A parse/connect failure yields a nil pool -> 503.
	if cfg.pgDSN != "" {
		if pc, err := pgxpool.ParseConfig(cfg.pgDSN); err == nil {
			pc.MaxConns = 16
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if pool, err := pgxpool.NewWithConfig(ctx, pc); err == nil {
				c.pg = pool
			}
			cancel()
		}
	}

	// Redis client, pool size 16.
	if cfg.redisAddr != "" {
		c.redis = redis.NewClient(&redis.Options{
			Addr:     cfg.redisAddr,
			PoolSize: 16,
		})
	}

	// Memcached client. MaxIdleConns is gomemcache's pool knob.
	if cfg.mcAddr != "" {
		mc := memcache.New(cfg.mcAddr)
		mc.MaxIdleConns = 16
		c.mc = mc
	}

	r.Get("/db/user/{id}", c.dbUserHandler)
	r.Get("/cache/{key}", c.cacheHandler)
	r.Get("/mc/{key}", c.mcHandler)
	r.Post("/session", c.sessionHandler)

	// v1.5.4 driver-depth routes (idiomatic pgx/go-redis/gomemcache).
	r.Post("/db/insert", c.dbInsertHandler)
	r.Post("/db/tx/user/{id}", c.dbTxHandler)
	r.Get("/db/users", c.dbUsersRangeHandler)
	r.Post("/cache", c.cacheSetHandler)
	r.Post("/mc", c.mcSetHandler)
	r.Get("/cache-pipeline", c.cachePipelineHandler)
	r.Get("/mc-multiget", c.mcMultiGetHandler)
}

// dbInsertHandler serves POST /db/insert: INSERT the body into bench_writes.
func (c *driverClients) dbInsertHandler(w http.ResponseWriter, r *http.Request) {
	if c.pg == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, _ := io.ReadAll(r.Body)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if _, err := c.pg.Exec(ctx, "INSERT INTO bench_writes(payload) VALUES($1)", string(body)); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, sessionResponse{OK: true})
}

// dbTxHandler serves POST /db/tx/user/{id}: BEGIN; UPDATE score+1; COMMIT.
func (c *driverClients) dbTxHandler(w http.ResponseWriter, r *http.Request) {
	if c.pg == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	tx, err := c.pg.Begin(ctx)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if _, err := tx.Exec(ctx, "UPDATE users SET score=score+1 WHERE id=$1", id); err != nil {
		_ = tx.Rollback(ctx)
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, sessionResponse{OK: true, Seq: id})
}

// dbUsersRangeHandler serves GET /db/users?limit=N: SELECT N rows -> JSON array.
func (c *driverClients) dbUsersRangeHandler(w http.ResponseWriter, r *http.Request) {
	if c.pg == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	limit, err := strconv.Atoi(r.URL.Query().Get("limit"))
	if err != nil || limit <= 0 || limit > 1000 {
		limit = 50
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	rows, err := c.pg.Query(ctx,
		"SELECT id, name, email, score FROM users WHERE id BETWEEN 1 AND $1 ORDER BY id", limit)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	defer rows.Close()
	out := make([]userRow, 0, limit)
	for rows.Next() {
		var row userRow
		if err := rows.Scan(&row.ID, &row.Name, &row.Email, &row.Score); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		out = append(out, row)
	}
	if rows.Err() != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, out)
}

// cacheSetHandler serves POST /cache: SET demo-write = body.
func (c *driverClients) cacheSetHandler(w http.ResponseWriter, r *http.Request) {
	if c.redis == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, _ := io.ReadAll(r.Body)
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := c.redis.Set(ctx, "demo-write", body, 0).Err(); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, sessionResponse{OK: true})
}

// mcSetHandler serves POST /mc: memcached SET demo-write = body.
func (c *driverClients) mcSetHandler(w http.ResponseWriter, r *http.Request) {
	if c.mc == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	body, _ := io.ReadAll(r.Body)
	if err := c.mc.Set(&memcache.Item{Key: "demo-write", Value: body}); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, sessionResponse{OK: true})
}

// cachePipelineHandler serves GET /cache-pipeline?n=N: pipeline N GETs of demo-key.
func (c *driverClients) cachePipelineHandler(w http.ResponseWriter, r *http.Request) {
	if c.redis == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	n, err := strconv.Atoi(r.URL.Query().Get("n"))
	if err != nil || n <= 0 || n > 100 {
		n = 10
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	pipe := c.redis.Pipeline()
	cmds := make([]*redis.StringCmd, n)
	for i := 0; i < n; i++ {
		cmds[i] = pipe.Get(ctx, "demo-key")
	}
	if _, err := pipe.Exec(ctx); err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	total := 0
	for _, cmd := range cmds {
		v, err := cmd.Bytes()
		if err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		total += len(v)
	}
	writeJSON(w, sessionResponse{OK: true, Seq: total})
}

// mcMultiGetHandler serves GET /mc-multiget?keys=N: GetMulti of N session keys.
func (c *driverClients) mcMultiGetHandler(w http.ResponseWriter, r *http.Request) {
	if c.mc == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	n, err := strconv.Atoi(r.URL.Query().Get("keys"))
	if err != nil || n <= 0 || n > 100 {
		n = 10
	}
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = "user:" + strconv.Itoa(i+1) + ":session"
	}
	items, err := c.mc.GetMulti(keys)
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, sessionResponse{OK: true, Seq: len(items)})
}

// writeJSON encodes v as the 200 JSON response body, matching the existing
// handlers' Content-Type/WriteHeader/Encode sequence.
func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(v)
}

// dbUserHandler serves GET /db/user/{id}: SELECT id,name,email,score and
// JSON-encode the row. nil pool / query error -> 503; bad id -> 400.
func (c *driverClients) dbUserHandler(w http.ResponseWriter, r *http.Request) {
	if c.pg == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.Atoi(chi.URLParam(r, "id"))
	if err != nil {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	var row userRow
	qerr := c.pg.QueryRow(ctx,
		"SELECT id, name, email, score FROM users WHERE id=$1", id,
	).Scan(&row.ID, &row.Name, &row.Email, &row.Score)
	if qerr != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(row)
}

// cacheHandler serves GET /cache/{key}: Redis GET, raw bytes back.
func (c *driverClients) cacheHandler(w http.ResponseWriter, r *http.Request) {
	if c.redis == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	val, err := c.redis.Get(ctx, chi.URLParam(r, "key")).Bytes()
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(val)
}

// mcHandler serves GET /mc/{key}: memcached GET, raw bytes back.
func (c *driverClients) mcHandler(w http.ResponseWriter, r *http.Request) {
	if c.mc == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	item, err := c.mc.Get(chi.URLParam(r, "key"))
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(item.Value)
}

// sessionHandler serves POST /session over a Redis-backed JSON blob on the
// fixed key pmsess:bench: GET the blob (redis.Nil on the unseeded key is
// ignored), merge any JSON request body, bump the seq hit counter, then SET
// the blob back with a 10-minute TTL. Exactly two round-trips (GET then SET)
// — the same workload every adapter runs, over go-redis here. No Redis -> 503.
func (c *driverClients) sessionHandler(w http.ResponseWriter, r *http.Request) {
	if c.redis == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	blob := map[string]any{}
	if raw, err := c.redis.Get(ctx, sessionKey).Bytes(); err == nil {
		_ = json.Unmarshal(raw, &blob)
	}

	// Merge a JSON request body if present (the scenario POSTs a ~256B
	// JSON-ish payload). Parse failures are non-fatal.
	if r.Body != nil {
		var payload map[string]any
		if err := json.NewDecoder(r.Body).Decode(&payload); err == nil {
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
		if err := c.redis.Set(ctx, sessionKey, raw, 10*time.Minute).Err(); err != nil {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sessionResponse{OK: true, Seq: seq})
}

// Compile-time guard so the errors import stays tied to real behavior even
// if a future edit drops its only use.
var _ = errors.New
