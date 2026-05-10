# probatorium

Bench + production-readiness validation suite for [celeris](https://github.com/goceleris/celeris).

Drives a 3-host cluster (msa2-client + msa2-server + msr1) via ansible. msa2-client runs the loadgen; msa2-server (amd64) and msr1 (aarch64) host the framework under test in parallel. Bench traffic flows over a 20G LACP fabric.

## Two tiers

- **bench** — distributed loadgen across celeris and ~13 competitor frameworks (Go, Rust, Bun, Python). Headline metric is `latency_at_slo`: max sustained RPS at which P99 stays under {10, 50, 100, 500, 1000} ms.
- **validation** — long-running soak with property invariants, RESTler-style stateful fuzzing, and a deterministic-seed replay harness for fault injection. After 10 days clean on both archs, the celeris release is operationally production-ready.

## Quick start

```sh
mage Status                                    # cluster reachability + manifest state
mage Deploy DEPLOY_COMPETITORS=go-only         # cross-compile + stage binaries
mage Bench BENCH_DURATION=30s BENCH_TARGET=msa2-server
mage Validate VALIDATE_DURATION=6h
mage Cleanup
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
