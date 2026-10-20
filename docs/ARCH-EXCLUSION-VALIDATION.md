# Arch exclusion: validation runs (probatorium#442)

Nothing in the arch lock has run on the cluster. These are the scoped both-arch runs that check it, with
their criteria fixed before they run. They do not close #442 by themselves: #442's own close criteria are
quoted at the end.

The default policy, `heavy`, keeps heavy cells of the two arches apart and leaves three exposures
unmeasured (README, "What it does not do"): an x86 heavy cell sharing the client with the arm64 light rows
(3.5-6% by the one coefficient we have, which was never measured on those rows), an arm64 heavy cell sharing
it with the x86 light rows (unquantified, 6-11% if the coefficient were symmetric), and the light rows' own
rated passes running under the other arch's heavy cells (173 rated light cells per arch in the default
order). Run A measures what the default does; run B, the strict `all`, is the comparator every light-row
pass can be held against, because the default leaves no light-row rated pass alone (below).

## The runs

Dispatches of the Benchmark Tier workflow, `publish=false` (data only, nothing reaches the docs site). Run A
is the default policy:

```
gh workflow run benchmark-tier.yml -R goceleris/probatorium \
  -f profile=full -f target=both -f publish=false \
  -f competitors='axum,celeris-adaptive-auto+upg-async,celeris-iouring-auto+upg-async' \
  -f deploy_competitors='axum,celeris' \
  -f cells='ws-large-echo/*,post-4k/*,get-simple-256c/*,churn-close/*,ws-hub-broadcast-128/*' \
  -f rated_cells='ws-large-echo/*,post-4k/*,churn-close/*,ws-hub-broadcast-128/*'
```

Run B is the same plus `-f arch_exclusive=all`. The workflow's budget and timeout follow the policy
(`workflow_arch_budget_test.go`); a scoped run passes the budget gate trivially, but B is also the first
dispatch of the strict option, so it checks the workflow path of the fallback celeris#892 names. Optional
arms, same inputs:

- **C, the idle-wait control**: `-f target=msa2-server`. One arch, no peer, no lock: the cells never wait. B's cells
  wait for the peer's cell before nearly every one, so a difference between C and B (x86 passes at the same
  fraction, cells at the same position) is the SUT idling through a lock wait plus run-to-run noise. 1.19 h
  of bench.
- **D, the control arm**: `-f arch_exclusive=off`. The arches overlap again; the checker must report
  heavy+heavy overlaps (it shows the setup can see the problem). Whether x86 passes of 1 s or more come back
  is not guaranteed (6 of 10 in 0829) and is not a criterion.

`rated_cells` is needed because ws-large-echo and ws-hub-broadcast-128 are not in `budget.RatedScenarios`;
both new inputs ship with the lock. All five rows schedule on all three columns and none needs a database
service. The three columns are the ones #442 named (axum and the celeris adaptive-auto and iouring-auto
columns, whose x86 passes read 3-12 s at the 0.75 and 0.9 passes in the 0829 run).

- **Rows.** Heavy: ws-large-echo and post-4k (saturation pass of 20 s warmup + 90 s and 4 rated passes of 20 s
  + 30 s each), get-simple-256c (saturation only: the row of the 0.18-0.31 coefficient, heavy, request-rate
  bound). Light: ws-hub-broadcast-128 (highest arm64 demand of the light rows, 122k frames/s) and churn-close,
  both rated here. churn-close was `suspect` at saturation on all three x86 columns in 0829 (and on the two
  celeris arm64 columns), and the runner skips the rated sweep of a suspect cell, so it may yield no rated
  passes: its criterion below is then inconclusive, which is a result about churn-close and not about the lock.
- **Cells.** 3 columns x 5 rows = 15 cells per arch.
- **Windows (P1).** Per arch 3 x (5 + 5 + 1 + 5 + 5) = 63 (ws-large-echo, post-4k, get-simple-256c,
  ws-hub-broadcast-128, churn-close: one saturation window, and 4 rated windows where rated); 4 fewer for each
  churn-close cell that is suspect (it then has 1): 63, 59, 55 or 51.
- **Duration, from the planner** (`go test ./cmd/runner -run ValidationScope -v`, output in the PR). A: 1.29 h
  of bench (1.5-2.5 h wall clock with the Deploy of axum, which is Rust, and celeris, and the runner
  bootstrap). B: 2.35 h (the arches take turns). C and D: 1.19 h each (one arch alone; the arches overlapping). A+B is
  about 3.6 h of bench.
