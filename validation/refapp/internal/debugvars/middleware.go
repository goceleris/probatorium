// Middleware-oracle counters for the /debug/vars document.
//
// The checker has always had predicates for the session, ratelimit and JWT
// middleware (I-MW-SESSION, I-MW-RATELIMIT, I-MW-JWT) and has never had data
// for them: their counters were specified to arrive over celeris's
// validation-build unix socket, the refapps are plain builds, and a missing
// counter parses as zero -- which reads exactly like "clean". All three sat
// in not_instrumented in all 48 cells of the passing nightly AND the failing
// v1.5.11 soak, I-MW-SESSION among them, while celeris#507 (a session bug)
// failed two consecutive soaks (probatorium#297).
//
// So the oracles live here, outside celeris, where they work in every build:
//
//   - Session: the refapp minted the id, so it knows whose it is. It keeps an
//     id→owner ledger and counts every request that presents a KNOWN id under
//     a different owner. Session bleed is observable end to end, no build tag.
//     celeris's own DroppedCookies() gauge covers the other half -- an
//     id-changing cookie that never reached the client (celeris#507).
//   - Ratelimit: the refapp configures RPS and burst, so it can re-derive the
//     bound the token bucket promises and count admissions the bound cannot
//     explain.
//   - JWT: the refapp mints the tokens, so it can re-read the exp claim of
//     every token the middleware ADMITTED and count the ones already expired.
//
// Declare is the other half of the fix. Eight of the nine matrix refapps do
// not install session middleware; if their structurally-zero counters counted
// as a pass, the vacuity would simply have moved. A refapp names only the
// predicates it can actually judge, and the evaluator reports the rest as
// not-instrumented for that cell.

package debugvars

import (
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// RateLimitShadowSlack multiplies the configured burst to size the shadow
// bucket. The shadow is deliberately LOOSER than celeris's limiter: it is
// sampled after the real admission (so it has seen at least as much elapsed
// time, i.e. at least as many refilled tokens) and it runs one bucket per key
// against a sharded limiter. Slack keeps every ordering and rounding
// difference on the permissive side, so a violation means the limiter admitted
// a full extra burst beyond its own bound -- the token-bucket structural bug
// the predicate names, not jitter.
const RateLimitShadowSlack = 2

// tokenShadowMaxKeys caps the shadow's key table. celeris's default KeyFunc
// is the client IP, which a forged X-Forwarded-For can vary without bound;
// past the cap the shadow stops tracking new keys and admits them, because a
// bucket it never filled cannot indict anyone.
const tokenShadowMaxKeys = 1024

// sessionLedgerMax caps the id→owner table. The refapps run for a full soak
// and every logout mints a fresh id, so the table has to be bounded. On
// overflow it is dropped whole rather than evicted piecemeal: an id the
// ledger has forgotten is judged as unknown, which loses coverage until that
// walker logs in again but can never produce a false mismatch.
const sessionLedgerMax = 1 << 16

// dropsFn is the celeris session.DroppedCookies function shape.
type dropsFn func() uint64

// middleware holds the middleware-oracle counters behind the document. The
// zero value is ready to use; a refapp that installs none of the middleware
// leaves every counter at zero and declares nothing.
type middleware struct {
	sessionMismatches atomic.Int64
	sessionsCreated   atomic.Int64
	sessionsExpired   atomic.Int64

	rlAllowed         atomic.Int64
	rlRejected        atomic.Int64
	rlTokenViolations atomic.Int64

	jwtOK         atomic.Int64
	jwtFail       atomic.Int64
	jwtLateAdmits atomic.Int64

	// drops reads celeris's own DroppedCookies gauge. A pointer so the
	// document can be served before the refapp has wired it (or at all).
	drops atomic.Pointer[dropsFn]

	declMu   sync.Mutex
	declared []string // sorted, de-duplicated

	ledgerMu sync.Mutex
	ledger   map[string]string // session id → owner
}

// Declare records that this refapp feeds the named property predicates, so
// the checker judges them for this cell instead of reporting them as
// not-instrumented. Call it once at startup, after the middleware is wired.
// Unknown or duplicate IDs are harmless; the list is sorted and deduplicated.
func (v *Vars) Declare(ids ...string) {
	m := &v.mw
	m.declMu.Lock()
	defer m.declMu.Unlock()
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if i := sort.SearchStrings(m.declared, id); i < len(m.declared) && m.declared[i] == id {
			continue
		}
		m.declared = append(m.declared, id)
	}
	sort.Strings(m.declared)
}

