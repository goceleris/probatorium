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

// The weekly (headline) profile no longer curates a SUBSET of servers or
// scenarios: it runs the FULL grid (every registered server x every
// registered scenario, capability-gated) via the "*/*" cells glob, the same
// coverage as the Full profile — only the per-cell window differs (a shorter
// weekly window that still fits the 24h budget). There is therefore no
// HeadlineServers / HeadlineScenarios list anymore; the SATURATION grid is
// "everything". The RATED sweep stays curated (RatedServers x RatedScenarios)
// because it is the expensive additive dimension — see RatedServers below.

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
// Derivation (headline): the weekly SATURATION grid is now the FULL grid
// (every server x every scenario, capability-gated), so its realized count
// is FullRealizedCells — the only thing that keeps weekly under 24h is the
// shorter per-cell window (see HeadlineWeekly), not a curated subset. The
// rated sweep stays curated, so HeadlineRatedRealizedCells is unchanged.
const (
	HeadlineRealizedCells      = FullRealizedCells
	HeadlineRatedRealizedCells = 24 // 8 rated servers x 3 rated scenarios, capability-gated

	// Full profile: every server x every scenario, capability-gated. After
	// the mid-size payload rows (get/post-json-8k/16k) and the native h2c
	// columns (axum/ntex/hyper/aspnet/fastapi/hono/elysia -h2) landed, the
	// nominal grid is ~36 columns x 45 rows ~ 1620; capability gating (the
	// streaming / driver / chain / TLS cells, plus the h2c-noupg columns
	// skipping every H1 row) lands the realized count near ~800. Pinned
	// conservatively high so FitWithin over-projects slightly and a registry
	// change that blows the budget fails loudly rather than overflowing the
	// run. Recompute with the scheduler's Applicable gate when the registry
	// grows again.
	FullRealizedCells      = 820
	FullRatedRealizedCells = 24
)

// HeadlineWeekly is the config the benchmark-tier workflow runs on the
// weekly (non-release) cadence. It now covers the FULL grid — every
// registered server x every registered scenario, capability-gated (Globs
// "*/*") — so no framework or scenario is silently left out of the weekly
// numbers. The ONLY thing distinguishing it from Full() is a shorter
// per-cell window (60s/15s vs 90s/20s) chosen so the whole grid still fits
// the 24h budget. The bench ALWAYS runs exactly one pass (Runs=1).
//
// Arches is 1: the bench runs amd64-only today (BENCH_TARGET=msa2-server;
// msr1/arm64 is out on a firmware bug, celeris#312), and BenchTier already
// overrides Arches to 1 at runtime for any non-"both" target — pinning 1
// here makes the static FitWithin projection match what actually runs
// instead of over-projecting a non-existent arm64 pass. If arm64 returns
// (BENCH_TARGET=both), BenchTier sets Arches=2 and FitWithin then aborts the
// full grid against the default 24h budget unless BENCH_BUDGET is raised —
// the correct loud failure, since the full grid x 2 serial arches cannot fit
// 24h until ArchParallel (#168, blocked on loadgen linux/arm64) lands.
//
// Budget: ~820 cells x (15+60+5+12)s x 1 arch = ~20.9h saturation + ~0.7h
// curated rated = ~21.6h < 24h. The rated sweep stays curated (RatedGlobs)
// because it is the expensive additive dimension; expanding it to the full
// grid would blow the budget many times over.
func HeadlineWeekly() Profile {
	return Profile{
		Name:          "headline",
		Cells:         HeadlineRealizedCells,
		Duration:      60 * time.Second,
		Warmup:        15 * time.Second,
		Cooldown:      defaultCooldown,
		Runs:          1,
		Arches:        1,
		ArchParallel:  false,
		RatedCells:    HeadlineRatedRealizedCells,
		RatedPasses:   4,
		RatedDuration: 20 * time.Second,
		RatedWarmup:   10 * time.Second,
		Globs:         []string{"*/*"},
		RatedGlobs:    ratedGlobs(),
	}
}

// Full is the exhaustive sweep: every server x every scenario at a
// slightly longer 90s/20s window, single pass and the rated subset. Far
// over the 24h weekly budget with the long window — Full is a manual
// dispatch that raises BENCH_BUDGET above 24h; FitWithin asserts the
// single-pass config fits the (raised) budget and fails loudly otherwise.
func Full() Profile {
	return Profile{
		Name:          "full",
		Cells:         FullRealizedCells,
		Duration:      90 * time.Second,
		Warmup:        20 * time.Second,
		Cooldown:      defaultCooldown,
		Runs:          1,
		Arches:        1,
		ArchParallel:  false,
		RatedCells:    FullRatedRealizedCells,
		RatedPasses:   4,
		RatedDuration: 30 * time.Second,
		RatedWarmup:   15 * time.Second,
		Globs:         []string{"*/*"},
		RatedGlobs:    ratedGlobs(),
	}
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
