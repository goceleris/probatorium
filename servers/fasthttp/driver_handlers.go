// Driver-backed handlers for the fasthttp adapter: the four Phase-2
// driver routes (postgres / redis / memcached / session). Clients are
// opened from the backend addresses the runner exports in the child
// environment (PROBATORIUM_PG_DSN / PROBATORIUM_REDIS_ADDR /
// PROBATORIUM_MEMCACHED_ADDR — the same vars the validation refapps read). An
// absent or unreachable backend leaves the matching route returning 503
// so the runner's per-cell guard records a clean error rather than a
// panic or a silent 0-RPS cell.
//
// fasthttp is router-less, so these mount through the Server's
// MountNative prefix dispatch (see server.go): "/db/user/" / "/cache/" /
// "/mc/" match any path under the prefix and the handler slices the path
// parameter off itself, while POST /session is an exact match.
package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
	"github.com/valyala/fasthttp"

	"github.com/goceleris/probatorium/servers/common"
)

// Backend address environment variables the runner sets before launching
// the adapter. An empty value means the backend is absent and the route
// responds 503. Names match the validation refapps and ansible/validate.yml.
const (
	envPGDSN     = "PROBATORIUM_PG_DSN"
	envRedisAddr = "PROBATORIUM_REDIS_ADDR"
	envMCAddr    = "PROBATORIUM_MEMCACHED_ADDR"
)

// driverPoolSize bounds each driver's connection pool so the bench is
// throughput-limited by the server, not the client.
const driverPoolSize = 16

// driverClients bundles the per-backend clients opened at mount time. A
// nil field is an absent backend. Held at package scope so closeDrivers
// can release them on shutdown.
type driverClients struct {
	pg  *pgxpool.Pool
	rdb *goredis.Client
	mc  *memcache.Client
}

var mountedDrivers driverClients

// userRow mirrors the seeded users table row.
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session; seq is the
// session's hit counter, incremented on every request bound to the same
// cookie.
type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

// driverPrefix turns a contract path template (e.g. "/db/user/:id") into
// the "/db/user/" prefix the Server's MountNative dispatch matches on.
func driverPrefix(template string) string {
	if i := strings.IndexByte(template, ':'); i >= 0 {
		return template[:i]
	}
	return template
}

// mountDriverHandlers opens the driver clients from the environment and
// registers the four driver routes onto s. Routes whose backend is absent
// still mount but answer 503 — the contract declares them and the runner
// treats a declared-but-unserved route as a hard error.
func mountDriverHandlers(s *Server) {
	if s == nil {
		return
	}

	if dsn := os.Getenv(envPGDSN); dsn != "" {
		if cfg, err := pgxpool.ParseConfig(dsn); err == nil {
			cfg.MaxConns = driverPoolSize
			if pool, perr := pgxpool.NewWithConfig(context.Background(), cfg); perr == nil {
				mountedDrivers.pg = pool
			}
		}
	}
	if addr := os.Getenv(envRedisAddr); addr != "" {
		mountedDrivers.rdb = goredis.NewClient(&goredis.Options{Addr: addr, PoolSize: driverPoolSize})
	}
	if addr := os.Getenv(envMCAddr); addr != "" {
		mc := memcache.New(addr)
		mc.MaxIdleConns = driverPoolSize
		mountedDrivers.mc = mc
	}

	// Resolve the concrete prefixes/paths from the contract so the adapter
	// and the loadgen side cannot drift on route spelling.
	var dbUserPrefix, cachePrefix, mcPrefix, sessionPath string
	for _, ep := range common.DriverEndpoints {
		switch {
		case strings.HasPrefix(ep.Path, "/db/user/"):
			dbUserPrefix = driverPrefix(ep.Path)
		case strings.HasPrefix(ep.Path, "/cache/"):
			cachePrefix = driverPrefix(ep.Path)
		case strings.HasPrefix(ep.Path, "/mc/"):
			mcPrefix = driverPrefix(ep.Path)
		case ep.Path == "/session":
			sessionPath = ep.Path
		}
	}

	s.MountNative(http.MethodGet, dbUserPrefix, dbUserHandler(dbUserPrefix))
	s.MountNative(http.MethodGet, cachePrefix, cacheHandler(cachePrefix))
	s.MountNative(http.MethodGet, mcPrefix, mcHandler(mcPrefix))
	s.MountNative(http.MethodPost, sessionPath, sessionHandler())

	// v1.5.4 driver-depth routes (idiomatic pgx/go-redis/gomemcache). The
	// literal paths /cache-pipeline and /mc-multiget are deliberately not
	// /cache/... or /mc/... so they don't collide with the cache/mc prefix
	// routes above. /db/tx/user/:id is a prefix route (the id is sliced off
	// the path); the rest are exact matches and the limit/n/keys parameters
	// arrive as query args.
	const dbTxPrefix = "/db/tx/user/"
	s.MountNative(http.MethodPost, "/db/insert", dbInsertHandler())
	s.MountNative(http.MethodPost, dbTxPrefix, dbTxHandler(dbTxPrefix))
	s.MountNative(http.MethodGet, "/db/users", dbUsersRangeHandler())
	s.MountNative(http.MethodPost, "/cache", cacheSetHandler())
	s.MountNative(http.MethodGet, "/cache-pipeline", cachePipelineHandler())
	s.MountNative(http.MethodGet, "/mc-multiget", mcMultiGetHandler())
}