// InstrumentedProperties is the declared set as the document carries it: a
// sorted, comma-joined list of predicate IDs.
func (v *Vars) InstrumentedProperties() string {
	m := &v.mw
	m.declMu.Lock()
	defer m.declMu.Unlock()
	return strings.Join(m.declared, ",")
}

// SessionLogin binds a session id to the owner the refapp just authenticated
// it as, and counts the session as created. A re-login on the same id rebinds
// it: the client holding the cookie is authoritative, so a stale ledger entry
// must not be reported as a bleed.
func (v *Vars) SessionLogin(id, owner string) {
	if id == "" || owner == "" {
		return
	}
	m := &v.mw
	m.sessionsCreated.Add(1)
	m.ledgerMu.Lock()
	defer m.ledgerMu.Unlock()
	if m.ledger == nil {
		m.ledger = make(map[string]string, 1024)
	}
	if len(m.ledger) >= sessionLedgerMax {
		m.ledger = make(map[string]string, 1024) // see sessionLedgerMax
	}
	m.ledger[id] = owner
}

// SessionCheck reports whether the request presenting id really belongs to
// owner. An id the ledger never saw (or has dropped) is unknown, not wrong,
// and is not judged. A KNOWN id arriving under a different owner is session
// bleed: it counts, and the caller gets false so it can also fail the request
// loudly.
func (v *Vars) SessionCheck(id, owner string) bool {
	if id == "" {
		return true
	}
	m := &v.mw
	m.ledgerMu.Lock()
	want, known := m.ledger[id]
	m.ledgerMu.Unlock()
	if !known || want == owner {
		return true
	}
	m.sessionMismatches.Add(1)
	return false
}

// SessionLogout forgets id and counts the session as expired.
func (v *Vars) SessionLogout(id string) {
	m := &v.mw
	m.sessionsExpired.Add(1)
	if id == "" {
		return
	}
	m.ledgerMu.Lock()
	delete(m.ledger, id)
	m.ledgerMu.Unlock()
}

// SessionOwnerMismatches is the running mismatch count.
func (v *Vars) SessionOwnerMismatches() int64 { return v.mw.sessionMismatches.Load() }

// TrackSessionCookieDrops wires celeris's middleware/session.DroppedCookies
// into the document. Refapps that install session middleware should pass that
// function directly; the gauge is process-wide and present in every build.
func (v *Vars) TrackSessionCookieDrops(fn func() uint64) {
	if fn == nil {
		return
	}
	f := dropsFn(fn)
	v.mw.drops.Store(&f)
}

// SessionCookieDrops is the tracked gauge, or 0 when nothing is tracked.
func (v *Vars) SessionCookieDrops() int64 {
	if f := v.mw.drops.Load(); f != nil {
		return int64((*f)())
	}
	return 0
}

// RateLimitAdmitted counts one request the limiter let through.
func (v *Vars) RateLimitAdmitted() { v.mw.rlAllowed.Add(1) }

// RateLimitRejected counts one request the limiter turned away (429).
func (v *Vars) RateLimitRejected() { v.mw.rlRejected.Add(1) }

// RecordRateLimitTokenViolation counts one admission the configured
// token-bucket bound cannot account for. See [TokenShadow].
func (v *Vars) RecordRateLimitTokenViolation() { v.mw.rlTokenViolations.Add(1) }

