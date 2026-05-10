# probatorium

Bench + production-readiness validation suite for [celeris](https://github.com/goceleris/celeris).

Drives a 3-host cluster (msa2-client + msa2-server + msr1) via ansible. msa2-client runs the loadgen; msa2-server (amd64) and msr1 (aarch64) host the framework under test in parallel. Bench traffic flows over a 20G LACP fabric.

## Two tiers

- **bench** — distributed loadgen across celeris and ~13 competitor frameworks (Go, Rust, Bun, Python). Headline metric is `latency_at_slo`: max sustained RPS at which P99 stays under {10, 50, 100, 500, 1000} ms.
- **validation** — long-running soak with property invariants, RESTler-style stateful fuzzing, and a deterministic-seed replay harness for fault injection. After 10 days clean on both archs, the celeris release is operationally production-ready.

## Quick start

```sh
# Cluster reachability + manifest state. Prints "no manifest (pristine)" on
# a freshly provisioned host, or "manifest present, no installs (Go-only
# deploy)" right after a Go-only deploy.
mage Status

# Cross-compile every binary and ship it. DEPLOY_COMPETITORS=go-only skips
# the native (rust/bun/python) toolchains so the deploy stays under a minute.
CLUSTER_USE_LAN=1 DEPLOY_COMPETITORS=go-only mage Deploy

# Smoke bench: 2 servers × 1 run × 15s on the amd64 target. The runner
# auto-deploys if no manifest is present, so you can skip `Deploy` if you
# just want one shot.
CLUSTER_USE_LAN=1 \
  BENCH_TARGET=msa2-server \
  BENCH_COMPETITORS=stdhttp,gin \
  BENCH_DURATION=15s BENCH_WARMUP=3s BENCH_RUNS=1 \
  mage Bench

# Always-on cluster pristine reset. Reverses every install the manifest
# tracked + removes /tmp/celeris-bench/ + the manifest itself.
CLUSTER_USE_LAN=1 mage Cleanup
```

`CLUSTER_USE_LAN=1` pins traffic to the 20G LACP fabric (192.168.50.0/24)
instead of the Tailscale overlay. Recommended for any actual bench — Tailscale
adds a ~30µs latency floor that swamps the smaller cells.

Bench results land under `results/<ts>-bench-<version>/`:

```
results.json                            # cross-host v5.0 roll-up
raw/<host>.json                         # per-host summary + every cell
<TS>-bench-<host>/<RR>-<comp>/
  loadgen.json                          # full loadgen.Result (saturation mode)
  observer.sqlite                       # 1Hz /proc + runtime time series
  cpu.log                               # mpstat -P ALL
  server.log
```

## Cluster

| host | arch | role |
|---|---|---|
| msa2-client | amd64 | loadgen + observer + checker |
| msa2-server | amd64 | server under test |
| msr1 | aarch64 | server under test |

LAN-IP fallback under `CLUSTER_USE_LAN=1` when Tailscale auth has expired.

## License

Apache-2.0.
