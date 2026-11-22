# Validation tier reference

Reference for the validation tiers of [probatorium](../README.md): the three layers inside every validation
cell, the invariants and their thresholds, cross-engine and cross-architecture divergence, the reference apps
and matrix mode. The front page is the [README](../README.md); this page is the detail. Every statement here
moved unchanged from the README.

Contents: [Tier 1](#tier-1-always-on-property-stress) · [Race tier semantics](#race-tier-semantics-liveness-is-gated-latency-is-recorded) ·
[Tier 2](#tier-2-stateful-api-fuzzing-not-implemented) · [Tier 3](#tier-3-deterministic-seed-replay) · [Invariants](#invariants) ·
[Divergence](#cross-engine-and-cross-arch-divergence) · [Reference apps](#reference-apps) ·
[Matrix mode](#matrix-mode-validate_matrix1)

Each validation cell runs Tier 1 and Tier 3 against one reference app on one engine; Tier 2 is still
scaffolding.

## Tier 1: always-on property stress

Five traffic slices fan out over `Concurrency` walker goroutines. The budget activates progressively, so
small smoke runs don't pay for the expensive slices:

| Slice | Share of walkers | Active at concurrency ≥ | What it does |
| --- | --- | --- | --- |
| Markov | ~60 % | 1 | Session-shaped traffic over the reference app's routes, with transitions weighted by `validation/markov/<refapp>.yaml` |
| Adversarial | ~20 % | 1 | Raw-TCP malformed HTTP/1.1: bad chunks, oversized headers, NUL in a header, CRLF injection, slowloris, double Content-Length |
| h2c upgrade churn | ~10 % | 10 | Valid h2c upgrade preambles, then an RST at three different stages. Only `kitchen_sink` runs on `Protocol: celeris.Auto`, so only it completes an upgrade; the other seven serve HTTP/1.1 and decline by design |
| WS frame torture | ~5 % | 4 | A real RFC 6455 handshake, then one of: fragmented reserved opcode, oversize payload, unmasked client frame, ping flood, continuation without start, invalid UTF-8 |
| SSE kill-mid-stream | ~5 % | 4 | Open an SSE stream, hold it 50 to 1500 ms, then RST; the broker must clean up the client slot |

From concurrency 4, two more walkers run outside that budget. One is an RFC conformance scraper that
reads the wire (`I-RFC-1`, `I-RFC-2`). The other is a 64 KiB WebSocket echo (`I-WS-ECHO`), which runs
where `/ws` is routed. The echo holds no connection for the first 18 s of every 54 s: a native engine
keeps each echo's backlog on the Go heap, and these gaps give every `I-MEM-1` trough bucket samples
without one ([`validation/ws_echo_quiet.go`](../validation/ws_echo_quiet.go), probatorium#478).

In a stall-capture run (the `stall_capture` dispatch input), a 64 KiB echo fire that times out is held
open: the sender stops, the walker keeps reading, and `ss -tmi` of both ends, the echo and byte
counters, the refapp's per-thread CPU masks and states, its `GOMAXPROCS` and its engine counters are
taken at t0 and t0 + 3 s. A record-only `I-WS-ECHO-STALL` dossier carries `ws-echo-diag.txt`, whose first
line is `WS_ECHO_VERDICT=SLOW_CLASS` (the server application read bytes or wrote more than a control frame
in the window, from the exact counters `bytes_received` less Recv-Q and `bytes_acked` plus Send-Q: slow),
`WEDGE_CLASS` (neither moved while bytes sat unread in the server's receive queue) or `INDETERMINATE`
(missing or contradictory data is never a wedge).
The timeout is counted after the second instant. `mage` and the race tier also take two off-by-default
experiment arms for msa2-server only, `refapp_cpus` (a `taskset -c` list of at least 2 CPUs) and
`refapp_workload_hint` (`high` starts the adaptive engine on io_uring); each cell records the arm it
actually ran under in `refapp_sched.txt` ([`validation/ws_echo_diag.go`](../validation/ws_echo_diag.go),
[`validation/refapparms`](../validation/refapparms), probatorium#481).

### Race tier semantics: liveness is gated, latency is recorded

The race tier's adaptive cell is deliberately small (`adaptiveCellWorkers` = 2 workers, a floor of
`adaptiveCellConcurrencyFloor` = 110 walkers, to force the epoll-to-io_uring promotion) and runs under
`-race` instrumentation, so a late 64 KiB echo there is a capacity and latency effect before it is a
liveness defect. Its amd64 adaptive cell timed out in every run, and every timeout the two-instant
capture classified was `SLOW_CLASS`: the server read and wrote, the echoes were only slower than the
fire's deadline. The race tier therefore judges liveness and records latency:

| In the race tier | Result |
| --- | --- |
| A `ws_echo` timeout classed `SLOW_CLASS` | not a failure: counted in `ws_echo_timeout_slow` (exported in `validate-results.json` under `tier_1.ws_echo`, printed by `mage ValidateGate`, which lists every cell that recorded one), and a record-only `I-WS-ECHO-SLOW` dossier whose `ws-echo-diag.txt` keeps `WS_ECHO_VERDICT` as its first line and adds `WS_ECHO_DISPOSITION=TOLERATED_SLOW` as its second |
| `WEDGE_CLASS`, `INDETERMINATE` (no capture included) | `ws_echo_timeout` as before: hard `I-WS-ECHO`, the cell stops, the gate fails |
| More than N = 3 `SLOW_CLASS` timeouts in one cell | the 4th is `ws_echo_timeout` (`WS_ECHO_DISPOSITION=FAIL reason=over_n`) |
| A `SLOW_CLASS` timeout after the first with no fire in the cell having drained cleanly since the first | `ws_echo_timeout` (`reason=no_clean_fire_since_first_slow`): every fire after the first timed out, which is a server delivering nothing, not one that is late now and then |
| Every other violation | unchanged |

The tolerance is explicit and scoped to this tier. The validator takes `-race-tier`, which
`ansible/validate.yml` passes only when `validate_race` is `1` (the `-race` refapp set), and
`mage ValidateGate` takes `VALIDATE_GATE_TOLERATE_WS_ECHO_SLOW=1`, which only `matrix-race-tier.yml`
sets. The gate repeats the rule on the results: it tolerates `ws_echo_timeout_slow` only with that
variable, only in a cell that declared `I-RACE` (a `-race` build), and only up to 3. Nightly, soak,
weekend, checkptr and PR runs have neither switch, so a `SLOW_CLASS` ws_echo timeout in them stays
`ws_echo_timeout` and fails: their timing budget is real. In the race tier the capture is armed by
`-race-tier` itself, so `stall_capture` is not needed for the tolerance. `ValidateDiff` does not list
`ws_echo_timeout_slow`, so an amd64-only slow echo is not a cross-arch divergence.

`ws_echo_timeout_slow` is a measurement, not an exemption: watch its count per run. Before this change
the first timeout ended its cell, so "one per run" was a censored count; the per-cell rate is now
measured by the race tier itself.

What a race run records about itself (schema 5.23, record only, nothing new is gated):

- **Every diagnosed SLOW fire is counted** (the capture budget is N+1 = 4 per cell; a later timeout is counted in `ws_echo_timeout` alone). A `SLOW_CLASS` fire the guard refuses is still the failing
  `ws_echo_timeout`; it is also counted by reason in `ws_echo_slow_over_n` and
  `ws_echo_slow_no_clean_fire` (subsets of `ws_echo_timeout`). `mage ValidateGate` prints each cell's total
  (tolerated plus refused) and lists the refused ones in a block of their own, marked FAILING, so the
  failure reads as slow-class latency past N and not as a wedge. A document without these keys prints as
  before.
- **`h2c_hang` and `ws_handshake_fail` have an instant.** `tier_1.h2c_hang_span` and
  `tier_1.ws_handshake_fail_span` hold the first and last event of the cell: UTC `ts`, `since_ready_ms`,
  `cause` (eof, reset, timeout, status, other), the verbatim `err` and the addresses (`since_ready_ms` is omitted when 0, i.e. when the refapp's ready instant is unknown or the event is within the same millisecond). The slow ring keeps
  only reads over one second, so a hang that closed in milliseconds had no timestamp anywhere. `ts` is UTC,
  like `ready_at`, the slow ring and the epoch seconds of `properties_series.csv` (whose
  `adaptive_switches` column steps at the switch), so the four join on one timeline; the refapp's own
  stderr lines carry host-local time with no zone and join through `since_ready_ms`.
- **The refapp's CPU placement is in the cell document in every run:** `tier_1.refapp_sched` has the
  per-thread `Cpus_allowed_list` counts (`thread_masks`) read from `/proc` at ready, and the leader's mask
  separately, which is not the process's mask. `gomaxprocs` is recorded only when the stall-capture side
  listener answered (never under the ssh driver); otherwise it is 0 with a note. Recording it in every run needs the refapp to publish
  it (`validation/refapp/internal/debugvars`), a change to the refapp build that is not made here.
- **The race tier runs the host sampler** (`HostSamplerStart` / `HostSamplerStop`, tag
  `race-<run id>`, lifetime fixed at 150m inside the job's 180 minute limit, each step bounded to 5 minutes), as the weekend soak does.
  The bench tier runs it too, with a 1 s fine series by default (tag `bench-<run id>`; [details](BENCH.md#the-host-sampler-beside-a-bench)).

The two streaming slices are **routed**. Once per cell, the tier probes `/ws` and `/events` with the
exact request its walker sends, and skips the slice when the reference app answers 404; only
`auth_session_ratelimit` routes them today. The verdict is recorded per cell (`ws_route_probed` /
`ws_route_present` and the `sse_` pair), so a zero is attributable. A matrix run also writes a
per-app coverage table to `streaming-coverage.txt`.

The first time one of these signals appears, the orchestrator acts on it mid-run
([`validation/runner.go`](../validation/runner.go)):

| Signal | Predicate | Meaning | On first hit |
| --- | --- | --- | --- |
| adversarial wrong-accepted > 0 | `I-ADV-ACCEPTED` | The server accepted malformed bytes: an RFC violation | hard fail: forensics first, then the cell stops |
| h2c crashed > 0 | `I-H2C-CRASHED` | The engine answered an upgrade with non-HTTP bytes | hard fail: forensics first, then the cell stops |
| WS accepted bad frame > 0 | `I-WS-ACCEPTED` | The server accepted an RFC 6455 violation | hard fail: forensics first, then the cell stops |
| WS hang (no close) > 0 | `I-WS-HANG` | A WebSocket connection hung past the close timeout | hard fail: forensics first, then the cell stops |
| 64 KiB echo corrupt / reordered / missing / timed out | `I-WS-ECHO` | WebSocket echoes were not byte-intact and in order | hard fail: forensics first, then the cell stops. In the race tier only, a timeout that the two-instant capture classed `SLOW_CLASS` is not this row: see the next one |
| 64 KiB echo timed out, `SLOW_CLASS` (race tier only) | `I-WS-ECHO-SLOW` | The server read and wrote in the window; the echoes were only later than the fire's deadline | record-only dossier, counted in `ws_echo_timeout_slow`, the cell runs on; the gate fails past 3 per cell |
| process died | `I-LIVENESS` | The reference app crashed mid-run | hard fail: forensics first, then the cell stops |
| process alive but wedged | `I-HANG` | The reference app stopped responding | hard fail: forensics first, then the cell stops |
| h2c hang > 0 | `I-H2C-HANG` | An upgrade was neither answered nor declined within 20 s | dossier without a core dump; the cell runs on |
| WS handshake fail > 0 | `I-WS-HANDSHAKE` | A WebSocket upgrade did not complete within 2 s | dossier without a core dump; the cell runs on |

A zero only means health when the oracle ran. `mage ValidateGate` therefore also fails a `kitchen_sink`
cell whose `h2c_upgraded` is 0 after sending preambles: every churn mode would then have degenerated
into a declined GET, and the h2c oracles would have judged a path the engine never entered. The same
rule covers dead cells (`requests_sent == 0`) and properties that were never evaluated.

## Tier 2: stateful API fuzzing (not implemented)

The intent is RESTler-style producer/consumer inference from `validation/spec/<refapp>.openapi.yaml`, to
catch API-level bugs Tier 1 misses. **The tier is scaffolding today:** `runTierRESTler` parks on the run
context and sends nothing, and every `plan.json` marks the tier `DISABLED` with that reason. Only
`auth_session_ratelimit` ships a spec.

## Tier 3: deterministic seed replay

This is a real kernel with no mocking. A seed expands into a workload plus a fault schedule
(`iptables` drops, `tc netem` loss and delay), so a bug is the tuple `(seed, git_commit, host_arch)` and
reproduces with `validator-replay` (see [Reproduce a run](../README.md#reproduce-a-run)). The corpus is the 100
hand-authored seeds in [`validation/corpus/seeds_initial.go`](../validation/corpus/seeds_initial.go). Each
cell replays seeds for its whole window at 15 s per seed, looping over the corpus.

## Invariants

Beyond the Tier 1 signals, the orchestrator runs a **per-second property loop** inside every cell
([`validation/propertyloop.go`](../validation/propertyloop.go), with the shared evaluator in
[`validation/checker`](../validation/checker)). Once the reference app announces its address, the loop:

- polls its `/debug/vars`, which every reference app serves through
  `validation/refapp/internal/debugvars`, alongside `/debug/pprof` for forensics;
- samples its RSS from `/proc`;
- evaluates the registered `I-*` predicates against a rolling one-hour history.

Every violation is counted into `tier_1.property_violations` / `property_violation_ids`, and
`mage ValidateGate` fails on any of them. What the first violation does to the *cell* is a switch:

- **Record-only** (the default): an incident dossier with live forensics (`/proc`, the `/debug/pprof`
  profiles, gcore, dmesg) is written while the reference app is still running. The cell then continues to
  its budget, so a leak's whole trajectory is on record.
- **Hard fail** (the validator's `-property-hard-fail`, or `VALIDATE_PROPERTY_HARD_FAIL=1` in its
  environment, which `ansible/validate.yml` sets from the `validate_property_hard_fail` extra-var): the
  same dossier is captured *before* the tiers are torn down, then the cell is cancelled and the validator
  exits non-zero.

Each cell reports `properties_passed`, `properties_failed`, `properties_not_instrumented` and
`properties_not_judged`. A predicate counts as passed only if it reached a **verdict** at least once and
never failed. One that only ever skipped (its window never became judgeable) is `not_judged`; one with no
data source is `not_instrumented`. By default the gate also fails a cell whose loop never evaluated
anything (`/debug/vars` unreachable), unless `tier_1.property_loop_skipped` names a reason.

Judged in every cell:

- `I-LIVENESS` / `I-HANG`: the reference app stayed up and responsive (judged from the walkers, not the
  snapshot loop).
- `I-PANIC`: no *unexpected* panic. This is `celeris.panic_count` from `/debug/vars` minus the panics
  the traffic corpus designs in, and it must persist for 3 samples.
- `I-CONN-2`: `accepted − closed − active == 0`, judged only when the drift keeps the same sign for 30
  samples.
- `I-MEM-1`: the `heap_inuse` trough slope is at most 1 KB/s over the trailing min(1 h, elapsed), after a
  5-minute warm-up, from 10 minutes of samples.
- `I-MEM-3`: the goroutine-count trough slope is at most 0.2/s over the trailing 10 minutes after
  warm-up.
- `I-MEM-4`: the RSS trough slope is at most 64 KB/s over the trailing min(1 h, elapsed) after warm-up
  (Linux, local driver).

The three slope oracles share a false-positive guard:
- The fit runs over per-150 s bucket **minima**, so a GC sawtooth cannot pass for a slope.
- A slope above budget counts only if the rise across the window also clears a floor, so one legitimate
  step (a cache filling once, a standby engine spinning up) cannot fire it. The floor is the larger of
  budget × 10 minutes, a fraction of the series' level (3 % heap, 5 % goroutines and RSS) and, for the
  heap, 8× its sampling noise and the rise the cell's own trough scatter can explain: the fitted slope
  must be significant (one-sided 0.001, a critical value per trough count) against the scatter of the
  window's troughs, estimated from their successive differences. **That is not free.** Measured against
  the rule without the term on the weekend soaks' 45 minute cells (25 noisy observability cells, trough
  sd 0.34-1.4 MB; 162 quiet cells; detected/sustained counts; all tables and the case definitions are in
  probatorium#520):
  - Noisy cells, straight leak from the end of warm-up: 1 KB/s is called in 9 of 25 cells (24 without
    the term), 2 KB/s in 24 (25), and a 3 KB/s leak about 11 minutes later (median 17.5 -> 28.7 min).
  - Noisy cells, a leak that starts late: from minute 20, 3 KB/s 25/25 -> 22/19; from minute 25, 3 KB/s
    21/10 -> 12/3, 4 KB/s 25/22 -> 21/10, 6 KB/s 25/25 -> 24/21.
  - Noisy cells, a stepped leak of 3 KB/s on average: 11 minutes later with a step every 15 minutes
    (median 23.7 -> 35.0), sustained 21 -> 9 with a step every 30.
  - Quiet cells, straight leak: 3 KB/s is called 3.7 minutes later at the median and 6.2 at most;
    1 KB/s in 146 of 162 cells instead of 150. Late-start and stepped leaks: up to 2.5-5 minutes later.
  - **Quiet cells, a leak that stops.** A leak that starts at the end of warm-up, runs 7.5 minutes and
    then plateaus is lost: 3 KB/s 154/153 -> 64/18 (90 quiet cells lost), 4 KB/s 162/156 -> 125/75,
    6 KB/s 162/162 -> 156/153; 3 KB/s for 10 minutes 162/162 -> 157/149. The estimator, not a floor
    that is too high, is the cause: an instant 2 MB step is still called in 156 cells, as without the
    term. After the leak stops, the shifted successive differences sit in windows of 6-8 troughs where
    the 75 % trim drops only one difference, which inflates the scatter estimate. The other floors do
    not catch it.
  - A single low trough (a 2-4 MB dip at minute 10-15) delays a 3-5 KB/s call by a median of 7.5-11.2
    minutes (max 11.2-13.7) on quiet cells, beyond the 10 minute maximum the straight-leak tests allow.
  - A single 1 MB step is called in 139 of 150 quiet cells that the rule without the term calls.
  - Cells that scatter heavily though quiet (1-2 MB of added trough scatter): 3 KB/s is lost in 19
    (1 MB) to 123 (2 MB) of 162 cells, and a 4 MB sawtooth loses 161. In the same cells with no leak
    the rule without the term trips falsely in 59 of 162 and this rule in none.
  - Short cells: the scatter term needs the load phase to last long enough for windows of 8 or more
    troughs. With the load phase cut to 20 minutes the quiet cells lose 99 of 162 at 3 KB/s, 75 at
    5 KB/s and 32 at 10 KB/s (noisy cells 22-25 of 25); at 25 minutes none are lost.
  - Windows of 5-8 troughs judge only a leak that is large against the scatter.
  - Nothing the floor absolves is hidden. The validator runs I-MEM-1 as it was before the floor
    beside the gated rule, on the same series, and records it per cell as `property_informational`
    (schema 5.20) with its violation count, the evaluations the gated rule absolved and a flag for a
    cell it absolved entirely. `mage ValidateGate` prints it on every run under "informational, NOT
    gating (the pre-scatter-floor rule of I-MEM-1 ...)". The gate never reads it: the verdict is the
    gated rule's alone. A reader of a soak sees every trip the floor absolved and can judge it.
  - The maintainer accepted these costs on 2026-10-17 after five pre-registered attempts (probatorium#520;
    the two later estimators that did not work, and why, are in its description).
- A verdict must persist for 150 consecutive evaluations before it is declared.

These oracles need cells of 15 minutes or more to judge at all, and I-MEM-1 needs about 25 minutes of
sustained load to keep its sensitivity: a shorter load phase loses leaks (see the cost list above). They
judge in the weekend soak's cells (about 45 minutes each: 24 h over 32 cells per architecture, which is
unaffected), and are `not_judged` in the short nightly cells.

Judged only where their data source exists, and reported as `not_instrumented` everywhere else:

- `I-MW-*`: only the apps that install the middleware.
- `I-DRV`: the three driver apps, which read every write back and publish the hit/miss tally.
- `I-RFC-1` / `I-RFC-2`: the conformance scraper, at concurrency ≥ 4.
- `I-CONN-1`: the per-connection last-byte table.
- `I-CHECKPTR`: Checkptr Validation cells; `I-ENG-IOURING`: that tier's io_uring cells.
- `I-RACE`: Race Validation cells, which count `WARNING: DATA RACE` reports on stderr.
- `I-ENG-ADAPTIVE`: adaptive cells.
- `I-MEM-2`: cells of 20 minutes or more, which idle the app twice, after a 60 s burst and at the end.

## Cross-engine and cross-arch divergence

A matrix run's `validate-results.json` holds one `Cells[]` entry per `(refapp, engine, arch)`.
`mage ValidateDiff` compares the two latest matrix documents and reports:

- **cross-engine** divergence: a counter non-zero on one engine (say `iouring`) but zero on another for
  the same `(refapp, arch)`, which is typically an engine-specific bug;
- **cross-arch** divergence: the same shape, comparing amd64 with arm64.

It exits non-zero on HIGH severity, and on MEDIUM too with `VALIDATE_DIFF_STRICT=1` (which every
validation tier sets). It writes `validate-diff/diff.{txt,json}`.

## Reference apps

Each reference app is a **separate Go module** under [`validation/refapp/`](../validation/refapp/), so the
validator never pulls in every middleware's dependency graph. The matrix runner discovers them at
runtime.

| Slug | Coverage |
| --- | --- |
| `auth_session_ratelimit` | Session cookie, rate limit, and the WebSocket / SSE detach paths (the only app that routes `/ws` and `/events`) |
| `auth_jwt_csrf` | JWT (HS256), CSRF synchronizer token, keyauth |
| `kitchen_sink` | 17 stateless middlewares (basicauth, bodylimit, cache, circuitbreaker, cors, etag, healthcheck, idempotency, methodoverride, ratelimit, recovery, redirect, requestid, rewrite, secure, singleflight, timeout). The only app on `Protocol: celeris.Auto`, so it is where the h1→h2c upgrade path is exercised |
| `driver_postgres` | celeris's native Postgres driver, `session/postgresstore`, and the `I-DRV` read-after-write tally |
| `driver_redis` | The native Redis driver, `session/redisstore` and a Redis-backed rate limiter |
| `driver_memcached` | The native memcached driver, `session/memcachedstore` and a memcached-backed rate limiter |
| `observability` | logger, metrics and otel middleware |
| `static_swagger_proxy` | static (`embed.FS`), swagger (OpenAPI 3.0) and proxy (X-Forwarded-For trust) |

Each follows the same shape: its own `go.mod`, `resolveEngine("auto")` (io_uring on Linux, std elsewhere),
signal-driven graceful shutdown, and a `ready addr=<bind-addr>` startup line.

## Matrix mode (`VALIDATE_MATRIX=1`)

Matrix mode iterates the `(refapp × engine)` cells and runs a fresh orchestrator per cell with a budget
of `total_duration / len(cells)`. It emits one `validate-results.json` with `Cells[]` populated. Filter
the matrix with:

- `VALIDATE_MATRIX_REFAPPS=driver_postgres,driver_redis` to limit the reference apps;
- `VALIDATE_MATRIX_ENGINES=iouring,epoll` to limit the engines. The default is the OS production set:
  iouring, epoll, std and adaptive on Linux, std elsewhere.

**A failing cell does not stop the run**, because cluster time is the scarce resource. Every cell carries
a `status`:
- `ok`;
- `failed`: it ran, then something failed it;
- `not_run`: it produced no requests and no property verdicts, because its app never started.

Each cell also carries the verbatim `failure_reason`. For a cell that never came up, that is the driver's
own account of why. The ledger names the cell's evidence (`refapp_stderr_tail.txt`, else the incident
dossier) only when that evidence has content. The end of the run prints the ledger and leaves it in
`cell-failures.txt`. The exit status is still non-zero for any cell that did not pass: this is a gate,
not a report.

Three conditions stop the run immediately, because none of them leaves a later cell able to measure
anything:
- the remote driver cannot be built;
- the reference-app staging directory is unreadable;
- a write fails with `ENOSPC`.

Two caps bound the damage when the matrix is failing wholesale:
- **8 consecutive** failures;
- **half the plan** failed in total.

Override them with `PROBATORIUM_MATRIX_MAX_CONSECUTIVE_FAILURES` / `PROBATORIUM_MATRIX_MAX_FAILED_CELLS`
(negative disables); `mage Validate` carries both across the SSH boundary. The weekend soak sets the
total cap to 8. Whatever stops a run early, the summary names the cells it never attempted and prints the
`-matrix-resume-from` line that finishes them.
