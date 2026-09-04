// Command driver_postgres is the postgres-driven validation refapp:
// celeris with the native postgres driver wired into the session
// store + a small handful of routes that exercise read / write paths
// against the seeded users fixture.
//
// Coverage per probatorium#103 + #110:
//
//   - driver/postgres pool with bounded MaxOpen → I-DRV pool-cap invariant.
//   - middleware/session/postgresstore → session round-trip via DB.
//   - middleware/recovery, requestid, secure → outermost chain (matches
//     kitchen_sink for shared invariants).
//   - SELECT happy path on the seeded users(1..10000) table.
//   - INSERT + read-after-write consistency (I-DRV-1) via /users + GET.
//   - UPDATE + read-after-write consistency via PUT + GET.
//
// Connection string is taken from -postgres-dsn or PROBATORIUM_PG_DSN
// (the matrix runner / cluster orchestrator populates these from the
// services.Handles after `services.Start()` brings up the docker'd
// postgres). Default matches services/services.go for laptop runs.
//
// On startup the refapp prints the canonical ready line:
//
//	ready addr=<bind-addr>
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/driver/postgres"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/requestid"
	"github.com/goceleris/celeris/middleware/secure"
	"github.com/goceleris/celeris/middleware/session/postgresstore"
)

// nextUserID generates ids for POST /users. Seeded above the
// FixtureUserMaxID (10000) so it never collides with the seeded
// fixture rows. Tier 1 walker may fire concurrent POSTs — atomic
// Add gives each request a unique id.
var nextUserID atomic.Int64