// JWTValidated counts one verdict from the JWT middleware.
func (v *Vars) JWTValidated(ok bool) {
	if ok {
		v.mw.jwtOK.Add(1)
		return
	}
	v.mw.jwtFail.Add(1)
}

// RecordJWTLateAdmit counts one request the JWT middleware admitted carrying
// a token whose exp had already passed.
func (v *Vars) RecordJWTLateAdmit() { v.mw.jwtLateAdmits.Add(1) }

// document adds the middleware-oracle keys to doc. Every key is present in
// every refapp's document -- the checker's parser reads a fixed shape, and
// celeris.instrumented_properties (not the presence of a key) is what says
// whether a zero means anything.
func (v *Vars) middlewareDocument(doc map[string]any) {
	m := &v.mw
	doc["celeris.session_owner_mismatches"] = m.sessionMismatches.Load()
	doc["celeris.sessions_created_total"] = m.sessionsCreated.Load()
	doc["celeris.sessions_expired_total"] = m.sessionsExpired.Load()
	doc["celeris.session_cookie_drops"] = v.SessionCookieDrops()
	doc["celeris.ratelimit_allowed"] = m.rlAllowed.Load()
	doc["celeris.ratelimit_rejected"] = m.rlRejected.Load()
	doc["celeris.ratelimit_token_violations"] = m.rlTokenViolations.Load()
	doc["celeris.jwt_validated_ok"] = m.jwtOK.Load()
	doc["celeris.jwt_validated_fail"] = m.jwtFail.Load()
	doc["celeris.jwt_late_admits"] = m.jwtLateAdmits.Load()
	doc["celeris.instrumented_properties"] = v.InstrumentedProperties()
	// True only in a -tags=checkptr build. The property loop declares
	// I-CHECKPTR on it, so a normal build never reports the predicate as
	// covered when its counter is merely zero.
	doc["celeris.checkptr_build"] = checkptrBuild
}

// TokenShadow is an independent token bucket the refapp runs alongside
// celeris's in-process rate limiter, keyed the same way and configured with
// the same RPS and burst. Every admission the limiter makes is offered to the
// shadow; an admission the shadow cannot pay for is one the token-bucket
// bound does not permit, which is what I-MW-RATELIMIT means by a token
// violation.
//
// Only sound for the IN-PROCESS limiter. A store-backed limiter
// (ratelimit/redisstore, memcachedstore) shares its budget with every other
// process against that store, so a local shadow would be re-deriving a bound
// that was never local; those refapps do not run one and do not declare
// I-MW-RATELIMIT.
type TokenShadow struct {
	rps      float64
	capacity float64

	mu      sync.Mutex
	buckets map[string]*shadowBucket
}

type shadowBucket struct {
	tokens float64
	last   time.Time
}

// NewTokenShadow returns a shadow for a limiter configured with rps and
// burst. Capacity is burst * [RateLimitShadowSlack]; see that constant for
// why the shadow is deliberately the looser of the two.
func NewTokenShadow(rps float64, burst int) *TokenShadow {
	if rps <= 0 {
		rps = 1
	}
	if burst <= 0 {
		burst = 1
	}
	return &TokenShadow{
		rps:      rps,
		capacity: float64(burst * RateLimitShadowSlack),
		buckets:  make(map[string]*shadowBucket),
	}
}

// Admit charges one token to key's bucket at now and reports whether the
// bucket could pay. False means the real limiter admitted a request its own
// bound does not allow.
func (s *TokenShadow) Admit(key string, now time.Time) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	b, ok := s.buckets[key]
	if !ok {
		if len(s.buckets) >= tokenShadowMaxKeys {
			return true // see tokenShadowMaxKeys
		}
		b = &shadowBucket{tokens: s.capacity, last: now}
		s.buckets[key] = b
	}
	if elapsed := now.Sub(b.last); elapsed > 0 {
		b.tokens = min(s.capacity, b.tokens+elapsed.Seconds()*s.rps)
		b.last = now
	}
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}
