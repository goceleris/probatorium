# Stress-testing celeris on GitHub-hosted runners

`.github/workflows/celeris-stress.yml` runs celeris tests many times, in
parallel, on GitHub-hosted runners: x86 (`ubuntu-24.04`) and arm64
(`ubuntu-24.04-arm`), the same Ubuntu release on both. celeris is public, so
both cost nothing. Use it to measure a flake and its rate in minutes instead
of hours on a laptop. A local timing is not the runner's timing: for a CI
flake, the runner is the truth.

It never touches the benchmark cluster: no self-hosted label, no share of the
`matrix-tier-cluster` concurrency group.

## What a run does

1. **plan** refuses a dispatch on the default branch (see below), checks every
   input against an allow-list (`tools/stresstally plan`) and resolves
   `celeris_ref` to one commit, so every shard tests the same code even if the
   branch moves during the run.
2. **shard**, one job per shard per arch, checks out celeris at that commit,
   sets `RLIMIT_MEMLOCK` with `sudo prlimit` on its shell (as celeris
   `ci.yml` does), and runs
   `go test -v -count=<count> -shuffle=<seed> -timeout=<timeout> -run=<run> [-race] <extra flags> <packages>`.
   The log header records the machine, kernel, `nproc`, runner image, the
   memlock the runner started with and the one actually in force, the commit,
   the job limit and the exact command. A shard that finds itself in another
   shape (memlock not applied, another commit, another arch) refuses to run.
   The raw log is uploaded as `stress-log-<case>-<arch>-<shard>`.
3. **summary** downloads every log and tallies it (`tools/stresstally summarize`).
   The job summary and the `stress-summary` artifact (`summary.md`,
   `report.json`, `tests.tsv`) hold:
   - per test (subtests included) and arch: the processes in which it failed
     and in which it ran, the per-process fail rate with an exact
     (Clopper-Pearson) 95% interval, and the PASS, FAIL, SKIP and no-verdict
     iteration counts;
   - every FAIL with its first failure lines, the shard and its `-shuffle` seed;
   - every shard's status;
   - any test that ran on one arch and never on the other, and any kernel or
     OS image that differs between the arches.

The summary job is red unless the run PASSed: on every arch at least one test
passed, no test failed, every shard is `complete`, and every test that
reached a verdict on one arch reached one on the other.

### Processes, not iterations

A **process** is one test binary: one package in one shard. go test runs a
package's `-count` iterations one after another inside that one process, so
they share its state: a leaked goroutine or descriptor, a package-level
variable, and the `RLIMIT_MEMLOCK` budget. That last one is measured in
exactly the package this workflow is built for: an io_uring ring's locked
memory is given back asynchronously, 12 to 23 ms after the ring is closed, so
a test that starts engines back to back can leave the next test without
budget (celeris#662 r6 gate2, T2). Iterations are therefore not independent
trials, and a rate over them with an interval would be too narrow.

Processes are independent: each shard is its own job on its own runner. So
the rate the summary puts an interval on is **per process**: processes in
which the test failed at
least once, over processes in which it reached a PASS or FAIL. The iteration
counts are reported beside it, with no interval. To get more processes, add
shards (up to 20 per arch); `count` makes each process longer, and the
per-process rate then means "fails at least once in `count` iterations".

### How the tally counts

Only `--- PASS: `, `--- FAIL: ` and `--- SKIP: ` lines count, subtests
included. A SKIP is its own column: it is never a pass, and a process in
which a test only skipped is not a process that ran it. A run in which
nothing but skips happened on an arch does not PASS (`nothing-ran`),
whatever the other arch did.

**Both arches.** A test that reached a PASS or FAIL on one arch and never on
the other fails the case (`arch-gap`): the run has no evidence for it on that
arch. The summary names each such test. It also warns, without failing the
case, when the kernel or the Ubuntu release differs between the arches, or
when one arch's shards ran on more than one kernel or image: a difference
between the arches may then be the environment's. (The two arches' runner
images are built separately, so their version numbers always differ; only
the release, `ubuntu24`, is compared.)

A shard is one of:

| status | meaning | counted? |
|---|---|---|
| `complete` | header, go test output closed by a package result line, trailer; every test that started has a verdict | yes |
| `UNPARSED` | no verdict lines at all, a panic, a go test timeout, a fatal error, a build failure, no trailer (the log was cut short), a test that started and never reported, a verdict glued onto a test's own output, or an exit status the verdicts do not explain | the verdicts it has, yes; the case fails |
| `MISSING` | the plan expected the shard and no log arrived | no; the case fails |
| `WRONG-SHAPE` | the shard ran with another memlock, commit, arch or configuration than planned (or refused to run) | no; the case fails |

