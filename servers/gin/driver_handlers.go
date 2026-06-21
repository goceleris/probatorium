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

	// v1.5.4 driver-depth routes (idiomatic pgx/go-redis/gomemcache).
	r.POST("/db/insert", st.handleDBInsert)
	r.POST("/db/tx/user/:id", st.handleDBTx)
	r.GET("/db/users", st.handleDBUsersRange)
	r.POST("/cache", st.handleCacheSet)
	r.GET("/cache-pipeline", st.handleCachePipeline)
	r.GET("/mc-multiget", st.handleMCMultiGet)
}

// handleDBInsert serves POST /db/insert: INSERT the body into bench_writes.
func (st *driverState) handleDBInsert(c *gin.Context) {
	if st.pg == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	body, _ := c.GetRawData()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if _, err := st.pg.Exec(ctx, "INSERT INTO bench_writes(payload) VALUES($1)", string(body)); err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, sessionResponse{OK: true})
}

// handleDBTx serves POST /db/tx/user/:id: BEGIN; UPDATE score+1; COMMIT.
func (st *driverState) handleDBTx(c *gin.Context) {
	if st.pg == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil {
		c.AbortWithStatus(http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	tx, err := st.pg.Begin(ctx)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	if _, err := tx.Exec(ctx, "UPDATE users SET score=score+1 WHERE id=$1", id); err != nil {
		_ = tx.Rollback(ctx)
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	if err := tx.Commit(ctx); err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, sessionResponse{OK: true, Seq: id})
}

// handleDBUsersRange serves GET /db/users?limit=N: SELECT N rows -> JSON array.
func (st *driverState) handleDBUsersRange(c *gin.Context) {
	if st.pg == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	limit, err := strconv.Atoi(c.Query("limit"))
	if err != nil || limit <= 0 || limit > 1000 {
		limit = 50
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	rows, err := st.pg.Query(ctx,
		"SELECT id, name, email, score FROM users WHERE id BETWEEN 1 AND $1 ORDER BY id", limit)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	defer rows.Close()
	out := make([]userRow, 0, limit)
	for rows.Next() {
		var r userRow
		if err := rows.Scan(&r.ID, &r.Name, &r.Email, &r.Score); err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		out = append(out, r)
	}
	if rows.Err() != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, out)
}

// handleCacheSet serves POST /cache: SET demo-write = body.
func (st *driverState) handleCacheSet(c *gin.Context) {
	if st.redis == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	body, _ := c.GetRawData()
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	if err := st.redis.Set(ctx, "demo-write", body, 0).Err(); err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, sessionResponse{OK: true})
}

// handleCachePipeline serves GET /cache-pipeline?n=N: pipeline N GETs of demo-key.
func (st *driverState) handleCachePipeline(c *gin.Context) {
	if st.redis == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	n, err := strconv.Atoi(c.Query("n"))
	if err != nil || n <= 0 || n > 100 {
		n = 10
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), 5*time.Second)
	defer cancel()
	pipe := st.redis.Pipeline()
	cmds := make([]*redis.StringCmd, n)
	for i := 0; i < n; i++ {
		cmds[i] = pipe.Get(ctx, "demo-key")
	}
	if _, err := pipe.Exec(ctx); err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	total := 0
	for _, cmd := range cmds {
		v, err := cmd.Bytes()
		if err != nil {
			c.AbortWithStatus(http.StatusServiceUnavailable)
			return
		}
		total += len(v)
	}
	c.JSON(http.StatusOK, sessionResponse{OK: true, Seq: total})
}

// handleMCMultiGet serves GET /mc-multiget?keys=N: GetMulti of N session keys.
func (st *driverState) handleMCMultiGet(c *gin.Context) {
	if st.mc == nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	n, err := strconv.Atoi(c.Query("keys"))
	if err != nil || n <= 0 || n > 100 {
		n = 10
	}
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = "user:" + strconv.Itoa(i+1) + ":session"
	}
	items, err := st.mc.GetMulti(keys)
	if err != nil {
		c.AbortWithStatus(http.StatusServiceUnavailable)
		return
	}
	c.JSON(http.StatusOK, sessionResponse{OK: true, Seq: len(items)})
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
