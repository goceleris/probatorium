package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/gofiber/fiber/v2"
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
// pgx/v5 + go-redis/v9 + gomemcache mirror the celeris driver stack so the
// chain/<-> driver overhead comparison reflects framework cost, not a
// client-library mismatch. Pool size 16 per backend keeps the bench
// concurrency-bounded by the server.
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

// mountDriverHandlers registers the 4 driver routes on app as native
// fiber v2 handlers. The workloads mirror the celeris reference: a PG
// row read, Redis/memcached GETs returning the raw blob, and a session
// load+merge+save round-trip on the fixed key pmsess:bench.
func mountDriverHandlers(app *fiber.App, ds *driverState) {
	app.Get("/db/user/:id", func(c *fiber.Ctx) error {
		if ds.pg == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		id, perr := strconv.Atoi(c.Params("id"))
		if perr != nil {
			return c.SendStatus(fiber.StatusBadRequest)
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()
		var row userRow
		if err := ds.pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(row))
	})

	app.Get("/cache/:key", func(c *fiber.Ctx) error {
		if ds.rdb == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()
		val, err := ds.rdb.Get(ctx, c.Params("key")).Bytes()
		if err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/octet-stream")
		return c.Send(val)
	})

	app.Get("/mc/:key", func(c *fiber.Ctx) error {
		if ds.mc == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		item, err := ds.mc.Get(c.Params("key"))
		if err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/octet-stream")
		return c.Send(item.Value)
	})

	// v1.5.4 driver-depth routes (idiomatic pgx/go-redis/gomemcache).
	// Paths are /cache-pipeline and /mc-multiget (not /cache/pipeline or
	// /mc/multi) so they do not collide with the /cache/:key and /mc/:key
	// param routes above.
	app.Post("/db/insert", func(c *fiber.Ctx) error {
		if ds.pg == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()
		if _, err := ds.pg.Exec(ctx,
			"INSERT INTO bench_writes(payload) VALUES($1)", string(c.Body()),
		); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(sessionResponse{OK: true}))
	})

	app.Post("/db/tx/user/:id", func(c *fiber.Ctx) error {
		if ds.pg == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		id, perr := strconv.Atoi(c.Params("id"))
		if perr != nil {
			return c.SendStatus(fiber.StatusBadRequest)
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()
		tx, err := ds.pg.Begin(ctx)
		if err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		if _, err := tx.Exec(ctx, "UPDATE users SET score=score+1 WHERE id=$1", id); err != nil {
			_ = tx.Rollback(ctx)
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		if err := tx.Commit(ctx); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(sessionResponse{OK: true, Seq: id}))
	})

	app.Get("/db/users", func(c *fiber.Ctx) error {
		if ds.pg == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		limit, err := strconv.Atoi(c.Query("limit"))
		if err != nil || limit <= 0 || limit > 1000 {
			limit = 50
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()
		rows, err := ds.pg.Query(ctx,
			"SELECT id, name, email, score FROM users WHERE id BETWEEN 1 AND $1 ORDER BY id", limit)
		if err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		defer rows.Close()
		out := make([]userRow, 0, limit)
		for rows.Next() {
			var r userRow
			if err := rows.Scan(&r.ID, &r.Name, &r.Email, &r.Score); err != nil {
				return c.SendStatus(fiber.StatusServiceUnavailable)
			}
			out = append(out, r)
		}
		if rows.Err() != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(out))
	})

	app.Post("/cache", func(c *fiber.Ctx) error {
		if ds.rdb == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()
		if err := ds.rdb.Set(ctx, "demo-write", c.Body(), 0).Err(); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(sessionResponse{OK: true}))
	})

	app.Post("/mc", func(c *fiber.Ctx) error {
		if ds.mc == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		if err := ds.mc.Set(&memcache.Item{Key: "demo-write", Value: c.Body()}); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(sessionResponse{OK: true}))
	})

	app.Get("/cache-pipeline", func(c *fiber.Ctx) error {
		if ds.rdb == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		n, err := strconv.Atoi(c.Query("n"))
		if err != nil || n <= 0 || n > 100 {
			n = 10
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()
		pipe := ds.rdb.Pipeline()
		cmds := make([]*goredis.StringCmd, n)
		for i := 0; i < n; i++ {
			cmds[i] = pipe.Get(ctx, "demo-key")
		}
		if _, err := pipe.Exec(ctx); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		total := 0
		for _, cmd := range cmds {
			v, err := cmd.Bytes()
			if err != nil {
				return c.SendStatus(fiber.StatusServiceUnavailable)
			}
			total += len(v)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(sessionResponse{OK: true, Seq: total}))
	})

	app.Get("/mc-multiget", func(c *fiber.Ctx) error {
		if ds.mc == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		n, err := strconv.Atoi(c.Query("keys"))
		if err != nil || n <= 0 || n > 100 {
			n = 10
		}
		keys := make([]string, n)
		for i := 0; i < n; i++ {
			keys[i] = "user:" + strconv.Itoa(i+1) + ":session"
		}
		items, err := ds.mc.GetMulti(keys)
		if err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(sessionResponse{OK: true, Seq: len(items)}))
	})

	app.Post("/session", func(c *fiber.Ctx) error {
		// Fixed-key round-trip over go-redis: GET the seeded blob (redis.Nil
		// on the unseeded key is ignored), merge the JSON request body, bump
		// the seq hit counter, then SET the blob back with a 10-minute TTL.
		// Exactly two round-trips (GET then SET) — the same workload every
		// adapter runs. No Redis -> 503.
		if ds.rdb == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()

		data := make(map[string]any, 4)
		if raw, err := ds.rdb.Get(ctx, sessionKey).Bytes(); err == nil {
			_ = json.Unmarshal(raw, &data)
		}

		// Merge the request body if it is JSON. Parse failures are
		// non-fatal — the hit counter still increments so the round-trip
		// is observable on the wire.
		if body := c.Body(); len(body) > 0 {
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
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		if err := ds.rdb.Set(ctx, sessionKey, buf, 10*time.Minute).Err(); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}

		c.Set("Content-Type", "application/json")
		return c.Send(mustJSON(sessionResponse{OK: true, Seq: newSeq}))
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