- **What the default leaves alone.** The same model says, for A, that of the 48 rated passes of the two light
  rows (2 rows x 3 columns x 2 arches x 4 passes), **0 run alone, 0 under a peer light cell and 48 run under
  a peer heavy cell** (with both arches forward: 0 alone, 8 under light, 40 under heavy). For P6 the model
  counts cells with at least a quarter of their saturation window under a peer light cell: x86 get-simple-256c
  3 of 3, **arm64 get-simple-256c 0 of 3** (arm64 runs it after a lock wait, while x86 is in its heavy rows),
  post-4k and ws-large-echo 3 of 3 on both arches. So P6 judges x86 on get-simple-256c and arm64 on post-4k
  and ws-large-echo, per arch. A within-run alone-versus-shared split of the light rows has an empty `alone`
  group by construction; B supplies it (all 48 alone, no heavy cell shared).

Dispatch only with the cluster group empty and `EV/_locks/HOLD-CLUSTER` absent, as for any cluster run.

## Pre-registered criteria

Read from the downloaded artifacts (`results-amd64.json`, `results-arm64.json`, `raw/`) with
`P442/scripts/read_validation.py <A dir> [<B dir>]` in the lane evidence (checked on synthetic artifacts by
`read_validation_selftest.py` there; it has never read a real run). It labels every rated pass with the
class of its own row (`heavy` or `light`) and with what it overlapped on the other arch (`alone`;
`shared-light`, only light windows; `shared-heavy`, a heavy window). A **light** pass in `shared-heavy` is
the case the default leaves exposed, not a mechanism failure; only a **heavy** pass in `shared-heavy` would
be one (the checker's P1 forbids it).

**P1, mechanism, run A (must hold, or the fix is broken and nothing below is read).**
`mage BenchArchOverlap BENCH_RESULTS_DIR=<A dir>` exits 0: 0 forbidden overlaps and 0 windows without usable
timestamps; both documents carry `environment.arch_exclusion` with `policy: heavy`, `violations: 0`,
`heavy_heavy_overlaps: 0`; `lock_wait_seconds` is greater than 0 on at least one arch and at least one cell
carries a `client_lock_wait_s` greater than 0 (a lock that was free at the first attempt now reports exactly
0, so this can fail); `heavy_light_overlaps` is greater than 0 (the run did put heavy cells under light
load); the windows are all there (per arch 63 minus 4 for every churn-close cell that is suspect at saturation and so
has no rated sweep: 63, 59, 55 or 51).
**P1b, run B:** `BENCH_ARCH_EXCLUSIVE=all mage BenchArchOverlap` exits 0, with `heavy_light_overlaps: 0` and
`light_light_overlaps: 0` in both documents, `lock_wait_seconds` greater than 0, and no cell failed as
`client-lock:` (the wait limit is a safety net; one firing in a healthy run is a bug to read first).

**P2, the ws-large-echo backlog clocks are gone** (A and B, x86). x86 ws-large-echo rated passes (3 columns x
4 = 12 per run): 0 of 12 with p99 at or above 1 s, in each run and in each sharing group of A. For
reference, 6 of 10 overlapping passes read 1 s or more in 0829, and the single-arch 0716 run has none in 52
(peak 35.7 ms). Secondary: the largest of the 12 is below 100 ms.

**P3, post-4k is back to its single-arch value** (A and B, x86). The two celeris columns, 0.9 pass: p99 at or
below 2 ms (0716 single-arch: 1.1-1.4 ms; the non-overlapping 0829 h1 columns: 1.7-1.8 ms; the overlapping
ones read 5.0 and 5.3 s), in each run and sharing group. axum is reported, not judged (0716: 2.4 ms).

**P5, the light rows' rated passes, A against B** (arm64 and x86; ws-hub-broadcast-128 and churn-close).
Pair every kept rated pass of A with the pass of B at the same (arch, column, row, fraction) and take
r = p99(A) / p99(B). Holds when there are at least 8 pairs, the median r is at most 1.5, and no pass reads
1 s or more in A where its B pair is under 1 s. Fewer than 8 pairs is **inconclusive**, not a pass (a
suspect churn-close can empty half the pairs; with ws-hub-broadcast-128 alone the maximum is 24). The 1.5 is
chosen here, before the run, as a deliberately loose ceiling: nothing measured says how much a rated p99
moves between two runs at the same fraction (saturation numbers of duplicate cells in 36441790519 differed by
|log| p50 0.027, p90 0.158 on arm64), and the 1 s clause is what #442 is about. A fail means the light rows'
rated passes were hurt by the peer's heavy cells.

**P6, a heavy cell under a light peer, A against B, per arch** (saturation requests/s). x86 is judged on
get-simple-256c (the row of the 0.18-0.31 coefficient) and arm64 on post-4k and ws-large-echo, because the
planner shows arm64's get-simple-256c cells do not share the client with an x86 light cell in the default
order while its post-4k and ws-large-echo cells do; this is the direction nobody has measured. The other
rows are printed, not judged (x86 post-4k and ws-large-echo sit on the #518 plateau, which makes them
insensitive). A cell of A is *shared* when at least a quarter of its saturation window overlapped a window of
the other arch's light cells. For each arch: holds when there are at least 3 shared cells, the median of
rps(A) / rps(B) over them is at least 0.965 (the low end of the 3.5-6% the coefficient predicts for x86 under
the arm64 light rows), and none is below 0.92. Fewer than 3 shared cells is inconclusive. P6 holds only if both
arches hold, fails if either fails. The unshared judged cells of A against B are printed as the noise reference
(they should sit near 1.00).

**What A and B do and do not hold equal.** arm64 walks each column in reverse under both policies (`all`
reverses it too, at no wall-clock cost), so a cell's position in its column, and its place on a freshly
started SUT, is the same in A and B. What differs is what the arches shared, and the idle time: under `all`
nearly every cell waits for the peer, under the default far fewer do. Arm C controls that idle only for x86,
and only for the amount B waits; an effect of idling would show as a B that reads differently from C and
would make A against B understate or overstate the sharing. It is read, not judged (P7).

**P7, idle wait (only with arm C).** x86 saturation requests/s and rated p99 of C against B at the same cell,
and against the A cells that waited 60 s or more. Reported, not judged: no threshold is pre-registered
because nothing says what an idle gap does to these rows. A difference that grows with the wait (the CPU
frequency or an adaptive engine's settled state) is a finding for a follow-up on the lock's wait, not a
reason to drop the lock.

**P4, the overlapping-pass census, restated.** #442's title counts passes that overlap the other arch's run
of the same row. With the lock the same-row heavy set is empty by construction, so "0 of N" for heavy passes
reads 0 of 0 and is not evidence; P1 is the real statement. Reported for completeness: the number of x86 and
arm64 rated passes at or above 1 s, any row (informational; arm64's were all celeris columns in
36441790519, a celeris follow-up that is on hold until this run says whether the overlap was the trigger).

## What the verdicts do

- P1 or P1b fails: stop. The exclusion does not hold (or the windows are wrong); the bug is in this change.
- P1 holds, P5 and P6 hold: the light exemption holds on this scope. `heavy` stays the default. It is a
  statement about these columns and rows, not about the driver rows, which need a database service and are
  not here.
- P5 or P6 fails: the exemption does not hold; the default becomes `all` (one input value to dispatch; the
  default itself is the `resolveArchExclusion` empty case and the two `'heavy'` fallbacks of the workflow's
  budget and timeout tables, which `TestWorkflowDefaultPolicyIsTheMageDefault` keeps in step). If only P5 fails
  the maintainer may prefer locking the rated sweeps instead (75.2 h in the model, not implemented).
- P5 or P6 inconclusive: say so; it is not a pass. Widen the scope (more columns) or take the strict policy.
- P2 and P3 hold (A and B): the overlap was the cause of the x86 clocks on these rows. #442's first close
  criterion (zero overlapping x86 passes) holds by P1; its second (ws-large-echo and post-4k match 0716) holds
  for this scope. It does not cover get-simple-1024c, get-json-h2 and post-4k-h2, which the issue also names:
  they are not in this scope, and only a full both-arch run reads them.
- P1 holds and P2 or P3 fails: the overlap is not the whole story on that row (loadgen's pacer, #441,
  loadgen#102, or the client itself). #442 stays open for that row; report the passes.

## A confounder that is not the overlap (#518)

Since about 2026-10-04 every amd64 server that can reach it sits on a 9.1-9.25 Gbit/s plateau on the three
bandwidth rows (post-4k, post-4k-h2, ws-large-echo), where earlier runs reached 13.6-18.4
([#518](https://github.com/goceleris/probatorium/issues/518); it is not the arch overlap: it shows in cells
with none). The rated targets are fractions of each cell's own saturation, so P2 and P3 stay readable, but
the 0716 and 0829 numbers they are compared with were taken at a different link state. Read each x86 cell's
saturation in Gbit/s (rps x payload x 8) next to its rated passes: if the saturation sits on the plateau,
say so in the verdict, and a backlog clock that remains is then not evidence about the overlap either way.

## Owed, not run

Everything above; also an amd64 native timing of the runner and a real both-arch run of the full grid, whose
wall-clock the PR estimates at about 54 h (a model, README "A both-arch run"). The arms C and D are optional.
