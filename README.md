# probatorium

Bench + production-readiness validation suite for [celeris](https://github.com/goceleris/celeris).

Drives a 3-host cluster (msa2-client + msa2-server + msr1) via ansible. msa2-client runs the loadgen + validator; msa2-server (amd64) and msr1 (aarch64) host the framework under test. Traffic flows over a 20G LACP fabric.

## What it does

| Tier | Hosts | Purpose | Default duration |
|---|---|---|---|
| **bench** | msa2-client → {msa2-server, msr1} | Throughput + latency-at-SLO across celeris and 13 competitor frameworks (Go, Rust, Bun, Python) | 5 runs × 120s + 30s warmup per cell |
| **validation** | msa2-client → {msa2-server, msr1} | Continuous property checks + RESTler-style fuzzing + replay-able deterministic fault injection | 6h validate / 24h+ soak |

Bench headline metric: **`latency_at_slo`** — max sustained RPS at which P99 stays under {10, 50, 100, 500, 1000} ms.

Validation operational claim: **10 days continuous soak with zero invariant violations on both archs → release is production ready.**

## Quick start

```sh
# Cluster reachability + manifest state.
mage Status

# Cross-compile every binary + ship to the cluster.
# DEPLOY_COMPETITORS=go-only skips native toolchains (sub-minute deploy).
CLUSTER_USE_LAN=1 DEPLOY_COMPETITORS=go-only mage Deploy

# Smoke bench: 2 servers × 1 run × 15s on amd64.
CLUSTER_USE_LAN=1 \
  BENCH_TARGET=msa2-server \
  BENCH_COMPETITORS=stdhttp,gin \
  BENCH_DURATION=15s BENCH_WARMUP=3s BENCH_RUNS=1 \
  mage Bench

# Validation smoke: 10-min Tier 1 + Tier 3 against amd64.
CLUSTER_USE_LAN=1 \
  VALIDATE_TARGET=msa2-server VALIDATE_DURATION=10m \
  mage Validate

# Always-on cluster pristine reset.
CLUSTER_USE_LAN=1 mage Cleanup
```

`CLUSTER_USE_LAN=1` pins traffic to the 20G LACP fabric (192.168.50.0/24) instead of the Tailscale overlay. Required for any meaningful bench — Tailscale adds a ~30µs latency floor that swamps the smaller cells.

## Validation tier

Three concurrent pipelines drive every validation run.

### Tier 1 — always-on property stress

Five slices fan over `Concurrency` walker goroutines. Walker budget activates progressively so small smoke runs don't pay for the expensive slices:

| Slice | % of walkers | Activates at concurrency ≥ | What it does |
|---|---|---|---|
| Markov | ~60% | 1 | Session-shaped traffic over the refapp's OpenAPI endpoints, transitions weighted by `validation/markov/auth_session_ratelimit.yaml` |
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

### Tier 2 — RESTler-style stateful fuzzing

Producer/consumer dependency inference from `validation/spec/auth_session_ratelimit.openapi.yaml`. Catches API-level bugs Tier 1 misses (e.g. "DELETE twice → does the second 404 corrupt the session?").

### Tier 3 — deterministic seed replay

Real kernel, no mocking. Seed → workload + fault schedule. Bug = `(seed, git_commit, host_arch)`. Reproducible via `validator-replay --seed=… --commit=… --target=msa2-server`.

50k-seed corpus under `validation/corpus/`. PR CI runs 200 seeds (~10min); soak loops continuously.

### Cross-arch divergence as invariant

amd64 + arm64 share seeds. Any HIGH-severity counter going non-zero on ONE arch but not the other is itself a bug (likely `engine/iouring/sqe.go` write paths). `mage ValidateDiff` walks the two latest `validate-results.json` files, fires non-zero exit on HIGH severity, persists `validate-diff/diff.{txt,json}` for the docs panel.

Auto-runs in `BenchAndValidate` when `VALIDATE_TARGET=both`, and in `validate.yml` + `soak.yml` CI workflows.

## Mage targets

| Target | Env knobs |
|---|---|
| `Status` | — |
| `Deploy` | `CLUSTER_USE_LAN`, `DEPLOY_COMPETITORS=all\|go-only\|<list>` |
| `Cleanup` | `CLEANUP_HOSTS=all\|<list>` |
| `Bench` | `BENCH_TARGET`, `BENCH_COMPETITORS`, `BENCH_DURATION`, `BENCH_WARMUP`, `BENCH_RUNS`, `BENCH_CELLS`, `CELERIS_VERSION` |
| `BenchSince` | `BASELINE_VERSION=v1.4.2`, `REGRESSION_THRESHOLD=0.05` |
| `Validate` | `VALIDATE_TARGET`, `VALIDATE_DURATION`, `VALIDATE_PARALLEL=1` (both archs concurrently), `CELERIS_VERSION`, `PROBATORIUM_VALIDATE_DRIVER=ssh` |
| `Soak` | `SOAK_DURATION=24h`, `VALIDATE_TARGET`, `VALIDATE_PARALLEL=1` |
| `ValidateDiff` | `VALIDATE_DIFF_STRICT=1` (treat MED as failure), `VALIDATE_DIFF_HOSTS=a,b` |
| `Fuzz` | `FUZZ_DURATION=30m`, `FUZZ_CORPUS` |
| `Publish` | `PUBLISH_VERSION`, `PUBLISH_EVENT_TYPE=celeris-bench`, `DOCS_TOKEN` |
| `PublishValidate` | same + `PUBLISH_EVENT_TYPE=celeris-validate` |
| `BenchAndValidate` | Validate → ValidateDiff → PublishValidate → Bench → Publish |

`VALIDATE_PARALLEL=1` on a two-arch run fans the per-target ansible-playbook invocations over goroutines, halving wall-clock time on long soaks.

## Result layout

```
results/<ts>-bench-<version>/
  results.json                             # cross-host v5.0 roll-up
  raw/<host>.json
  <TS>-bench-<host>/<RR>-<comp>/
    loadgen.json                           # saturation-mode loadgen.Result
    observer.sqlite                        # 1Hz /proc + runtime metrics
    cpu.log, server.log

results/<ts>-validate-<version>/
  <host>-validate-<refapp>/
    validate-results.json                  # canonical v5 ValidationResults
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
```

## v5 result schema

The validation document mirrors the orchestrator's `tier1TallySnapshot` exactly:

```jsonc
{
  "schema_version": "5.0",
  "host_arch_pair": "msa2-server-amd64",
  "validation_results": {
    "started_at": "...", "finished_at": "...",
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
  }
}
```

Sub-tallies are `map[string]int64` — schema doesn't re-version when the validator grows a counter.

## CI cascade

```
poll-celeris-release.yml (cron 15min)
  → repository_dispatch: celeris-release
    → validate.yml (self-hosted celeris-cluster, 8h timeout)
        mage Validate → ValidateDiff (HARD gate)
                     → PublishValidate (best-effort)
                     → dispatch celeris-validate-passed
      → bench.yml (4h timeout)
          mage Bench → Publish

soak.yml (cron Sundays 02:00 UTC, 36h)
  mage Soak → ValidateDiff → PublishValidate (celeris-soak-validate)
            → Publish (celeris-soak)
```

## Cluster

| host | arch | role |
|---|---|---|
| msa2-client | amd64 | loadgen + validator orchestrator + checker |
| msa2-server | amd64 | server under test |
| msr1 | aarch64 | server under test |

LAN-IP fallback under `CLUSTER_USE_LAN=1` when Tailscale auth has expired.

## License

Apache-2.0.