// dbInsertHandler serves POST /db/insert — INSERT the request body into
// bench_writes.
func dbInsertHandler() fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		pg := mountedDrivers.pg
		if pg == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := pg.Exec(ctx,
			"INSERT INTO bench_writes(payload) VALUES($1)", string(rc.PostBody()),
		); err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(sessionResponse{OK: true}))
	}
}

// dbTxHandler serves POST /db/tx/user/:id — BEGIN; UPDATE score+1; COMMIT.
func dbTxHandler(prefix string) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		pg := mountedDrivers.pg
		if pg == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		idStr := strings.TrimPrefix(string(rc.Path()), prefix)
		if idStr == "" || strings.Contains(idStr, "/") {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			return
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		tx, err := pg.Begin(ctx)
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		if _, err := tx.Exec(ctx, "UPDATE users SET score=score+1 WHERE id=$1", id); err != nil {
			_ = tx.Rollback(ctx)
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		if err := tx.Commit(ctx); err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(sessionResponse{OK: true, Seq: id}))
	}
}

// dbUsersRangeHandler serves GET /db/users?limit=N — SELECT N rows -> JSON array.
func dbUsersRangeHandler() fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		pg := mountedDrivers.pg
		if pg == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		limit, err := strconv.Atoi(string(rc.QueryArgs().Peek("limit")))
		if err != nil || limit <= 0 || limit > 1000 {
			limit = 50
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		rows, err := pg.Query(ctx,
			"SELECT id, name, email, score FROM users WHERE id BETWEEN 1 AND $1 ORDER BY id", limit)
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		defer rows.Close()
		out := make([]userRow, 0, limit)
		for rows.Next() {
			var r userRow
			if err := rows.Scan(&r.ID, &r.Name, &r.Email, &r.Score); err != nil {
				rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
				return
			}
			out = append(out, r)
		}
		if rows.Err() != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(out))
	}
}

// cacheSetHandler serves POST /cache — redis SET demo-write = request body.
func cacheSetHandler() fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		rdb := mountedDrivers.rdb
		if rdb == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := rdb.Set(ctx, "demo-write", rc.PostBody(), 0).Err(); err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(sessionResponse{OK: true}))
	}
}

// cachePipelineHandler serves GET /cache-pipeline?n=N — pipeline N GETs of
// demo-key, returning the summed value length as seq.
func cachePipelineHandler() fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		rdb := mountedDrivers.rdb
		if rdb == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		n, err := strconv.Atoi(string(rc.QueryArgs().Peek("n")))
		if err != nil || n <= 0 || n > 100 {
			n = 10
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		pipe := rdb.Pipeline()
		cmds := make([]*goredis.StringCmd, n)
		for i := 0; i < n; i++ {
			cmds[i] = pipe.Get(ctx, "demo-key")
		}
		if _, err := pipe.Exec(ctx); err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		total := 0
		for _, cmd := range cmds {
			v, err := cmd.Bytes()
			if err != nil {
				rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
				return
			}
			total += len(v)
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(sessionResponse{OK: true, Seq: total}))
	}
}

