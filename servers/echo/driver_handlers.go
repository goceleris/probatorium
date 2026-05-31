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
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	echov4 "github.com/labstack/echo/v4"
	"github.com/redis/go-redis/v9"

	"github.com/goceleris/probatorium/servers/common"
)

// driverPoolSize bounds every backing client so the bench is gated by the
// server under test rather than by an unbounded client-side connection
// fan-out. Mirrors the pool size the celeris in-tree drivers use (16).
const driverPoolSize = 16

// driverOpTimeout caps a single backing round-trip, matching the reference
// celeris adapter's 5s per-op deadline.
const driverOpTimeout = 5 * time.Second

// userRow mirrors the seeded users table (services.Seed): id, name, email,
// score. The JSON shape is the wire contract probes shape-check /db/user/:id
// against.
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session; Seq is the
// session's hit counter, incremented on every request bound to the same
// cookie.
type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

// driverClients holds the lazily-constructed backing clients. A nil field
// means the corresponding service was not configured (its env var was
// empty) or the client could not be built; the matching handler then
// answers 503 so loadgen counts driver errors deterministically — the same
// contract the celeris adapter and the validation refapps follow.
type driverClients struct {
	pg    *pgxpool.Pool
	redis *redis.Client
	mc    *memcache.Client
}

// envOr returns the env var value for key, or def when unset/empty. The
// service endpoints are injected by the orchestrator as PROBATORIUM_PG_DSN
// / PROBATORIUM_REDIS_ADDR / PROBATORIUM_MC_ADDR (see ansible/validate.yml);
// the -postgres-dsn / -redis-addr / -mc-addr flags override them.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// newDriverClients builds every backing client from the resolved endpoints.
// Construction never aborts startup: a missing or unreachable backend leaves
// its field nil and degrades only that one route to 503, so a bench run that
// exercises (say) only Redis is not blocked by an absent Postgres.
func newDriverClients(pgDSN, redisAddr, mcAddr string) *driverClients {
	dc := &driverClients{}

	if pgDSN != "" {
		cfg, err := pgxpool.ParseConfig(pgDSN)
		if err == nil {
			cfg.MaxConns = driverPoolSize
			ctx, cancel := context.WithTimeout(context.Background(), driverOpTimeout)
			if pool, perr := pgxpool.NewWithConfig(ctx, cfg); perr == nil {
				dc.pg = pool
			}
			cancel()
		}
	}

	if redisAddr != "" {
		dc.redis = redis.NewClient(&redis.Options{
			Addr:     redisAddr,
			PoolSize: driverPoolSize,
		})
	}

	if mcAddr != "" {
		mc := memcache.New(mcAddr)
		mc.Timeout = driverOpTimeout
		mc.MaxIdleConns = driverPoolSize
		dc.mc = mc
	}

	return dc
}

// close releases every open client. Safe on a nil receiver.
func (dc *driverClients) close() {
	if dc == nil {
		return
	}
	if dc.pg != nil {
		dc.pg.Close()
	}
	if dc.redis != nil {
		_ = dc.redis.Close()
	}
	// gomemcache has no Close — its connections are reaped by the idle pool.
}

// registerDriverHandlers mounts the four driver-backed routes on e. Each
// route degrades to 503 when its backing client is nil so a declared-but-
// unconfigured service surfaces as a deterministic error rather than a 404.
func registerDriverHandlers(e *echov4.Echo, dc *driverClients) {
	// GET /db/user/:id — single indexed read of the seeded users table.
	e.GET("/db/user/:id", func(c echov4.Context) error {
		if dc.pg == nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil {
			return c.NoContent(http.StatusBadRequest)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), driverOpTimeout)
		defer cancel()
		row := userRow{}
		qerr := dc.pg.QueryRow(ctx,
			"SELECT id, name, email, score FROM users WHERE id=$1", id,
		).Scan(&row.ID, &row.Name, &row.Email, &row.Score)
		if qerr != nil {
			if errors.Is(qerr, pgx.ErrNoRows) {
				return c.NoContent(http.StatusNotFound)
			}
			return c.NoContent(http.StatusServiceUnavailable)
		}
		return c.JSON(http.StatusOK, row)
	})

	// GET /cache/:key — Redis byte fetch.
	e.GET("/cache/:key", func(c echov4.Context) error {
		if dc.redis == nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), driverOpTimeout)
		defer cancel()
		val, err := dc.redis.Get(ctx, c.Param("key")).Bytes()
		if err != nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		return c.Blob(http.StatusOK, "application/octet-stream", val)
	})

	// GET /mc/:key — memcached byte fetch.
	e.GET("/mc/:key", func(c echov4.Context) error {
		if dc.mc == nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		item, err := dc.mc.Get(c.Param("key"))
		if err != nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		return c.Blob(http.StatusOK, "application/octet-stream", item.Value)
	})

	// POST /session — cookie-keyed session round-trip backed by Redis.
	//
	// Echo ships no session middleware in the core module and there is no
	// celeris session store to reuse, so the round-trip is expressed
	// directly: read (or mint) the pmsid cookie, merge the request body
	// into the session blob, increment a per-session hit counter, and
	// reply with the current seq. This is the semantic equal of the
	// celeris adapter's session.New(...) + sessionHandler — a load + merge
	// + save + counter round-trip per request on the same cookie — using
	// the same Redis backend.
	e.POST("/session", func(c echov4.Context) error {
		if dc.redis == nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}

		sid := ""
		if ck, err := c.Cookie(common.SessionCookieName); err == nil && ck.Value != "" {
			sid = ck.Value
		}
		if sid == "" {
			sid = uuid.NewString()
			c.SetCookie(&http.Cookie{
				Name:     common.SessionCookieName,
				Value:    sid,
				Path:     "/",
				HttpOnly: true,
			})
		}

		ctx, cancel := context.WithTimeout(c.Request().Context(), driverOpTimeout)
		defer cancel()

		key := "pmsess:" + sid

		// Merge the request body into the session blob when it parses as
		// JSON. Parse failures are non-fatal: the counter round-trip below
		// still runs so the session path is observable on every request.
		if body, err := io.ReadAll(c.Request().Body); err == nil && len(body) > 0 {
			var payload map[string]any
			if json.Unmarshal(body, &payload) == nil && len(payload) > 0 {
				fields := make(map[string]any, len(payload))
				for k, v := range payload {
					fields["d:"+k] = v
				}
				_ = dc.redis.HSet(ctx, key, fields).Err()
			}
		}

		seq, err := dc.redis.HIncrBy(ctx, key, "seq", 1).Result()
		if err != nil {
			return c.NoContent(http.StatusServiceUnavailable)
		}
		_ = dc.redis.Expire(ctx, key, 10*time.Minute).Err()

		return c.JSON(http.StatusOK, sessionResponse{OK: true, Seq: int(seq)})
	})
}
