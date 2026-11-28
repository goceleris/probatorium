# Bare-metal idle cell (celeris#1485)

The idle-CPU and first-request cells of the celeris#1485 decision (epoll: sweep connections only when a deadline is due) on the
cluster hosts msr1 (arm64) and msa2-server (amd64), one workflow, `.github/workflows/idle-cell-baremetal.yml`, and
`tools/idlecell/`. The release bot's ruling is the design; this page says how to dispatch it, what each stage is and costs, what the
host has to offer, and where the ruling could not be followed literally.

Nothing here is dispatched by an agent: the main session dispatches. The workflow is `workflow_dispatch` from `main` only.

## Dispatch

```sh
# 1. the guard's refusal, shown (no build, no cluster job, nothing queued): run it while another cluster run is going
gh workflow run idle-cell-baremetal.yml --repo goceleris/probatorium --ref main -f mode=guard-probe -f host=msr1 -f reason="show the guard refuses"
# 2. the plumbing, about 5 minutes on the host (1 pass of ONE cell, one empty-server first-request pair), when the cluster is idle
gh workflow run idle-cell-baremetal.yml --repo goceleris/probatorium --ref main -f mode=dry-run -f host=msr1 -f reason="idle cell plumbing"
gh workflow run idle-cell-baremetal.yml --repo goceleris/probatorium --ref main -f mode=dry-run -f host=msa2-server -f reason="idle cell plumbing"
# 3. the cells, one host (about 2 h of wall time) or both in parallel (host jobs on distinct hosts; the same wall time)
gh workflow run idle-cell-baremetal.yml --repo goceleris/probatorium --ref main -f mode=run -f host=msr1 -f stages=a,b,c -f reason="#1485 bare-metal cells"
gh workflow run idle-cell-baremetal.yml --repo goceleris/probatorium --ref main -f mode=run -f host=msa2-server -f stages=a,b,c -f reason="#1485 bare-metal cells"
gh workflow run idle-cell-baremetal.yml --repo goceleris/probatorium --ref main -f mode=run -f host=both -f stages=a,b,c -f reason="#1485 bare-metal cells"
```

Two separate `run` dispatches (one per host) SERIALISE: the second one's guard refuses while the first holds the group (it counts as a holder), so dispatch the
second only when the first has ended, or use `host=both` (one dispatch, one group hold, the two host jobs in parallel on distinct hosts). A `dry-run` or `run` dispatched while another cluster
run is going (FWKXF2 included) is simply refused: the run is red at `pre-guard`, nothing is built, nothing is queued; `guard-probe` is the green demonstration of that refusal.
Each stage's artifact exists as soon as the stage ends, while the job goes on: `gh run download <run id> --repo goceleris/probatorium -n idlecell-raw-<host>-stage-a-<run id>`
(`TABLE-a.md` in it is the gate table with the decision rule).

`stages` takes `a,b,c` (default), `a,b`, `a`, `b,c`, `b`, `c` or `a,c`. The job ends cleanly after the last stage requested (the steps of the
others are skipped), so `stages=a,b` frees the cluster after the first-request cells. Other inputs: `celeris_main_sha` (empty: origin/main at
dispatch, recorded), `celeris_head_sha` (default `783d0019`), `celeris_old_sha` (default `ab0da663`), `passes` (3), `pairs` (10; `run` refuses
fewer than 3 and 10), `max_load` (1.0), `server_cpus` / `client_cpus` (override of the CPU sets), `reason`.

## Job graph, and why it cannot touch a run that is going

`plan -> pre-guard -> build -> cluster-guard -> cluster (bootstrap -> host -> teardown) -> collect -> verdict`

`matrix-tier-cluster` holds ONE running job and ONE pending job, and a job that requests the group while one is already pending CANCELS the
pending one (`cancel-in-progress: false` protects only the running job). So a second dispatch could disturb a running cluster workflow in exactly one way:
by creating a job that joins the group. This workflow has one such job, `cluster`, declared at JOB level (a workflow-level group would enter at
dispatch, before any guard looked), and:

