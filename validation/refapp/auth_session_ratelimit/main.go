// Command auth_session_ratelimit is the first validation-tier reference
// application. It runs celeris with a composed session + ratelimit +
// jwt middleware stack over a CRUD surface backed by an in-memory user
// store. Endpoints match validation/spec/auth_session_ratelimit.openapi.yaml,
// the Markov matrix at validation/markov/auth_session_ratelimit.yaml,
// and the seed corpus's middleware-interaction sub-band (0x600-0x60b).
//
// On startup the refapp prints one canonical line to stdout:
//
//	ready addr=<bind-addr>
//
// — same convention as competitor adapters. The validator orchestrator
// keys off it as the "process is up" signal. Subsequent stdout lines
// are informational; stderr carries warnings and panics.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/goceleris/celeris"
	"github.com/goceleris/celeris/middleware/ratelimit"
	"github.com/goceleris/celeris/middleware/recovery"
	"github.com/goceleris/celeris/middleware/session"
	"github.com/goceleris/celeris/middleware/sse"
	"github.com/goceleris/celeris/middleware/websocket"
	"github.com/goceleris/probatorium/validation/refapp/internal/debugvars"
)

// User is the in-memory record served at /api/users/{id}. The JSON
// shape matches the OpenAPI spec's User schema; the validation
// tier's RESTler-style fuzzer keys off these field names when
// inferring dependencies.
type User struct {
	ID        string    `json:"id"`
	Username  string    `json:"username"`
	Email     string    `json:"email,omitempty"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// store is the in-memory User store. Single-writer-per-key model:
// writes hold the RWMutex's write lock; reads only need the read lock.
// Deterministic id allocation (nextID counter) keeps the validator's
// shadow map cheap.
type store struct {
	mu     sync.RWMutex
	users  map[string]*User
	byName map[string]string
	nextID int64
}

func newStore() *store {
	return &store{
		users:  map[string]*User{},
		byName: map[string]string{},
	}
}

func (s *store) create(username, email string) *User {
	s.mu.Lock()
	defer s.mu.Unlock()
	if existing, ok := s.byName[username]; ok {
		return s.users[existing]
	}
	s.nextID++
	id := strconv.FormatInt(s.nextID, 10)
	now := time.Now().UTC()
	u := &User{
		ID:        id,
		Username:  username,
		Email:     email,
		CreatedAt: now,
		UpdatedAt: now,
	}
	s.users[id] = u
	s.byName[username] = id
	return u
}

func (s *store) get(id string) (*User, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.users[id]
	return u, ok
}

func (s *store) update(id string, patch User) (*User, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return nil, false
	}
	if patch.Username != "" {
		// re-index byName if the username changed.
		if patch.Username != u.Username {
			delete(s.byName, u.Username)
			s.byName[patch.Username] = id
		}
		u.Username = patch.Username
	}
	if patch.Email != "" {
		u.Email = patch.Email
	}
	u.UpdatedAt = time.Now().UTC()
	return u, true
}

func (s *store) delete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	u, ok := s.users[id]
	if !ok {
		return false
	}
	delete(s.users, id)
	delete(s.byName, u.Username)
	return true
}

func (s *store) list(limit int) []*User {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]*User, 0, len(s.users))
	for _, u := range s.users {
		out = append(out, u)
	}
	sort.Slice(out, func(i, j int) bool {
		ai, _ := strconv.ParseInt(out[i].ID, 10, 64)
		aj, _ := strconv.ParseInt(out[j].ID, 10, 64)
		return ai < aj
	})
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out
}

func main() {
	bind := flag.String("bind", "127.0.0.1:8080", "address:port to listen on")
	rps := flag.Float64("rps", 1000, "ratelimit RPS per key")
	burst := flag.Int("burst", 200, "ratelimit burst per key")
	engineFlag := flag.String("engine", "auto", "engine: iouring | epoll | std | adaptive | auto (picks iouring on Linux, std elsewhere)")
	workersFlag := flag.Int("workers", 0, "io worker count (0 = celeris default GOMAXPROCS); celeris requires >=2 if set")
	asyncHandlers := flag.Bool("async-handlers", true,
		"celeris.Config.AsyncHandlers. Set false to exercise the hasAsyncRoutes() derivation: "+
			"AsyncHandlers off, but the .Async() route below still forces async dispatch — the bench "+
			"epoll-h1-sync config that crashed in celeris#309 when a sync ws/sse handler ran inline.")
	flag.Parse()

	users := newStore()
	// Seed two starter users so empty-store edge cases don't dominate
	// the Markov trajectories. Both are referenceable by both the
	// RESTler fuzzer (it captures these ids from initial /api/users
	// list responses) and by the Tier-1 traffic generator.
	users.create("alice", "alice@example.com")
	users.create("bob", "bob@example.com")

	// Engine selection. Production deploys to Linux and wants iouring,
	// but local dev / CI lint hosts (macOS, Windows) can't load that
	// engine — config validation rejects it. "auto" picks iouring on
	// linux, std elsewhere, so this same binary runs both places.
	engineType := resolveEngine(*engineFlag)

	dv := debugvars.New() // /debug/vars + /debug/pprof for the validator's property loop
	srv := dv.NewServer(celeris.Config{
		Addr:            *bind,
		Engine:          engineType,
		Workers:         *workersFlag,
		Protocol:        celeris.HTTP1,
		AsyncHandlers:   *asyncHandlers,
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
	srv.Use(recovery.New(recovery.Config{Logger: dv.RecoveryLogger(discardLog)}))
	// /ws and /events are transport-level endpoints (WS upgrade + SSE
	// long-poll). Both are exercised by Tier 1 walkers that don't carry
	// a session — and they shouldn't have to: WS handshakes are
	// authenticated at the protocol layer (Origin / Sec-WebSocket-Key),
	// SSE is anonymous broker fan-out. Carving them out of the session
	// + ratelimit middleware so the walker's torture frames + kill-
	// mid-stream RSTs actually reach the engine.
	//
	// The 3-day soak that just landed found this gap: ws_torture and
	// sse_kill slices were vacuously passing because every upgrade was
	// being 401'd / 429'd before reaching the WS or SSE handler. With
	// these skips in place the next soak will exercise the engine paths
	// these slices were built to test.
	transportEndpoints := []string{"/ws", "/events"}
	srv.Use(session.New(session.Config{
		CookieName: "sid",
		SkipPaths:  transportEndpoints,
	}))
	srv.Use(ratelimit.New(ratelimit.Config{
		RPS:   *rps,
		Burst: *burst,
		// Skip paths that are part of the auth handshake so a
		// rate-limited login does not lock out the entire suite. Also
		// skip transport endpoints — they're walker targets that need
		// to reach the engine without contention for the shared rate
		// budget.
		SkipPaths: append([]string{"/login"}, transportEndpoints...),
	}))

	registerRoutes(srv, users)

	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
		<-sig
		log.Printf("auth_session_ratelimit: signal received, shutting down")
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = srv.Shutdown(ctx)
	}()

	// Print the canonical ready line BEFORE Start (which blocks). The
	// orchestrator parses this line to know when to start probing.
	ln, err := net.Listen("tcp", *bind)
	if err != nil {
		log.Fatalf("auth_session_ratelimit: listen: %v", err)
	}
	fmt.Printf("ready addr=%s\n", ln.Addr().String())
	if err := srv.StartWithListener(ln); err != nil {
		log.Fatalf("auth_session_ratelimit: start: %v", err)
	}
}

// registerRoutes wires every endpoint listed in the OpenAPI spec to
// the in-memory store, with one twist: every handler short-circuits
// to 401 if no session is established (except /login itself).
func registerRoutes(srv *celeris.Server, users *store) {
	// /async-data is a trivial .Async() route. Its mere presence flips the
	// engine's hasAsyncRoutes() to true, so even with -async-handlers=false the
	// listener runs async — the EXACT derivation the bench epoll-h1-sync config
	// uses, and the one that crashed in celeris#309 when a SYNC ws/sse handler
	// ran inline and Detached. Registered here (with the other routes, after all
	// Use middleware) so it never trips the Use-after-route guard.
	srv.GET("/async-data", func(c *celeris.Context) error {
		return c.JSON(200, map[string]string{"mode": "async-route"})
	}).Async()
	srv.POST("/login", func(c *celeris.Context) error {
		var req struct {
			Username string `json:"username"`
			Password string `json:"password"`
		}
		if err := json.Unmarshal(c.Body(), &req); err != nil || req.Username == "" {
			return c.JSON(400, map[string]string{"error": "bad request"})
		}
		// Refapp policy: any non-empty (username, password) authenticates.
		// The refapp's whole purpose is to exercise middleware behaviour,
		// not auth correctness — real password hashing is wave 8.
		sess := session.FromContext(c)
		if sess == nil {
			return c.JSON(500, map[string]string{"error": "session middleware missing"})
		}
		sess.Set("user", req.Username)
		_ = sess.Save()
		u := users.create(req.Username, "")
		return c.JSON(200, map[string]any{
			"sid":        sess.ID(),
			"user_id":    u.ID,
			"expires_at": time.Now().Add(time.Hour).UTC().Format(time.RFC3339),
		})
	})

	srv.POST("/logout", func(c *celeris.Context) error {
		if sess := session.FromContext(c); sess != nil {
			_ = sess.Destroy()
		}
		return c.NoContent(204)
	})

	authed := func(c *celeris.Context, h celeris.HandlerFunc) error {
		sess := session.FromContext(c)
		if sess == nil || sess.GetString("user") == "" {
			return c.JSON(401, map[string]string{"error": "unauthenticated"})
		}
		return h(c)
	}

	srv.GET("/me", func(c *celeris.Context) error {
		return authed(c, func(c *celeris.Context) error {
			sess := session.FromContext(c)
			username := sess.GetString("user")
			if id, ok := users.byNameLookup(username); ok {
				if u, ok2 := users.get(id); ok2 {
					return c.JSON(200, u)
				}
			}
			return c.JSON(404, map[string]string{"error": "not found"})
		})
	})

	srv.GET("/api/users", func(c *celeris.Context) error {
		return authed(c, func(c *celeris.Context) error {
			limit := 20
			if v := c.Query("limit"); v != "" {
				if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 100 {
					limit = n
				}
			}
			list := users.list(limit)
			return c.JSON(200, map[string]any{
				"users":       list,
				"next_cursor": "",
			})
		})
	})

	srv.POST("/api/users", func(c *celeris.Context) error {
		return authed(c, func(c *celeris.Context) error {
			var req struct {
				Username string `json:"username"`
				Email    string `json:"email"`
			}
			if err := json.Unmarshal(c.Body(), &req); err != nil || req.Username == "" {
				return c.JSON(400, map[string]string{"error": "schema validation failed"})
			}
			u := users.create(req.Username, req.Email)
			c.SetHeader("Location", "/api/users/"+u.ID)
			return c.JSON(201, u)
		})
	})

	srv.GET("/api/users/:id", func(c *celeris.Context) error {
		return authed(c, func(c *celeris.Context) error {
			u, ok := users.get(c.Param("id"))
			if !ok {
				return c.JSON(404, map[string]string{"error": "not found"})
			}
			return c.JSON(200, u)
		})
	})

	srv.PUT("/api/users/:id", func(c *celeris.Context) error {
		return authed(c, func(c *celeris.Context) error {
			var patch User
			if err := json.Unmarshal(c.Body(), &patch); err != nil {
				return c.JSON(400, map[string]string{"error": "schema validation failed"})
			}
			u, ok := users.update(c.Param("id"), patch)
			if !ok {
				return c.JSON(404, map[string]string{"error": "not found"})
			}
			return c.JSON(200, u)
		})
	})

	srv.DELETE("/api/users/:id", func(c *celeris.Context) error {
		return authed(c, func(c *celeris.Context) error {
			if !users.delete(c.Param("id")) {
				return c.JSON(404, map[string]string{"error": "not found"})
			}
			return c.NoContent(204)
		})
	})

	srv.GET("/api/users/:id/posts", func(c *celeris.Context) error {
		return authed(c, func(c *celeris.Context) error {
			if _, ok := users.get(c.Param("id")); !ok {
				return c.JSON(404, map[string]string{"error": "not found"})
			}
			// Posts are synthesised — deterministic per-user id seeded
			// list. The validator's read-after-write shadow check does
			// NOT touch posts (they have no write endpoint), so the
			// stable derivation is fine.
			id := c.Param("id")
			posts := []map[string]any{
				{"id": "p1-" + id, "author_id": id, "body": "hello from " + id, "posted_at": time.Unix(0, 0).UTC()},
				{"id": "p2-" + id, "author_id": id, "body": "another post", "posted_at": time.Unix(0, 0).UTC()},
			}
			return c.JSON(200, posts)
		})
	})

	// /ws — echo WebSocket endpoint. Tier 1's WS torture walker dials
	// here with malformed frames (fragmented continuation, oversize,
	// ping floods, unmasked client→server) and asserts the engine
	// closes the conn with the appropriate close code (1002 / 1003 /
	// 1009) rather than accepting + echoing the bad bytes.
	//
	// CheckOrigin returns true so the validator can dial without a
	// matching Origin header. Same-origin enforcement is exercised by
	// the auth-flow tests, not WS torture.
	srv.GET("/ws", websocket.New(websocket.Config{
		CheckOrigin: func(c *celeris.Context) bool { return true },
		// 256 KiB read limit is generous — large enough that legitimate
		// large messages don't trip it during normal traffic, small
		// enough that the oversize-payload torture mode (which declares
		// a 1 GiB length) actually fires the limit check.
		ReadLimit: 256 * 1024,
		Handler: func(c *websocket.Conn) {
			for {
				mt, msg, err := c.ReadMessage()
				if err != nil {
					return
				}
				if err := c.WriteMessage(mt, msg); err != nil {
					return
				}
			}
		},
	}))

	// /events — SSE long-poll endpoint. Tier 1's SSE walker dials
	// here, holds for a randomised duration, then RSTs the conn
	// mid-stream. The broker MUST clean up the client slot (no
	// goroutine leak, no FD leak). The validator's I-CONN-2 invariant
	// (accepted − closed − active == 0) catches a stuck broker.
	//
	// Stream tick is 100ms — fast enough that even a 1s-held client
	// reliably sees a few events, slow enough that the connection
	// isn't bottlenecked on broker throughput.
	srv.GET("/events", sse.New(sse.Config{
		HeartbeatInterval: 1 * time.Second,
		Handler: func(client *sse.Client) {
			tick := time.NewTicker(100 * time.Millisecond)
			defer tick.Stop()
			n := 0
			ctx := client.Context()
			for {
				select {
				case <-ctx.Done():
					return
				case <-tick.C:
					n++
					if err := client.Send(sse.Event{
						Event: "tick",
						Data:  strconv.Itoa(n),
					}); err != nil {
						return
					}
				}
			}
		},
	}))
}

// byNameLookup exposes the byName map under a read lock. Placed here
// (not on the original store type) so the entire HTTP layer stays in
// main.go for legibility — the refapp's surface is small enough that
// a single file is the right shape.
func (s *store) byNameLookup(name string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	id, ok := s.byName[name]
	return id, ok
}

// resolveEngine maps the -engine flag value to a celeris.EngineType.
//
// `auto` picks iouring on Linux and std elsewhere — the production
// celeris configuration. The previous "auto → std on Linux"
// workaround for goceleris/celeris#273 (iouring + epoll hung on WS
// hijack + SSE streaming) is no longer needed: celeris v1.4.4 fixed
// both bugs and the soak now exercises the engine path it actually
// ships under in production.
//
// The explicit `iouring`/`epoll`/`std`/`adaptive` flag values still
// work — the engine matrix runner (probatorium#103) uses them to
// exercise the full grid per refapp/cell/arch.
func resolveEngine(name string) celeris.EngineType {
	switch name {
	case "iouring":
		return celeris.IOUring
	case "epoll":
		return celeris.Epoll
	case "std":
		return celeris.Std
	case "adaptive":
		return celeris.Adaptive
	case "auto":
		if isLinux() {
			return celeris.IOUring
		}
		return celeris.Std
	}
	// Unknown value — fall back to std rather than crash; the engine
	// validation will succeed on every platform.
	return celeris.Std
}
