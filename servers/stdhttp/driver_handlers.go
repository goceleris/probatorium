// driver_handlers.go mounts the four Phase-2 driver-backed routes onto the
// stdhttp adapter, ported from the celeris reference
// (test/perfmatrix/servers/celeris/driver_handlers.go) and expressed with
// the mainstream Go client libraries against net/http's ServeMux:
//
//   - GET  /db/user/{id}  — jackc/pgx/v5 pool, SELECT by id, JSON row.
//   - GET  /cache/{key}   — redis/go-redis/v9, GET raw bytes.
//   - GET  /mc/{key}      — bradfitz/gomemcache, GET raw bytes.
//   - POST /session       — go-redis-backed session: load/merge/save a hit
//     counter keyed by the pmsid cookie, JSON {ok,seq} reply.
//
// This is the chi adapter's driver layer with chi.URLParam swapped for
// ServeMux's r.PathValue and chi.Router swapped for *http.ServeMux — the
// client construction, pool sizing (16), 503-on-missing-backend contract,
// and the JSON/blob bodies are byte-identical to the chi and celeris
// adapters so a driver throughput delta reflects the runtime, never a
// payload artefact.
//
// Backends are addressed via environment variables (see driverConfig).
// The probatorium binary takes only -bind/-engine flags, so the
// orchestrator's service-provisioning step exports these before exec; an
// unset/unreachable backend makes the corresponding handler answer 503 so
// loadgen counts errors deterministically.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
)

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
// session's hit counter, incremented on every request carrying the same
// pmsid cookie. Shape matches the celeris reference.
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

// mountDriverHandlers attaches the four driver routes onto mux. Clients are
// opened eagerly here; a failed open leaves the handle nil and the route
// answers 503 (deterministic error), never a panic.
func mountDriverHandlers(mux *http.ServeMux) {
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

	mux.HandleFunc("GET /db/user/{id}", c.dbUserHandler)
	mux.HandleFunc("GET /cache/{key}", c.cacheHandler)
	mux.HandleFunc("GET /mc/{key}", c.mcHandler)
	mux.HandleFunc("POST /session", c.sessionHandler)
}

// dbUserHandler serves GET /db/user/{id}: SELECT id,name,email,score and
// JSON-encode the row. nil pool / query error -> 503; bad id -> 400.
func (c *driverClients) dbUserHandler(w http.ResponseWriter, r *http.Request) {
	if c.pg == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
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
	val, err := c.redis.Get(ctx, r.PathValue("key")).Bytes()
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
	item, err := c.mc.Get(r.PathValue("key"))
	if err != nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	w.Header().Set("Content-Type", "application/octet-stream")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(item.Value)
}

// sessionHandler serves POST /session with a Redis-backed session store,
// mirroring the celeris reference's redisstore session: load the blob by
// the pmsid cookie, merge any JSON request body, bump the seq hit
// counter, persist, and reply {ok,seq}. No Redis -> 503.
//
// The store is a plain Redis JSON blob under "sess:<sid>" with a 10-minute
// TTL (the reference's IdleTimeout). A loadgen client that reuses the
// cookie observes a monotonically increasing seq, proving the store
// round-trip; a fresh client starts at 1.
func (c *driverClients) sessionHandler(w http.ResponseWriter, r *http.Request) {
	if c.redis == nil {
		w.WriteHeader(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()

	sid, fresh := sessionID(r)
	key := "sess:" + sid

	blob := map[string]any{}
	if !fresh {
		if raw, err := c.redis.Get(ctx, key).Bytes(); err == nil {
			_ = json.Unmarshal(raw, &blob)
		}
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
		_ = c.redis.Set(ctx, key, raw, 10*time.Minute).Err()
	}

	if fresh {
		http.SetCookie(w, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(sessionResponse{OK: true, Seq: seq})
}

// sessionID returns the session id from the pmsid cookie, or a freshly
// generated one (with fresh=true so the caller emits a Set-Cookie).
func sessionID(r *http.Request) (id string, fresh bool) {
	if ck, err := r.Cookie(sessionCookieName); err == nil && ck.Value != "" {
		return ck.Value, false
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16), true
	}
	return hex.EncodeToString(b[:]), true
}

// Compile-time guard so the errors import stays tied to real behavior even
// if a future edit drops its only use.
var _ = errors.New
