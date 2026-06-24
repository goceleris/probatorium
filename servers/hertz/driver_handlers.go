package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/cloudwego/hertz/pkg/app"
	"github.com/cloudwego/hertz/pkg/app/server"
	"github.com/cloudwego/hertz/pkg/protocol/consts"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

// sessionKey is the fixed key POST /session round-trips against, so the
// workload is a load+merge+save of one seeded blob — identical to the
// other adapters.
const sessionKey = "pmsess:bench"

// driverState holds the lazily-opened backend clients. Each field is nil
// when the matching PROBATORIUM_* env var is unset; the handler then
// answers 503 so loadgen counts errors deterministically rather than the
// cell silently recording a hang.
//
// pgx/v5 + go-redis/v9 + gomemcache mirror the celeris driver stack (and
// the other Go adapters) so the chain/driver overhead comparison reflects
// framework cost, not a client-library mismatch. Pool size 16 per backend
// keeps the bench concurrency-bounded by the server.
type driverState struct {
	pg  *pgxpool.Pool
	rdb *goredis.Client
	mc  *memcache.Client
}

type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

// buildDriverState opens whatever backends the orchestrator advertised via
// PROBATORIUM_PG_DSN / PROBATORIUM_REDIS_ADDR / PROBATORIUM_MEMCACHED_ADDR.
// An unset var (or an open error) leaves that client nil and the route
// answers 503.
func buildDriverState() *driverState {
	ds := &driverState{}
	if dsn := os.Getenv("PROBATORIUM_PG_DSN"); dsn != "" {
		if pc, err := pgxpool.ParseConfig(dsn); err == nil {
			pc.MaxConns = 16
			if pool, perr := pgxpool.NewWithConfig(context.Background(), pc); perr == nil {
				ds.pg = pool
			}
		}
	}
	if addr := os.Getenv("PROBATORIUM_REDIS_ADDR"); addr != "" {
		ds.rdb = goredis.NewClient(&goredis.Options{Addr: addr, PoolSize: 16})
	}
	if addr := os.Getenv("PROBATORIUM_MEMCACHED_ADDR"); addr != "" {
		ds.mc = memcache.New(addr)
		ds.mc.MaxIdleConns = 16
	}
	return ds
}

// mountDriverHandlers registers the 4 driver routes on h as native hertz
// handlers. The workloads mirror the fiber/celeris reference: a PG row
// read, Redis/memcached GETs returning the raw blob, and a session
// load+merge+save round-trip on the fixed key pmsess:bench.
func mountDriverHandlers(h *server.Hertz, ds *driverState) {
	h.GET("/db/user/:id", func(c context.Context, ctx *app.RequestContext) {
		if ds.pg == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		id, perr := strconv.Atoi(ctx.Param("id"))
		if perr != nil {
			ctx.AbortWithStatus(consts.StatusBadRequest)
			return
		}
		qctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		var row userRow
		if err := ds.pg.QueryRow(qctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score); err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/json", mustJSON(row))
	})

	h.GET("/cache/:key", func(c context.Context, ctx *app.RequestContext) {
		if ds.rdb == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		qctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		val, err := ds.rdb.Get(qctx, ctx.Param("key")).Bytes()
		if err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/octet-stream", val)
	})

	h.GET("/mc/:key", func(_ context.Context, ctx *app.RequestContext) {
		if ds.mc == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		item, err := ds.mc.Get(ctx.Param("key"))
		if err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/octet-stream", item.Value)
	})

	h.POST("/session", func(c context.Context, ctx *app.RequestContext) {
		// Fixed-key round-trip over go-redis: GET the seeded blob (redis.Nil
		// on the unseeded key is ignored), merge the JSON request body, bump
		// the seq hit counter, then SET the blob back with a 10-minute TTL.
		// Exactly two round-trips (GET then SET) — the same workload every
		// adapter runs. No Redis -> 503.
		if ds.rdb == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		qctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()

		data := make(map[string]any, 4)
		if raw, err := ds.rdb.Get(qctx, sessionKey).Bytes(); err == nil {
			_ = json.Unmarshal(raw, &data)
		}

		// Merge the request body if it is JSON. Parse failures are
		// non-fatal — the hit counter still increments so the round-trip
		// is observable on the wire.
		if body := ctx.Request.Body(); len(body) > 0 {
			var incoming map[string]any
			if err := json.Unmarshal(body, &incoming); err == nil {
				for k, v := range incoming {
					data[k] = v
				}
			}
		}

		seq, _ := data["seq"].(float64)
		newSeq := int(seq) + 1
		data["seq"] = newSeq

		buf, err := json.Marshal(data)
		if err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		if err := ds.rdb.Set(qctx, sessionKey, buf, 10*time.Minute).Err(); err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}

		ctx.Data(consts.StatusOK, "application/json", mustJSON(sessionResponse{OK: true, Seq: newSeq}))
	})

	mountDriverDepthHandlers(h, ds)
}

