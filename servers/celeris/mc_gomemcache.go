package main

// L857F (celeris#857) arm G: the celeris engine and the celeris HTTP routes, with the memcached backend served by github.com/bradfitz/gomemcache
// exactly as the fasthttp adapter calls it (servers/fasthttp/driver_handlers.go mcHandler / mcMultiGetHandler / mcSetHandler): mc.Get(key),
// mc.GetMulti(keys), mc.Set(&Item{Key: "demo-write", Value: body}), MaxIdleConns 16, gomemcache's own default timeout, NO per-request context.
// The HTTP responses are byte-for-byte those of the celeris handlers (Blob / JSON sessionResponse). Selected only by PROBATORIUM_MC_CLIENT=gomemcache
// (mc_knobs.go); with the knob unset none of this code runs.

import (
	"strconv"

	"github.com/bradfitz/gomemcache/memcache"
	"github.com/goceleris/celeris"
)

// gomemcacheIdle is MaxIdleConns of the fasthttp adapter's gomemcache client (driverPoolSize there).
const gomemcacheIdle = 16

type gomemcacheMC struct{ mc *memcache.Client }

func newGomemcacheMC(addr string) *gomemcacheMC {
	mc := memcache.New(addr)
	mc.MaxIdleConns = gomemcacheIdle
	return &gomemcacheMC{mc: mc}
}

// getHandler serves GET /mc/:key (cf. driverClients.mcHandler).
func (g *gomemcacheMC) getHandler(ctx *celeris.Context) error {
	item, err := g.mc.Get(ctx.Param("key"))
	if err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.Blob(200, "application/octet-stream", item.Value)
}

// multiGetHandler serves GET /mc-multiget?keys=N (cf. driverClients.mcMultiGetHandler).
func (g *gomemcacheMC) multiGetHandler(ctx *celeris.Context) error {
	n, err := strconv.Atoi(ctx.Query("keys"))
	if err != nil || n <= 0 || n > 100 {
		n = 10
	}
	keys := make([]string, n)
	for i := 0; i < n; i++ {
		keys[i] = "user:" + strconv.Itoa(i+1) + ":session"
	}
	items, err := g.mc.GetMulti(keys)
	if err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.JSON(200, sessionResponse{OK: true, Seq: len(items)})
}

// setHandler serves POST /mc (cf. driverClients.mcSetHandler).
func (g *gomemcacheMC) setHandler(ctx *celeris.Context) error {
	if err := g.mc.Set(&memcache.Item{Key: "demo-write", Value: ctx.Body()}); err != nil {
		return ctx.AbortWithStatus(503)
	}
	return ctx.JSON(200, sessionResponse{OK: true})
}
