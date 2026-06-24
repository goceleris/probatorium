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
// scenarios where throughput-at-SLO carries the most signal. The two
// registered static rows every rated server runs — a GET read and a 4 KiB
// POST. (A third entry, "auto-mix-111", used to be listed here but the
// scenario was deleted and never registered, so the -cells filter silently
// matched nothing and the rated grid was 16 cells while the pin claimed 24;
// removed from the rated pass in the v1.5.4 pre-run audit.)
var RatedScenarios = []string{
	"get-json",
	"post-4k",
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
	HeadlineRatedRealizedCells = 16 // 8 rated servers x 2 rated scenarios, capability-gated

	// Full profile: every server x every scenario, capability-gated. This is
	// the SAME realized "*/*" grid Fast runs (FullRealizedCells ==
	// FastRealizedCells); the profiles differ only by per-cell window. The
	// v1.5.4 redesign reshaped the grid — saturated static rows pruned (W1),
	// the driver set deepened 4->10 (W3), WS/SSE coverage added to three more
	// columns (W4), the 12 middleware/chain scenarios REMOVED (pre-run audit:
	// unequal work across adapters), and the 1 MiB post-1m row REMOVED
	// (wire-bound, never a ranking signal) — so the realized count moved off
	// the older ~800/1257/1111/835/790 pins to 813 (v1.5.5 added driver-mc-set,
	// +23 — one per H1 driver column). Recompute with
	// `cmd/runner -dry-run -cells '*/*' | grep -c '^run0'` when the registry
	// changes; the grid is now 52 columns x 29 rows, capability-gated.
	FullRealizedCells      = 813
	FullRatedRealizedCells = 16
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
// Budget: ~813 cells x (12+40+5+12)s x 1 arch = ~15.6h saturation + ~0.7h
// curated rated = ~16.3h < 24h. The per-cell window stays at the v1.5.4
// 40s/12s (chain + post-1m removals then the v1.5.5 driver-mc-set add left the grid at 813, so there
// is now ample headroom). The rated sweep stays curated (RatedGlobs) because
// it is the expensive additive dimension; expanding it to the full grid
// would blow the budget many times over.
func HeadlineWeekly() Profile {
	return Profile{
		Name:          "headline",
		Cells:         HeadlineRealizedCells,
		Duration:      40 * time.Second,
		Warmup:        12 * time.Second,
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

// FastRealizedCells is the live capability-gated saturation cell count of
// the full "*/*" grid (every server × every scenario the scheduler keeps).
// Recompute with `cmd/runner -dry-run -cells '*/*' | grep -c '^run0'` when
// the registry grows; FitWithin uses it to assert the fast profile still
// fits 24h, so an over-large grid fails loudly instead of overrunning.
// v1.5.4 redesign: 1257 -> 1111 -> 835 -> 790; v1.5.5: -> 813 (W1 pruned
// saturated static rows; W3 deepened drivers 4->10; W4 added WS/SSE to three
// columns; pre-run audit REMOVED the 12 middleware/chain scenarios; post-1m
// removed as wire-bound; v1.5.5 added driver-mc-set, +23).
const FastRealizedCells = 813

// Fast is the DEFAULT routine + weekly profile: the FULL grid (every server
// × every scenario, capability-gated, "*/*") in SATURATION ONLY — no rated
// sweep — at a 35s/10s window so the whole grid fits comfortably under 24h
// on one arch. Saturation gives the headline ceiling (max RPS + tail latency
// at saturation) for every cell; the rated/SLO sweep (4 closed-loop passes
// per cell, the dominant cost) is intentionally OFF here and belongs in a
// separate, scoped dispatch when latency-under-controlled-load is the story.
//
// Budget: 813 cells × (10+35+5+12)s × 1 arch = ~14.0h saturation, rated=0
// → ~14.0h < 24h. RatedPasses=0 makes BenchTier skip the rated flag entirely
// (rated OFF for every cell), so this is the cheap, full-breadth mode.
func Fast() Profile {
	return Profile{
		Name:         "fast",
		Cells:        FastRealizedCells,
		Duration:     35 * time.Second,
		Warmup:       10 * time.Second,
		Cooldown:     defaultCooldown,
		Runs:         1,
		Arches:       1,
		ArchParallel: false,
		RatedCells:   0, // rated OFF — saturation-only
		RatedPasses:  0,
		Globs:        []string{"*/*"},
		RatedGlobs:   nil,
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