None of the last three is ever a pass. `go test -count` does not repeat
examples; an `Example` runs once per shard.

### Seeds

Shard `n` of run `R` uses `-shuffle=R*100+n`. Every shard tests a different
order; the same shard number on x86 and arm64 tests the same order, so an
arch difference is not an order difference. To replay an order, pass the seed
from the summary: `extra=-shuffle=<seed>`.

## Inputs

| input | allowed | default |
|---|---|---|
| `celeris_ref` | branch, tag, full 40-hex commit sha, or `refs/pull/N/head` | `main` |
| `packages` | up to 16 patterns under the celeris module: `.` (the root package), `./dir`, `./dir/...`, `./...` | `./engine/iouring` |
| `run` | a Go regexp, printable ASCII without spaces, up to 2048 characters; empty runs every test | empty |
| `count` | 1 to 1000: `-count`, the iterations inside each process | 10 |
| `shards` | 1 to 20 per arch: one job, and one process per package, each | 5 |
| `arches` | `both`, `x86`, `arm64` | `both` |
| `memlock` | `8m` (celeris CI's unit-job shape), `128m`, `unlimited` | `8m` |
| `race` | `true`, `false` | `false` |
| `timeout` | a go test `-timeout` from `1s` to `5h30m`; see below | `30m` |
| `extra` | space-separated, each token one of: `-short`, `-failfast`, `-cpu=N[,N]`, `-parallel=N`, `-skip=REGEXP`, `-tags=LIST`, `-shuffle=off\|N`, or `NAME=VALUE` for a variable celeris's tests read (below) | empty |

Anything else is refused by name before any shard starts. `-run`, `-count`,
`-timeout`, `-race` and `-v` have their own inputs (or are always set) and are
refused in `extra`.

**timeout.** go test applies `-timeout` to each test binary on its own, one
binary per package, and runs binaries side by side up to `-p` (the runner's
CPU count). So a shard of `n` packages can run up to `n` timeouts end to
end. The job limit is `n` timeouts plus 20 minutes (checkout, toolchain,
compiling with `-race`), capped at the 360 minutes a hosted job may run; with
a `...` pattern the number of packages is not known in advance and the limit
is 360. A binary that overruns its timeout prints go test's goroutine dump and
its shard is `UNPARSED` (`timeout`).

**Environment settings.** `NAME=VALUE` is accepted for the variables celeris's
own tests read, and nothing else (`celerisTestEnv` in
`tools/stresstally/plan.go`, built from
`git grep -nE 'os\.(Getenv|LookupEnv)\(' -- '*_test.go'` at celeris
`9f4d89b`, with every name passed through a constant or a helper resolved):

- `CELERIS_*`, by prefix: celeris's own namespace, where a branch under test
  adds its knobs before `main` has them. `CELERIS_REQUIRE_IOURING_WORKERS=1`
  and `CELERIS_REQUIRE_UPSWITCH=1` turn an environment skip into a failure,
  the way celeris CI runs its skipping-forbidden steps.
- By name: `GOTEST_BACKPRESSURE` (`=1` runs
  `TestWriteBufBackpressureClosesSlowConsumer` in `./engine/epoll`, which
  otherwise skips as "non-deterministic on CI", so CI never runs it),
  `TESTING_STRICT_ALLOC_BUDGETS`, `DRAIN583_REPS`, the WebSocket rig knobs
  `WS482_*`, `WS484_*` and `WS583_*`, `SOAK_DURATION`, `SOAK_CLIENTS`,
  `CHAOS_CONC`, `CHAOS_DURATION`, `CHAOS_P99_CEILING_MS`, `DEBUG_TOKEN` and
  `PPROF_TOKEN`.
- Never, whatever the lists say: `PATH` and `LD_*`, which choose the binaries
  and libraries that run, and the runner's own `GITHUB_*`, `RUNNER_*` and
  `ACTIONS_*`.

A knob outside `CELERIS_*` that a later celeris adds must be added to the
list, with the grep that found it.

## Sharing the organization's runners

goceleris is on GitHub's Free plan: at most 20 GitHub-hosted jobs run at once
across the whole organization, and celeris and probatorium CI draw on the same
20. Without a cap, one dispatch of 20 shards on both arches would ask for 40
at once and hold every slot until its shards finished, so required CI of any
pull request would wait behind it.

The shard matrix is capped at `max-parallel: 4`. The cap is sized for the
largest use this document describes, a base-vs-branch pair (two runs side by
side): the pair holds at most 8 slots (a run's `plan` and `summary` never
overlap its own shards), which leaves at least 12. One celeris CI push is 8
Linux jobs (`ci.yml` at `9f4d89b`: lint, unit, adaptive, iouring,
conformance, driver-conformance, build, vulncheck), so it still starts in full
while a pair runs. A probatorium pull request's CI is about 22 jobs and queues
on its own; the cap keeps a pair from taking more than 8 of its slots.

The price is time: 40 shard jobs run in 10 waves of 4. Do not dispatch more
than one pair at a time; the cap is per run, and GitHub has no cap across
runs.

## Where to dispatch from

A shard runs the celeris code under test, which may be anyone's pull request,
and code a job runs holds that job's runtime token. Two walls keep that code
away from what other workflows trust:

1. **`cache-mode: none`**, on the workflow and again on the shard job. The
   token of every job in the run has no Actions cache access at all, neither
   restore nor save; GitHub enforces it with scoped cache tokens, so it holds
   against the code under test itself, not only against actions that honour
   it. Every job's log shows `Cache mode: none` under "Set up job". No job
   needs a cache: `setup-go` runs with `cache: false` and nothing uses
   `actions/cache`. That also matters within the workflow: a shard that
   restored what an earlier run's code under test had saved could have its
   test binary, and so its verdicts, forged.
2. **Not from `main`.** `plan` refuses a dispatch on the default branch, and
   the shard job carries the same rule. This is defence in depth: if
   `cache-mode` were ever dropped from the file, a run on another branch could
   still reach only that branch's cache scope, never the one every workflow
   here restores from (the cluster tiers included); and no run on `main` is
   ever the code under test's.

CodeQL's `actions/cache-poisoning/poisonable-step` still reports the shard
job. Its model (CodeQL 2.27.1, and `github/codeql` main as of 2026-09-26)
does not read `cache-mode`, treats every `workflow_dispatch` run as a run on
the default branch, and honours no `if:`, environment or other check for that
event (`ControlChecks.qll`). So neither wall above is visible to it, and it
cannot be satisfied by anything short of dropping `workflow_dispatch` or
hiding the checkout from it. By its source, a checkout whose ref is a field
named like `*sha*`, `*head*` or `*commit*` counts as untrusted
(`UntrustedCheckoutQuery.qll`); renaming `celeris_sha` might silence the
alert and would change nothing, which is why it is not done.

Keep a standing branch for it. Refresh it from `main` when the workflow changes
(the run uses the branch's copy of the workflow and of `tools/stresstally`):

```sh
gh api repos/goceleris/probatorium/git/refs -f ref=refs/heads/stress/runs \
  -f sha="$(gh api repos/goceleris/probatorium/commits/main --jq .sha)"      # once
gh api -X PATCH repos/goceleris/probatorium/git/refs/heads/stress/runs \
  -f sha="$(gh api repos/goceleris/probatorium/commits/main --jq .sha)" -F force=true   # refresh
```

Do not re-run failed shard jobs to "fix" a run: retrying only the shards that
failed selects for passes and biases the rate. Dispatch a new run instead.

## Examples

A dispatch needs the workflow on the default branch, and runs from the branch
named by `--ref`. Every command names the repository explicitly. Every
`gh workflow run` below is checked against the plan's validation by
`TestDocsExamplesAreValidDispatches`.

### 1. Base vs branch: does celeris#696 take TestDriverHTTPZeroOverhead's failure off the runners?

What is known. `TestDriverHTTPZeroOverhead` (`./engine/iouring`) fails
because of celeris#691, a defect on `main`: `UnregisterConn` only queues a
cancel keyed by the descriptor number, the drivers close the descriptor right
after, and a RECV already armed stays armed on a socket the process no longer
has, so `onClose` never fires. A deterministic reproduction (R1, R3) fails
5 of 5 on `main` (`9f4d89b`) and on the #674 branch alike, and the natural
test failed on `main` with no other test running
(`evidence/celeris-662/r6/gate2/90-GATE2.md`, T1). celeris#696 is the proposed
fix, with R3 as its regression test.

The open question. How often the natural test fails on the CI runners, per
process, on `main` and with #696. The deterministic test answers whether the
fix works; this pair answers what the runners see.

The arms:

- **base**: the commit #696 branches from, as a full sha; **branch**: #696's
  head, as a full sha. A branch name could move between the two dispatches.
- **Everything else identical**, down to the probatorium commit: both
  dispatched from the same `stress/runs`, back to back. `compare` refuses a
  pair that differs in any input but the commit; the per-process rate grows
  with `count`, so even that must match.
- **One test**, by `-run`, that exists on both commits (the defect needs no
  other test). A test only one arm has shows as "not in this arm".
- **CI's shape** for this package: `-race` at the runner's 8 MiB memlock, as
  celeris CI's unit job runs `./engine/iouring`.
- **20 shards per arch**, the most there is: 20 processes per arch per arm.

```sh
branch=$(gh api repos/goceleris/celeris/pulls/696 --jq .head.sha)
base=$(gh api "repos/goceleris/celeris/compare/main...$branch" --jq .merge_base_commit.sha)
for ref in "$base" "$branch"; do
  gh workflow run celeris-stress.yml --repo goceleris/probatorium --ref stress/runs \
    -f celeris_ref="$ref" -f packages=./engine/iouring -f run='^TestDriverHTTPZeroOverhead$' \
    -f count=20 -f shards=20 -f arches=both -f memlock=8m -f race=true -f timeout=10m
done
```

Both runs must PASS or FAIL for the right reason before they are compared:
every shard `complete`, no `arch-gap`. Then, with the two `stress-summary`
artifacts downloaded:

```sh
gh run download <base-run-id> --repo goceleris/probatorium -n stress-summary -D base
gh run download <branch-run-id> --repo goceleris/probatorium -n stress-summary -D branch
go run ./tools/stresstally compare base branch
```

`compare` prints, per test and arch, the base's and the branch's failed / ran
processes and Fisher's exact test (two-sided) on those counts, and a `NOTE`
for anything that qualifies the pair: a shard that did not complete, or a
kernel or image that differs between the arms.

What the pair can and cannot say, at 20 processes per arch per arm:

- **The fix takes the failure off the runners** only if the base fails in at
  least 5 of 20 processes on an arch and the branch in none: 5 of 20 against
  0 of 20 is p = 0.047, 4 of 20 against 0 of 20 is p = 0.106.
- If the base fails in fewer, the pair cannot tell the arms apart at this
  size, whatever the branch does. 0 failing processes of 20 bounds the
  per-process rate at 16.8% (exact, 95%). The evidence that the fix works is
  then #696's deterministic test, not this pair.
- An A/A pair (the base dispatched twice) shows how far two runs of the same
  commit drift; `compare` labels it.

### 2. The rate of one flaky test under -race

```sh
gh workflow run celeris-stress.yml --repo goceleris/probatorium --ref stress/runs \
  -f celeris_ref=main -f packages=./internal/wakefd -f run='^TestConcurrentSetAndSignal$' \
  -f count=100 -f shards=20 -f arches=both -f memlock=8m -f race=true -f timeout=20m
```

20 processes per arch, 100 iterations each. No failing process in 20 bounds
the per-process rate at 16.8% (exact, 95%). The 2000 iterations would bound a
per-iteration rate at about 0.18% (the exact 95% upper bound for 0 in 2000 is
0.184%) only if the 100 iterations inside one process were independent
trials, which the summary does not assume: it reports them as counts.

### 3. Skipping forbidden, memlock raised

The celeris#656 init-failure tests need two io_uring workers; at 8 MiB they
skip. Raise memlock and forbid the skip:

```sh
gh workflow run celeris-stress.yml --repo goceleris/probatorium --ref stress/runs \
  -f celeris_ref=main -f packages=./engine/iouring \
  -f run='^TestListenCloses(ListenSocketsWhenEveryWorkerRingSetupFails|ListenSocketWhenOneWorkerRingSetupFails|ListenSocketRingAndEventfdWhenInitialSubmitFails)$' \
  -f count=50 -f shards=4 -f arches=both -f memlock=unlimited -f race=true -f timeout=20m \
  -f extra='CELERIS_REQUIRE_IOURING_WORKERS=1'
```

### 4. A test CI never runs

`TestWriteBufBackpressureClosesSlowConsumer` skips unless
`GOTEST_BACKPRESSURE=1` ("loopback TCP auto-tuning makes this
non-deterministic on CI"), so no CI job runs it. Its rate on the runners:

```sh
gh workflow run celeris-stress.yml --repo goceleris/probatorium --ref stress/runs \
  -f celeris_ref=main -f packages=./engine/epoll -f run='^TestWriteBufBackpressureClosesSlowConsumer$' \
  -f count=5 -f shards=20 -f arches=both -f memlock=8m -f race=false -f timeout=10m \
  -f extra='GOTEST_BACKPRESSURE=1'
```

### Finding the result

```sh
gh run list --repo goceleris/probatorium --workflow celeris-stress.yml --limit 5
gh run view <run-id> --repo goceleris/probatorium            # jobs; the summary job's page has the tables
gh run download <run-id> --repo goceleris/probatorium -n stress-summary -D stress-<run-id>
gh run download <run-id> --repo goceleris/probatorium -p 'stress-log-*' -D logs-<run-id>
```

`tools/stresstally tally <dir>` judges a directory of downloaded shard logs
locally (`go run ./tools/stresstally tally logs-<run-id>`).

## The self-test

A pull request that changes the workflow or `tools/stresstally/` runs the
workflow's self-test instead of a dispatch: five fixed cases, each with a fixed
expected outcome, on both arches, against a pinned celeris commit
(`selfTests` in `tools/stresstally/plan.go`). Each case is written as the
dispatch inputs a person would type (`IN_*`, exactly as the workflow passes
them) and planned by the dispatch code (`inputsFrom`, `planDispatch`,
`Entries`), after the same default-branch refusal a dispatch meets:

| case | what runs | expected |
|---|---|---|
| `good` | pure tests with subtests, `-race`, 3 runs x 2 shards per arch, one test skipping under `-short` | PASS; each test PASS exactly 6 times in 2 processes per arch, 0 failing; the skipping test SKIP 6, PASS 0, in no process |
| `fail` | the three celeris#656 tests at 8 MiB with `CELERIS_REQUIRE_IOURING_WORKERS=1`, 2 runs in one process | FAIL (`test-failures` only); each test FAIL twice per arch, in 1 failing process of 1 |
| `skip` | the same tests at 8 MiB without it | FAIL (`nothing-ran` only); each test SKIP once per arch |
| `timeout` | a test that needs 1.3 s under a 1 s go test timeout | FAIL; every shard `UNPARSED` (`timeout`); the test has one run without a verdict |
| `none` | a `-run` that matches nothing (go test exits 0) | FAIL; every shard `UNPARSED` (`no-verdicts`) |

The run is green only when every case comes out exactly as expected, so a
change that let a failure, a skip, a timeout or an empty run pass as a PASS
turns it red.

## First dispatch after a change to the workflow

The self-test runs the dispatch path's planning, but not a `workflow_dispatch`
event itself, which only exists once the workflow is on `main`. After a merge
that changes the workflow, refresh `stress/runs` and run these two:

```sh
# 1. The default-branch refusal, on the runner. Nothing of celeris runs.
gh workflow run celeris-stress.yml --repo goceleris/probatorium --ref main \
  -f celeris_ref=9f4d89b171db7838dbcc3ece2107191bc15b25f8 -f packages=./engine/iouring \
  -f run='^(TestSockaddrString|TestParseSendZCResult|TestUseSendZC|TestAbandonedResponseCountsAsSendPeerGone)$' \
  -f count=3 -f shards=2 -f arches=both -f memlock=8m -f race=true -f timeout=5m -f extra=-short
# 2. The self-test's good case as a real dispatch.
gh workflow run celeris-stress.yml --repo goceleris/probatorium --ref stress/runs \
  -f celeris_ref=9f4d89b171db7838dbcc3ece2107191bc15b25f8 -f packages=./engine/iouring \
  -f run='^(TestSockaddrString|TestParseSendZCResult|TestUseSendZC|TestAbandonedResponseCountsAsSendPeerGone)$' \
  -f count=3 -f shards=2 -f arches=both -f memlock=8m -f race=true -f timeout=5m -f extra=-short
```

Expected:

1. The run on `main` is red at `plan`, which prints
   `dispatch this workflow from a branch other than main`; no shard job
   starts and `summary` is skipped.
2. The run on `stress/runs` is green. `plan` prints `event workflow_dispatch`,
   1 case and 4 shard jobs with a 25-minute job limit; all four shards are
   `complete` on `ubuntu-24.04` and `ubuntu-24.04-arm`; the summary says
   `1 of 1 case(s) as expected` and case `stress` PASS; per arch,
   `TestSockaddrString`, `TestSockaddrString/ipv4`,
   `TestSockaddrString/ipv6-loopback`, `TestParseSendZCResult`,
   `TestUseSendZC` and `TestUseSendZC/large-linked` PASS 6 times with
   `0 / 2` processes failed, and `TestAbandonedResponseCountsAsSendPeerGone`
   SKIP 6, PASS 0, `0 / 0`; no arch gap; every job's "Set up job" shows
   `Cache mode: none`. A kernel warning, if the two arches got different
   kernels that day, is information, not a failure.