- `cluster` needs `cluster-guard`, `build` and `pre-guard`, and its `if` requires `needs.cluster-guard.result == 'success'`, a verdict from its own
  attempt, and `mode != guard-probe`. A skipped job never enters a concurrency group.
- both guards run `tools/stresstally guard -idle`: it refuses while ANY run of a cluster workflow holds the group (`in_progress`) or waits in it
  (`queued`, `pending`, `waiting`, ...), this workflow included (it is in the guard's list of cluster-group workflows), and fails closed when it cannot
  tell. FWKXF2 is run 45969386818 of `benchmark-tier.yml`, `in_progress`: the guard refuses at once, in seconds, in `pre-guard`, before the build.
- `pre-guard` is first, before the build, so a dispatch made early by mistake costs seconds and builds nothing; `cluster-guard` looks again
  seconds before `cluster` is created (the build takes minutes), and it is the verdict `cluster` requires and the eviction audit reads.
- no other job declares a concurrency group, no job outside `cluster` runs on a self-hosted runner, and no job outside it holds a cluster secret.
- `mode=guard-probe` has no `cluster` job at all, and no build: `plan` and `pre-guard` only, whose verdict is printed and written to the step
  summary (REFUSED with the runs named, or IDLE) and the run ends green either way. Run it while FWKXF2 is going to see the refusal without risk.

What remains open, and is said, not hidden: the seconds between `cluster-guard` and `cluster` entering the group. A cluster workflow dispatched in that window
is not seen. If it holds the group, `cluster` waits behind it as the pending job (it never cancels a running job); if one was already pending, `cluster` cancels
it. The bootstrap's eviction audit (`tools/stresstally/eviction-audit.sh`) names any cluster run cancelled in the window.

Tests: `workflow_idle_cell_test.go` (the graph as text, each of the conditions above, with mutants that turn it red; the `plan` script run for real
with a fake `gh`), `tools/stresstally/idle_cell_guard_test.go` (the guard against the benchmark tier of FWKXF2, running, queued, finished), and the existing
`TestStressClusterGuardKnowsEveryClusterWorkflow`, which keeps the guard's list equal to the workflows that name the group.

## The host job

`runs-on: [self-hosted, celeris-cluster, <host>]`: the runner IS the host being measured. While the job runs the host runs: the runner agent
(Runner.Listener and Runner.Worker), the tool process under test, and the holder of the idle connections; nothing else of this repository. The runner exists
only between this run's bootstrap and teardown (runners are ephemeral and de-registered), every other cluster workflow holds the same group, so no host job of another
workflow can overlap it. The bootstrap registers a runner on every cluster host, as for the tiers (the idle ones are not measured). The host script records
what else it sees (`ps` by CPU, `vmstat`, the timers, the load) before the first cell, and refuses to start if load1 stays above `max_load` for 180 s.

It changes nothing on the host: no sudo, no package, no kernel parameter, no governor, no idle state, no IRQ, no file under `/sys` or `/proc` written
(a test scans the scripts). It reads, pins its processes with `taskset`, and records what is missing. The governor, the cpuidle states, the kernel, the load
average, the CPU model and frequency are recorded as FOUND, before and after (`facts/state-before.txt`, `state-after.txt`, `state-diff.txt`).

Per cell: the server (`idlehold-server-<arm>`, epoll or io_uring, `Workers` unset) pinned to four CPUs on four distinct physical cores; the holder of the idle
keep-alive connections (`idleclient`) on other cores of the SAME host (no container: `taskset`); 12 s settle after the last connection is counted, then a 20 s
window; load1 must be at or below `max_load` (60 s of waiting, then the cell runs anyway and the INDEX says `forced=1`). Recorded per cell: process CPU
(utime+stime), loop-thread CPU and wake-ups per loop (voluntary context switches of the threads pinned to one CPU), run-queue time and timeslices, RSS,
`perf stat -e instructions,cycles` of the server process in user and kernel (`:u`, `:k`) when perf counts, package energy, load average, the SHUTDOWN line.
An idle baseline (the same window with nothing served, `engine none`) is measured at the start of every pass and once more at the end; the energy in the table is
above that baseline.

### What the host must offer (and what is recorded when it does not)

| need | if missing |
|---|---|
| `taskset`, `timeout`, `ss`, `pgrep`, `vmstat` (util-linux, coreutils, iproute2, procps) | the job aborts with the missing name |
| at least 6 CPUs allowed to the job (4 for the server, 2 for the holder) | aborts |
| open-files limit (soft raised to the hard) of at least 120,000 | the 50,000-connection cells are skipped, INDEX says `SKIP-nofile` |
| `kernel.io_uring_disabled=0` | io_uring cells are skipped (`SKIP-iouring`) |
| `perf` (not installed by us) and `perf_event_paranoid` low enough | user-only (`instructions:u`, `cycles:u`) if kernel counting is refused; otherwise "none" in the table |
| RAPL powercap `energy_uj` readable (usually root-only on recent kernels), or `perf stat -a -e power/energy-pkg/` allowed | other readable hwmon `energy*_input` / `power*_input`; otherwise "none" |
| `python3` | the stage's table is not rendered on the host; the collect job renders every table from the artifacts |

## Stages, and the expected minutes per host

Per host, one host job (the two hosts run in parallel with `host=both`). Minutes are estimates from the cell structure (a cell is 12 s settle + 20 s window + about 3 s of start,
hold and shutdown) and from the plumbing test in a container; the dry-run measures them on the real host (`wall_s` per cell in the INDEX, a total in the report).

| stage | what | runs | expected | uploaded when it ends |
|---|---|---|---|---|
| setup | build (hosted, in parallel with nothing on the cluster), bootstrap of the runners, facts and the quiet check | | build 5, bootstrap 5 to 15 (a cold runner cache: 25), facts 1 | `idlecell-raw-<host>-facts-<run>` |
| a | the #1485 gate: epoll, main / head / head-aw / main-skip at 100, 1,000, 10,000, 50,000 idle keep-alive connections, 3 passes (pass 2 reversed: N descending, the later builds first), an idle baseline at the start of each pass and at the end | 3 x 16 cells + 4 baselines | about 31 | `idlecell-raw-<host>-stage-a-<run>`: `a/` (INDEX, raw logs), `TABLE-a.md` (the gate table with the decision rule) |
| b | the paired first-request cells, 2 s gap, ABBA pairs, 10 pairs: b1 empty server, the OLD 4 ms build (ab0da663) against main, for epoll, io_uring and adaptive (60 runs); b2 1,000 connections held, main against head, epoll, for #1485 (20 runs) | 80 runs | about 33 (b1) + 18 (b2) = 51 | `idlecell-raw-<host>-stage-b-<run>`: `b1/`, `b2/`, `TABLE-b.md` |
| c | io_uring, main against head, 10,000 and 50,000 connections, 3 passes (pass 2 reversed), for the record | 12 cells + 2 baselines | about 10 | `idlecell-raw-<host>-stage-c-<run>` |
| end | the state as found against the state before, the verdict on the cells, teardown | | teardown 3 to 6 | `-final-`, then the report |

Host job: about 95 minutes for `a,b,c` (ceiling 230: the job's `timeout-minutes` is 20 + 80 for a + 100 for b + 30 for c, per stages requested); the whole run about 2 hours. `stages=a`:
about 35 minutes on the host. Dry-run: about 5 minutes on the host (ceiling 40), about 25 minutes whole.

## The reader

`tools/idlecell/reader.py` (standard library; `python3 -I`) turns the raw logs into the tables, on the host when a stage ends (`TABLE-<stage>.md`, also in the step summary)
and in the `collect` job for the whole report (`REPORT.md` per host: host as found, every stage, data quality, the failed cells, and `PROVENANCE.txt`: the binaries' sha256 and what they are
made of). Gate table, per N: process CPU for main / head / head-aw / main-skip with the per-pass values and the ranges, the head - main difference by pass, wake-ups per
second per loop, loop-thread CPU, microseconds per wake, instructions and cycles per second (user and kernel), package energy in watts above the idle baseline; the paired
first-request differences with n, median, spread and the sign test; and the bot's decision rule under the table:

> head at or below main at every count (CPU, or instructions plus energy) => **no loss**; head's CPU higher but instructions and energy not higher => **accounting effect**;
> instructions or energy higher on head => **real loss**; loss reproduced at 100 to 1,000 => **no merge: the bot takes the table to Albert**.

"Higher" at a count means: the median over the passes is above main's by more than a tolerance (CPU 0.05 point, one clock tick over the window; instructions 1 %; energy 0.05 W)
AND head is above main in more than half of the same-pass pairs. A figure the host could not measure is said so (`UNDECIDED`), never read as equal. The reader reproduces the
figures of the laptop-VM measurement (`SUMMARY.md` of m1485, sections 2, 4 and 6) from its raw data: `tools/idlecell/reader_test.py` with the fixtures in `tools/idlecell/testdata`.

## Where the ruling could not be followed literally

- **The isolation builds** (head-aw, main-skip) are rebuilt from minimal patches (`tools/idlecell/patch/`) on the same base: one line each, without the counter and diagnostic
  instrumentation the laptop's builds carried. They test the same two hypotheses; their figures are not byte-comparable with the laptop's.
- **The first-request cell on an empty server** uses `idlecost` as the existing paired design does (`curve-epoll/run.sh`): the client is in the same process, on the server's four CPUs; the run is
  8 requests on new connections after a 2 s gap, with a 10 s idle window in front (not the gate's 20 s). The 1,000-held cell uses a separate holder process on other cores (`firstreq-epoll.sh`).
- **No main-vs-main twin pairs** (the noise calibration of m1485 section 4) are in the ruling and are not in the stages; they would add about 18 minutes to stage b if the bot wants them.
- **Energy** may be "none": the kernel gives RAPL `energy_uj` to root only on recent kernels, and the ruling forbids changing the host (no sysctl, no chmod, no sudo). The same for perf's
  kernel-side counting under a high `perf_event_paranoid`. The dry-run's facts say which on each host before the full run; msr1's board exposure (hwmon, powercap) is whatever it is.
- **The CPU sets** are chosen by the script: the server on 4 distinct physical cores (a core is the set of CPUs sharing `core_cpus_list`), the highest `cpu_capacity` first, so msr1's
  big.LITTLE SoC puts the server on its big cores; the holder on other cores. celeris plans its loops from the CPUs the process may use, so the taskset mask places them; each run's IDLE line
  records `loop_cpus`. `server_cpus` / `client_cpus` override. perf on a heterogeneous host prints one line per core type: the reader sums them.
- **Likely outcome to expect**: if both unprivileged perf kernel-side counting and RAPL are denied on a host (RAPL's `energy_uj` is root-only on recent kernels; `perf_event_paranoid` is 2 or higher on
  stock Ubuntu), the table has CPU, wake-ups and (user-only) instructions at best, and a head CPU that is higher at a count reads `UNDECIDED` or `ACCOUNTING EFFECT` on user instructions alone, never
  `real loss` on kernel work or energy it could not see. The dry-run's facts show which it is on each host, before the full run; nothing may be changed on the host to improve it.
- **The load threshold** is 1.0 on a bare-metal host (4 on the laptop): a host above it waits, then runs `forced` (recorded per cell), and the job refuses to start above it.
- **The bootstrap and teardown are the house ones** (cluster-runner-up / cluster-runner-down): they run Ansible against all three hosts before and after the job (runner install in the runner's directory,
  cleanup, forensics), never during the cells. They install no system package.
