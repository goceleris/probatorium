package scenarios

import (
	"github.com/goceleris/loadgen"

	"github.com/goceleris/probatorium/servers"
	"github.com/goceleris/probatorium/services"
)

// DriverKind names the driver-backed scenarios. v1.5.4 deepened the set
// from 4 single-op reads to 10 — adding writes, an explicit transaction, a
// multi-row range, a pipelined batch, and a multi-key fetch — so the
// native-driver-vs-ecosystem comparison becomes multi-dimensional instead
// of a single GET number.
const (
	DriverPG        = "driver-pg-read"    // GET /db/user/42 — 1 SELECT (hot row)
	DriverRedis     = "driver-redis-get"  // GET /cache/<key> — 1 GET
	DriverMemcached = "driver-mc-get"     // GET /mc/<key> — 1 GET
	DriverSession   = "driver-session-rw" // POST /session — fixed-key GET+SET round-trip (no cookie)

	// v1.5.4 depth additions:
	DriverPGWrite       = "driver-pg-write"       // POST /db/insert — 1 INSERT (write path)
	DriverPGUpdateTx    = "driver-pg-update-tx"   // POST /db/tx/user/42 — BEGIN;UPDATE;COMMIT
	DriverPGReadRange   = "driver-pg-read-range"  // GET /db/users?limit=50 — N-row result set
	DriverRedisSet      = "driver-redis-set"      // POST /cache — 1 SET (write path)
	DriverRedisPipeline = "driver-redis-pipeline" // GET /cache-pipeline?n=10 — pipelined GETs
	DriverMCMultiGet    = "driver-mc-multiget"    // GET /mc-multiget?keys=10 — multi-key fetch
	DriverMCSet         = "driver-mc-set"         // POST /mc — 1 SET (memcached write path)
)

// DriverKinds is the canonical ordered list of driver scenarios.
var DriverKinds = []string{
	DriverPG, DriverRedis, DriverMemcached, DriverSession,
	DriverPGWrite, DriverPGUpdateTx, DriverPGReadRange,
	DriverRedisSet, DriverRedisPipeline, DriverMCMultiGet, DriverMCSet,
}

// sessionBody is the 256-byte payload POSTed by driver-session-rw. It is
// deterministic so repeat runs send byte-identical requests — any
// throughput delta then reflects server-side work rather than payload
// entropy.
var sessionBody = makeSessionBody()

func makeSessionBody() []byte {
	const size = 256
	b := make([]byte, size)
	for i := range b {
		b[i] = 'x'
	}
	// Make it look like JSON so bodylimit / content-type sniffing
	// middlewares don't choke on it.
	b[0] = '{'
	b[1] = '"'
	b[2] = 'k'
	b[3] = '"'
	b[4] = ':'
	b[5] = '"'
	b[size-2] = '"'
	b[size-1] = '}'
	return b
}

// DriverScenario benches a single hot-path call through a driver (PG
// read of user id=42, Redis GET demo, memcached GET demo, session read/
// write round-trip).
type DriverScenario struct {
	name string
	kind string
}

// NewDriverScenario constructs a [DriverScenario].
func NewDriverScenario(name, kind string) *DriverScenario {
	return &DriverScenario{name: name, kind: kind}
}

// Name implements [Scenario].
func (s *DriverScenario) Name() string { return s.name }

// Category implements [Scenario].
func (s *DriverScenario) Category() string { return CategoryDriver }

// Kind returns the driver kind (one of [DriverPG], [DriverRedis],
// [DriverMemcached], [DriverSession]).
func (s *DriverScenario) Kind() string { return s.kind }

// Workload returns the loadgen.Config for this driver scenario.
// driver-pg-read pins id=42 (seeded by services.Seed); driver-redis-get
// and driver-mc-get both request services.FixtureDemoKey; driver-session-rw
// POSTs a 256-byte payload to /session, which the handler turns into a
// GET+SET round-trip on the fixed server-side key services.FixtureSessionKey
// (no cookie — the key is constant).
func (s *DriverScenario) Workload(target string) loadgen.Config {
	cfg := loadgen.Config{
		Connections: 128,
	}
	switch s.kind {
	case DriverPG:
		cfg.Method = "GET"
		// Fixed id=42 keeps the bench hitting the same row in PG's
		// buffer cache; the scenario is a bounded-reads-per-second
		// comparison, not a random-access workload.
		cfg.URL = target + "/db/user/42"
	case DriverRedis:
		cfg.Method = "GET"
		cfg.URL = target + "/cache/" + services.FixtureDemoKey
	case DriverMemcached:
		cfg.Method = "GET"
		cfg.URL = target + "/mc/" + services.FixtureDemoKey
	case DriverSession:
		cfg.Method = "POST"
		cfg.URL = target + "/session"
		cfg.Body = sessionBody
	case DriverPGWrite:
		cfg.Method = "POST"
		cfg.URL = target + "/db/insert"
		cfg.Body = sessionBody // 256B payload inserted into bench_writes
	case DriverPGUpdateTx:
		cfg.Method = "POST"
		cfg.URL = target + "/db/tx/user/42" // BEGIN;UPDATE score+1;COMMIT on the hot row
		cfg.Body = sessionBody              // body ignored by the handler
	case DriverPGReadRange:
		cfg.Method = "GET"
		cfg.URL = target + "/db/users?limit=50" // 50-row SELECT -> JSON array
	case DriverRedisSet:
		cfg.Method = "POST"
		cfg.URL = target + "/cache"
		cfg.Body = sessionBody // SET services.FixtureRedisWriteKey = body
	case DriverRedisPipeline:
		cfg.Method = "GET"
		// Distinct path (not /cache/pipeline) so it can't collide with the
		// /cache/:key param route in any of the framework routers.
		cfg.URL = target + "/cache-pipeline?n=10" // 10x GET FixtureDemoKey, pipelined
	case DriverMCMultiGet:
		cfg.Method = "GET"
		cfg.URL = target + "/mc-multiget?keys=10" // GetMulti of 10 seeded session keys
	case DriverMCSet:
		cfg.Method = "POST"
		cfg.URL = target + "/mc" // SET the fixed memcached write key = body
		cfg.Body = sessionBody
	}
	return cfg
}

// Applicable requires the server to declare Drivers=true and speak
// HTTP/1.1 on the wire. Driver scenarios drive the server with plain H1
// so a server that only accepts H2C prior-knowledge is skipped —
// otherwise every request is rejected at the parser and the cell
// silently records 0 RPS.
func (s *DriverScenario) Applicable(fs servers.FeatureSet) bool {
	return fs.Drivers && fs.HTTP1
}

// Compile-time assertion that DriverScenario satisfies Scenario.
var _ Scenario = (*DriverScenario)(nil)

func init() {
	Register(NewDriverScenario(DriverPG, DriverPG))
	Register(NewDriverScenario(DriverRedis, DriverRedis))
	Register(NewDriverScenario(DriverMemcached, DriverMemcached))
	Register(NewDriverScenario(DriverSession, DriverSession))
	Register(NewDriverScenario(DriverPGWrite, DriverPGWrite))
	Register(NewDriverScenario(DriverPGUpdateTx, DriverPGUpdateTx))
	Register(NewDriverScenario(DriverPGReadRange, DriverPGReadRange))
	Register(NewDriverScenario(DriverRedisSet, DriverRedisSet))
	Register(NewDriverScenario(DriverRedisPipeline, DriverRedisPipeline))
	Register(NewDriverScenario(DriverMCMultiGet, DriverMCMultiGet))
	Register(NewDriverScenario(DriverMCSet, DriverMCSet))
}
