package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"strconv"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/bradfitz/gomemcache/memcache"
)

// driverState holds the lazily-opened driver clients shared across the
// four driver routes. Each client is opened once at registration from
// the PROBATORIUM_* env vars the orchestrator injects; a nil client
// means the corresponding service was not provisioned for this run, and
// the route answers 503 so loadgen records a deterministic error rather
// than a panic. This mirrors the chi adapter's driverState so the
// gin-vs-chi delta reflects router/handler cost, not driver wiring.
type driverState struct {
	pg    *pgxpool.Pool
	redis *redis.Client
	mc    *memcache.Client
}

// mountDriverHandlers wires the four driver routes onto r. Clients are
// opened from PROBATORIUM_PG_DSN / PROBATORIUM_REDIS_ADDR /
// PROBATORIUM_MEMCACHED_ADDR; an unset var leaves that client nil and
// its route returns 503. The handlers use each client's idiomatic Go
// library (pgx / go-redis / gomemcache) so the comparison is
// framework-vs-framework with each one's blessed driver.
func mountDriverHandlers(r *gin.Engine) {
	st := &driverState{}
	if dsn := os.Getenv("PROBATORIUM_PG_DSN"); dsn != "" {
		cfg, err := pgxpool.ParseConfig(dsn)
		if err == nil {
			cfg.MaxConns = 16
			if pool, perr := pgxpool.NewWithConfig(context.Background(), cfg); perr == nil {
				st.pg = pool
			}
		}
	}
	if addr := os.Getenv("PROBATORIUM_REDIS_ADDR"); addr != "" {
		st.redis = redis.NewClient(&redis.Options{Addr: addr, PoolSize: 16})
	}
	if addr := os.Getenv("PROBATORIUM_MEMCACHED_ADDR"); addr != "" {
		st.mc = memcache.New(addr)
		st.mc.MaxIdleConns = 16
	}

	r.GET("/db/user/:id", st.handleDBUser)
	r.GET("/cache/:key", st.handleCache)
	r.GET("/mc/:key", st.handleMC)
	r.POST("/session", st.handleSession)
}

func (st *driverState) handleDBUser(c *gin.Context) {
	if st.pg == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	row := userRow{}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	qerr := st.pg.QueryRow(ctx,
		"SELECT id, name, email, score FROM users WHERE id=$1", id,
	).Scan(&row.ID, &row.Name, &row.Email, &row.Score)
	if qerr != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, row)
}

func (st *driverState) handleCache(c *gin.Context) {
	if st.redis == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	val, err := st.redis.Get(ctx, c.Param("key")).Bytes()
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", val)
}

func (st *driverState) handleMC(c *gin.Context) {
	if st.mc == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	it, err := st.mc.Get(c.Param("key"))
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.Data(http.StatusOK, "application/octet-stream", it.Value)
}

func (st *driverState) handleSession(c *gin.Context) {
	if st.redis == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	var sid string
	if ck, err := c.Request.Cookie(sessionCookieName); err == nil {
		sid = ck.Value
	}
	if sid == "" {
		sid = newSessionID()
		http.SetCookie(c.Writer, &http.Cookie{
			Name:     sessionCookieName,
			Value:    sid,
			Path:     "/",
			HttpOnly: true,
		})
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	key := "sess:" + sid
	seq, _ := st.redis.Incr(ctx, key).Result()
	st.redis.Expire(ctx, key, 10*time.Minute)
	c.JSON(http.StatusOK, sessionResponse{OK: true, Seq: int(seq)})
}

// userRow mirrors the users table row returned by GET /db/user/:id.
type userRow struct {
	ID    int    `json:"id"`
	Name  string `json:"name"`
	Email string `json:"email"`
	Score int    `json:"score"`
}

// sessionResponse is the JSON body returned by POST /session.
type sessionResponse struct {
	OK  bool `json:"ok"`
	Seq int  `json:"seq"`
}

// newSessionID returns a random 16-byte hex session id.
func newSessionID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}