// mountDriverDepthHandlers registers the v1.5.4 driver-depth routes
// (idiomatic pgx/go-redis/gomemcache). The paths are /cache-pipeline and
// /mc-multiget rather than /cache/pipeline and /mc/multi so they do not
// shadow the existing /cache/:key and /mc/:key param routes.
func mountDriverDepthHandlers(h *server.Hertz, ds *driverState) {
	// POST /db/insert: INSERT the body into bench_writes.
	h.POST("/db/insert", func(c context.Context, ctx *app.RequestContext) {
		if ds.pg == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		body := ctx.Request.Body()
		qctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		if _, err := ds.pg.Exec(qctx, "INSERT INTO bench_writes(payload) VALUES($1)", string(body)); err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/json", mustJSON(sessionResponse{OK: true}))
	})

	// POST /db/tx/user/:id: BEGIN; UPDATE score+1; COMMIT.
	h.POST("/db/tx/user/:id", func(c context.Context, ctx *app.RequestContext) {
		if ds.pg == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		id, perr := strconv.Atoi(ctx.Param("id"))
		if perr != nil {
			ctx.AbortWithStatus(consts.StatusBadRequest)
			return
		}
		qctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		tx, err := ds.pg.Begin(qctx)
		if err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		if _, err := tx.Exec(qctx, "UPDATE users SET score=score+1 WHERE id=$1", id); err != nil {
			_ = tx.Rollback(qctx)
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		if err := tx.Commit(qctx); err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/json", mustJSON(sessionResponse{OK: true, Seq: id}))
	})

	// GET /db/users?limit=N: SELECT N rows -> JSON array.
	h.GET("/db/users", func(c context.Context, ctx *app.RequestContext) {
		if ds.pg == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		limit, err := strconv.Atoi(string(ctx.QueryArgs().Peek("limit")))
		if err != nil || limit <= 0 || limit > 1000 {
			limit = 50
		}
		qctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		rows, err := ds.pg.Query(qctx,
			"SELECT id, name, email, score FROM users WHERE id BETWEEN 1 AND $1 ORDER BY id", limit)
		if err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		defer rows.Close()
		out := make([]userRow, 0, limit)
		for rows.Next() {
			var r userRow
			if err := rows.Scan(&r.ID, &r.Name, &r.Email, &r.Score); err != nil {
				ctx.AbortWithStatus(consts.StatusServiceUnavailable)
				return
			}
			out = append(out, r)
		}
		if rows.Err() != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/json", mustJSON(out))
	})

	// POST /cache: SET demo-write = body.
	h.POST("/cache", func(c context.Context, ctx *app.RequestContext) {
		if ds.rdb == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		body := ctx.Request.Body()
		qctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		if err := ds.rdb.Set(qctx, "demo-write", body, 0).Err(); err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/json", mustJSON(sessionResponse{OK: true}))
	})

	// POST /mc: SET demo-write = body.
	h.POST("/mc", func(_ context.Context, ctx *app.RequestContext) {
		if ds.mc == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		body := ctx.Request.Body()
		if err := ds.mc.Set(&memcache.Item{Key: "demo-write", Value: body}); err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/json", mustJSON(sessionResponse{OK: true}))
	})

	// GET /cache-pipeline?n=N: pipeline N GETs of demo-key.
	h.GET("/cache-pipeline", func(c context.Context, ctx *app.RequestContext) {
		if ds.rdb == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		n, err := strconv.Atoi(string(ctx.QueryArgs().Peek("n")))
		if err != nil || n <= 0 || n > 100 {
			n = 10
		}
		qctx, cancel := context.WithTimeout(c, 5*time.Second)
		defer cancel()
		pipe := ds.rdb.Pipeline()
		cmds := make([]*goredis.StringCmd, n)
		for i := 0; i < n; i++ {
			cmds[i] = pipe.Get(qctx, "demo-key")
		}
		if _, err := pipe.Exec(qctx); err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		total := 0
		for _, cmd := range cmds {
			v, err := cmd.Bytes()
			if err != nil {
				ctx.AbortWithStatus(consts.StatusServiceUnavailable)
				return
			}
			total += len(v)
		}
		ctx.Data(consts.StatusOK, "application/json", mustJSON(sessionResponse{OK: true, Seq: total}))
	})

	// GET /mc-multiget?keys=N: GetMulti of N session keys.
	h.GET("/mc-multiget", func(_ context.Context, ctx *app.RequestContext) {
		if ds.mc == nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		n, err := strconv.Atoi(string(ctx.QueryArgs().Peek("keys")))
		if err != nil || n <= 0 || n > 100 {
			n = 10
		}
		keys := make([]string, n)
		for i := 0; i < n; i++ {
			keys[i] = "user:" + strconv.Itoa(i+1) + ":session"
		}
		items, err := ds.mc.GetMulti(keys)
		if err != nil {
			ctx.AbortWithStatus(consts.StatusServiceUnavailable)
			return
		}
		ctx.Data(consts.StatusOK, "application/json", mustJSON(sessionResponse{OK: true, Seq: len(items)}))
	})
}

// shutdownDriverState closes any open backend clients so a re-spawned
// adapter process does not leak pool connections.
func shutdownDriverState(ds *driverState) {
	if ds == nil {
		return
	}
	if ds.pg != nil {
		ds.pg.Close()
		ds.pg = nil
	}
	if ds.rdb != nil {
		_ = ds.rdb.Close()
		ds.rdb = nil
	}
	ds.mc = nil
}

func mustJSON(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
