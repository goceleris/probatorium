# Stress-testing celeris on GitHub-hosted runners

`.github/workflows/celeris-stress.yml` runs celeris tests many times, in
parallel, on GitHub-hosted runners: x86 (`ubuntu-latest`, the image celeris CI
uses) and arm64 (`ubuntu-24.04-arm`). celeris is public, so both cost nothing.
Use it to measure a flake and its rate in minutes instead of hours on a
laptop. A local timing is not the runner's timing: for a CI flake, the runner
is the truth.

It never touches the benchmark cluster: no self-hosted label, no share of the
`matrix-tier-cluster` concurrency group.

## What a run does

1. **plan** checks every input against an allow-list
   (`tools/stresstally plan`) and resolves `celeris_ref` to one commit, so every
   shard tests the same code even if the branch moves during the run.
2. **shard**, one job per shard per arch, checks out celeris at that commit,
   sets `RLIMIT_MEMLOCK` with `sudo prlimit` on its shell (as celeris
   `ci.yml` does), and runs
   `go test -v -count=<count> -shuffle=<seed> -timeout=<timeout> -run=<run> [-race] <extra flags> <packages>`.
   The log header records the machine, kernel, `nproc`, runner image, the
   memlock actually in force, the commit and the exact command. A shard that
   finds itself in another shape (memlock not applied, another commit, another
   arch) refuses to run. The raw log is uploaded as `stress-log-<case>-<arch>-<shard>`.
3. **summary** downloads every log and tallies it (`tools/stresstally summarize`).
   The job summary and the `stress-summary` artifact (`summary.md`,
   `report.json`, `tests.tsv`) hold:
   - per test (subtests included) and arch: PASS, FAIL, SKIP, runs with no
     verdict, and the fail rate `fail / (pass + fail)` with an exact
     (Clopper-Pearson) 95% interval;
   - every FAIL with its first failure lines, the shard and its `-shuffle` seed;
   - every shard's status.

The summary job is red unless the run PASSed: at least one test passed, no
test failed, and every shard is `complete`.

### How the tally counts

Only `--- PASS: `, `--- FAIL: ` and `--- SKIP: ` lines count, subtests
included. A SKIP is its own column: it is never a pass and never in the
denominator of a rate. A run in which nothing but skips happened does not
PASS (`nothing-ran`).

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
| `packages` | up to 16 patterns under the celeris module: `./dir`, `./dir/...`, `./...` | `./engine/iouring` |
| `run` | a Go regexp, printable ASCII without spaces, up to 2048 characters; empty runs every test | empty |
| `count` | 1 to 1000 (`-count` per shard) | 10 |
| `shards` | 1 to 20 per arch | 5 |
| `arches` | `both`, `x86`, `arm64` | `both` |
| `memlock` | `8m` (celeris CI's unit-job shape), `128m`, `unlimited` | `8m` |
| `race` | `true`, `false` | `false` |
| `timeout` | a go test `-timeout` from `1s` to `5h30m`; the job limit is this plus 20 minutes | `30m` |
| `extra` | space-separated, each token one of: `-short`, `-failfast`, `-cpu=N[,N]`, `-parallel=N`, `-skip=REGEXP`, `-tags=LIST`, `-shuffle=off\|N`, `CELERIS_<NAME>=VALUE`, or an issue-numbered test knob such as `WS484_CONNS=16` | empty |

Anything else is refused by name before any shard starts. `-run`, `-count`,
`-timeout`, `-race` and `-v` have their own inputs (or are always set) and are
refused in `extra`. The environment settings exist for celeris's own test
knobs; `CELERIS_REQUIRE_IOURING_WORKERS=1` and `CELERIS_REQUIRE_UPSWITCH=1`
turn an environment skip into a failure, the way celeris CI runs its
skipping-forbidden steps.

## Examples

A dispatch needs the workflow on the default branch. Every command names the
repository explicitly.

### 1. celeris#674: TestDriverHTTPZeroOverhead, full package, base vs branch

The failure shows only when the whole `./engine/iouring` package runs at the
8 MiB memlock of celeris CI's unit job (alone it passes), so run the full
package, not `-run`, and dispatch the same configuration twice, once per
commit:

```sh
for ref in 9f4d89b171db7838dbcc3ece2107191bc15b25f8 <branch-sha>; do
  gh workflow run celeris-stress.yml --repo goceleris/probatorium \
    -f celeris_ref="$ref" -f packages=./engine/iouring -f run= \
    -f count=3 -f shards=10 -f arches=both -f memlock=8m -f race=false -f timeout=45m
done
```

That is 30 full-package runs per arch per commit (one full-package run took
about two minutes in a local arm64 container; the shard table's `elapsed`
column has the runner's figure). Compare the two summaries' rows for
`TestDriverHTTPZeroOverhead`: the fail counts and their 95% intervals, per arch.
With the two `stress-summary` artifacts downloaded:

```sh
gh run download <base-run-id> --repo goceleris/probatorium -n stress-summary -D base
gh run download <branch-run-id> --repo goceleris/probatorium -n stress-summary -D branch
awk -F'\t' 'FNR == 1 || $3 == "TestDriverHTTPZeroOverhead"' base/tests.tsv branch/tests.tsv
```

### 2. The rate of one flaky test under -race

```sh
gh workflow run celeris-stress.yml --repo goceleris/probatorium \
  -f celeris_ref=main -f packages=./internal/wakefd -f run='^TestConcurrentSetAndSignal$' \
  -f count=200 -f shards=10 -f arches=both -f memlock=8m -f race=true -f timeout=20m
```

2000 runs per arch. Zero failures in 2000 bounds the rate below 0.18% (95%).

### 3. Skipping forbidden, memlock raised

The celeris#656 init-failure tests need two io_uring workers; at 8 MiB they
skip. Raise memlock and forbid the skip:

```sh
gh workflow run celeris-stress.yml --repo goceleris/probatorium \
  -f celeris_ref=main -f packages=./engine/iouring \
  -f run='^TestListenCloses(ListenSocketsWhenEveryWorkerRingSetupFails|ListenSocketWhenOneWorkerRingSetupFails|ListenSocketRingAndEventfdWhenInitialSubmitFails)$' \
  -f count=50 -f shards=4 -f arches=both -f memlock=unlimited -f race=true -f timeout=20m \
  -f extra='CELERIS_REQUIRE_IOURING_WORKERS=1'
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
(`selfTestCases` in `tools/stresstally/plan.go`):

| case | what runs | expected |
|---|---|---|
| `good` | pure tests with subtests, `-race`, 3 runs x 2 shards per arch, one test skipping under `-short` | PASS; each test PASS exactly 6 times per arch; the skipping test SKIP 6 and PASS 0 |
| `fail` | the three celeris#656 tests at 8 MiB with `CELERIS_REQUIRE_IOURING_WORKERS=1` | FAIL (`test-failures` only); each test FAIL once per arch |
| `skip` | the same tests at 8 MiB without it | FAIL (`nothing-ran` only); each test SKIP once per arch |
| `timeout` | a test that needs 1.3 s under a 1 s go test timeout | FAIL; every shard `UNPARSED` (`timeout`); the test has one run without a verdict |
| `none` | a `-run` that matches nothing (go test exits 0) | FAIL; every shard `UNPARSED` (`no-verdicts`) |

The run is green only when every case comes out exactly as expected, so a
change that let a failure, a skip, a timeout or an empty run pass as a PASS
turns it red.
