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

// RatedScenarios is the rated/SLO subset (#156): the rows that get the rated
// sweep (four open-loop passes at 0.25-0.9 of the cell's saturation RPS).
// For probatorium#418 the maintainer chose option B (2026-09-28): the 14 rows
// the v1.5.5 audit rated, plus ws-echo.
//
// The 14 audit rows keep the v1.5.5 audit's reasons; #418 did NOT re-measure
// them, it only counted their passes (below):
//   - the driver rows: adapters pile up near a store-bound saturation ceiling,
//     so latency at load is what ranks them (the audit's claim; in v1.5.8 the
//     x86 saturation spread across adapters is still 1.2-1.75x per row);
//   - get-json and post-4k, the static headline rows;
//   - churn-close: latency under connection churn.
//
// ws-echo was added by #418, measured over the per-pass data of the published
// full runs (x86 0716 and 0829, arm64 0829). It is the only rated row that
// shows the celeris loop engines' light-load WebSocket tail (celeris#755),
// which saturation hides; ws-large-echo shows their 64 KiB floor but is not
// rated (below). The loop engines' 0.25 pass reads p99 2.5-10.4 ms on x86,
// where celeris-std holds 0.13 ms at twice the rate, and 2.2-2.8 ms on
// arm64, where celeris-std and axum hold 0.45-0.48 ms at more than twice the
// rate. No ws-echo pass reached 100 ms in any full run. It sends 7 B, not
// the 256 B the row declares (probatorium#444); the rated number is right
// for the payload actually sent.
//
// Not rated, with the reason #418 measured over the same runs:
//   - get-simple-1c: one paced connection reads loadgen's ~1 ms timer
//     quantum plus the RTT (loadgen#102), not the server.
//   - get-simple, get-simple-128c, get-json-1k: the same client and handler
//     shape as get-json. On x86 at 0.5-0.9 of saturation, get-json-1k's p99
//     matches get-json's within get-json-1k's own run-to-run spread, and the
//     two get-simple rows rank the servers as get-json does (Spearman
//     0.83-0.95). At 0.25 (both arches) and at 0.9 on arm64 they agree less,
//     so "adds too little to rate" is a judgment, not a proof. get-simple and
//     get-simple-128c are one workload (probatorium#446).
//   - get-simple-256c/512c/1024c: on x86, above ~0.5-0.6M req/s at these
//     connection counts no server's pass held 10 ms, where the same servers
//     saturate at 1.0-1.4M; the fastest servers publish loadgen's backlog,
//     and the rated ranking comes out inverted against saturation. On arm64
//     (targets below 0.4M) their latency_at_slo ranks the servers almost
//     exactly as saturation does (Spearman 0.96-1.00 at 100 ms and 1 s).
//   - get-json-h2, post-4k-h2: on x86, no H2 pass at 0.4M req/s or more held
//     10 ms, on six H2 stacks. A second loadgen process restored the target
//     in a laptop split test, but celeris-epoll and celeris-adaptive fail
//     their 0.5 pass at 18-20% SUT CPU (celeris#757), so the cause is not
//     proven to be loadgen only.
//   - ws-hub-broadcast-*, sse-fanout-*: not a latency. The server publishes
//     on its own tick at every fraction and frames carry no timestamp, so the
//     target only sets how fast loadgen reads.
//   - ws-large-echo: a real, replicated signal (the loop engines' 64 KiB
//     echo floor, celeris#756), but 5 of its x86 cells in the arch-parallel
//     0829 run published 3-12 s backlog clocks at 0.75 and 0.9
//     (probatorium#442).
//
// The concurrency sweep (get-simple-*c) is NOT a latency-vs-load curve: every
// sweep point is the server at 100% load (RPS x mean latency = connections,
// within 0.3% at the median, over the 810 sweep cells of the three runs).
//
// Candidates to re-add once the runner records each pass's achieved rate and
// refuses a pass that missed it (probatorium#441), overlapping arch runs of
// the same heavy row are prevented (probatorium#442) and loadgen paces each
// worker (loadgen#102): ws-large-echo, get-json-h2 (post-4k-h2 only if it
// shows something get-json-h2 does not) and get-simple-1024c (the only row at
// partial load with a large keep-alive pool).
//
// #441 and #442 gate any rated publish, the rated rows included: they sit at
// the same loadgen edge, less often, and #418's pass census of them is not
// clean (the counts are in #441). 17 of the 350 in-set cells x86 0829 rated
// have a pass of 1 s or more. x86 0829 driver-pg-write rated 16 of 23 cells,
// 15 of them missing passes. On arm64 the top pass of driver-redis-get, -set,
// -pipeline and driver-session-rw reads above the cell's own saturation max
// in 12-20 of 23 cells. churn-close was rated on 1 of 45 x86 cells, the rest
// suspect at saturation (loadgen#87, probatorium#424).
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
// FitWithin projects accordingly. The parallel path is safe for artefacts:
// every artefact is keyed by bench_target (bench_run_dir AND the loadgen
// transport tarball). It is not shown safe for rated passes: msa2-client is
// far from saturation while driving one arch, but both arches walk the same
// cell order, and in the 0829 run x86 rated passes that overlapped the arm64
// run of the same heavy row read 1 s or more far more often
// (probatorium#442: ws-large-echo 6 of 10 overlapping passes, 0 of 42 others).
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
