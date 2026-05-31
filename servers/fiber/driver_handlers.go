package main

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"time"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
	goredis "github.com/redis/go-redis/v9"
)

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
// read-merge-write round-trip keyed on the pmsid cookie.
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

	app.Post("/session", func(c *fiber.Ctx) error {
		// The session round-trip is Redis-backed (parity with celeris's
		// redisstore). Without Redis there is nowhere to persist the blob,
		// so the route is 503 — matching the driver-unavailable contract.
		if ds.rdb == nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(c.UserContext(), 5*time.Second)
		defer cancel()

		sid := c.Cookies("pmsid")
		data := make(map[string]any, 4)
		if sid != "" {
			raw, err := ds.rdb.Get(ctx, "pmsess:"+sid).Bytes()
			if err == nil {
				_ = json.Unmarshal(raw, &data)
			} else if err != goredis.Nil {
				return c.SendStatus(fiber.StatusServiceUnavailable)
			}
		}
		if sid == "" {
			sid = uuid.NewString()
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
		if err := ds.rdb.Set(ctx, "pmsess:"+sid, buf, 10*time.Minute).Err(); err != nil {
			return c.SendStatus(fiber.StatusServiceUnavailable)
		}

		c.Cookie(&fiber.Cookie{Name: "pmsid", Value: sid, Path: "/", HTTPOnly: true})
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