// mcMultiGetHandler serves GET /mc-multiget?keys=N — GetMulti of N session keys.
func mcMultiGetHandler() fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		mc := mountedDrivers.mc
		if mc == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		n, err := strconv.Atoi(string(rc.QueryArgs().Peek("keys")))
		if err != nil || n <= 0 || n > 100 {
			n = 10
		}
		keys := make([]string, n)
		for i := 0; i < n; i++ {
			keys[i] = "user:" + strconv.Itoa(i+1) + ":session"
		}
		items, err := mc.GetMulti(keys)
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(sessionResponse{OK: true, Seq: len(items)}))
	}
}

// dbUserHandler serves GET /db/user/:id — a single-row primary-key lookup
// against postgres, marshalled to JSON.
func dbUserHandler(prefix string) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		pg := mountedDrivers.pg
		if pg == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		idStr := strings.TrimPrefix(string(rc.Path()), prefix)
		if idStr == "" || strings.Contains(idStr, "/") {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			return
		}
		id, err := strconv.Atoi(idStr)
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		var row userRow
		if err := pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score); err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(row))
	}
}

// cacheHandler serves GET /cache/:key — a redis GET returning the raw
// value bytes.
func cacheHandler(prefix string) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		rdb := mountedDrivers.rdb
		if rdb == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(string(rc.Path()), prefix)
		if key == "" {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		val, err := rdb.Get(ctx, key).Bytes()
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		rc.SetContentType("application/octet-stream")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(val)
	}
}

// mcHandler serves GET /mc/:key — a memcached GET returning the raw value
// bytes.
func mcHandler(prefix string) fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		mc := mountedDrivers.mc
		if mc == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		key := strings.TrimPrefix(string(rc.Path()), prefix)
		if key == "" {
			rc.SetStatusCode(fasthttp.StatusBadRequest)
			return
		}
		item, err := mc.Get(key)
		if err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		rc.SetContentType("application/octet-stream")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(item.Value)
	}
}

// sessionHandler serves POST /session — a redis-backed session round-trip
// that merges the request body into the stored blob and bumps a hit
// counter. Matches the celeris perfmatrix reference's wire behaviour
// (common.SessionCookieName cookie, "pmsess:" key prefix, 10-minute TTL)
// so the session scenario is comparable across frameworks.
func sessionHandler() fasthttp.RequestHandler {
	return func(rc *fasthttp.RequestCtx) {
		rdb := mountedDrivers.rdb
		if rdb == nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()

		sid := string(rc.Request.Header.Cookie(common.SessionCookieName))
		data := make(map[string]any, 4)
		if sid != "" {
			raw, err := rdb.Get(ctx, "pmsess:"+sid).Bytes()
			switch {
			case err == nil:
				_ = json.Unmarshal(raw, &data)
			case err == goredis.Nil:
				// New session under an unknown cookie: start fresh.
			default:
				rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
				return
			}
		}
		if sid == "" {
			sid = newSessionID()
		}
		if body := rc.PostBody(); len(body) > 0 {
			var incoming map[string]any
			if json.Unmarshal(body, &incoming) == nil {
				for k, v := range incoming {
					data[k] = v
				}
			}
		}
		seq, _ := data["seq"].(float64)
		newSeq := int(seq) + 1
		data["seq"] = newSeq

		buf, _ := json.Marshal(data)
		if err := rdb.Set(ctx, "pmsess:"+sid, buf, 10*time.Minute).Err(); err != nil {
			rc.SetStatusCode(fasthttp.StatusServiceUnavailable)
			return
		}

		ck := fasthttp.AcquireCookie()
		ck.SetKey(common.SessionCookieName)
		ck.SetValue(sid)
		ck.SetPath("/")
		ck.SetHTTPOnly(true)
		rc.Response.Header.SetCookie(ck)
		fasthttp.ReleaseCookie(ck)

		rc.SetContentType("application/json")
		rc.SetStatusCode(fasthttp.StatusOK)
		_, _ = rc.Write(mustJSON(sessionResponse{OK: true, Seq: newSeq}))
	}
}

// closeDrivers releases the driver clients. Called on shutdown so a
// re-launched adapter does not leak pool connections.
func closeDrivers() {
	if mountedDrivers.pg != nil {
		mountedDrivers.pg.Close()
		mountedDrivers.pg = nil
	}
	if mountedDrivers.rdb != nil {
		_ = mountedDrivers.rdb.Close()
		mountedDrivers.rdb = nil
	}
	mountedDrivers.mc = nil
}

// newSessionID returns a random 128-bit hex session id.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

// mustJSON marshals v or panics; the driver response types are fixed
// shapes that cannot fail to marshal.
func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
