package budget

import "time"

// Curated matrix definitions (#166). These are DATA: the concrete server
// and scenario sets each profile expands into, plus the realized
// (capability-gated) cell counts pinned as constants the budget test
// asserts. A mage-tagged helper (mage_tier.go) recomputes the realized
// counts from the live scenarios + servers registries through the same
// filter the runner uses; these constants are the pinned expectation that
// helper's output must match, so a registry change that blows the budget
// surfaces as a failing test rather than a silently-overflowing run.

// HeadlineServers is the curated weekly column set (~15): the four celeris
// engine modes worth comparing, the headline Go competitors, and one
// representative per non-Go language. Drops the -h2 duplicate columns,
// the chi/iris mid-pack routers, and the long-tail competitors
// (drogon / zig_zap / ntex / fastapi / hono / elysia / gorilla_ws) the
// full profile keeps.
var HeadlineServers = []string{
	"celeris-iouring-h1-async",
	"celeris-iouring-auto+upg-async",
	"celeris-epoll-h1-sync",
	"celeris-std-h1",
	"stdhttp-h1",
	"gin-h1",
	"echo-h1",
	"fiber-h1",
	"fasthttp-h1",
	"gnet-h1",
	"hertz-h1",
	"axum",
	"actix-web",
	"aspnet",
	"hyper",
}

// HeadlineScenarios is the curated weekly row set (~12): the static /
// payload-size / concurrency / mix / chain / streaming scenarios that
// carry the most signal. Capability gating means the streaming + chain
// cells only land on servers that declare those capabilities, so the
// realized count is lower than len(servers) x len(scenarios).
var HeadlineScenarios = []string{
	"get-simple",
	"get-json",
	"get-json-1k",
	"get-json-64k",
	"post-4k",
	"post-64k",
	"get-simple-128c",
	"get-simple-1024c",
	"auto-mix-111",
	"chain-fullstack-get-json",
	"ws-echo",
	"sse-fanout-128",
}

// RatedScenarios is the curated rated/SLO subset (#156): the SLO-knee
// scenarios where throughput-at-SLO carries the most signal.
var RatedScenarios = []string{
	"get-json",
	"post-4k",
	"auto-mix-111",
}

// RatedServers is the curated rated column subset: the four celeris modes
// plus the four strongest competitors. Rated runs ONLY on this subset so
// the expensive sweep is additive, not a whole-grid multiplier.
var RatedServers = []string{
	"celeris-iouring-h1-async",
	"celeris-iouring-auto+upg-async",
	"celeris-epoll-h1-sync",
	"celeris-std-h1",
	"gin-h1",
	"fasthttp-h1",
	"axum",
	"aspnet",
}

// Realized (capability-gated) cell counts for the headline weekly
// profile. Pinned here as the source of truth the budget test asserts and
// the mage-tagged realized-count helper validates against the live
// registries.
//
// Derivation (headline): 15 servers x 12 scenarios = 180 nominal cells.
// Capability gating drops the streaming cells (ws-echo, sse-fanout-128)
// and the chain cell on servers that don't advertise WebSocket / SSE /
// chain support, plus a handful of payload-size cells inapplicable to a
// given adapter — landing the realized grid near ~150. The constant is
// the conservative pinned figure the workflow runs against; the helper
// fails the build if the live count exceeds it (which would invalidate
// the budget assertion).
const (
	HeadlineRealizedCells      = 150
	HeadlineRatedRealizedCells = 24 // 8 rated servers x 3 rated scenarios, capability-gated

	// Full profile: every server x every scenario, capability-gated. The
	// nominal grid is ~25 columns x 33 rows ~ 825; gating lands it near
	// ~520. Pinned conservatively high so the budget test catches an
	// overflow even after registry growth.
	FullRealizedCells      = 520
	FullRatedRealizedCells = 24
)

// HeadlineWeekly is the exact config the benchmark-tier workflow runs on
// the weekly (non-release) schedule: the curated ~15x12 grid at
// 60s/15s/3, plus the curated rated subset. N=1 weekly; the workflow
// bumps Runs to the back-to-back release count via FitWithin when a new
// release is detected (#167), and the NewReleaseConfig test pins that
// N=3 still fits.
//
// ArchParallel is false: arm64 loadgen federation (#168) is blocked on
// the loadgen repo shipping linux/arm64, so both arches run serially
// today. The aggressive curation is precisely what keeps the serial
// run under 24h until that win lands.
func HeadlineWeekly() Profile {
	return Profile{
		Name:  "headline",
		Cells: HeadlineRealizedCells,
		// Per-cell window is 40s/10s (not the nominal 60s/15s) because the
		// two arches run SERIALLY today — arm64 loadgen federation (#168,
		// ArchParallel) is blocked on the loadgen repo. At runs=3 x 150
		// cells x 2 serial arches, a 60s window plus the rated pass
		// overflows 24h; 40s lands the whole run at ~21h with headroom.
		// When #168 lands and ArchParallel flips on, this can grow back.
		Duration:      40 * time.Second,
		Warmup:        10 * time.Second,
		Cooldown:      defaultCooldown,
		Runs:          3,
		Arches:        2,
		ArchParallel:  false,
		RatedCells:    HeadlineRatedRealizedCells,
		RatedPasses:   4,
		RatedDuration: 20 * time.Second,
		RatedWarmup:   10 * time.Second,
		Globs:         headlineGlobs(),
		RatedGlobs:    ratedGlobs(),
	}
}

// Full is the exhaustive sweep: every server x every scenario at a
// slightly longer 90s/20s window, same back-to-back Runs and rated
// subset. Far over the 24h budget with the long window — Full is intended
// for ArchParallel + a trimmed Runs, and FitWithin will report it does
// not fit at Runs=3 so the caller fails loudly rather than overruns.
func Full() Profile {
	return Profile{
		Name:          "full",
		Cells:         FullRealizedCells,
		Duration:      90 * time.Second,
		Warmup:        20 * time.Second,
		Cooldown:      defaultCooldown,
		Runs:          3,
		Arches:        2,
		ArchParallel:  false,
		RatedCells:    FullRatedRealizedCells,
		RatedPasses:   4,
		RatedDuration: 30 * time.Second,
		RatedWarmup:   15 * time.Second,
		Globs:         []string{"*/*"},
		RatedGlobs:    ratedGlobs(),
	}
}

// headlineGlobs expands the curated headline scenario x server lists into
// the "<scenario>/<server>" glob set the runner's -cells filter consumes.
// The runner skips capability-inapplicable pairs, so emitting the full
// cartesian product here is correct — the realized count is the gated
// subset, not len(Globs).
func headlineGlobs() []string {
	return crossGlobs(HeadlineScenarios, HeadlineServers)
}

// ratedGlobs expands the curated rated scenario x server subset into its
// "<scenario>/<server>" glob set.
func ratedGlobs() []string {
	return crossGlobs(RatedScenarios, RatedServers)
}

// crossGlobs builds the "<scenario>/<server>" glob for every pair.
func crossGlobs(scenarios, servers []string) []string {
	out := make([]string, 0, len(scenarios)*len(servers))
	for _, s := range scenarios {
		for _, srv := range servers {
			out = append(out, s+"/"+srv)
		}
	}
	return out
}
