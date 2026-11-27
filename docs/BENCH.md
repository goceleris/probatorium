# Bench tier reference

Reference for the bench tier of [probatorium](../README.md): what the grid contains, how a run is budgeted and
scheduled, how the two architectures share one load generator, how a rated result is recorded, the host
sampler, the msr1 interrupt placement and the same-run celeris baseline. The front page is the
[README](../README.md); this page is the detail. Every statement here moved unchanged from the README unless
the Validation paragraph of "A both-arch run" says otherwise.

Contents: [The field](#the-field) · [Capability gating](#capability-gating) · [Profiles](#profiles) ·
[A both-arch run](#a-both-arch-run-the-two-arches-share-one-client-442) ·
[The host sampler beside a bench](#the-host-sampler-beside-a-bench) · [Result: latency at SLO](#result-latency-at-slo) ·
[msr1 NIC interrupt placement](#msr1-nic-interrupt-placement) · [Same-run celeris baseline](#same-run-celeris-baseline)

## The field

The grid is capability-gated: 52 server columns from `servers.Registry` (the single source of truth,
in [`servers/servers.go`](../servers/servers.go)) × 32 registered scenarios, of which 29 schedule cells.
The three `tls-*` scenarios need a TLS terminator that no run configures today.

- **celeris** contributes **9 columns** that pick out an engine, a wire mode and a handler dispatch
  mode: `iouring` (`h1-sync`, `h1-async`, `auto+upg-async`), `epoll` (`h1-sync`, `h1-async`,
  `auto+upg-async`), `adaptive` (`h1-async`, `auto+upg-async`) and `std-h1`. This is how the grid
  separates, for example, an io_uring regression from a wire-mode one.
- **`stdhttp`** is the Go `net/http` baseline (`h1`, `h2`, `hybrid`).
- **29 competitor frameworks across 10 languages** make up the rest:

| Language | Frameworks |
| --- | --- |
| Go (10) | chi, echo, fasthttp, fiber, gin, gnet, gorilla, hertz, iris, nbio |
| Rust (4) | actix-web, axum, hyper, ntex |
| Bun (3) | bunraw, elysia, hono |
| Node / JS (3) | express, fastify, uWebSockets.js |
| C++ (2) | drogon, lithium |
| Java (2) | netty, vertx |
| Python (2) | fastapi, starlette |
| C (1) | h2o |
| C# (1) | aspnet |
| Zig (1) | httpzig |

Count from the registry, not from adapter directories: a directory can exist without being registered.
Adding a framework takes a registry entry and an adapter directory, plus a `nativeBuildSpecs` entry in
[`mage_cluster.go`](../mage_cluster.go) for a non-Go adapter.

## Capability gating

Not every scenario runs against every server. Each adapter declares its `Capabilities`, which the runner
projects into a `servers.FeatureSet`, and each scenario implements `Applicable(servers.FeatureSet) bool`.
The scheduler skips the mismatches **before** dialing loadgen, so a framework with no native Postgres
path never shows up as a spurious zero in the driver table. That is why a full run has 813 cells rather
than the raw 52 × 32 product.

## Profiles

`mage BenchTier` runs a **profile** from `budget.ForProfile` ([`budget/`](../budget/)). Every profile
covers the same full grid in exactly one pass; they differ in the per-cell window and in whether the
rated sweep runs.

| Profile | Per-cell window | Rated sweep | Published by the Benchmark Tier workflow |
| --- | --- | --- | --- |
| **fast** (the `ForProfile` default) | 35 s active / 10 s warmup | off: saturation only | never (the workflow forces `BENCH_PUBLISH=0`) |
| **headline** | 40 s / 12 s | on | yes, unless dispatched with `publish=false` |
| **full** | 90 s / 20 s | on | yes, unless dispatched with `publish=false` |

The workflow publishes only rated profiles, because saturation-only data would publish datasets with no
`latency_at_slo`. `mage BenchTier` run by hand publishes unless `BENCH_PUBLISH=0`. The workflow sets
`BENCH_BUDGET` from a table of profile, baseline switch and arch policy (single arch, no baseline: 24 h, 36 h
and 60 h; the policies of a both-arch run and the baseline columns have their own entries, see below). The
budget model projects about 30 h for headline and 51 h for full on one arch. A preset `cells` or `rated_cells`
scope is projected from the runner's own dry run (the realized cell and rated cell counts, baseline columns
included), not from those pins: `rated_cells='*'` rates every cell, about twice the model's 401, and a full
both-arch dispatch with it no longer passes the gate at 101.9 h. A scope the dry run cannot resolve is refused. Runs before [#418](https://github.com/goceleris/probatorium/issues/418) measured about 53 h
and 69 h, because they rated every clean cell.

**Rated data is not publishable yet.** [#441](https://github.com/goceleris/probatorium/issues/441) gates
any rated publish, and [#442](https://github.com/goceleris/probatorium/issues/442) does until it is settled.
The validation run of 2026-10-21 showed the arch lock holds on the scoped rows (P1b, P2, P3, P5) and that the
exemption of light cells did not (P6 fails on arm64), so the default policy is now `all`
([`docs/ARCH-EXCLUSION-VALIDATION.md`](ARCH-EXCLUSION-VALIDATION.md), "Verdict"). #442 stays open for the
three rows only a full both-arch run reads (get-simple-1024c, get-json-h2, post-4k-h2), so dispatch headline and
full with `publish=false` until both are settled. Since schema 5.24 the artifact keeps the achieved rate, the
errors and a status for every rated pass ([#441](https://github.com/goceleris/probatorium/issues/441),
see "Result: latency at SLO"), so a rated number can be read against the load loadgen actually sent. #441
stays open for the rest: no pass is yet marked invalid for a shortfall, `latency_at_slo` still counts every
kept pass whatever it achieved, and loadgen does not export its end-of-window schedule lag (loadgen#102).
Until those land, read `achieved_ratio` before you read a rated p99 as the server's.

## A both-arch run: the two arches share one client (#442)

`BENCH_TARGET=both` (the default) runs the x86 pass and the arm64 pass at the same time, and both are
driven from the one loadgen host, msa2-client (#168 asked for a loadgen per arch; as landed they share
it). One arch alone leaves msa2-client far from saturation, but two arches loading it together did not
leave the heavy rows alone: in the 0829 run, x86 ws-large-echo passes that overlapped the arm64 run of
the same row read a backlog clock of 1 s or more in 6 of 10 passes, against 0 of 42 for the rest, and in
run 36441790519 the saturation numbers of the bandwidth rows moved with the other arch's load (the 2x10G
bond carries both arches). A run made before the fix marks nothing of this.

**What the harness does now.** The default policy is `all` (since the validation read of 2026-10-21): the
arches take turns on the client and no pass of one arch overlaps any load of the other. The bullets below
describe the lock; under `all` every cell takes it, under `heavy` (the previous default, still selectable)
only the heavy ones.

- Every scenario has a client-load class, `heavy` or `light` ([`scenarios/clientload.go`](../scenarios/clientload.go),
  the table, its demand figures and the rule). Light is a row whose p90 stays at or below 150k requests/s
  and 4 Gbit/s on both arches in the published 0829 run: the 11 driver rows, churn-close, get-simple-1c,
  ws-hub-broadcast-128 and sse-fanout-128 (369 of the 813 cells). Everything else, and any scenario the
  table does not name, is heavy (a test fails until a new scenario is classified).
- The runner holds a host-wide `flock` on msa2-client (`/tmp/probatorium-client.lock`) around every heavy
  cell, saturation pass, rated sweep and warmups included ([`cmd/runner/clientlock.go`](../cmd/runner/clientlock.go)).
  A heavy cell of one arch therefore waits for the other arch's heavy cell. The wait polls once a second,
  ends with the run context, and the kernel drops the lock when a runner dies, so a runner killed by its hang
  guard frees the other. Neither arch can stall the other for more than a cell: the holder releases the lock
  itself after the longest a cell can run when its own contexts work (its saturation pass plus every rated pass
  at their timeouts, plus a minute: a wedged cell, loadgen workers blocked in reads, has driven nothing since
  its context fired), and a waiter gives up after that plus two minutes and fails the cell as
  `client-lock: ...` (a DNF with its wait recorded, not an "interrupted" cell). The checker still reads the
  passes' own timestamps, so a cell that did overlap its peer is reported. `started_at` is stamped after a
  wait, so the idle wait never lands in the per-scenario CPU window (`scenario_resources`); the wait is
  recorded as `client_lock_wait_s` and is exactly 0 for a lock that was free at the first attempt. After a wait
  of 5 s or more the runner probes the SUT again before it drives load. The column's hang guard grows by
  `lockWaitAllowance` so a queued but healthy runner is not killed. The lock file is opened without following
  symlinks, its times are refreshed at every acquire (a host that age-cleans `/tmp` must not unlink it
  mid-run), and a lock taken on an inode the path no longer names is dropped and retaken.
  Two things the wait does to a measurement. The SUT idles through it (minutes, 6.7 h in total per arch in the
  model) where the only gap between cells used to be the 5 s cooldown; whether the first cell after a long
  wait reads differently (CPU frequency, an adaptive engine's settled state) is not measured, and the
  validation run has an item for it. And the column-wide `resources` mean (every scenario of the column,
  not the window) now includes that idle time, diluting it by an amount that differs per column: read
  `scenario_resources` for per-cell CPU. The lock is released when a cell's load ends, before the 5 s
  cooldown, so the peer's heavy cell can start while the previous cell's sockets drain on the client; they
  are connections to the other arch's SUT, so they share no 4-tuple with the next cell's, and the planner
  models the release at the same point.
- The arm64 pass walks each column's cells **in reverse** (`-cell-order reverse`). The registry order runs
  churn-close and the driver rows (light) first and the static, h2 and WebSocket/SSE rows (mostly heavy)
  after them; reversed on one arch, the two arches' heavy cells fall in different halves of a column and
  the lock rarely makes one wait (it matters: 57.6 h against 65.9 h in the model below). The order is recorded
  (`environment.arch_exclusion.cell_order`). It is a comparability change for arm64 only: its heavy rows now
  run first on a freshly started SUT and its light rows last, where every earlier arm64 run did the opposite.
  x86, the arch #442 contaminated and the one the single-arch references are for, keeps the registry order.
  `all` reverses arm64 too: its wall-clock does not depend on the order (99.7 h either way in the model), and
  it keeps every cell's position in its column the same under both policies, so a comparison of the two
  (the validation's run A against run B) differs in what the arches shared, not in where a cell sat.
- `BENCH_ARCH_EXCLUSIVE=all|heavy|off` (workflow input `arch_exclusive`) picks the policy. `all` is the
  default for `target=both`: it locks every cell, so the arches take turns (about twice one arch's
  wall-clock). `heavy` locks the heavy cells only (+14%, the previous default). `off` is the pre-fix
  behaviour, kept as a control arm: the publish gate refuses its documents. A single-arch target has no
  peer and ignores the knob.

**What it costs.** `interleave.SimulateArchPlan` replays the two passes on the real 813-cell, 52-column
grid at the `full` profile's window (`go test ./cmd/runner -run 'FullGridArchPlan|DriftSweep' -v`; the output
of this change is in the PR). One arch alone is 50.9 h in the model (bench-full 38431303162 measured 47.6 h
with both arches, so the model runs about 7% high). The first table starts both arches together with equal
column set-up and walks the same order, which is the lockstep 36441790519 showed (the same cell +61 s apart);
38431303162 was not in lockstep (median -247 s, same-row overlap fraction 0.06, #518), so "444 of 444" is the
lockstep case, not every past run.

| both-arch schedule (lockstep model) | model | vs one arch | heavy cells overlapping a heavy cell of the other arch |
| --- | --- | --- | --- |
| today: no lock | 50.9 h | +0% | 444 of 444 |
| lock on heavy cells, both arches forward | 65.9 h | +30% | 0 |
| lock on heavy cells, arm64 cells reversed (`heavy`, the previous default) | 57.6 h | +13% | 0 |
| lock on heavy cells, arm64 columns reversed | 58.6 h | +15% | 0 |
| **lock on every cell (`all`, the default)** | **99.7 h** | **+96%** | 0 |
| option, not implemented: heavy cells and every rated sweep, arm64 cells reversed | 75.2 h | +48% | 0 |

The lock's cost depends on how far apart the arches run. The drift sweep moves the arm64 start by 0 to 3 h
and the column set-up time of each arch by 0-120 s (20 points):

| makespan / one arch alone | min | mean | max |
| --- | --- | --- | --- |
| arm64 reversed, arches within 10 min (both full runs) | 1.12 | 1.14 | 1.17 |
| arm64 reversed, arm64 1-3 h later | 1.16 | 1.22 | 1.25 |
| both forward, arches within 10 min | 1.15 | 1.24 | 1.30 |
| both forward, arm64 1-3 h later | 1.11 | 1.14 | 1.21 |

So the reversal pays while the arches stay together, as in both full runs, and costs a few points if one
falls hours behind the other. A `full` target=both run goes from the measured 47.6 h to about 54 h (the
model's 1.14 mean times 47.6 h), 53-60 h across the sweep; `budget.FitWithin` projects 58 h
(`ArchLockOverheadHeavy` = 14%, pinned to the lockstep planner by a test) and the workflow budgets
(40 h headline, 69 h full) cover the sweep's 1.30 worst case. `headline` goes from about 30 h to about 34 h.
`all`, the default, is about twice, and the workflow budgets it separately (the budget is a table of profile,
baseline switch and policy and the job timeout a table of policy, `workflow_arch_budget_test.go`): 34 h fast,
72 h headline, 108 h full under a 7080 min (118 h) job timeout, which sits under the 5-day (7200 min) limit. A
default `full` dispatch therefore projects 101.9 h, so its budget has a 6% margin where every other entry has
18% or more; the model ran 7% high against the one measured run, so about 95 h is the expectation, but it is
the thinnest margin of any dispatch, and the weekly headline run, when its schedule is restored, projects 59.7 h
under the default instead of 34 h. Projected hours of `profile=full` with the opt-in baseline columns
(`baseline=true`, +222 saturation and +120 rated cells per arch), from the budget model and from the planner
(`go test ./budget ./cmd/runner -run 'FullBaselineUnderAll|BaselineGridArchPlan' -v`):

| `profile=full`, `baseline=true` | model | planner | job limit 120 h |
| --- | --- | --- | --- |
| one arch (`target=msa2-server` or `msr1`) | 65.5 h | 65.4 h | fits |
| both arches, `heavy` | 74.6 h | 72.1 h | fits (budget 90 h) |
| both arches, `all` (the default) | **130.9 h** | **128.0 h** | **does not fit** |

`full` + `baseline=true` + `target=both` under `all` cannot be held by any job: the workflow refuses it at the
start of the `setup` job, before any runner is provisioned and before Deploy (the run has already entered the
workflow's cluster concurrency group, so it may have queued behind a running one; the step is guarded by exactly
that dispatch and tested against the model). Dispatch it as two single-arch runs, `target=msa2-server` and then
`target=msr1`. `arch_exclusive=heavy` is not an alternative for a published run: its documents are refused at
publish (policy `heavy` failed P6), and the `setup` job refuses a `heavy` dispatch that would publish; `heavy` stays
available with `publish=false`. These are a model of fixed window lengths and the lock's first-come order, not a
cluster run: how often the lock really queues a runner is the first thing the validation run measures
(`lock_wait_seconds`).

**What `heavy` does not do** (the reason the default moved). Under `heavy` a heavy cell still shares the client with the other arch's light
cells, and a light cell's own measurement runs under the other arch's heavy cells. Neither is measured.

- *Heavy under light, x86 side.* The coefficient that chose the rule (an x86 cell lost 0.18-0.31 requests/s per
  concurrent arm64 request, partial r -0.92 to -0.97, on static rows of 128 and 256 connections over arm64
  demand of 174-321k requests/s; from run 36441790519, `evidence/bench-36441790519/rank/skeptic/
  out_s18_cpu_contention2.txt`) was never measured on the light rows. At the arm64 light rows' demand
  (p90 over columns, `out/demand_by_row.out` of the lane evidence): ws-hub-broadcast-128 122.5k frames/s and
  sse-fanout-128 125.7k events/s cost an x86 heavy cell 22-39k requests/s, 3.5-6.0% of a 650k requests/s cell
  (2.3-3.9% of a 1M one); every other light row stays at or below 30k, under 1.5%.
- *Heavy under light, arm64 side.* The x86 light rows reach 120-140k (churn-close 140k, driver-mc-set 130k,
  ws-hub-broadcast-128 127k). The coefficient was measured only as x86 loss per arm64 request. If it were
  symmetric an arm64 heavy cell of about 400k requests/s would lose 25-43k, 6-11%. This direction is
  unquantified.
- *Light under heavy.* 12 of the 15 `RatedScenarios` rows are light (churn-close and the 11 driver rows,
  ranked by latency at load). In the default order, 173 of each arch's rated light cells run their rated sweep
  while a heavy cell of the other arch does (346 cells in the model, 15.7 h in total;
  `go test ./cmd/runner -run FullGridArchPlan -v`). That is #442's own mechanism (rated passes under the other
  arch's heavy load) on most of the rated rows. It is exposure, not measured contamination.

`BENCH_ARCH_EXCLUSIVE=all` removes all three at about twice the wall-clock (99.7 h in the model, against
57.6 h). A third option, not implemented, locks every rated sweep as well as every heavy cell: the planner
prices it at 75.2 h (+48%) and it removes the third exposure only. The default order itself is a trade: with
both arches forward there are 230 heavy+light cell overlaps at 65.9 h; reversing arm64 makes it 793 at 57.6 h.
`all` removes that trade too. The validation run measured them
([`docs/ARCH-EXCLUSION-VALIDATION.md`](ARCH-EXCLUSION-VALIDATION.md), "Verdict"): P5 held (light rows'
rated p99, A against B) and P6 failed on arm64 (a heavy cell under the x86 light rows, celeris-iouring post-4k,
minimum rps ratio 0.827 against a 0.92 floor, median 1.001), so by the pre-registered rule the default is `all`.

**The check on a finished run.** Every saturation and every rated pass records the interval it drove load
in, on msa2-client's own clock (the same clock for both arches), kept or skipped
(`cell.pass_windows` in `raw/<host>.json`). `Bench` folds them into `load_windows` of each
`results-<arch>.json` and, since each arch is published alone, stamps the cross-arch verdict into both:
`environment.arch_exclusion` (policy, peer arch, cell order, windows checked, `heavy_heavy_overlaps`,
`heavy_light_overlaps`, `light_light_overlaps`, `violations`, `missing_timestamps`, `lock_wait_seconds`;
schema 5.22). `report.CheckArchOverlap` is the check, with a fixture self-test (a clean pair, a pair with
one overlapping heavy pass, the policies, windows with no usable timestamps). It runs three times:

- in `Bench`, which prints the report after the merge;
- in the publish gate (`checkDataCompleteness`): a document whose verdict shows a violation, a window that
  could not be checked, or policy `off` is refused (`BENCH_PUBLISH_FORCE=1` overrides, as for the other
  completeness checks);
- on demand, `mage BenchArchOverlap` (`BENCH_RESULTS_DIR`, default the latest run; `BENCH_ARCH_EXCLUSIVE`
  to check against another policy), which reads the two `results-<arch>.json` and fails on a forbidden
  overlap, a window without timestamps, or documents with no `load_windows` at all.

`load_windows` stays in `results-<arch>.json` (the run artifact); the published `summary.json` drops it,
and `env.json` keeps the verdict.

**Results from before the fix.** No earlier field says a document came from a both-arch run, so the mark is
a rule, not a flag. A `results-<arch>.json` or published cell with **no `environment.arch_exclusion` and
`schema_version` below 5.22** that came from a `target=both` run ran the arches concurrently with nothing
excluded. Such a run is recognisable by its two arch documents carrying the same `benchmark_config.started_at`
(Bench stamps one start for both; checked on the results of runs 33256654220 and 37091941051, where
`started_at` is identical to the nanosecond and `finished_at` differs by under 3 s) and by the workflow's
dispatch input `target`. The runs this applies to
include 33256654220 (0829), 36441790519, 37091941051 (schema 5.18, identical `started_at`, checked) and
38431303162 (bench-full). A post-fix both-arch document always
carries `arch_exclusion`, whatever the policy, so absence can no longer be read two ways. The docs site
does not flag it yet; that is for goceleris/docs.

**Validation.** The two scoped both-arch runs that check it (then the default policy `heavy`, and `all`, about 3.6 h of bench together, criteria pre-registered, including the three
exposures above) have run: the verdict, read on 2026-10-21, and what is still owed are in [`ARCH-EXCLUSION-VALIDATION.md`](ARCH-EXCLUSION-VALIDATION.md).

## The host sampler beside a bench

`Benchmark Tier` brackets `mage BenchTier` with `mage HostSamplerStart` / `HostSamplerStop`, as the race and weekend
tiers do (the nightly tier runs `mage Validate`, not the bench, and has the race tier's path). Before this a bench
ran no host sampler at all, so it had no series of its own: the forensics artifact fetched whatever the last weekend soak had left on the hosts, and
a 60 s cadence could not see the 1-3 s stalls of the io512 cell (celeris#1036). Two read-only processes run on
each host of the run (`BENCH_TARGET` limits the playbook to the target and the loadgen, as `Deploy` does):

- the 60 s shell sampler (`ops/host-sampler/celeris-host-sampler`, the script unchanged; this tier starts it for the
  first time): memory, load, the OOM and link-flap counters, the boot id, the kernel log;
- the **fine series** (`ops/host-sampler/celeris-host-sampler-fine`, python3, no subprocess), at the `sampler_interval`
  dispatch input: `1` s (the default, also on a schedule), `0.5`, `0.2`, `0.1` or `0.05` s (20 Hz). One text line per
  tick: the per-CPU deltas of user+system, irq, softirq and steal, the NET_RX softirq runs and the `ksoftirqd`
  run time of every CPU that did anything, the CPU pressure total, the run queue and the load; plus the two
  whole-run files below. `fine.stats` has the achieved rate, the interval percentiles and the sampler's own CPU.

**Not in the measurement path.** `BenchTier`, `bench.yml` and `run_bench_cell.yml` are not touched, no BenchTier
variable is added, and the SUT is not told. Placement: both processes run under `SCHED_IDLE` (the fine one sets it
itself (after `nice 10`), falling back to `SCHED_BATCH` at nice 19 if refused, and says which took in `fine-run.txt`;
the 60 s shell sampler is launched `nice -n 19 chrt -i 0` by the bench tier only, and its children inherit it; the
race and weekend starts are unchanged). A `SCHED_IDLE` task never preempts a normal one on wake-up and its CPU counts
as idle to the load balancer. It is not pinned to a CPU set: on msr1 every CPU hosts a loop. The price is visible, not
silent: a starved sampler shows as a gap in its own series (`gap` events, interval p99 and max in `fine.stats`). On a
container with 8 spinning loops on 2 CPUs of quota it got 0.94 Hz of a 1 Hz target (max interval 3.6 s) and 2.6 Hz
of 20: if a scoped io512 run shows many `gap` lines inside the cell's window, switch the class to `SCHED_BATCH` + nice 19
(one line in `lower_priority`) and judge the cell, not the whole run.

**Load.** Reads four `/proc` files and each `ksoftirqd` thread's `schedstat` per tick (4 + nCPU `pread`s, a 64 KiB
buffer reused, no fork). The 60 s sampler finds its three pids by reading `/proc/*/comm`, not with `pgrep` (which opens every process's
`cmdline`, taking its `mmap_lock`: under `SCHED_IDLE` it can be preempted holding it and stall the SUT for hundreds of
ms); a test forbids `pgrep`, `ps`, `cmdline`, `maps` and the like in both samplers. Measured by the review with `ksoftirqd` visible, on 8 CPUs, contended: **0.051 % of one CPU at
1 s and 0.711 % at 20 Hz**; expect about 0.1-0.2 % and 1.5-2 % on a 32-CPU host. (The first figures, 0.03 % and
0.35 %, came from a container that could not see `ksoftirqd`.) `self_cpu_pct` in each host's `fine/fine.stats` is the
figure of the real run. The 60 s shell sampler costs about 24 ms per sample (25-30 fork/execs; 0.04 % of a CPU) and
this tier starts it for the first time.

**Retention.** A tick is about 45 bytes plus 18-23 per CPU that did anything (every CPU, on msr1 with RPS on all 12 and
on the loadgen). The open generation is plain text on tmpfs (`/run/celeris/fine`); at 16 MiB it is closed into
`fine.<seq>.log.z` (zlib level 1, about 50 ms of CPU per generation, logged as a `generation_closed` event so its pause
is not read as starvation). **The compression ratio is 2-5x, depending on how much the counters vary:** 5x on a quiet
host, 2.1-2.4x with bench-like counters (NET_RX runs in the tens of thousands, user and system in the tens of percent).
Compressed generations are kept up to 256 MiB, the oldest dropped *and counted* (`generations_dropped`, a
`generation_dropped` event); the newest is never dropped. At 1 s, 234,000 ticks for a 65 h bench:

| host | bytes a tick | text, 65 h | compressed, 65 h | 111 h (the `all` deadline) |
|---|---|---|---|---|
| msr1 (12 CPUs) | 261-321 | 61-75 MB | 13-35 MB | 23-60 MB |
| 32 CPUs (msa2-server, msa2-client) | 621-791 | 145-185 MB | 33-87 MB | 55-148 MB |

All of it fits in 256 MiB, so the whole run is kept at 1 s (at the pessimistic ratio a 111 h run on 32 CPUs is 148 MB; with
half this cap it would have dropped its oldest generations). The artifact of a 65 h single-arch run is about
50-175 MB for the two hosts (already compressed). The tmpfs worst case is 256 MiB + 16 MiB + the minute and event logs,
about 0.6 % of msa2-client's RAM. At 0.05 s the ticks are 20 times as many and the cap holds the last 23-55 h on msr1 and
9-23 h on a 32-CPU host: use 0.05 s for a scoped run (`cells=...`). Whatever the cadence, two files are never rotated:
`fine-minute.log` (per CPU and minute: the highest softirq percent of a tick, the lowest user+system percent, the minute's
softirq percent, the longest tick interval; about 1-2 MB for 65 h) and `fine-events.log` (starvation episodes as a
begin/end pair, the sampler's own gaps, generations closed and dropped; 4 MiB with a drop counter). A 1 to 2 s stall is
seen by a tick only part of the time at 1 s (a stall that straddles a tick boundary reads about 70 % on two ticks), which
is why the full series is kept at 1 s and the minute record is a coarse index into it, not a substitute: it is meant to be
re-cut offline with any criterion.

**Lifetime and the artifact.** `SAMPLER_DURATION` is keyed like the job's `timeout-minutes` (the largest budget the key
admits plus 3 h, inside the ceiling). The stop step (`if: always()`, `continue-on-error`, 10 minutes: the compressed
series goes through ansible's `fetch`, in memory under `become`) ends both processes with a sentinel file and fetches into
`results/host-sampler/<host>/` (fine files in `fine/`) before the upload. The fine files are fetched only if the host's
`fine-run.txt` carries this run's tag (`bench-<run id>`), so a Deploy that failed before the start step cannot hand over the
previous bench's series (the tag carries the run attempt, `bench-<run id>-<attempt>`, so a re-run does not take its
first attempt's), and the race and weekend stops fetch no fine file; a new start deletes the files of the previous
run in its directory. If the runner dies and the stop step never runs, the fine sampler ends 60 s after its
`Runner.Listener` is gone, any later `HostSamplerStart` / `Stop` (the race and weekend tiers) retires it too, and the
forensics play fetches its events, minute record, stats and run file, and, if nobody stopped the sampler, its newest 24
per-tick generations (at most about 150 MB a host). A host without python3 gets the 60 s sampler only,
with a warning.

## Result: latency at SLO

In the rated sweep, each cell gets four constant-rate passes at 25, 50, 75 and 90 % of that cell's
saturation RPS, with loadgen's coordinated-omission correction. `latency_at_slo` maps each budget in
`{10, 50, 100, 500, 1000}` ms to the highest pass target whose P99 (the median across runs) stayed within
it. Bigger is better, and it ranks the field where saturation ceilings collapse together, such as the
store-bound driver rows. Only the 401 cells of `budget.RatedScenarios` (every rated scenario on every
server that can serve it) get the sweep: `BenchTier` passes their glob to the runner's `-rated-cells`
(`BENCH_RATED_CELLS`), with the profile's pass window in `BENCH_RATED_DURATION`. The rated rows are the
driver rows, `get-json`, `post-4k`, `churn-close` and `ws-echo`. The other scenarios (`get-simple` and its
concurrency sweep, `get-json-1k`, the H2 rows, `ws-large-echo`, and the WS hub and SSE fan-out rows)
publish saturation data only; [`budget/profiles.go`](../budget/profiles.go) gives the measured reason for
each. Datasets published before [#418](https://github.com/goceleris/probatorium/issues/418) also carry
rated data for those scenarios.

### What the artifact records per rated pass (schema 5.24, #441)

Every fraction the sweep plans leaves exactly one record, whatever became of it. `benchmarks[].rated_passes`
maps a scenario to the list of its passes, one entry per pass per run. `run` counts the runs of the cell that carried rated
data, from 0: it is not the cell's run index (a DNF run takes no number), so it does not join to `cell_run_statuses`:

| field | meaning |
| --- | --- |
| `fraction`, `target_rps` | the offered load: the fraction of the cell's saturation rate, and the rate |
| `achieved_rps`, `achieved_ratio` | requests per second loadgen delivered over the measured window, and that over the target. Below 1 is load loadgen never sent: the p99 of that pass is its backlog (about 0.99 x window x (1 - ratio)), not the server's latency. The ratio reads slightly low: `requests` counts successes only and loadgen's elapsed time includes teardown, so set any validity line (the 0.98 of #441) knowing that |
| `requests`, `errors`, `connect_errors`, `dial_retries`, `close_aborts` | loadgen's counts. `requests` are successes, `errors` failed requests, `connect_errors` the dial/handshake failures (TCP connect, TLS, WS/SSE upgrade, H1 reconnect dials). In loadgen v1.4.14 each also fails the request and is counted in `errors`, so `connect_errors` sits inside `errors`, give or take a few attempts cut off at phase boundaries (loadgen's own description: counted at the driver, it can differ from the matching `errors` increments by a few); `errors` close to `connect_errors` means the server was unreachable, not misbehaving |
| `shortfall_requests` | target x duration - requests - errors: the slots of the schedule loadgen did not fill |
| `p99` (ns), `p50`, `p99_9`, `max`, `duration`, `start`, `end`, `loadgen_version` | the pass's latency, window and loadgen build (`start`/`end` are on the loadgen host's clock, warmup included, the same clock as `load_windows`) |
| `status` | `ok`: counted in `latency_at_slo` and the headline. `skipped`: the pass ran and produced nothing usable. `not_run`: it never started |
| `reason`, `detail` | for a pass that is not `ok` (and `interrupted` on a cut-short `ok` one): `loadgen_new` (loadgen would not build), `run_error`, `nil_result`, `zero_requests`, `interrupted` (the run was cancelled: any pass after the cancel, and also an `ok` pass cut short, which loadgen returns as a short, error-free result and which stays `ok` and kept, as it always was, with the measured window in `detail`), `cell_flagged` (the cell's saturation pass was flagged suspect, so no sweep ran), `no_saturation_rate`, `non_positive_target`; and the error text |

`rated_headline_target_rps` names the target `rated_mode_p99_at_target_rps` was read at: the highest target
among the `ok` passes. When the 0.9 pass was lost it is the 0.75 pass's (or lower), and this says so; compare
it with the `fraction` of the cell's passes. A cell with no `ok` pass has records and no
`latency_at_slo`, headline or `rated_headline_target_rps`.

`mage Publish` prints a rated line in its integrity summary (passes by status, `ok` passes below 98 % of their
target, and the cells whose headline is not their top planned pass). It is informational: nothing is refused
for it, because the validity rule is still open on #441. `summary.json` carries `rated_passes`: a full record is about 0.7 KB
indented, so about 1.1 MB per arch for a full rated run (about 6 % of the 18 MB `summary.json` of 38431303162).

**Records from runs that predate 5.24.** A pass record without `status` was written by a runner before 5.24:
only `target_rps` and `p99`, kept, and the merge does not fill the status in. Such a run never recorded a pass it
dropped, so a document merged from it (a re-merge of bench 38431303162, whose stderr shows 41 skipped passes)
says nothing about lost passes. `mage Publish` counts those records as `legacy=N`, prints a `RATED LEGACY`
line, and its `skipped=0` is not a statement that none was lost.

**For a reader that predates 5.24.** Nothing it reads changes. `latency_at_slo` and
`rated_mode_p99_at_target_rps` are built from the `ok` passes exactly as they were built from the kept passes;
`saturation_mode_rps` and every other key are untouched; the two new keys are ignored. In the runner's per-cell
JSON and `raw/<host>.json`, `rated_passes` still holds only the passes the runner kept (each now with the added
keys, `target_rps` and `p99` as before); the passes it did not keep are a separate array,
`rated_passes_not_kept`, so a script that reads `target_rps` and `p99` out of `rated_passes` never counts a
pass that sent nothing as a pass with a p99 of zero. A script that wants the full picture reads both arrays.
A cell in the rated scope whose saturation pass was flagged now has `rated_passes_not_kept` with `not_run`
records where it used to have no rated key at all. A cell that failed before its saturation pass finished
(`dnf`, `not_applicable`) has no rated record: its status in `cell_statuses` is the explicit record.

**Two single-arch artifacts and `mage Publish`.** `Publish` takes ONE results file per call (`PUBLISH_RESULTS`,
or the newest `results/*-bench-*/results.json`; the arch tag comes from the document's own `host_arch_pair`) and
writes `results/<version>/<date>/<arch>/`. It merges nothing: two single-arch artifacts (a `target=msa2-server`
run and a `target=msr1` run, each with `publish=false`) are two calls, each with its artifact unpacked under
`results/<ts>-bench-<version>/` and `PUBLISH_VERSION` set, and they land in two arch directories of the same
version. Set `BENCH_START_DATE` to the same day on both calls if you want them in the same date directory;
the date only names the directory. The docs build ([goceleris/docs](https://github.com/goceleris/docs),
`scripts/build-data.ts`, `src/lib/results/aggregate.ts`) groups the cells by (version, arch) over every date
and run and AVERAGES them, so the two arches compose into one release dataset by version, and a second publish
of the same version and arch, on any date, is averaged with the first, not a replacement for it. (Checked by
`PUBLISH_DRYRUN=1` on both arch files of run 37091941051, and by reading the docs build.)

## msr1 NIC interrupt placement

msr1 has two bonded Realtek r8127 ports (enp1s0, enp49s0) with one RX and one TX queue each and no RSS.
The bench pins the TX-completion vector of enp1s0 (interrupt name `enp1s0-8`; its IRQ number changes per boot)
to CPU 1 before every bench run on msr1. This is a host-tuning choice made by the maintainer on 2026-10-17
([goceleris/celeris#1036](https://github.com/goceleris/celeris/issues/1036)), not a measured fact about the
hardware:

- **What the evidence is.** It is exploratory and post hoc. X2: an extra loop on CPU 4 (where the vector sat)
  cleared a whole-port TX stall of about 200 ms in 8 of 8 runs. X3 (run 39060669852): 21 of 21 columns with the
  vector on a loop CPU were clean and 3 of 3 with it on CPU 4 stalled. But the pre-registered gate of that X3
  run failed and its row was INCONCLUSIVE, and the h-irq4to1 arm always ran last, so order is not separated
  from placement. The mechanism (a TX-completion vector serviced by a CPU without a loop stalls the port) is
  the working explanation, not an established one. A +35% rps seen with the vector on CPU 1 is relative to
  CPU 8 and is unexplained; do not quote it as an effect of this placement.
- **What CPU 1 is.** A loop CPU only in celeris's #1049 (C1) layout. The non-celeris columns have no loops
  there, so for them CPU 1 is simply the CPU the vector was put on, and the placement does not mean the same
  thing across engines.

- **Where.** `nic_irq_placement` on the msr1 entry of [`ansible/inventory.yml`](../ansible/inventory.yml)
  (`iface`, `vector`, `cpu`), applied by [`ansible/tasks/nic_irq_placement.yml`](../ansible/tasks/nic_irq_placement.yml)
  through [`ansible/files/nic_irq_pin.sh`](../ansible/files/nic_irq_pin.sh) in the stage play of `bench.yml`.
  Empty or unset means do nothing: msa2-server and msa2-client never have it. A one-off run without it:
  `-e '{"nic_irq_placement": {}}'`. enp49s0's vector is not touched (the maintainer excluded it from the placement).
- **How.** The vector is found by name in `/proc/interrupts`, an explicit single CPU is written to
  `/proc/irq/N/smp_affinity_list`, and both `smp_affinity_list` and `effective_affinity_list` are read
  back. IFACE is bound to the vector: it is accepted when either (a) each IRQ found by that name is listed
  under `/sys/class/net/<iface>/device/msi_irqs/` or (b) the vector name starts with `<iface>-`, and refused
  (exit 2, nothing written) only when both fail; the record says which check passed (`binding`: `msi_irqs` or
  `name`). Either alone suffices because an out-of-tree driver may expose one of them differently. A readback that differs from the
  CPU fails the play before the first cell. It is a pin, not a
  restore: writing the old mask (0-11) back does not return the vector to its previous CPU, the kernel
  picks one from the mask. Applying it again is a no-op. Nothing is undone at teardown.
- **Record.** Every bench run on a host with the placement writes `nic-irq-placement.json` into its run
  directory (interface, vector, binding, irq, cpu, the affinity found before and read back after, and a second
  readback taken after the last cell) and prints the same in the log. `mage Bench` folds it into
  `environment.nic_irq_placement` of the results document and so into `env.json` (schema 5.21); a
  run without a placement has no such key.
- **Where it applies.** Only `bench.yml` pins. `mage Validate` (PR, nightly and checkptr tiers) and the stress
  playbooks do not, so on msr1 their vector placement depends on history: pinned if a bench ran since the
  boot, else the boot default. It is not recorded for them. irqbalance is not handled: nothing stops or
  configures it, so it can move the vector between the pin and the end of a run (the end-of-run readback
  below only detects that).
- **Invariant.** Every bench run records its placement, and a changed placement (another vector, another
  CPU, none) changes comparability: runs are compared only across the same `nic_irq_placement`. The docs
  site does not flag `nic_irq_placement` changes, so the first arm64 publish after this change crosses a
  placement change unflagged. An end-of-run readback that differs from the pin (something moved the vector
  mid-run) or that could not run at all (a become or transport failure) is logged as
  `WARNING: NIC IRQ placement DRIFTED` and recorded as `at_end.verified: false`; treat that run as unplaced.

Tests: `bash ansible/tests/test_nic_irq_pin.sh` (the shell logic against a fake `/proc` and `/sys`; also
runs in a plain container) and `bash ansible/tests/test_nic_irq_placement_play.sh` (the real tasks files
under ansible, run by the ansible CI job; `PROBATORIUM_ANSIBLE_TESTS=1 go test` runs it too). `go test` runs the first.

## Same-run celeris baseline

`servers/celeris_v1511` is the bench server built against the released celeris v1.5.11 instead of the
pinned main, registered as eight opt-in columns `baseline-celeris-v1511-<engine>` (epoll, iouring and adaptive in
every protocol flavour the `celeris-*` columns have, without `celeris-std-h1`). They exist so one dispatch
holds the same-run interleaved comparison against the v1.5.x release. They are left out of every "all"
expansion (the `*/*` grid, the default competitor set, `DEPLOY_COMPETITORS=all`, the budget pins) unless
`BENCH_BASELINE=1`, which `benchmark-tier.yml` sets from its `baseline` input (default `false`). Switched on it adds
222 saturation and 120 rated cells per architecture (about +14.5 h projected on `full`, +8.5 h on
`headline`) and the workflow raises `BENCH_BUDGET` for it, for every arch policy. With `target=both` under the default
policy `all`, `profile=full` with the baseline projects 130.9 h and is refused by the `setup` job (above the
7200 min job limit; see "A both-arch run"): dispatch it as two single-arch runs (`target=msa2-server`, then
`target=msr1`; 65.5 h each). Columns run as blocks in list order, so
`servers.InterleaveBaseline` places each baseline column directly beside its `celeris-*` twin (about 1.8 h
apart, alternating which runs first) instead of the sorted `baseline-*` block fifteen hours earlier. Rows publish with framework `celeris-baseline`,
framework_version `v1.5.11`. The module is a frozen fork of `servers/celeris` (go.mod pinned, not in
Dependabot); `servers/baseline_test.go` fails when the two drift. Dry-run the extra cells with
`BENCH_BASELINE=1 go run ./cmd/runner -dry-run -runs 1 -cells '*/*'`.