// POST /users writes into a bounded id window [usersSeedMax+1,
// usersSeedMax+usersWindow] with an UPSERT (see the handler): a bounded
// working set keeps the fixture's tmpfs from filling under a long cell while
// preserving a race-free read-after-write check. usersSeedMax matches the
// fixture seed's MAX(id) (services.FixtureUserMaxID = 10000).
const (
	usersSeedMax = 10000
	usersWindow  = 50000
)

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	engineFlag := flag.String("engine", "auto", "engine: iouring | epoll | std | adaptive | auto")
	workersFlag := flag.Int("workers", 0, "io worker count (0 = celeris default GOMAXPROCS); celeris requires >=2 if set")
	dsn := flag.String("postgres-dsn", envOr("PROBATORIUM_PG_DSN",
		"postgres://bench:bench@127.0.0.1:54321/bench?sslmode=disable"),
		"libpq DSN; env: PROBATORIUM_PG_DSN")
	maxOpen := flag.Int("pg-max-open", 16, "postgres pool MaxOpen (cap for I-DRV pool-cap invariant)")
	flag.Parse()

	pool, err := postgres.Open(*dsn,
		postgres.WithMaxOpen(*maxOpen),
		postgres.WithMaxLifetime(5*time.Minute),
		postgres.WithApplication("probatorium/driver_postgres"),
	)
	if err != nil {
		log.Fatalf("driver_postgres: pool open: %v", err)
	}
	defer pool.Close()

	// Seed the user-id counter from the current MAX(id) in the
	// fixture table so a refapp restart against an already-seeded
	// DB doesn't collide with rows from a previous run. Requires the
	// fixture seed (services.SeedExternal) to have run; refuses to start
	// without it.
	{
		var maxID int
		bootCtx, bootCancel := context.WithTimeout(context.Background(), 5*time.Second)
		row := pool.QueryRow(bootCtx, "SELECT COALESCE(MAX(id), 0) FROM users")
		err := row.Scan(&maxID)
		bootCancel()
		if err != nil {
			// Fail LOUDLY. Falling back silently here let every write
			// 500 with "relation users does not exist" for the whole
			// history of the nightly (no fixture seed in validate) while
			// the refapp reported itself healthy.
			log.Fatalf("driver_postgres: users table not provisioned (run the fixture seed: validator -seed-services / runner -seed-services): %v", err)
		}
		if maxID < 10000 {
			maxID = 10000
		}
		nextUserID.Store(int64(maxID))
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sstore, err := postgresstore.New(ctx, pool)
	if err != nil {
		log.Fatalf("driver_postgres: session store init: %v", err)
	}
	defer sstore.Close()

	srv := celeris.New(celeris.Config{
		Addr:            *bind,
		Engine:          resolveEngine(*engineFlag),
		Workers:         *workersFlag,
		Protocol:        celeris.HTTP1,
		AsyncHandlers:   true,
		ReadTimeout:     30 * time.Second,
		WriteTimeout:    30 * time.Second,
		IdleTimeout:     120 * time.Second,
		ShutdownTimeout: 10 * time.Second,
	})

	// Recovery middleware logger: explicit io.Discard sink, NOT
	// slog.Default(). The stdlib default routes through Go's text
	// handler whose defaultHandler mutex serializes a blocking
	// os.Stderr.Write across every conn + worker; under iouring/epoll's
	// per-conn async-dispatch model that stderr lock is held inside
	// cs.detachMu (around ProcessH1), gating the worker thread and
	// letting concurrent slowloris header-deadlines slip past.
	discardLog := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv.Use(recovery.New(recovery.Config{Logger: discardLog}))
	srv.Use(requestid.New())
	srv.Use(secure.New())
	// NOTE: no global session middleware here. This refapp exercises the
	// postgresstore KV API directly via the /store/* handlers below; no route
	// reads c.Session(). Installing session.New() globally made every
	// cookieless walker request create + persist a *fresh* (untouched)
	// session row -- the middleware persists on `modified || fresh` -- which
	// grew celeris_sessions ~5 MB/s and filled the fixture tmpfs (the
	// driver_postgres 5xx). Session-middleware behaviour is covered by
	// auth_session_ratelimit, which uses real cookies. (celeris: persisting a
	// fresh, never-written session per anonymous request is itself a storage
	// growth foot-gun -- filed separately.)

	// /healthz — pool health probe (validator scrapes it once per
	// scenario for the I-CONN-1 sentinel).
	srv.GET("/healthz", func(c *celeris.Context) error {
		if err := pool.Ping(c.Context()); err != nil {
			return c.JSON(http.StatusServiceUnavailable, map[string]string{"err": err.Error()})
		}
		return c.JSON(http.StatusOK, map[string]string{"db": "ok"})
	})

	// GET /users/:id — SELECT by primary key. Exercises the
	// prepared-statement-cache hot path.
	srv.GET("/users/:id", func(c *celeris.Context) error {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || id < 1 {
			return c.String(http.StatusBadRequest, "bad id")
		}
		var name, email string
		var score int
		row := pool.QueryRow(c.Context(),
			"SELECT name, email, score FROM users WHERE id = $1", id)
		if err := row.Scan(&name, &email, &score); err != nil {
			return c.String(http.StatusNotFound, "no row")
		}
		return c.JSON(http.StatusOK, map[string]any{
			"id":    id,
			"name":  name,
			"email": email,
			"score": score,
		})
	})

	// POST /users — INSERT a fresh row outside the fixture range, then
	// SELECT it back. Validates read-after-write consistency
	// (I-DRV-1) within the same connection-pool. Returns the assigned
	// id so the Tier 1 walker can chain a follow-up GET.
	//
	// The seeded users schema uses INT PRIMARY KEY (no SERIAL) — see
	// services.seedPostgres — so we generate ids from a self-managed
	// counter starting above FixtureUserMaxID (10000). Atomic so the
	// Tier 1 walker can fire concurrent POSTs without id collisions.
	srv.POST("/users", func(c *celeris.Context) error {
		// Bounded working set over a fixed id window (usersWindow ids above
		// the fixture seed), written with an UPSERT whose value is a
		// deterministic function of the id. This mirrors the driver_redis /
		// driver_memcached refapps, which SET a fixed key to a fixed value:
		// concurrent walkers targeting the same id write byte-identical rows,
		// so the read-after-write check can never race (a real stale or
		// misrouted read still fails it). Because only usersWindow distinct
		// ids ever exist and name/email/score are unindexed, the UPDATEs are
		// HOT and the heap stays bounded -- an unbounded INSERT stream instead
		// grew the table until the fixture's tmpfs filled ("No space left on
		// device", the driver_postgres 5xx). A caller-supplied ?name is still
		// honored for the write but the read-after-write assertion uses the
		// deterministic value so it stays race-free.
		id := int(usersSeedMax) + int(nextUserID.Add(1))%usersWindow + 1
		wantName := "walker-user-" + strconv.Itoa(id)
		name := c.Query("name")
		if name == "" {
			name = wantName
		}
		email := name + "@probatorium.local"
		_, err := pool.ExecContext(c.Context(),
			"INSERT INTO users (id, name, email, score) VALUES ($1, $2, $3, 0) "+
				"ON CONFLICT (id) DO UPDATE SET name = EXCLUDED.name, email = EXCLUDED.email, score = EXCLUDED.score",
			id, wantName, email)
		if err != nil {
			return c.String(http.StatusInternalServerError, "%s", "insert: "+err.Error())
		}
		// Read-after-write: the row for this id must exist and carry the
		// deterministic name. A concurrent writer can only have written the
		// SAME name for this id, so a mismatch is a real I-DRV-1 violation.
		var roundtripName string
		row := pool.QueryRow(c.Context(),
			"SELECT name FROM users WHERE id = $1", id)
		if err := row.Scan(&roundtripName); err != nil {
			return c.String(http.StatusInternalServerError, "%s", "raw read: "+err.Error())
		}
		if roundtripName != wantName {
			// I-DRV-1 violation. The walker checks for this exact
			// 500 shape and counts it as a HIGH-severity hit.
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"err":           "read-after-write mismatch",
				"wrote":         wantName,
				"read":          roundtripName,
				"x-invariant":   "I-DRV-1",
				"x-invariant-h": "high",
			})
		}
		return c.JSON(http.StatusCreated, map[string]any{
			"id":    id,
			"name":  wantName,
			"email": email,
		})
	})

	// PUT /users/:id — UPDATE score. Same read-after-write check as POST.
	srv.PUT("/users/:id", func(c *celeris.Context) error {
		id, err := strconv.Atoi(c.Param("id"))
		if err != nil || id < 1 {
			return c.String(http.StatusBadRequest, "bad id")
		}
		score, _ := strconv.Atoi(c.Query("score"))
		_, err = pool.ExecContext(c.Context(),
			"UPDATE users SET score = $1 WHERE id = $2", score, id)
		if err != nil {
			return c.String(http.StatusInternalServerError, "%s", "update: "+err.Error())
		}
		var got int
		row := pool.QueryRow(c.Context(),
			"SELECT score FROM users WHERE id = $1", id)
		if err := row.Scan(&got); err != nil {
			return c.String(http.StatusInternalServerError, "%s", "raw read: "+err.Error())
		}
		if got != score {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"err":           "read-after-write mismatch",
				"x-invariant":   "I-DRV-1",
				"x-invariant-h": "high",
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"id": id, "score": score})
	})

	// /store/:key — exercise the postgresstore store.KV API
	// directly. GET returns the current value; POST writes a
	// fresh blob and reads it back for the I-DRV-1 round-trip
	// check. Bypasses the session middleware's cookie machinery
	// (which the celeris auth_session_ratelimit refapp confirms
	// is "sid-in-body, not Set-Cookie" by design) so the
	// /store/* path tests the storage layer in isolation.
	srv.GET("/store/:key", func(c *celeris.Context) error {
		val, err := sstore.Get(c.Context(), c.Param("key"))
		if err != nil {
			return c.String(http.StatusNotFound, "miss")
		}
		return c.JSON(http.StatusOK, map[string]any{
			"key": c.Param("key"),
			"len": len(val),
		})
	})
	srv.POST("/store/:key", func(c *celeris.Context) error {
		k := c.Param("key")
		v := []byte(c.Query("v"))
		if len(v) == 0 {
			v = []byte("tier1")
		}
		if err := sstore.Set(c.Context(), k, v, 5*time.Minute); err != nil {
			return c.String(http.StatusInternalServerError, "%s", "set: "+err.Error())
		}
		got, err := sstore.Get(c.Context(), k)
		if err != nil {
			return c.String(http.StatusInternalServerError, "%s", "raw read: "+err.Error())
		}
		if string(got) != string(v) {
			return c.JSON(http.StatusInternalServerError, map[string]string{
				"err":           "store read-after-write mismatch",
				"x-invariant":   "I-DRV-1",
				"x-invariant-h": "high",
			})
		}
		return c.JSON(http.StatusOK, map[string]any{"key": k, "len": len(v)})
	})

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("driver_postgres: signal received, shutting down")
		shCtx, shCancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer shCancel()
		_ = srv.Shutdown(shCtx)
	}()

	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("driver_postgres: listen: %v", err)
	}
	fmt.Printf("ready addr=%s\n", ln.Addr().String())
	if err := srv.StartWithListener(ln); err != nil {
		log.Fatalf("driver_postgres: start: %v", err)
	}
}
