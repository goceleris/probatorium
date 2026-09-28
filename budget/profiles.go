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
// "everything". The RATED sweep stays curated (RatedScenarios, on every
// capable server) because it is the expensive additive dimension.

// RatedScenarios is the rated/SLO subset (#156): the rows whose rated sweep
// (four open-loop passes at 0.25-0.9 of the cell's saturation RPS) measures
// the server and says something no other rated row says. The scope was
// measured for probatorium#418 over the per-pass data of the published full
// runs (x86 0716 and 0829, arm64 0829), and the maintainer chose it (option
// B, 2026-09-28):
//   - the driver rows: adapters pile up at the same store-bound saturation
//     ceiling there, so latency at load is what ranks them;
//   - get-json and post-4k, the static headline rows;
//   - churn-close: latency under connection churn;
//   - ws-echo: the only row that shows the celeris loop engines' WebSocket
//     tail at light load (celeris#755: p99 2.5-10.4 ms at 25% load in
//     v1.5.8, where celeris-std holds 0.13 ms at twice the rate), which
//     saturation hides. No ws-echo pass reached 100 ms in any full run. It
//     sends 7 B, not the 256 B the row declares (probatorium#444); the rated
//     number is right for the payload actually sent.
//
// Not rated, with the measured reason:
//   - get-simple-1c: one paced connection reads loadgen's ~1 ms timer
//     quantum plus the RTT (loadgen#102), not the server.
//   - get-simple, get-simple-128c, get-json-1k: the same client and handler
//     shape as get-json, which they match within its own run-to-run noise
//     (get-simple and get-simple-128c are one workload, probatorium#446).
//   - get-simple-256c/512c/1024c: above ~0.5-0.6M req/s at these connection
//     counts no server's pass held 10 ms, where the same servers saturate at
//     1.0-1.4M. The fastest servers publish loadgen's backlog, and the rated
//     ranking comes out inverted against saturation.
//   - get-json-h2, post-4k-h2: no H2 pass at 0.4M req/s or more held 10 ms,
//     on six H2 stacks. A second loadgen process restored the target in a
//     laptop split test, but a celeris epoll H2 fault below that rate is not
//     excluded (celeris#757), so the cause is not proven to be loadgen only.
//   - ws-hub-broadcast-*, sse-fanout-*: not a latency. The server publishes
//     on its own tick at every fraction and frames carry no timestamp, so the
//     target only sets how fast loadgen reads.
//   - ws-large-echo: a real, replicated signal (the loop engines' 64 KiB
//     echo floor, celeris#756), but its high passes in the arch-parallel 0829
//     run published 3-12 s backlog clocks (probatorium#442).
//
// The concurrency sweep (get-simple-*c) is NOT a latency-vs-load curve: every
// sweep point is the server at 100% load (RPS x mean latency = connections,
// within 0.3% at the median).
//
// Candidates to re-add once the runner records each pass's achieved rate and
// refuses a pass that missed it (probatorium#441), the arches stop sharing
// one loadgen host (probatorium#442) and loadgen paces each worker
// (loadgen#102): ws-large-echo, get-json-h2 (post-4k-h2 only if it shows
// something get-json-h2 does not) and get-simple-1024c (the only row at
// partial load with a large keep-alive pool). The rated rows sit at the same
// loadgen edge, less often, so #441 and #442 gate any rated publish.
//
// Every rated scenario runs on EVERY participating, capability-gated server
// (see ratedGlobs) — not a curated column subset — so the rated table ranks
// each row against its real field leader. That makes the rated sweep large: it
// no longer fits the 24h budget, so the rated profiles (headline/full) are
// manual BENCH_BUDGET dispatches. The weekly cron runs Fast (rated OFF), which
// still fits 24h.
var RatedScenarios = []string{
	"get-json", "post-4k", "churn-close",
	"driver-pg-read", "driver-pg-write", "driver-pg-update-tx", "driver-pg-read-range",
	"driver-redis-get", "driver-redis-set", "driver-redis-pipeline",
	"driver-mc-get", "driver-mc-set", "driver-mc-multiget",
	"driver-session-rw",
	"ws-echo",
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
	HeadlineRealizedCells = FullRealizedCells
	// Rated runs the RatedScenarios on ALL participating servers
	// (capability-gated), not a curated column subset. Realized via
	// `cmd/runner -dry-run -runs 1 -cells '*/*' -rated -rated-cells '<RatedGlob>'`,
	// whose stderr counts the rated cells: 3 static rows × 45 H1 cols +
	// 11 driver rows × 23 driver-capable cols + ws-echo × 13 WS-capable
	// cols = 401. Re-pin when RatedScenarios or the registry changes.
	HeadlineRatedRealizedCells = 401

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
	FullRatedRealizedCells = 401 // same rated set as Headline (all participating servers)
)

