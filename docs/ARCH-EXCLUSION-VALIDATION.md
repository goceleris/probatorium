# Arch exclusion: validation run (probatorium#442)

Nothing in the arch lock has run on the cluster. This is the scoped both-arch run that checks it, with
its criteria fixed before it runs. It does not close #442 by itself: #442's own close criteria are
quoted at the end.

## The run

One dispatch of the Benchmark Tier workflow, `publish=false` (data only, nothing reaches the docs site):

```
gh workflow run benchmark-tier.yml -R goceleris/probatorium \
  -f profile=full -f target=both -f publish=false \
  -f competitors='axum,celeris-adaptive-auto+upg-async,celeris-iouring-auto+upg-async' \
  -f deploy_competitors='axum,celeris' \
  -f cells='ws-large-echo/*,post-4k/*,churn-close/*,ws-hub-broadcast-128/*' \
  -f rated_cells='ws-large-echo/*,post-4k/*'
```

(`arch_exclusive` is left empty: the default, `heavy`. `rated_cells` is needed because ws-large-echo is
not in `budget.RatedScenarios`; both inputs are new in the PR that adds the lock. All four rows schedule on
all three columns and none needs a database service.)

- **Cells.** 3 columns x 4 rows = 12 cells per arch. The heavy rows, ws-large-echo and post-4k, get a
  saturation pass (20 s warmup + 90 s) and 4 rated passes (20 s warmup + 30 s each): 322 s plus a 5 s
  cooldown, 6 cells per arch. The light rows, churn-close and ws-hub-broadcast-128, get the saturation
  pass only: 122 s, 6 cells per arch. The three columns are the ones #442 named: axum and the celeris
  adaptive-auto and iouring-auto columns, whose x86 passes read 3-12 s at 0.75 and 0.9 in the 0829 run.
- **Why the light rows.** Without them the run is all heavy, fully serial, and never puts a heavy pass
  under the other arch's light load, which is the assumption the default policy rests on. With arm64
  walking each column in reverse, the light cells of one arch fall under the heavy cells of the other.
- **Duration.** The heavy cells serialise completely: 12 x 327 s = 65 min, and the light cells fit in the
  gaps, so about 1.2 h of bench; 1.5-2 h wall clock with the Deploy of axum (Rust) and celeris and the
  runner bootstrap. The lock queues on every heavy cell, so a run in which `lock_wait_seconds` is 0 would
  prove nothing.
- **Control arm (optional, same inputs plus `-f arch_exclusive=off`).** About 35 min (the arches overlap
  again). It shows the setup can see the problem: the checker must report heavy+heavy overlaps. Whether x86
  passes of 1 s or more come back is not guaranteed (it was 6 of 10 in 0829) and is not a criterion.

Dispatch only with the cluster group empty and `EV/_locks/HOLD-CLUSTER` absent, as for any cluster run.

## Pre-registered criteria (the locked run)

Read from the downloaded artifact (`results-amd64.json`, `results-arm64.json`, `raw/`). The per-pass split
below is `P442/scripts/read_validation.py <artifact dir>` in the lane evidence: it matches each rated window
to its kept pass by target and labels it `alone`, `shared-light` (it overlapped only light windows of the
other arch) or `shared-heavy` (impossible under the lock).

**P1, mechanism (must hold, or the fix is broken and nothing below is read).**
`mage BenchArchOverlap BENCH_RESULTS_DIR=<artifact dir>` exits 0: 0 forbidden overlaps and 0 windows
without usable timestamps; both documents carry `environment.arch_exclusion` with `policy: heavy`,
`violations: 0`, `heavy_heavy_overlaps: 0`; and `lock_wait_seconds` is greater than 0 on at least one arch
(the lock engaged). The windows are all there: per arch, 6 heavy cells x (1 saturation + 4 rated) + 6 light
cells x 1 = 36. `heavy_light_overlaps` is greater than 0 (the run did put heavy cells under light load).

**P2, the ws-large-echo backlog clocks are gone.** x86 ws-large-echo rated passes (3 columns x 4 = 12): 0 of
12 with p99 at or above 1 s, in both the `alone` and the `shared-light` group. For reference, 6 of 10
overlapping passes read 1 s or more in 0829, and the single-arch 0716 run has none in 52 (peak 35.7 ms).
Secondary: the largest of the 12 is below 100 ms.

**P3, post-4k is back to its single-arch value.** x86 post-4k, the two celeris columns, 0.9 pass: p99 at
or below 2 ms (0716 single-arch: 1.1-1.4 ms; the non-overlapping 0829 h1 columns: 1.7-1.8 ms; the
overlapping ones read 5.0 and 5.3 s), in both groups. axum is reported, not judged (0716: 2.4 ms). If the
`shared-light` passes fail where the `alone` ones hold, the light-row exemption is wrong and the default
policy must become `all`.

**P4, the overlapping-pass census, restated.** #442's title counts passes that overlap the other arch's run
of the same row. With the lock the same-row set is empty by construction, so "0 of N" for heavy passes reads
0 of 0 and is not evidence; P1 is the real statement, and the `shared-light` group is what is left to read.
Reported for completeness: the number of x86 and arm64 rated passes at or above 1 s, any row (informational;
arm64's were all celeris columns in 36441790519, a celeris follow-up that is on hold until this run says
whether the overlap was the trigger).

## A confounder that is not the overlap (#518)

Since about 2026-10-04 every amd64 server that can reach it sits on a 9.1-9.25 Gbit/s plateau on the three
bandwidth rows (post-4k, post-4k-h2, ws-large-echo), where earlier runs reached 13.6-18.4
([#518](https://github.com/goceleris/probatorium/issues/518); it is not the arch overlap: it shows in cells
with none). The rated targets are fractions of each cell's own saturation, so P2 and P3 stay readable, but
the 0716 and 0829 numbers they are compared with were taken at a different link state. Read each x86 cell's
saturation in Gbit/s (rps x payload x 8) next to its rated passes: if the saturation sits on the plateau,
say so in the verdict, and a backlog clock that remains is then not evidence about the overlap either way.

## Reading it

- P1 fails: stop. The exclusion does not hold (or the windows are wrong); the bug is in this change.
- P1 holds and P2 and P3 hold: the overlap was the cause of the x86 clocks on these rows. #442's first close
  criterion (zero overlapping x86 passes) holds by P1; its second (ws-large-echo and post-4k match 0716) holds
  for this scope, light-row sharing included. It does not cover get-simple-1024c, get-json-h2 and
  post-4k-h2, which the issue also names: they are not in this scope, and only a full both-arch run reads them.
- P1 holds and P2 or P3 fails: the overlap is not the whole story on that row (loadgen's pacer, #441,
  loadgen#102, or the client itself). #442 stays open for that row; report the passes.

## Owed, not run

Everything above; also an amd64 native timing of the runner and a real both-arch run of the full grid, whose
wall-clock the PR estimates at about 54 h (a model, README "A both-arch run").
