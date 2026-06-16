# probatorium

[![Nightly Validation](https://github.com/goceleris/probatorium/actions/workflows/matrix-nightly-tier.yml/badge.svg?branch=main)](https://github.com/goceleris/probatorium/actions/workflows/matrix-nightly-tier.yml?query=branch%3Amain)
[![Weekend Soak](https://github.com/goceleris/probatorium/actions/workflows/matrix-weekend-tier.yml/badge.svg?branch=main)](https://github.com/goceleris/probatorium/actions/workflows/matrix-weekend-tier.yml?query=branch%3Amain)
[![Test](https://github.com/goceleris/probatorium/actions/workflows/test.yml/badge.svg?branch=main)](https://github.com/goceleris/probatorium/actions/workflows/test.yml)
[![Lint](https://github.com/goceleris/probatorium/actions/workflows/lint.yml/badge.svg?branch=main)](https://github.com/goceleris/probatorium/actions/workflows/lint.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](LICENSE)

Bench + production-readiness validation suite for [celeris](https://github.com/goceleris/celeris).

Drives a 3-host cluster (msa2-client + msa2-server + msr1) via ansible. msa2-client orchestrates loadgen + validator; msa2-server (amd64) and msr1 (aarch64) host the framework under test. Traffic flows over a 20G LACP fabric.

The two tier badges above are the canonical release-gate signal for celeris: they reflect whichever celeris commit was on `main` at the time the nightly / weekend tier last ran. Green means the matrix (refapp × engine × arch) is currently clean — no HIGH-severity invariant violations, no cross-engine + cross-arch divergence.

## What it does

| Tier | Hosts | Purpose | Default duration |
|---|---|---|---|
| **bench** | msa2-client → {msa2-server, msr1} | Throughput + latency-at-SLO across celeris and 13 competitor frameworks (Go, Rust, Bun, Python) | 5 runs × 120s + 30s warmup per cell |
| **validation** | msa2-client → {msa2-server, msr1} | Continuous property checks + RESTler-style fuzzing + replay-able deterministic fault injection | 10m PR-tier · 1h nightly · 24h weekend |

Bench headline metric: **`latency_at_slo`** — max sustained RPS at which P99 (HdrHistogram-merged across runs via `goceleris/loadgen` v1.4.4+) stays under {10, 50, 100, 500, 1000} ms.

Validation operational claim: **N days continuous soak with zero invariant violations on both archs across all refapps × all engines → release is production ready.** The "definitive" claim asks for 10 days; the current weekend tier runs 24h on every `main` to track regressions between cycles.

## Quick start

```sh
# Cluster reachability + manifest state.
mage Status

# Cross-compile every binary + ship to the cluster.
# DEPLOY_COMPETITORS=go-only skips native toolchains (sub-minute deploy).
CLUSTER_USE_LAN=1 DEPLOY_COMPETITORS=go-only mage Deploy

# Smoke bench: 2 servers × 15s on amd64 (the bench always runs one pass).
CLUSTER_USE_LAN=1 \
  BENCH_TARGET=msa2-server \
  BENCH_COMPETITORS=stdhttp,gin \
  BENCH_DURATION=15s BENCH_WARMUP=3s \
  mage Bench

# Single-cell validation smoke (one refapp, one engine): 10-min on amd64.
CLUSTER_USE_LAN=1 \
  VALIDATE_TARGET=msa2-server VALIDATE_DURATION=10m \
  mage Validate

# Full-matrix validation: every refapp × every engine, populating Cells[].
CLUSTER_USE_LAN=1 \
  VALIDATE_TARGET=msa2-server VALIDATE_DURATION=10m \
  VALIDATE_MATRIX=1 \
  mage Validate

# Always-on cluster pristine reset.
CLUSTER_USE_LAN=1 mage Cleanup
```

`CLUSTER_USE_LAN=1` pins traffic to the 20G LACP fabric (192.168.50.0/24) instead of the Tailscale overlay. Required for any meaningful bench — Tailscale adds a ~30µs latency floor that swamps the smaller cells.

## Validation tier

Three pipelines drive every validation run.

### Tier 1 — always-on property stress

Five slices fan over `Concurrency` walker goroutines. Walker budget activates progressively so small smoke runs don't pay for the expensive slices:

| Slice | % of walkers | Activates at concurrency ≥ | What it does |
|---|---|---|---|
| Markov | ~60% | 1 | Session-shaped traffic over the refapp's OpenAPI endpoints, transitions weighted by `validation/markov/<refapp>.yaml` |
| Adversarial | ~20% | 1 | Raw-TCP malformed HTTP/1.1 — bad-chunks, oversized headers, NUL in header, CRLF injection, slowloris, double Content-Length |
| h2c upgrade churn | ~10% | 10 | Valid h2c upgrade preambles followed by RST at three different stages — exercises the engine's PauseAccept race (celeris commits ed55fb6 + bd675f9) |
| WS frame torture | ~5% | 20 | Real RFC 6455 handshake then send one of: fragmented-reserved opcode, oversize-payload, unmasked-client, ping-flood, continuation-no-start, invalid-utf8 |
| SSE kill-mid-stream | ~5% | 20 | Establish SSE long-poll, hold for 50–1500ms, RST — broker must clean up the client slot (I-CONN-2 catches a stuck broker) |

Each slice has its own `tally` of counters. HIGH-severity counters (must-be-zero invariants) trip the orchestrator's reactive incident path the FIRST time they go non-zero, firing forensics + auto-bisect mid-run rather than at end-of-run:

| Counter | Predicate ID | Interpretation |
|---|---|---|
| `adv.wrong_accepted > 0` | `I-ADV-ACCEPTED` | Server accepted malformed bytes — RFC violation |
| `h2c.crashed > 0` | `I-H2C-CRASHED` | Engine crashed on upgrade — PauseAccept race fired |
| `ws.accepted_bad_frame > 0` | `I-WS-ACCEPTED` | Server accepted RFC 6455 violation |
| `ws.hang_no_close > 0` | `I-WS-HANG` | WebSocket goroutine wedged |
| `drv.read_after_write_mismatch > 0` | `I-DRV-1` | Postgres / Redis / Memcached driver lost a write |

### Tier 2 — RESTler-style stateful fuzzing

Producer/consumer dependency inference from `validation/spec/<refapp>.openapi.yaml`. Catches API-level bugs Tier 1 misses (e.g. "DELETE twice → does the second 404 corrupt the session?").

### Tier 3 — deterministic seed replay

Real kernel, no mocking. Seed → workload + fault schedule. Bug = `(seed, git_commit, host_arch)`. Reproducible via `validator-replay --seed=… --commit=… --target=msa2-server`.

50k-seed corpus under `validation/corpus/`. PR tier runs 200 seeds (~10min); nightly 1000; weekend continuous-loop.

### Cross-engine + cross-arch divergence as invariant

A matrix run produces a v5.1 `validate-results.json` with `Cells[]` containing one entry per `(refapp, engine, arch)`. `mage ValidateDiff` walks the two latest matrix docs and reports:

- **Cross-engine** divergences: any HIGH-severity counter non-zero on one engine (e.g. `iouring`) but zero on another (`epoll` / `std`) for the same `(refapp, arch)` — typically an iouring/epoll-specific bug.
- **Cross-arch** divergences: same shape, comparing amd64 ↔ aarch64.

Fires non-zero exit on HIGH severity; persists `validate-diff/diff.{txt,json}` for dashboards. Auto-runs in the three CI tiers below.

## Refapps

Each refapp is a separate Go module under `validation/refapp/<slug>/` so the validator binary doesn't pull in every middleware's dependency graph. The matrix runner auto-discovers them at runtime.

| Slug | Coverage |
|---|---|
| `auth_session_ratelimit` | session cookie + ratelimit + WS / SSE detach paths |
| `auth_jwt_csrf` | JWT (HS256), CSRF synchronizer-token, keyauth |
| `kitchen_sink` | 16 stateless middlewares: recovery, requestid, secure, cors, bodylimit, methodoverride, rewrite, redirect, healthcheck, ratelimit, timeout, circuitbreaker, idempotency, singleflight, basicauth + per-route etag/cache |
| `driver_postgres` | native postgres driver + session/postgresstore + I-DRV-1 round-trip |
| `driver_redis` | native redis driver + session/redisstore + ratelimit/redisstore (atomic EVALSHA token-bucket) |
| `driver_memcached` | native memcached driver + session/memcachedstore + ratelimit/memcachedstore (CAS-loop token-bucket) |
| `observability` | logger + metrics + otel, scraped via `/metrics` for histogram-monotonicity + log-drop invariants |
| `static_swagger_proxy` | static (embed.FS) + swagger (OpenAPI 3.0) + proxy (X-Forwarded-For trust) |

Each refapp follows the same shape: own `go.mod`, `engine.go` (`resolveEngine("auto")` → iouring on Linux, std elsewhere), `platform_{linux,other}.go` for the `isLinux()` split, signal-driven graceful shutdown, canonical `ready addr=<bind-addr>` startup line.

## Mage targets

| Target | Env knobs |
|---|---|
| `Status` | — |
| `Deploy` | `CLUSTER_USE_LAN`, `DEPLOY_COMPETITORS=all\|go-only\|<list>` |
| `Cleanup` | `CLEANUP_HOSTS=all\|<list>` |
| `Bench` | `BENCH_TARGET`, `BENCH_COMPETITORS`, `BENCH_DURATION`, `BENCH_WARMUP`, `BENCH_CELLS`, `CELERIS_VERSION` |
| `BenchSince` | `BASELINE_VERSION=v1.4.3`, `REGRESSION_THRESHOLD=0.05` |
| `Validate` | `VALIDATE_TARGET`, `VALIDATE_DURATION`, `VALIDATE_PARALLEL=1`, `VALIDATE_MATRIX=1`, `VALIDATE_MATRIX_REFAPPS=<csv>`, `VALIDATE_MATRIX_ENGINES=<csv>`, `VALIDATE_REFAPP_ENGINE`, `CELERIS_VERSION`, `PROBATORIUM_VALIDATE_DRIVER=ssh` |
| `Soak` | `SOAK_DURATION=24h`, `VALIDATE_TARGET`, `VALIDATE_PARALLEL=1`, `VALIDATE_MATRIX=1` |
| `ValidateDiff` | `VALIDATE_DIFF_STRICT=1` (treat MED as failure), `VALIDATE_DIFF_HOSTS=a,b` |
| `Fuzz` | `FUZZ_DURATION=30m`, `FUZZ_CORPUS` |
| `Publish` | `PUBLISH_VERSION`, `PUBLISH_EVENT_TYPE=celeris-bench`, `DOCS_TOKEN` |
| `PublishValidate` | same + `PUBLISH_EVENT_TYPE=celeris-validate` |
| `BenchAndValidate` | Validate → ValidateDiff → PublishValidate → Bench → Publish |

`VALIDATE_PARALLEL=1` on a two-arch run fans the per-target ansible-playbook invocations over goroutines, halving wall-clock time on long soaks.

### Matrix mode (`VALIDATE_MATRIX=1`)

Iterates `(refapp × engine)` cells, runs a fresh orchestrator per cell with a per-cell budget = `total_duration / len(cells)`, emits one matrix-aware v5.1 `validate-results.json` with `Cells[]` populated. Falls back to single-cell behaviour when unset (preserves backward compat). Filter the matrix via:

- `VALIDATE_MATRIX_REFAPPS=driver_postgres,driver_redis` — limit refapps.
- `VALIDATE_MATRIX_ENGINES=iouring,epoll` — limit engines (defaults to OS production set: iouring+epoll+std on linux, std elsewhere).

## Result layout

```
results/<ts>-bench-<version>/
  results.json                             # cross-host v5 roll-up
  raw/<host>.json
  <TS>-bench-<host>/<RR>-<comp>/
    loadgen.json                           # HdrHistogram-bearing loadgen.Result
    observer.sqlite                        # 1Hz /proc + runtime metrics
    cpu.log, server.log

results/<ts>-validate-<version>/
  <host>-validate-<refapp>/
    validate-results.json                  # single-cell v5.1 ValidationResults
    tier1_tally.json                       # Tier 1 sub-tally sidecar
    tier3_tally.json                       # Tier 3 seed corpus sidecar
    incidents/<ts>-<predicate>/            # forensics dossier per violation
      forensics_status.txt
      proc-maps.txt, proc-status.txt, proc-fd.txt
      pprof.heap.gz, pprof.goroutine.txt
      shrink/                              # auto-bisect repro
  validate-diff/
    diff.txt                               # severity-sorted divergence table
    diff.json                              # structured findings for dashboards

results/<ts>-validate-matrix-<arch>/       # matrix-mode runs
  validate-results.json                    # v5.1 top-level (Cells[] populated)
  cell-<NN>-<refapp>-<engine>/
    validate-results.json                  # per-cell single-doc
```

## v5.1 result schema

The matrix-mode document carries a per-cell breakdown; single-cell runs leave `Cells[]` empty and populate `Tier1`/`Tier3` at the top level for back-compat.

```jsonc
{
  "schema_version": "5.1",
  "host_arch_pair": "msa2-server-amd64",
  "validation_results": {
    "started_at": "...", "finished_at": "...",
    "cells": [
      {
        "refapp": "auth_session_ratelimit",
        "engine": "iouring",
        "arch": "amd64",
        "tier_1": {
          "requests_sent": 1234567, "requests_2xx": 1230000, ...,
          "adversarial": { "adv_sent": 50000, "adv_well_rejected": 49998,
                           "adv_wrong_accepted": 0, "adv_hang_until_timeout": 2 },
          "h2c_churn":   { "h2c_sent": 25000, "h2c_upgraded": 0,
                           "h2c_declined": 25000, "h2c_crashed": 0, "h2c_hang": 0 },
          "ws_torture":  { "ws_sent": 12000, "ws_upgraded": 12000,
                           "ws_closed_correctly": 12000, "ws_accepted_bad_frame": 0,
                           "ws_hang_no_close": 0 },
          "sse_kill":    { "sse_sent": 8000, "sse_established": 8000,
                           "sse_events_read": 240000, "sse_killed_mid_stream": 7950,
                           "sse_server_closed_early": 50, "sse_handshake_fail": 0 }
        },
        "tier_3": { "seeds_attempted": 144, "seeds_passed": 144,
                    "seeds_failed": 0, "seeds_errored": 0 }
      },
      { "refapp": "auth_session_ratelimit", "engine": "epoll", "arch": "amd64", ... },
      { "refapp": "auth_session_ratelimit", "engine": "std", "arch": "amd64", ... },
      { "refapp": "kitchen_sink", "engine": "iouring", "arch": "amd64", ... }
    ]
  }
}
```

Sub-tallies are `map[string]int64` — schema doesn't re-version when the validator grows a counter.

## CI cascade

Two parallel ladders:

**Celeris-release-triggered** (gates upstream celeris releases):

```
poll-celeris-release.yml (cron 15min)
  → repository_dispatch: celeris-release
    → validate.yml (self-hosted celeris-cluster, 8h timeout)
        mage Validate → ValidateDiff (HARD gate)
                     → PublishValidate (best-effort)
                     → dispatch celeris-validate-passed
      → bench.yml (4h timeout)
          mage Bench → Publish
```

**Probatorium-internal regression ladder** (matrix-aware, gates probatorium PRs + provides the badges at the top):

```
matrix-pr-tier.yml      (on PR + workflow_dispatch)     10m budget
matrix-nightly-tier.yml (cron 02:00 UTC daily)          1h budget
matrix-weekend-tier.yml (cron 02:00 UTC Sundays)        24h budget
```

All three matrix tiers share `concurrency: matrix-tier-cluster` with `cancel-in-progress: false` so they serialize on the shared cluster — a queued PR-tier waits for any in-flight nightly/weekend before starting. Each tier is a 3-job flow: `setup` → `matrix` → `teardown`.

### Self-hosted runner bootstrap

The matrix tier workflows use `[self-hosted, celeris-cluster]` runners that are provisioned on-demand and torn down at end-of-run (no daemons left on the cluster between tier runs — the pristine rule applies). The bootstrap lives in:

- `.github/actions/cluster-runner-up/` — composite that joins the tailnet via `tailscale/github-action@v3` (ephemeral `tag:ci` node), waits for `/tmp/celeris-bench-manifest.json` to clear, mints a registration token, runs `ansible/runner-setup.yml`, and confirms ≥3 runners online.
- `.github/actions/cluster-runner-down/` — matching teardown: mints removal-token, runs `ansible/runner-teardown.yml`, sweeps any orphan offline runner registrations.
- `ansible/runner-setup.yml` + `ansible/runner-teardown.yml` — per-host provisioning. Everything lives under `/tmp/actions-runner-<host>/`, no systemd unit, no package install.

Operator setup (one-time): see [`ansible/RUNNER_BOOTSTRAP.md`](ansible/RUNNER_BOOTSTRAP.md) — four repo secrets (Tailscale OAuth client + secret, GitHub PAT, cluster SSH key) and a Tailscale ACL rule.

## Cluster

| host | arch | role |
|---|---|---|
| msa2-client | amd64 | loadgen + validator orchestrator + checker |
| msa2-server | amd64 | server under test |
| msr1 | aarch64 | server under test |

LAN-IP fallback under `CLUSTER_USE_LAN=1` when Tailscale auth has expired.

## License

Apache-2.0.