// HeadlineWeekly is the config the benchmark-tier workflow runs on the
// weekly (non-release) cadence. It now covers the FULL grid — every
// registered server x every registered scenario, capability-gated (Globs
// "*/*") — so no framework or scenario is silently left out of the weekly
// numbers. The ONLY thing distinguishing it from Full() is a shorter
// per-cell window (60s/15s vs 90s/20s) chosen so the whole grid still fits
// the 24h budget. The bench ALWAYS runs exactly one pass (Runs=1).
//
// Arches is 1 as the static default; BenchTier sets Arches=2 at runtime for
// BENCH_TARGET=both. ArchParallel is now true (#168 landed 2026-08-28): the
// two arch passes run CONCURRENTLY against distinct SUT hosts, sharing only
// the msa2-client loadgen, so two arches cost the same wall-clock as one and
// FitWithin projects accordingly. The parallel path is safe because every
// artefact is keyed by bench_target (bench_run_dir AND the loadgen transport
// tarball) and msa2-client is far from saturation while driving one arch.
//
// Budget: ~813 cells x (12+40+5+12)s x 1 arch = ~15.6h saturation + ~14.3h
// rated (401 rated cells x 4 x (12+20)s, each rated pass re-running the
// saturation warmup) = ~29.8h bench — this NO LONGER fits 24h. (Until
// probatorium#418 the runner rated every clean cell, ~770, at 30s passes,
// and the measured run was ~53h.) The v1.5.5 audit expanded rated to
// run the meaningful scenarios (drivers + static headline + churn-close) on
// EVERY participating server (RatedGlobs) so each rated row ranks against its
// real field leader, which is the whole point of the rated table. Headline and
// Full are therefore MANUAL BENCH_BUDGET dispatches now; the weekly cron runs
// Fast (rated OFF), which still fits 24h. Per-cell window stays 40s/12s.
func HeadlineWeekly() Profile {
	return Profile{
		Name:          "headline",
		Cells:         HeadlineRealizedCells,
		Duration:      40 * time.Second,
		Warmup:        12 * time.Second,
		Cooldown:      defaultCooldown,
		Runs:          1,
		Arches:        1,
		ArchParallel:  true,
		RatedCells:    HeadlineRatedRealizedCells,
		RatedPasses:   4,
		RatedDuration: 20 * time.Second,
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
		ArchParallel: true,
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
//
// Budget: 813 cells x (20+90+5+12)s = ~28.7h saturation + 401 rated cells
// x 4 x (20+30)s = ~22.3h rated = ~51.0h on one arch (the arches run in
// parallel). Until probatorium#418 every clean cell (~770) was rated and a
// full run measured ~69h.
func Full() Profile {
	return Profile{
		Name:          "full",
		Cells:         FullRealizedCells,
		Duration:      90 * time.Second,
		Warmup:        20 * time.Second,
		Cooldown:      defaultCooldown,
		Runs:          1,
		Arches:        1,
		ArchParallel:  true,
		RatedCells:    FullRatedRealizedCells,
		RatedPasses:   4,
		RatedDuration: 30 * time.Second,
		Globs:         []string{"*/*"},
		RatedGlobs:    ratedGlobs(),
	}
}

// ratedGlobs expands the rated scenario set into "<scenario>/*" globs — every
// rated scenario on every participating server, capability-gated by the runner
// — so the rated/SLO table ranks each row against its true field leader rather
// than a curated column subset.
func ratedGlobs() []string {
	out := make([]string, 0, len(RatedScenarios))
	for _, s := range RatedScenarios {
		out = append(out, s+"/*")
	}
	return out
}
